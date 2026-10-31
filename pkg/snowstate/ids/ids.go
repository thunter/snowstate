// Package ids generates the UUIDs snowstate stores. It is a few lines of
// stdlib code, so it avoids a dependency that §2 of the spec does not list.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

func format(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// V4 returns a random UUID.
func V4() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return format(b)
}

// v7 state makes IDs strictly increasing within a process, using the 12-bit
// rand_a field as a sequence (RFC 9562 §6.2, method 1). Version listings and GC
// order by ID, so two writes in one millisecond must not sort randomly.
var (
	v7Mu   sync.Mutex
	v7Last int64
	v7Seq  uint16
)

const v7MaxSeq = 0xfff

func nextV7() (int64, uint16) {
	v7Mu.Lock()
	defer v7Mu.Unlock()
	now := time.Now().UnixMilli()
	if now > v7Last {
		v7Last, v7Seq = now, 0
		return v7Last, v7Seq
	}
	// Same millisecond, or the clock stepped back: stay ahead of the last ID.
	if v7Seq == v7MaxSeq {
		v7Last, v7Seq = v7Last+1, 0
		return v7Last, v7Seq
	}
	v7Seq++
	return v7Last, v7Seq
}

// V7 returns a time-ordered UUID, strictly increasing within a process.
func V7() string {
	ms, seq := nextV7()
	var b [16]byte
	_, _ = rand.Read(b[:])
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], uint64(ms))
	copy(b[0:6], be[2:8])
	b[6] = 0x70 | byte(seq>>8)
	b[7] = byte(seq)
	b[8] = b[8]&0x3f | 0x80
	return format(b)
}
