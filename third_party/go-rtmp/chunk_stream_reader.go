//
// Copyright (c) 2018- yutopp (yutopp@gmail.com)
//
// Distributed under the Boost Software License, Version 1.0. (See accompanying
// file LICENSE_1_0.txt or copy at  https://www.boost.org/LICENSE_1_0.txt)
//

package rtmp

import (
	"bytes"
)

// ExtendedTimestampMode records whether a chunk stream's timestamp (or
// timestamp delta) is carried via the 4-byte extended timestamp field —
// used when the regular 24-bit field would overflow (timestamps at or
// past 0xFFFFFF, i.e. RTMP streams running past ~4.66 hours, or any
// stream whose incoming timestamps already start with a large offset).
// Needed on a Type 3 (fmt=3) chunk, which otherwise carries no timestamp
// fields of its own and must remember whether the Type 0/1/2 chunk that
// started this message used one; get this wrong and Type 3 chunks
// mis-consume (or fail to consume) the 4 extended-timestamp bytes,
// desyncing the reader's byte count from the actual message boundary —
// surfacing later as `panic("invalid state")` in readChunk once the
// reader thinks it already has a full message before it actually does
// (fixed upstream: https://github.com/yutopp/go-rtmp/pull/72).
type ExtendedTimestampMode byte

const (
	ExtendedTimestampUnused    ExtendedTimestampMode = 0
	ExtendedTimestampUsed      ExtendedTimestampMode = 1
	ExtendedTimestampDeltaUsed ExtendedTimestampMode = 2
)

type ChunkStreamReader struct {
	basicHeader   chunkBasicHeader
	messageHeader chunkMessageHeader

	timestamp       uint32
	timestampDelta  uint32
	messageLength   uint32 // max, 24bits
	messageTypeID   byte
	messageStreamID uint32

	extendedTimestampMode ExtendedTimestampMode

	buf       bytes.Buffer
	completed bool
}

func (r *ChunkStreamReader) Read(b []byte) (int, error) {
	return r.buf.Read(b)
}
