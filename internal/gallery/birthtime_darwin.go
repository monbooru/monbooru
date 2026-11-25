//go:build darwin

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
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		if sys.Birthtimespec.Sec > 0 {
			return time.Unix(sys.Birthtimespec.Sec, sys.Birthtimespec.Nsec)
		}
	}
	return fi.ModTime()
}
