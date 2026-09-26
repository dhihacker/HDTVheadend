// Package rtmp implements an RTMP output: it demuxes a stream's MPEG-TS
// content back into H.264+AAC access units, remuxes them into FLV tags,
// and pushes them to a remote RTMP ingest (e.g. YouTube Live, Twitch) as
// an RTMP client — the common "restream my channel to a platform" use
// case. Only H.264 video and AAC audio are supported, since that's what
// RTMP/FLV itself supports on essentially every modern ingest.
package rtmp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	amf0 "github.com/yutopp/go-amf0"
	flvtag "github.com/yutopp/go-flv/tag"
	yutopprtmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"

	"hdtvheadend/internal/codecs/aac"
	"hdtvheadend/internal/codecs/h264"
	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsmux"
)

const (
	videoChunkStreamID = 6
	audioChunkStreamID = 4
)

// Run demuxes bus's MPEG-TS content and publishes it to the remote RTMP(S)
// server named by rtmpURL (e.g. "rtmp://a.rtmp.youtube.com/live2/xxxx-xxxx"
// or "rtmps://..."), the last path segment being the stream key, until ctx
// is canceled. On disconnect it retries with backoff.
func Run(ctx context.Context, rtmpURL string, bus *streambus.Bus) error {
	u, err := url.Parse(rtmpURL)
	if err != nil {
		return fmt.Errorf("rtmp output: parse url: %w", err)
	}
	useTLS := false
	switch u.Scheme {
	case "rtmp":
	case "rtmps":
		useTLS = true
	default:
		return fmt.Errorf("rtmp output: unsupported scheme %q", u.Scheme)
	}
	dir, key := path.Split(strings.TrimPrefix(u.Path, "/"))
	app := strings.TrimSuffix(dir, "/")
	if key == "" {
		return fmt.Errorf("rtmp output: url must end with a stream key, e.g. rtmp://host/app/streamkey")
	}
	hostport := withDefaultPort(u.Host, useTLS)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := publishOnce(ctx, hostport, useTLS, app, key, rtmpURL, bus); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("rtmp output: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

// withDefaultPort appends the scheme's conventional default port (1935 for
// RTMP, 443 for RTMPS — the port most RTMPS ingests actually listen on,
// since it doubles as the port already open through most firewalls) if
// hostport didn't specify one.
func withDefaultPort(hostport string, useTLS bool) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	if useTLS {
		return net.JoinHostPort(hostport, "443")
	}
	return net.JoinHostPort(hostport, "1935")
}

func publishOnce(ctx context.Context, hostport string, useTLS bool, app, key, tcURL string, bus *streambus.Bus) error {
	var client *yutopprtmp.ClientConn
	var err error
	if useTLS {
		client, err = yutopprtmp.TLSDial("rtmps", hostport, &yutopprtmp.ConnConfig{}, &tls.Config{})
	} else {
		client, err = yutopprtmp.Dial("rtmp", hostport, &yutopprtmp.ConnConfig{})
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", hostport, err)
	}
	defer client.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	}()

	if err := client.Connect(&rtmpmsg.NetConnectionConnect{Command: rtmpmsg.NetConnectionConnectCommand{
		App: app, TCURL: tcURL, FlashVer: "FMLE/3.0 (compatible; HDTVheadend)",
	}}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	stream, err := client.CreateStream(nil, 4096)
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}

	if err := stream.Publish(&rtmpmsg.NetStreamPublish{PublishingName: key, PublishingType: "live"}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	ch, unsub := bus.Subscribe(1024)
	defer unsub()

	demux := tsmux.NewDemuxer()
	w := &rtmpWriter{stream: stream}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				return nil
			}
			var writeErr error
			demux.Feed(chunk, func(au tsmux.AccessUnit) {
				if writeErr != nil {
					return
				}
				if au.Video {
					writeErr = w.writeVideo(au)
				} else {
					writeErr = w.writeAudio(au)
				}
			})
			if writeErr != nil {
				return writeErr
			}
		}
	}
}

// rtmpWriter tracks the state needed to convert TS access units to FLV/RTMP
// tags: the last SPS/PPS (to know when a new sequence header must be sent)
// and whether the AAC sequence header has been sent yet.
type rtmpWriter struct {
	stream *yutopprtmp.Stream

	sentVideoCfg     bool
	lastSPS, lastPPS []byte

	sentAudioCfg bool
	sentMetadata bool
}

