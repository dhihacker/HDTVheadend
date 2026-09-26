// Package playlist generates the /playlist.m3u8 channel list from the
// configured streams.
package playlist

import (
	"fmt"
	"net/http"
	"strings"

	"hdtvheadend/internal/config"
)

// Handler returns an http.HandlerFunc serving an M3U playlist of every
// enabled stream's HTTP-TS output(s), suitable for Jellyfin/Plex/TiviMate.
func Handler(cfg func() *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := cfg()
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base := fmt.Sprintf("%s://%s", scheme, r.Host)

		var b strings.Builder
		b.WriteString("#EXTM3U\n")
		for _, s := range c.Streams {
			if !s.Enabled {
				continue
			}
			path := firstHTTPTSPath(s)
			if path == "" {
				continue
			}
			b.WriteString(fmt.Sprintf(
				"#EXTINF:-1 tvg-id=%q tvg-logo=%q group-title=%q,%s\n%s%s\n",
				s.EPGID, s.LogoURL, s.Group, s.Name, base, path,
			))
		}

		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(b.String()))
	}
}

func firstHTTPTSPath(s config.Stream) string {
	for _, o := range s.Outputs {
		if o.Type == config.OutputHTTPTS && o.Path != "" {
			return o.Path
		}
	}
	return ""
}
