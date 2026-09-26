# Local patch

Vendored from github.com/yutopp/go-rtmp v0.0.7 (Boost Software License 1.0,
see LICENCE.txt) with one fix: `conn_state.go` defaulted `MaxChunkStreams`
and other `int`-typed fields to `math.MaxUint32`, which doesn't fit in a
32-bit `int` — the package failed to compile at all for GOARCH=386 or
GOARCH=arm. Changed those defaults to `math.MaxInt32` (still an effectively
unlimited value for these counters) so it builds on every architecture
HDTVheadend targets, including 32-bit ones.

If upstream fixes this, drop this vendored copy and the `replace` directive
in the root go.mod.
