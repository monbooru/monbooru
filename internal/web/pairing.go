package web

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/monloader"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/plugins"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/pairing"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The pairing routes sit outside the api auth wrapper, since the operator's
// approval is the gate, so they apply its CORS policy themselves.
func (s *Server) pairCORS(w http.ResponseWriter, r *http.Request) bool {
	return api.CORSOK(w, r, s.cfgSnapshot())
}

func pairRefuse(w http.ResponseWriter, f *pairing.Refusal) {
	contract.WriteJSON(w, f.Status, contract.Error{Error: f.Msg, Code: f.Code})
}

func (s *Server) pairedWith(app string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Auth.Tokens.Paired(app) != nil
}

func (s *Server) pairRequest(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	var body struct {
		pairing.RequestBody
		Buttons  []config.PluginButton `json:"buttons"`
		Contract int                   `json:"contract"`
	}
	if f := pairing.DecodeOffer(w, r, &body); f != nil {
		pairRefuse(w, f)
		return
	}
	if err := peer.ValidateOffer(body.App, body.Version, body.Buttons); err != nil {
		pairRefuse(w, &pairing.Refusal{Status: http.StatusBadRequest, Code: pairing.CodeInvalidRequest, Msg: err.Error()})
		return
	}
	if body.Contract > plugins.Contract {
		msg := fmt.Sprintf("%s speaks plugin contract %d, this monbooru speaks %d", body.App, body.Contract, plugins.Contract)
		logx.Warnf("pairing: refused: %s", msg)
		contract.WriteJSON(w, http.StatusConflict, contract.Error{Error: msg, Code: "contract_unsupported"})
		return
	}
	// A re-pair is not a conflict: a replaced plugin comes back with no
	// credentials. It still queues for approval.
	req := peer.Req{
		Request:   pairing.Request{App: body.App, Label: body.Label, Source: session.ClientIP(r), Scopes: peer.GrantedScopes(body.RequestedScopes)},
		URL:       body.URL,
		PeerToken: body.PeerToken, Version: body.Version, Buttons: body.Buttons,
	}
	if body.App == contract.AppMonworker {
		if req.WorkerName() == tagger.LocalWorker {
			pairRefuse(w, &pairing.Refusal{Status: http.StatusBadRequest, Code: pairing.CodeInvalidRequest, Msg: "the name local is the local worker's; set MONWORKER_NAME"})
			return
		}
		req.Scopes, req.Buttons = nil, nil
		// Two machines under one name would take each other's place.
		s.cfgMu.RLock()
		listed := peer.FindWorker(s.cfg, req.WorkerName())
		manual := listed != nil && listed.Manual
		s.cfgMu.RUnlock()
		var msg string
		if manual {
			msg = fmt.Sprintf("a worker named %s is already listed, added by address; set MONWORKER_NAME to another name", req.WorkerName())
		} else if s.pairedWith(req.Paired()) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			at, answers := s.pool.Answers(ctx, req.WorkerName())
			cancel()
			if answers {
				msg = fmt.Sprintf("a worker named %s is already paired and answers at %s; set MONWORKER_NAME to another name", req.WorkerName(), at)
			}
		}
		if msg != "" {
			logx.Infof("pairing: refused %s from %s: %s", req.Paired(), session.ClientIP(r), msg)
			pairRefuse(w, &pairing.Refusal{Status: http.StatusConflict, Code: "name_taken", Msg: msg})
			return
		}
	}
	req.Repair = s.pairedWith(req.Paired())
	req.PrivateSource = privateSource(r)
	req.Auto = s.autoApproves(r, req)
	accepted, f := s.pairs.Offer(req)
	if f != nil {
		pairRefuse(w, f)
		return
	}
	logx.Infof("pairing: request from %s (%s)", req.Paired(), body.URL)
	if req.Auto {
		s.pairs.SetState(accepted.RequestID, pairing.Approved)
		logx.Infof("pairing: approved request %s from %s without a click", accepted.RequestID, req.Source)
	}
	contract.WriteJSON(w, http.StatusOK, accepted)
}

func (s *Server) pairStatus(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	body, f := s.pairs.Poll(r.URL.Query().Get("id"), s.mintPairedToken)
	if f != nil {
		pairRefuse(w, f)
		return
	}
	if body.Token != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	contract.WriteJSON(w, http.StatusOK, body)
}

// Removes only locally: calling the peer back would loop.
func (s *Server) pairTeardown(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.cfgMu.RLock()
	tok := s.cfg.Auth.Tokens.ByHash(apitoken.Hash(secret))
	var paired string
	if tok != nil {
		paired = tok.Paired
	}
	s.cfgMu.RUnlock()
	if secret == "" || paired == "" {
		pairRefuse(w, pairing.NoPairingToken())
		return
	}
	if err := s.removePairing(paired); err != nil {
		pairRefuse(w, &pairing.Refusal{Status: http.StatusInternalServerError, Code: pairing.CodeRemoveFailed, Msg: err.Error()})
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", paired)
	contract.WriteJSON(w, http.StatusOK, pairing.StatusBody{Status: pairing.Removed})
}

// A worker never calls monbooru but to pair, so at start it asks whether
// its pairing still holds: a 401 tells it to offer again.
func (s *Server) pairCheck(w http.ResponseWriter, r *http.Request) {
	if !s.pairCORS(w, r) {
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.cfgMu.RLock()
	var paired string
	if tok := s.cfg.Auth.Tokens.ByHash(apitoken.Hash(secret)); tok != nil {
		paired = tok.Paired
	}
	s.cfgMu.RUnlock()
	if secret == "" || paired == "" {
		pairRefuse(w, pairing.NoPairingToken())
		return
	}
	// It also says where it listens now: a new port, or a new host.
	var body pairing.RequestBody
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	if u, err := url.Parse(body.URL); err != nil || u.Host == "" {
		body.URL = ""
	}
	s.repointPeer(secret, body.URL, session.ClientIP(r))
	contract.WriteJSON(w, http.StatusOK, pairing.StatusBody{Status: pairing.Approved})
}

// Any call carrying a peer's own token names where it is now, so a
// recreated container's new IP is followed without a re-pair.
func (s *Server) notePeerAddress(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			s.repointPeer(secret, "", session.ClientIP(r))
		}
		next(w, r)
	}
}

