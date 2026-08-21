package store

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford is the ULID alphabet (Crockford base32, excluding I, L, O and U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewEventID mints an event id in the form the spec's examples use:
// "evt_" + a 26-character ULID (ids must be unique and sortable, "ULID or
// UUIDv7"). Sortable ids matter because the resolver and the export walk the
// log in time order.
func NewEventID() string { return "evt_" + newULID(time.Now()) }

// NewRequestID mints a request-scoped id ("req_" + ULID). Kept here so the
// gateway and the store agree on one format.
func NewRequestID() string { return "req_" + newULID(time.Now()) }

func newULID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	// 48-bit big-endian timestamp
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// 80 bits of randomness. crypto/rand.Read never returns an error since
	// Go 1.24; it panics instead of producing a non-random id.
	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	copy(b[6:], r[:])

	// Base32-encode 128 bits into 26 characters, most significant first.
	var out [26]byte
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = (lo >> 5) | (hi << 59)
		hi >>= 5
	}
	return string(out[:])
}
