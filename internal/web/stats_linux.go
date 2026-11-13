//go:build linux

package web

import "syscall"

func mountUsage(path string) (mountStats, uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return mountStats{}, 0, false
	}
	// Bavail, as df reports it: Bfree also counts the root-reserved blocks.
	bsize := uint64(st.Bsize)
	fsid := uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
	return usageStats(st.Blocks*bsize, st.Bavail*bsize), fsid, true
}
