//go:build !linux

package web

import "github.com/monbooru/monbooru/internal/shared/desktop"

func mountUsage(path string) (mountStats, uint64, bool) {
	total, free, err := desktop.DiskUsage(path)
	if err != nil {
		return mountStats{}, 0, false
	}
	return usageStats(total, free), 0, true
}
