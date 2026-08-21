//go:build !unix

package store

import "errors"

// FreeBytes is not implemented outside unix. The gateway is deployed on Linux;
// returning an error rather than a fabricated number keeps the free-space guard from
// silently passing.
func FreeBytes(path string) (uint64, error) {
	return 0, errors.New("store: free space check not supported on this platform")
}
