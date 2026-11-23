package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/desktop"
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
	// Manual is a worker added by address and token, which no pairing
	// brought.
	Manual bool
	// OnDemand is a worker left alone between the jobs that name it.
	OnDemand bool
	// Missing is the local worker turned on with no monworker to run.
	Missing bool
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
	// MonbooruURL is base_url when a worker on another machine can use
	// it; LoopbackBind the bind address when it listens on loopback only.
	MonbooruURL, LoopbackBind string
	// Swap carries the settings nav's count of requests waiting here, on
	// every answer but the page's own render.
	Swap bool
}

// None is no worker to tag with, running or not.
func (d workersData) None() bool {
	return !slices.ContainsFunc(d.Rows, func(r workerRow) bool { return !r.Disabled && !r.Missing })
}

func (s *Server) workersView() workersData {
	cfg := s.cfgSnapshot()
	d := workersData{LocalFound: s.local != nil && s.local.path != "", LocalPin: cfg.EnvPinned("tagger.local_worker")}
	if u, err := url.Parse(cfg.Server.BaseURL); err == nil && u.Host != "" && !desktop.IsLoopbackAddr(u.Host) {
		d.MonbooruURL = cfg.Server.BaseURL
	}
	if desktop.IsLoopbackAddr(cfg.Server.BindAddress) {
		d.LoopbackBind = cfg.Server.BindAddress
	}
	views := tagger.CurrentPool().Workers()
	switch {
	case slices.ContainsFunc(views, func(v tagger.WorkerView) bool { return v.Local }):
	case d.LocalFound:
		d.Rows = append(d.Rows, workerRow{Name: tagger.LocalWorker, Local: true, Disabled: true, Dot: "paused", State: "disabled"})
	case cfg.Tagger.LocalWorker:
		d.Rows = append(d.Rows, workerRow{Name: tagger.LocalWorker, Local: true, Missing: true, Dot: "down", State: "not found"})
	}
	first := map[string]tagger.WorkerView{}
	for _, v := range views {
		row := workerRow{Name: v.Name, Version: v.Info.Version, URL: v.URL, Local: v.Local, Paused: v.Paused, OnDemand: v.OnDemand, ModelsDir: v.Info.ModelsDir,
			Loaded: slices.Sorted(slices.Values(v.Info.Loaded))}
		if wc := peer.FindWorker(cfg, v.Name); wc != nil && !v.Local {
			row.Manual = wc.Manual
		}
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
	case v.OnDemand:
		return onDemandState(v)
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

// An on-demand worker is asleep unless a job has it: offline is kept for
// a start that failed.
func onDemandState(v tagger.WorkerView) (dot, state string) {
	switch {
	case v.Waking:
		return "paused", "starting"
	case v.InUse:
		return "ok", "tagging"
	case v.Checked && !v.Up:
		return "down", "offline since " + v.DownSince.Local().Format("15:04")
	case v.Seen.IsZero():
		return "paused", "asleep"
	}
	return "paused", "asleep, last seen " + v.Seen.Local().Format("15:04")
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
	d.OOB, d.Swap = oob, true
	s.renderTemplate(w, "partials/tagger_workers.html", d)
}

func (s *Server) taggerWorkersGet(w http.ResponseWriter, r *http.Request) {
	peek, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	tagger.CurrentPool().Peek(peek)
	cancel()
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

// Remove drops the pairing here, then tells the worker, so it stops
// offering itself; one that cannot be reached is removed all the same.
func (s *Server) taggerWorkerRemovePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	var url, token string
	s.cfgMu.RLock()
	// A worker added by address holds no pairing to be told the end of.
	if wc := peer.FindWorker(s.cfg, name); wc != nil && !wc.Manual {
		url, token = wc.URL, wc.Token
	}
	s.cfgMu.RUnlock()
	var notifyErr error
	saveErr := s.removePairing(peer.WorkerKey(name))
	if saveErr == nil {
		logx.Infof("pairing: removed worker %s from %s", name, session.ClientIP(r))
		notifyErr = peer.NotifyTeardown(name, url, token)
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
		writeFlashOOB(w, "flash-tagger", "warn", "Removed here, but could not reach "+name+": it offers itself again at its next start.")
	default:
		writeFlashOOB(w, "flash-tagger", "", "")
	}
}

// taggerWorkerAddPost lists a worker monbooru reaches by itself, started
// with a token of the operator's, once it answers with that token.
func (s *Server) taggerWorkerAddPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	def := tagger.WorkerDef{
		Name:     strings.TrimSpace(r.FormValue("name")),
		URL:      strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/"),
		Token:    strings.TrimSpace(r.FormValue("token")),
		Gateway:  strings.TrimSpace(r.FormValue("gateway_token")),
		OnDemand: r.FormValue("on_demand") == "on",
	}
	u, err := url.Parse(def.URL)
	switch {
	case !pairing.ValidApp(def.Name):
		writeInlineFlash(w, "err", "The name takes letters, digits, - and _, 32 at most.")
		return
	case def.Name == tagger.LocalWorker:
		writeInlineFlash(w, "err", "local is the worker monbooru runs itself.")
		return
	case err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https":
		writeInlineFlash(w, "err", "The address must start with http:// or https://.")
		return
	case def.Token == "":
		writeInlineFlash(w, "err", "A token is required: the worker's MONWORKER_TOKEN.")
		return
	}
	taken := func(c *config.Config) bool { return peer.FindWorker(c, def.Name) != nil }
	if taken(s.cfgSnapshot()) {
		writeInlineFlash(w, "err", "A worker named "+def.Name+" is already listed.")
		return
	}
	// A platform that starts the worker for this call holds it meanwhile.
	wait := 20 * time.Second
	if def.OnDemand {
		wait = 150 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	info, err := tagger.CurrentPool().Probe(ctx, def)
	var se *worker.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusUnauthorized:
		writeInlineFlash(w, "err", "The worker at "+def.URL+" refused the token.")
		return
	case err != nil:
		writeInlineFlash(w, "err", "No worker answered at "+def.URL+": "+err.Error())
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		if taken(c) {
			return errors.New("a worker named " + def.Name + " is already listed")
		}
		c.Workers = append(c.Workers, config.WorkerConfig{Name: def.Name, URL: def.URL, Token: def.Token, GatewayToken: def.Gateway, Manual: true, OnDemand: def.OnDemand})
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: worker %s added at %s (monworker %s) from %s", def.Name, def.URL, info.Version, session.ClientIP(r))
	// The taggers list and the dialogs follow the workers, so the page reloads.
	w.Header().Set("HX-Refresh", "true")
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
