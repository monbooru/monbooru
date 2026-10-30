package web

import (
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
)

type schedHost struct{ s *Server }

// Unpaired, the lookup phases are off in this copy only; the saved
// switches wait for the next pairing.
func (h schedHost) View() (config.ScheduleConfig, []string) {
	h.s.cfgMu.RLock()
	defer h.s.cfgMu.RUnlock()
	sched := h.s.cfg.Schedule
	if !h.s.cfg.MonloaderPaired() {
		sched.LookupPTR, sched.LookupBooru = false, false
	}
	return sched, galleryNames(h.s.cfg.Galleries)
}

func (h schedHost) Config() *config.Config { return h.s.cfgSnapshot() }

func (h schedHost) Naming(name string) gallery.Naming { return h.s.ingestNaming(name) }

func (h schedHost) MemoryMark() func(scope string) { return autotagMemoryMark() }
