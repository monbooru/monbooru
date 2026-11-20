package web

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/pairing"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/shared/worker"
	"github.com/monbooru/monbooru/internal/tagger"
)

type workerModel struct {
	Name string
	// Differs names the worker whose files the model was taken from, when
	// this one holds other files under the same name.
	Differs string
}

type workerRow struct {
	Name, Version, URL, Provider, ModelsDir string
	Local, Paused, Disabled                 bool
	// Dot is the footer dot's state: ok, down or paused.
	Dot, State string
	Models     []workerModel
	// Loaded names the models the worker has loaded now.
	Loaded []string
}

type workersData struct {
	Rows    []workerRow
	Pending []peer.Req
	// LocalFound is a monworker beside monbooru or on PATH; LocalPin the
	// variable that sets local_worker, when one does.
	LocalFound bool
	LocalPin   string
	OOB        bool
}

// None is no worker to tag with, running or not.
func (d workersData) None() bool {
	return !slices.ContainsFunc(d.Rows, func(r workerRow) bool { return !r.Disabled })
}

func (s *Server) workersView() workersData {
	cfg := s.cfgSnapshot()
	d := workersData{LocalFound: s.local != nil && s.local.path != "", LocalPin: cfg.EnvPinned("tagger.local_worker")}
	views := tagger.CurrentPool().Workers()
	if d.LocalFound && !slices.ContainsFunc(views, func(v tagger.WorkerView) bool { return v.Local }) {
		d.Rows = append(d.Rows, workerRow{Name: tagger.LocalWorker, Local: true, Disabled: true, Dot: "paused", State: "disabled"})
	}
	first := map[string]tagger.WorkerView{}
	for _, v := range views {
		row := workerRow{Name: v.Name, Version: v.Info.Version, URL: v.URL, Local: v.Local, Paused: v.Paused, ModelsDir: v.Info.ModelsDir,
			Loaded: slices.Sorted(slices.Values(v.Info.Loaded))}
		if v.Info.Provider != "" {
			row.Provider = config.ExecutionProviderLabel(v.Info.Provider)
		}
		row.Dot, row.State = workerState(v)
		for _, m := range v.Models {
			wm := workerModel{Name: m.Name}
			if f, ok := first[m.Name]; !ok {
				first[m.Name] = v
			} else if held := modelDigest(f, m.Name); held != "" && m.Digest != "" && held != m.Digest {
				wm.Differs = f.Name
			}
			row.Models = append(row.Models, wm)
		}
		d.Rows = append(d.Rows, row)
	}
	for _, req := range s.pairs.ListPending() {
		if req.App == contract.AppMonworker {
			d.Pending = append(d.Pending, req)
		}
	}
	return d
}

func workerState(v tagger.WorkerView) (dot, state string) {
	switch {
	case v.Paused:
		return "paused", "paused"
	case !v.Checked:
		return "paused", "starting"
	case !v.Up:
		if v.DownSince.IsZero() {
			return "down", "offline"
		}
		return "down", "offline since " + v.DownSince.Local().Format("15:04")
	case v.InUse || v.Info.Busy:
		return "ok", "tagging"
	case v.Standby:
		return "ok", "standby"
	}
	return "ok", "idle"
}

func modelDigest(v tagger.WorkerView, name string) string {
	i := slices.IndexFunc(v.Models, func(m worker.Model) bool { return m.Name == name })
	if i < 0 {
		return ""
	}
	return v.Models[i].Digest
}

func (s *Server) renderWorkers(w http.ResponseWriter, oob bool) {
	d := s.workersView()
	d.OOB = oob
	s.renderTemplate(w, "partials/tagger_workers.html", d)
}

func (s *Server) taggerWorkersGet(w http.ResponseWriter, r *http.Request) {
	s.renderWorkers(w, false)
}

func (s *Server) taggerStatsGet(w http.ResponseWriter, r *http.Request) {
	peek, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	tagger.CurrentPool().Peek(peek)
	cancel()
	s.renderTemplate(w, "partials/tagger_stats.html", gatherTaggerStats(s))
}

