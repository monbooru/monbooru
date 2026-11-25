//go:build !windows && !darwin && !linux

package gallery

import (
	"os"
	"time"
)

// FileBirthTime returns the file creation time (birthtime) if available,
// falling back to ModTime().
func FileBirthTime(fi os.FileInfo, path string) time.Time {
	if fi == nil {
		return time.Time{}
	}
	return fi.ModTime()
}
