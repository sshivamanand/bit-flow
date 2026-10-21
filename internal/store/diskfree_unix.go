//go:build darwin || linux || freebsd

package store

import "golang.org/x/sys/unix"

// diskFree returns the bytes available to this user on the filesystem holding path.
func diskFree(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return -1, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
