package testutil

import (
	"fmt"
	"sync"
)

// IDSeq mints deterministic, monotonically increasing identifiers of the form
// "<prefix>0001". Tests use it where the store would otherwise mint a random
// ULID, so that failures name a specific id instead of a fresh random one.
//
// It is safe for concurrent use.
type IDSeq struct {
	mu     sync.Mutex
	prefix string
	n      int
}

// NewIDSeq returns an IDSeq producing ids beginning with prefix.
func NewIDSeq(prefix string) *IDSeq { return &IDSeq{prefix: prefix} }

// Next returns the next id.
func (s *IDSeq) Next() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("%s%04d", s.prefix, s.n)
}
