//go:build windows

package gallery

import (
	"os"
	"syscall"
	"time"
)

// FileBirthTime returns the file creation time (birthtime) if available,
// falling back to ModTime().
func FileBirthTime(fi os.FileInfo, path string) time.Time {
	if fi == nil {
		return time.Time{}
	}
	if sys, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		nsec := sys.CreationTime.Nanoseconds()
		if nsec > 0 {
			return time.Unix(0, nsec)
		}
	}
	return fi.ModTime()
}
