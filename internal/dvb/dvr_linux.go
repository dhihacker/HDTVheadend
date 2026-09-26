//go:build linux

package dvb

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"hdtvheadend/internal/streambus"
	"hdtvheadend/internal/tsutil"
)

// dmx_input / dmx_output / dmx_ts_pes / dmx_pes_filter_params (linux/dvb/dmx.h).
const (
	dmxInFrontend = 0
	dmxOutTSTap   = 2
	dmxPESOther   = 20
	dmxImmediate  = 4 // DMX_IMMEDIATE_START flag
	fullTSPID     = 0x2000
)

type dmxPESFilterParams struct {
	Pid     uint16
	Input   int32
	Output  int32
	PESType int32
	Flags   uint32
}

var (
	dmxSetPESFilter = ioW('o', 44, unsafe.Sizeof(dmxPESFilterParams{}))
	dmxSetBufSize   = io0('o', 45)
)

// OpenFullTSCapture configures a demux to route the tuner's entire
// transport stream to the DVR device, and returns that DVR device open for
// reading raw 188-byte-aligned TS data.
func OpenFullTSCapture(adapter, demux, dvr int) (*os.File, error) {
	demuxPath := fmt.Sprintf("/dev/dvb/adapter%d/demux%d", adapter, demux)
	df, err := os.OpenFile(demuxPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", demuxPath, err)
	}
	defer df.Close()

	params := dmxPESFilterParams{
		Pid:     fullTSPID,
		Input:   dmxInFrontend,
		Output:  dmxOutTSTap,
		PESType: dmxPESOther,
		Flags:   dmxImmediate,
	}
	if err := ioctlPtr(df, dmxSetPESFilter, unsafe.Pointer(&params)); err != nil {
		return nil, fmt.Errorf("dvb: set full-TS filter: %w", err)
	}

	dvrPath := fmt.Sprintf("/dev/dvb/adapter%d/dvr%d", adapter, dvr)
	rf, err := os.OpenFile(dvrPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("dvb: open %s: %w", dvrPath, err)
	}
	return rf, nil
}

// RunDVR reads raw TS data from an open DVR file and republishes
// 188-byte-aligned chunks to bus until ctx is canceled.
func RunDVR(ctx context.Context, dvr *os.File, bus *streambus.Bus) error {
	go func() {
		<-ctx.Done()
		dvr.Close()
	}()

	var carry []byte
	buf := make([]byte, 64*tsutil.PacketSize)
	for {
		n, err := dvr.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			aligned := (len(data) / tsutil.PacketSize) * tsutil.PacketSize
			if aligned > 0 {
				chunk := make([]byte, aligned)
				copy(chunk, data[:aligned])
				bus.Publish(chunk)
			}
			carry = append([]byte(nil), data[aligned:]...)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dvb: dvr read: %w", err)
		}
	}
}
