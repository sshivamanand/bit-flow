package store

import "golang.org/x/sys/windows"

// diskFree returns the bytes available to this user on the volume holding path.
func diskFree(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return -1, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return -1, err
	}
	return int64(avail), nil
}
