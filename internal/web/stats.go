package web

import (
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/shared/desktop"
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

// workerStats is one worker as the Stats block shows it; the memory is
// the local worker's only, the one process monbooru can read.
type workerStats struct {
	Name, Provider, State string
	// Up is a worker answering, whose mode and models are worth a row.
	Up         bool
	Loaded     []string
	WorkerPID  int
	WorkerRSS  int64
	WorkerAnon int64
	WorkerFile int64
}

type statsData struct {
	Mem        desktop.Memory
	Galleries  []galleryDBStats
	Mounts     []mountStats
	FSWarnings []string
	Tagger     []workerStats
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

func gatherTaggerStats(s *Server) []workerStats {
	rows := s.workersView().Rows
	out := make([]workerStats, 0, len(rows))
	for _, r := range rows {
		st := workerStats{Name: r.Name, Provider: r.Provider, State: r.State, Up: r.Dot == "ok", Loaded: r.Loaded}
		// The models live in the worker's inference child, so its memory
		// counts with the worker's.
		if pid := s.peers.PID(localWorkerName); r.Local && !r.Disabled && pid > 0 {
			st.WorkerPID = pid
			for _, p := range append([]int{pid}, procChildren(pid)...) {
				if rss, ok := desktop.ProcRSS(p); ok {
					st.WorkerRSS += int64(rss.Total)
					st.WorkerAnon += int64(rss.Anon)
					st.WorkerFile += int64(rss.File)
				}
			}
		}
		out = append(out, st)
	}
	return out
}

// procChildren reads every thread's list: a child is listed under the
// thread that forked it, and Go forks from any of them.
func procChildren(pid int) []int {
	dir := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, task := range tasks {
		data, err := os.ReadFile(dir + "/" + task.Name() + "/children")
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
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