// repointPeer moves the peer holding secret to advertised, else to the
// address it has, on the host it called from.
func (s *Server) repointPeer(secret, advertised, source string) {
	s.cfgMu.RLock()
	var app, was string
	if tok := s.cfg.Auth.Tokens.ByHash(apitoken.Hash(secret)); tok != nil {
		app, was = tok.Paired, tok.PeerURL
	}
	s.cfgMu.RUnlock()
	moved := peer.CallbackURL(cmp.Or(advertised, was), source)
	if app == "" || moved == was {
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		peer.Repoint(c, app, moved)
		return nil
	}); err == nil {
		logx.Infof("pairing: %s moved to %s", app, moved)
	}
}

func (s *Server) mintPairedToken(req peer.Req) (string, error) {
	name, scopes := req.App+" (paired)", peer.GrantedScopes(req.Scopes)
	if req.App == contract.AppMonworker {
		name, scopes = req.WorkerName()+" (worker)", []string{}
	}
	tok, secret := apitoken.New(name, scopes)
	tok.Paired = req.Paired()
	tok.PeerURL = peer.CallbackURL(req.URL, req.Source)
	if err := s.withConfig(func(c *config.Config) error {
		peer.Grant(c, req, tok)
		return nil
	}); err != nil {
		return "", err
	}
	if req.Auto || s.win.approved(req.ID) {
		s.win.record(req)
	} else {
		s.win.forget(req.Paired())
	}
	logx.Infof("pairing: issued token to %s", req.Paired())
	return secret, nil
}

func (s *Server) pluginPairApprove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	req, ok := s.pairs.Get(id)
	if !ok {
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData())
		return
	}
	// A peer monbooru cannot reach would pair dead, so approval waits
	// until the url it will call answers.
	base := cmp.Or(s.peerOverrideURL(req.App), peer.CallbackURL(req.URL, req.Source))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	isWorker := req.App == contract.AppMonworker
	if !peer.Reachable(ctx, base) {
		msg := "no api url to reach " + req.App + "; pairing not completed."
		if isWorker {
			msg = req.WorkerName() + " is unreachable at " + base + "; pairing not completed. Check that it is running and that this machine can reach it."
		} else if base != "" {
			msg = req.App + " is unreachable at " + base + "; pairing not completed. Check the api url and that it is running."
		}
		if isWorker {
			s.workerPairDecided(w, pairing.Pending, msg)
			return
		}
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData())
		writeFlashOOB(w, "flash-plugins", "warn", msg)
		return
	}
	s.pairs.SetState(id, pairing.Approved)
	logx.Infof("pairing: approved request %s from %s", id, session.ClientIP(r))
	if isWorker {
		s.workerPairDecided(w, pairing.Approved, "")
		return
	}
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData())
	writeFlashOOB(w, "flash-plugins", "", "")
	s.renderPluginRows(w, r, true)
}

func (s *Server) monloaderLightDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.pairedWith(monloader.App) {
		if err := s.setPluginPaused(monloader.App, true); err != nil {
			logx.Errorf("pairing: pause failed: %v", err)
		}
		logx.Infof("pairing: monloader link paused from %s", session.ClientIP(r))
	}
	s.renderMonloaderLight(w, r, peer.ConnPaused, "")
}

func (s *Server) monloaderLightReconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if err := s.setPluginPaused(monloader.App, false); err != nil {
		logx.Errorf("pairing: resume failed: %v", err)
	}
	logx.Infof("pairing: monloader link resumed from %s", session.ClientIP(r))
	s.renderMonloaderLight(w, r, "", "")
}

func (s *Server) renderMonloaderLight(w http.ResponseWriter, r *http.Request, conn, version string) {
	s.renderTemplate(w, "webui/peer_light", s.monloaderLight(conn, version, "", r.FormValue("seen")))
}

func (s *Server) teardownMonloaderPairing(r *http.Request) (saveErr, notifyErr error) {
	notifyErr = s.ml.Teardown(func() error {
		saveErr = s.removePairing(monloader.App)
		return saveErr
	})
	if saveErr != nil {
		logx.Errorf("pairing: remove failed: %v", saveErr)
		return saveErr, nil
	}
	logx.Infof("pairing: removed monloader pairing from %s", session.ClientIP(r))
	return nil, notifyErr
}

func (s *Server) removePairing(app string) error {
	err := s.withConfig(func(c *config.Config) error {
		peer.Revoke(c, app)
		return nil
	})
	if err == nil {
		s.win.forget(app)
		if name, ok := peer.WorkerLabel(app); ok {
			tagger.CurrentPool().Forget(name)
		}
	}
	return err
}
