package rtmp

import "testing"

func TestNormalizeListenAddr(t *testing.T) {
	cases := []struct {
		addr, scheme, want string
	}{
		{"0.0.0.0:1935", "rtmp", "0.0.0.0:1935"},
		{"195.250.31.2:1935", "rtmp", "195.250.31.2:1935"},
		{"195.250.31.2", "rtmp", "195.250.31.2:1935"},
		{"195.250.31.2", "rtmps", "195.250.31.2:443"},
		// The exact mistake a user made: a full RTMP URL pasted into what
		// should be a bare listen address, with no port and a path that
		// means nothing to a TCP listener.
		{"rtmp://195.250.31.2/static", "rtmp", "195.250.31.2:1935"},
		{"rtmps://host.example.com/live", "rtmps", "host.example.com:443"},
		{"rtmp://host.example.com:9000/live/key", "rtmp", "host.example.com:9000"},
		{"  0.0.0.0:1935  ", "rtmp", "0.0.0.0:1935"},
	}
	for _, c := range cases {
		if got := normalizeListenAddr(c.addr, c.scheme); got != c.want {
			t.Errorf("normalizeListenAddr(%q, %q) = %q, want %q", c.addr, c.scheme, got, c.want)
		}
	}
}