func (w *rtmpWriter) writeVideo(au tsmux.AccessUnit) error {
	nalus := h264.SplitAnnexB(au.Data)

	var sps, pps []byte
	var frameNALUs [][]byte
	keyframe := false
	for _, n := range nalus {
		switch h264.Type(n) {
		case h264.NALTypeSPS:
			sps = n
		case h264.NALTypePPS:
			pps = n
		case h264.NALTypeAUD:
			// dropped: RTMP/FLV doesn't carry access unit delimiters
		case h264.NALTypeIDR:
			keyframe = true
			frameNALUs = append(frameNALUs, n)
		default:
			frameNALUs = append(frameNALUs, n)
		}
	}

	if len(sps) > 0 && (!bytes.Equal(sps, w.lastSPS) || !bytes.Equal(pps, w.lastPPS)) {
		cfg, err := h264.BuildAVCDecoderConfigurationRecord(h264.DecoderConfig{SPS: sps, PPS: pps})
		if err != nil {
			return fmt.Errorf("rtmp output: build AVC sequence header: %w", err)
		}
		if err := w.writeVideoTag(0, flvtag.VideoData{
			FrameType:     flvtag.FrameTypeKeyFrame,
			CodecID:       flvtag.CodecIDAVC,
			AVCPacketType: flvtag.AVCPacketTypeSequenceHeader,
			Data:          bytes.NewReader(cfg),
		}); err != nil {
			return err
		}
		w.lastSPS, w.lastPPS = sps, pps
		w.sentVideoCfg = true

		if !w.sentMetadata {
			// Real encoders (OBS, ffmpeg's own flv muxer) always send
			// this "@setDataFrame"/onMetaData message right after
			// publish, before any audio/video — this package never did,
			// and at least one real-world RTMP ingest was confirmed to
			// silently accept the connection and publish command but
			// then reset the connection a few seconds later without it
			// (while both OBS and ffmpeg push to the very same URL
			// successfully). Not fatal if it fails: better to keep
			// streaming than abort over a best-effort informational
			// message that many other ingests don't require at all.
			if err := w.writeMetadata(sps); err != nil {
				log.Printf("rtmp output: send onMetaData: %v", err)
			}
			w.sentMetadata = true
		}
	}

	if !w.sentVideoCfg || len(frameNALUs) == 0 {
		return nil // wait for a sequence header (and an actual slice) before sending frames
	}

	avcc := h264.AnnexBToAVCC(frameNALUs)
	frameType := flvtag.FrameTypeInterFrame
	if keyframe {
		frameType = flvtag.FrameTypeKeyFrame
	}
	compositionTimeMS := int32((au.PTS - au.DTS) / 90)
	dtsMS := uint32(au.DTS / 90)

	return w.writeVideoTag(dtsMS, flvtag.VideoData{
		FrameType:       frameType,
		CodecID:         flvtag.CodecIDAVC,
		AVCPacketType:   flvtag.AVCPacketTypeNALU,
		CompositionTime: compositionTimeMS,
		Data:            bytes.NewReader(avcc),
	})
}

// writeMetadata sends the "@setDataFrame"/onMetaData message describing
// the stream, matching what OBS and ffmpeg's own FLV muxer always send
// right after publish. sps is used to fill in width/height on a
// best-effort basis (see h264.ParseSPSDimensions's own doc comment: this
// is purely informational, never required for actual decoding, so a
// parse failure here just omits those two fields rather than failing the
// whole message).
func (w *rtmpWriter) writeMetadata(sps []byte) error {
	data := amf0.ECMAArray{
		"duration":     float64(0),
		"videocodecid": float64(7),  // AVC
		"audiocodecid": float64(10), // AAC
		"encoder":      "HDTVheadend",
	}
	if dim, err := h264.ParseSPSDimensions(sps); err == nil {
		data["width"] = float64(dim.Width)
		data["height"] = float64(dim.Height)
	}
	return w.stream.WriteDataMessage(audioChunkStreamID, 0, "@setDataFrame", &rtmpmsg.NetStreamSetDataFrame{
		AmfData: data,
	})
}

func (w *rtmpWriter) writeVideoTag(timestamp uint32, data flvtag.VideoData) error {
	buf := new(bytes.Buffer)
	if err := flvtag.EncodeVideoData(buf, &data); err != nil {
		return err
	}
	return w.stream.Write(videoChunkStreamID, timestamp, &rtmpmsg.VideoMessage{Payload: buf})
}

func (w *rtmpWriter) writeAudio(au tsmux.AccessUnit) error {
	frames, err := aac.SplitADTS(au.Data)
	if err != nil {
		return fmt.Errorf("rtmp output: split ADTS: %w", err)
	}
	for _, f := range frames {
		if !w.sentAudioCfg {
			asc := aac.BuildAudioSpecificConfig(f.Profile, f.SampleRateIndex, f.ChannelConfig)
			if err := w.writeAudioTag(0, flvtag.AudioData{
				SoundFormat: flvtag.SoundFormatAAC, SoundRate: flvtag.SoundRate44kHz,
				SoundSize: flvtag.SoundSize16Bit, SoundType: flvtag.SoundTypeStereo,
				AACPacketType: flvtag.AACPacketTypeSequenceHeader, Data: bytes.NewReader(asc),
			}); err != nil {
				return err
			}
			w.sentAudioCfg = true
		}
		ptsMS := uint32(au.PTS / 90)
		if err := w.writeAudioTag(ptsMS, flvtag.AudioData{
			SoundFormat: flvtag.SoundFormatAAC, SoundRate: flvtag.SoundRate44kHz,
			SoundSize: flvtag.SoundSize16Bit, SoundType: flvtag.SoundTypeStereo,
			AACPacketType: flvtag.AACPacketTypeRaw, Data: bytes.NewReader(f.Payload),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (w *rtmpWriter) writeAudioTag(timestamp uint32, data flvtag.AudioData) error {
	buf := new(bytes.Buffer)
	if err := flvtag.EncodeAudioData(buf, &data); err != nil {
		return err
	}
	return w.stream.Write(audioChunkStreamID, timestamp, &rtmpmsg.AudioMessage{Payload: buf})
}
