//go:build linux

package gallery

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// FileBirthTime returns the file creation time (birthtime) if available,
// falling back to ModTime().
func FileBirthTime(fi os.FileInfo, path string) time.Time {
	if fi == nil {
		return time.Time{}
	}
	if path != "" {
		var stat unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err == nil {
			if stat.Mask&unix.STATX_BTIME != 0 && stat.Btime.Sec > 0 {
				return time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec))
			}
		}
	}
	return fi.ModTime()
}