// The rest of the section shows or hides with the local worker, so the
// page reloads.
func (s *Server) taggerLocalWorkerPost(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseFormOK(w, r) {
			return
		}
		if err := s.withConfig(func(c *config.Config) error {
			c.Tagger.LocalWorker = on
			return nil
		}); err != nil {
			writeFlashOOB(w, "flash-tagger", "err", "Could not save: "+err.Error())
			return
		}
		logx.Infof("settings: local worker %s from %s", map[bool]string{true: "enabled", false: "disabled"}[on], session.ClientIP(r))
		w.Header().Set("HX-Refresh", "true")
	}
}

func (s *Server) taggerWorkerPausePost(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !parseFormOK(w, r) {
			return
		}
		name := r.PathValue("name")
		if err := s.withConfig(func(c *config.Config) error {
			if wc := peer.FindWorker(c, name); wc != nil {
				wc.Paused = paused
			}
			return nil
		}); err != nil {
			writeFlashOOB(w, "flash-tagger", "err", "Could not save: "+err.Error())
		}
		s.renderWorkers(w, false)
	}
}

// Remove tells the worker first, so it stops offering itself, then drops
// the pairing here even when the worker could not be reached.
func (s *Server) taggerWorkerRemovePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	var url, token string
	s.cfgMu.RLock()
	if wc := peer.FindWorker(s.cfg, name); wc != nil {
		url, token = wc.URL, wc.Token
	}
	s.cfgMu.RUnlock()
	notifyErr := peer.NotifyTeardown(name, url, token)
	saveErr := s.removePairing(peer.WorkerKey(name))
	if saveErr == nil {
		logx.Infof("pairing: removed worker %s from %s", name, session.ClientIP(r))
	}
	s.renderWorkers(w, false)
	s.renderAuthTokensOOB(w, r)
	panel := s.pairViewData()
	panel["OOB"] = true
	s.renderTemplate(w, "partials/plugin_pairing.html", panel)
	switch {
	case saveErr != nil:
		writeFlashOOB(w, "flash-tagger", "err", "Could not save: "+saveErr.Error())
	case notifyErr != nil:
		writeFlashOOB(w, "flash-tagger", "warn", "Removed here, but could not reach "+name+" - remove the pairing there too.")
	default:
		writeFlashOOB(w, "flash-tagger", "", "")
	}
}

func (s *Server) workerPairDecided(w http.ResponseWriter, state pairing.State, msg string) {
	s.renderWorkers(w, false)
	if state == pairing.Approved {
		writeFlashOOB(w, "flash-tagger", "", "")
		return
	}
	if msg != "" {
		writeFlashOOB(w, "flash-tagger", "warn", msg)
	}
}

// taggerProvider is what the badge names: the provider of the worker that
// tags first, the local one's setting until one has answered.
func (s *Server) taggerProvider(cfg *config.Config) string {
	for _, v := range tagger.CurrentPool().Workers() {
		if v.Up && !v.Paused && !v.Standby && v.Info.Provider != "" {
			return v.Info.Provider
		}
	}
	return cfg.Tagger.ExecutionProvider
}

type installTarget struct{ Name, Where, Dir string }

// installTargets are the workers a model can be installed on, the local
// one first; with none, the models folder of this machine.
func (s *Server) installTargets(cfg *config.Config, d workersData) []installTarget {
	var out []installTarget
	for _, r := range d.Rows {
		switch {
		case r.Local && !r.Disabled:
			out = append(out, installTarget{Name: r.Name, Where: "this machine", Dir: cfg.Paths.ModelPath})
		case !r.Local && r.ModelsDir != "":
			out = append(out, installTarget{Name: r.Name, Where: r.Name, Dir: r.ModelsDir})
		}
	}
	if len(out) == 0 {
		out = append(out, installTarget{Name: tagger.LocalWorker, Where: "this machine", Dir: cfg.Paths.ModelPath})
	}
	return out
}
