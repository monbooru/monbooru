package web

import (
	"slices"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/tagger"
)

type galleryDBStats struct {
	Name   string
	DBPath string
	DBSize int64
}

type mountStats struct {
	Labels    []string
	TotalSize int64
	FreeSize  int64
	UsedSize  int64
	UsedPct   int
}

type taggerCacheStats struct {
	Loaded           bool
	Provider         string
	InUse            bool
	Sessions         []string
	IdleFor          time.Duration
	IdleReleaseAfter time.Duration
	WorkerPID        int
	WorkerRSS        int64
	WorkerAnon       int64
	WorkerFile       int64
}

type statsData struct {
	Mem        desktop.Memory
	Galleries  []galleryDBStats
	Mounts     []mountStats
	FSWarnings []string
	Tagger     taggerCacheStats
}

// Cheap by design: no directory walks.
func (s *Server) gatherStats() statsData {
	out := statsData{Mem: desktop.ReadMemory(), Tagger: gatherTaggerStats(s)}

	galleries := s.galleryList()
	out.Galleries = make([]galleryDBStats, 0, len(galleries))
	for _, g := range galleries {
		out.Galleries = append(out.Galleries, galleryDBStats{
			Name:   g.Name,
			DBPath: g.DBPath,
			DBSize: dbFileSize(g.DBPath),
		})
	}

	type mountAcc struct {
		galleries []string
		stats     mountStats
	}
	byKey := map[string]*mountAcc{}
	var order []*mountAcc
	addProbe := func(galleryName, path string) {
		if path == "" {
			return
		}
		st, fsid, ok := mountUsage(path)
		if !ok {
			return
		}
		// Some filesystems (overlay, some tmpfs) report fsid 0; keying
		// those by path keeps distinct volumes apart.
		key := strconv.FormatUint(fsid, 10)
		if fsid == 0 {
			key = "path:" + path
		}
		if acc, ok := byKey[key]; ok {
			if !slices.Contains(acc.galleries, galleryName) {
				acc.galleries = append(acc.galleries, galleryName)
			}
			return
		}
		acc := &mountAcc{
			galleries: []string{galleryName},
			stats:     st,
		}
		byKey[key] = acc
		order = append(order, acc)
	}
	for _, g := range galleries {
		addProbe(g.Name, g.DBPath)
		addProbe(g.Name, g.GalleryPath)
		addProbe(g.Name, g.ThumbnailsPath)
	}
	// Docker bind mounts and overlay layers report distinct fsids for one
	// filesystem, so rows with identical sizes merge too.
	type sizeKey struct{ Total, Free, Used int64 }
	bySize := map[sizeKey]int{}
	out.Mounts = make([]mountStats, 0, len(order))
	for _, m := range order {
		st := m.stats
		st.Labels = m.galleries
		key := sizeKey{st.TotalSize, st.FreeSize, st.UsedSize}
		if idx, ok := bySize[key]; ok {
			for _, name := range st.Labels {
				if !slices.Contains(out.Mounts[idx].Labels, name) {
					out.Mounts[idx].Labels = append(out.Mounts[idx].Labels, name)
				}
			}
			continue
		}
		bySize[key] = len(out.Mounts)
		out.Mounts = append(out.Mounts, st)
	}
	if len(out.Mounts) == 0 {
		out.FSWarnings = append(out.FSWarnings,
			"Filesystem usage unavailable on this platform.")
	}
	return out
}

func gatherTaggerStats(s *Server) taggerCacheStats {
	st := tagger.Status()
	out := taggerCacheStats{
		Loaded:   st.Loaded,
		Provider: st.Provider,
		InUse:    st.InUse,
		Sessions: st.Sessions,
	}
	s.cfgMu.RLock()
	mins := s.cfg.Tagger.IdleReleaseAfterMinutes
	s.cfgMu.RUnlock()
	if mins > 0 {
		out.IdleReleaseAfter = time.Duration(mins) * time.Minute
	}
	if st.Loaded && !st.InUse && !st.LastUsed.IsZero() {
		out.IdleFor = time.Since(st.LastUsed)
	}
	if pid, ok := tagger.WorkerPID(); ok {
		out.WorkerPID = pid
		if r, rok := desktop.ProcRSS(pid); rok {
			out.WorkerRSS = int64(r.Total)
			out.WorkerAnon = int64(r.Anon)
			out.WorkerFile = int64(r.File)
		}
	}
	return out
}

func usageStats(total, free uint64) mountStats {
	used := total - min(free, total)
	pct := 0
	if total > 0 {
		pct = int((used * 100) / total)
	}
	return mountStats{TotalSize: int64(total), FreeSize: int64(free), UsedSize: int64(used), UsedPct: pct}
}
