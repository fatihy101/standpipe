//go:build unix

package store

import (
	"fmt"
	"syscall"
)

// FreeBytes reports the bytes available to an unprivileged process on the
// filesystem holding path.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("store: statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
