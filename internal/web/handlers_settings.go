package web

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/schedule"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
	"github.com/monbooru/monbooru/internal/shared/webui"
	"github.com/monbooru/monbooru/internal/tagger"
	"golang.org/x/crypto/bcrypt"
)

// Galleries shadows the layout's list with the rows this page's table renders.
// ExecutionProviders is not probed: the parent must not load ONNX Runtime on
// a page render, so a bad pick is refused at save time.
type settingsData struct {
	baseData
	Galleries          []galleryRow
	Config             *config.Config
	Taggers            []tagger.TaggerStatus
	TaggerRows         []taggerRow
	ScheduleStatus     schedule.Status
	ScheduleScope      scheduleScope
	Stats              statsData
	ExecutionProviders []struct{ Name, Label string }
	ScheduleModes      []scheduleModeRow
	PluginPending      []pendingOffer
	PluginPaired       int
	PluginRows         []pluginRowView
	PluginsDir         string
	Themes             themeCluster
	ThemesDir          string
	DesktopProfile     bool
	DesktopFolders     []string
	Desktop            desktop.Integration
	DesktopJobWarning  string
	PluginsRepoURL     string
}

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "settings", "Settings - "+s.booruName())
	modelPath := s.modelPath()
	catalog := tagger.LoadCatalog(modelPath)
	s.reconcileTaggers(catalog)
	cfg := s.cfgSnapshot()
	taggers := tagger.AvailableTaggers(cfg)
	taggerByName := map[string]tagger.TaggerStatus{}
	for _, t := range taggers {
		taggerByName[t.Name] = t
	}
	var supportedRows, unsupportedRows []taggerRow
	totalGalleries := len(s.galleries())
	catalogNames := map[string]bool{}
	for _, e := range catalog {
		catalogNames[e.Name] = true
		target := filepath.Join(modelPath, e.Name)
		get := e.Missing(target)
		row := taggerRow{}
		if t, installed := taggerByName[e.Name]; installed {
			row = s.installedTaggerRow(t, totalGalleries, modelPath, &e)
			if !t.Missing || t.Malformed {
				get.Files = nil
			}
		}
		row.Name = e.Name
		row.Files, row.TargetDir = get.Files, target
		row.Supported = true
		row.Description = e.Description
		row.Gated = e.Gated
		row.HostCommand = get.HostCommand()
		supportedRows = append(supportedRows, row)
	}
	for _, t := range taggers {
		if catalogNames[t.Name] {
			continue
		}
		unsupportedRows = append(unsupportedRows, s.installedTaggerRow(t, totalGalleries, modelPath, nil))
	}
	taggerRows := append(supportedRows, unsupportedRows...)
	s.renderTemplate(w, "settings.html", settingsData{
		baseData:           base,
		Galleries:          s.galleryRowsWithSnapshot(s.activeGallery(), base.VisibleCount, base.TagCount),
		Config:             cfg,
		Taggers:            taggers,
		TaggerRows:         taggerRows,
		ScheduleStatus:     s.sched.Status(),
		ScheduleScope:      scheduleScopeView(cfg, taggers, base.MonloaderPaired),
		Stats:              s.gatherStats(),
		ExecutionProviders: config.ExecutionProviders,
		ScheduleModes:      scheduleModeRows(),
		PluginPending:      s.pendingOffers(),
		PluginPaired:       s.pairedPeerCount(),
		PluginRows:         s.pluginRows(),
		PluginsDir:         s.pluginsDir(),
		Themes:             s.themeCluster(),
		ThemesDir:          s.themesDir(),
		DesktopProfile:     s.desktopLocal(r),
		DesktopFolders:     s.availableFolders(),
		Desktop:            s.desktopIntegration(),
		DesktopJobWarning:  s.desktopJobWarning(),
		PluginsRepoURL:     pluginsRepoURL(RepoURL),
	})
}

// The registry sits beside this repository on whichever forge serves it.
func pluginsRepoURL(repo string) string {
	repo = strings.TrimRight(repo, "/")
	return repo[:strings.LastIndex(repo, "/")+1] + "monbooru-plugins"
}

func (s *Server) settingsSchedulePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	timeVal := cmp.Or(strings.TrimSpace(r.FormValue("time")), "01:00")
	if err := config.ValidateScheduleTime(timeVal); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !config.IsValidScheduleMode(mode) {
		mode = config.ScheduleAtTime
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Schedule.Time = timeVal
		c.Schedule.Mode = mode
		c.Schedule.SyncGallery = r.FormValue("sync_gallery") == "on"
		c.Schedule.RemoveOrphans = r.FormValue("remove_orphans") == "on"
		c.Schedule.RunAutoTaggers = r.FormValue("run_auto_taggers") == "on"
		c.Schedule.FindRelationPairs = r.FormValue("find_relation_pairs") == "on"
		// Drawn only while monloader is paired; a box never on the page was
		// not unticked.
		if r.FormValue("lookup_switches") != "" {
			c.Schedule.LookupPTR = r.FormValue("lookup_ptr") == "on"
			c.Schedule.LookupBooru = r.FormValue("lookup_booru") == "on"
		}
		c.Schedule.Galleries = scheduleGalleriesForm(r, c.Schedule.Galleries, c.Galleries)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	s.sched.RequestReload()
	logx.Infof("settings: schedule updated (time=%s mode=%s)", timeVal, mode)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/schedule_status.html", map[string]any{
		"Status": s.sched.Status(),
		"OOB":    true,
	})
}

// Only the columns the table drew are read: a box never on the page was
// not unticked.
func scheduleGalleriesForm(r *http.Request, lists map[string][]string, galleries []config.Gallery) map[string][]string {
	out := maps.Clone(lists)
	names := galleryNames(galleries)
	for _, action := range r.Form["scope_action"] {
		if !slices.Contains(config.ScheduleActions, action) {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		if r.FormValue("every_"+action) == "on" {
			delete(out, action)
			continue
		}
		picked := []string{}
		for _, name := range names {
			if slices.Contains(r.Form["on_"+action], name) {
				picked = append(picked, name)
			}
		}
		out[action] = picked
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type scheduleScope struct {
	Show      bool
	Galleries []string
	Columns   []scheduleColumn
}

type scheduleColumn struct {
	Action string
	Label  string
	Title  string
	Accent string
	On     bool
	Every  bool // no list: every gallery, including ones added later
	Picked map[string]bool
	Idle   map[string]bool
}

var scheduleColumns = []scheduleColumn{
	{Action: config.ActionSyncGallery, Label: "Sync", Title: "Sync gallery"},
	{Action: config.ActionRemoveOrphans, Label: "Orphans", Title: "Remove orphaned thumbnails"},
	{Action: config.ActionRunAutoTaggers, Label: "Auto-tag", Title: "Run enabled auto-taggers", Accent: "tagger-accent"},
	{Action: config.ActionFindRelationPairs, Label: "Pairs", Title: "Find relation pairs"},
	{Action: config.ActionLookupPTR, Label: "PTR", Title: "Look up unsourced images in the Public Tag Repository", Accent: "peer-accent"},
	{Action: config.ActionLookupBooru, Label: "Boorus", Title: "Look up unsourced images on online boorus", Accent: "peer-accent"},
}

// Shown with one gallery too while a list is set, or that list would
// restrict it unseen.
func scheduleScopeView(cfg *config.Config, taggers []tagger.TaggerStatus, monloaderPaired bool) scheduleScope {
	sched := cfg.Schedule
	names := galleryNames(cfg.Galleries)
	v := scheduleScope{Show: len(names) > 1 || len(sched.Galleries) > 0, Galleries: names}
	if !v.Show {
		return v
	}
	for _, c := range scheduleColumns {
		if !monloaderPaired && (c.Action == config.ActionLookupPTR || c.Action == config.ActionLookupBooru) {
			continue
		}
		list, listed := sched.Galleries[c.Action]
		c.On, c.Every = sched.Enabled(c.Action), !listed
		c.Picked, c.Idle = map[string]bool{}, map[string]bool{}
		for _, name := range names {
			c.Picked[name] = slices.Contains(list, name)
			if c.Action == config.ActionRunAutoTaggers {
				c.Idle[name] = !slices.ContainsFunc(taggers, func(t tagger.TaggerStatus) bool {
					return t.Enabled && t.Available && t.AppliesToGallery(name)
				})
			}
		}
		v.Columns = append(v.Columns, c)
	}
	return v
}

func galleryNames(galleries []config.Gallery) []string {
	names := make([]string, len(galleries))
	for i, g := range galleries {
		names[i] = g.Name
	}
	slices.Sort(names)
	return names
}

// Not gated on the desktop profile: a container operator wants to test
// the schedule too.
func (s *Server) settingsScheduleRunPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.jobs.IsRunning() {
		writeInlineFlash(w, "err", "A job is already running.")
		return
	}
	go s.sched.RunNow()
	writeInlineFlash(w, "ok", "Started. Watch the job status bar.")
}

type scheduleModeRow struct {
	Name  string
	Label string
}

func scheduleModeRows() []scheduleModeRow {
	return []scheduleModeRow{
		{config.ScheduleAtTime, "Every day at the time above"},
		{config.ScheduleAtTimeCatchup, "Every day at the time above, and later if that run was missed"},
		{config.ScheduleOnStart, "At startup only, at most once a day"},
		{config.ScheduleOff, "Never"},
	}
}

func (s *Server) settingsGeneralPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	uploadFolder := strings.TrimSpace(r.FormValue("default_upload_folder"))
	folderTmpl, err := gallery.ParseNameTemplate(uploadFolder, gallery.ScopeUploadFolder)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	// A tokened folder resolves per image, so only a literal one can be
	// checked here.
	if !folderTmpl.HasTokens() {
		if _, err := s.boundary().ResolveSubdir(uploadFolder); err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
	}
	uploadName := strings.TrimSpace(r.FormValue("default_upload_name"))
	if _, err := gallery.ParseNameTemplate(uploadName, gallery.ScopeUploadName); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	var autoMeta bool
	if err := s.withConfig(func(c *config.Config) error {
		if c.EnvPinned("gallery.watch_enabled") == "" {
			c.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
		}
		if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil && n >= 0 && c.EnvPinned("gallery.max_file_size_mb") == "" {
			c.Gallery.MaxFileSizeMB = n
		}
		c.Gallery.DefaultUploadFolder = uploadFolder
		c.Gallery.DefaultUploadName = uploadName
		c.Gallery.RenameOnIngest = r.FormValue("rename_on_ingest") == "on"
		if c.EnvPinned("gallery.auto_meta_tags") == "" {
			c.Gallery.AutoMetaTags = r.FormValue("auto_meta_tags") == "on"
		}
		if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
			c.UI.PageSize = min(n, config.MaxPageSize)
		}
		if fit := r.FormValue("thumbnail_fit"); fit == "square" || fit == "natural" {
			c.UI.ThumbnailFit = fit
		}
		autoMeta = c.Gallery.AutoMetaTags
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	gallery.MetaTagsEnabled.Store(autoMeta)
	logx.Infof("settings: general updated")
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) settingsIgnorePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ignore, err := config.NormalizeIgnore(strings.Split(r.FormValue("ignore"), "\n"))
	if err != nil {
		writeInlineFlash(w, "err", err.Error()+".")
		return
	}
	uploadFolder := s.cfgSnapshot().Gallery.DefaultUploadFolder
	if tmpl, err := gallery.ParseNameTemplate(uploadFolder, gallery.ScopeUploadFolder); err == nil && !tmpl.HasTokens() {
		if _, err := s.set.Draw(ignore)[s.activeGallery()].ResolveSubdir(uploadFolder); err != nil {
			writeInlineFlash(w, "err", "Received files go to a folder this list leaves out: "+err.Error()+".")
			return
		}
	}
	var changed bool
	if err := s.withConfig(func(c *config.Config) error {
		changed = !slices.Equal(c.Gallery.Ignore, ignore)
		c.Gallery.Ignore = ignore
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	msg := "Saved."
	if changed {
		s.set.Redraw()
		msg = "Saved. A sync applies the ignore list to what is already indexed."
		if n := s.set.LeftOutBy(ignore); n > 0 {
			msg = fmt.Sprintf("Saved. The next sync sets aside %d indexed %s this list leaves out. Tags are kept, and a file the list lets in again comes back with them.",
				n, models.Plural(n, "image", "images"))
		}
	}
	logx.Infof("settings: ignore list updated")
	writeInlineFlash(w, "ok", msg)
	s.renderTemplate(w, "partials/ignore_list.html", map[string]any{"Ignore": ignore, "OOB": true})
}

func (s *Server) settingsMonloaderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	webURL := strings.TrimSpace(r.FormValue("monloader_url"))
	apiURL := strings.TrimSpace(r.FormValue("api_url"))
	if !httpURLOrBlank(webURL) || !httpURLOrBlank(apiURL) {
		writeInlineFlash(w, "err", "The monloader addresses must be http(s) URLs. Nothing was saved.")
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		if c.EnvPinned("server.monloader_url") == "" {
			c.Server.MonloaderURL = webURL
		}
		if c.EnvPinned("monloader.api_url") == "" {
			c.Monloader.APIURL = apiURL
		}
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: monloader link updated")
	writeInlineFlash(w, "ok", "Saved.")
}

func httpURLOrBlank(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if v := s.passwordPinned(); v != "" {
		writeInlineFlash(w, "err", "The password is set by "+v+" in the environment.")
		return
	}
	currentPass := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	if newPass == "" {
		writeInlineFlash(w, "err", "New password required.")
		return
	}
	if current := s.passwordHash(); current != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(current), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		writeInlineFlash(w, "err", "Error hashing password.")
		return
	}
	wasOn := s.authEnabled()
	if err := s.withConfig(func(c *config.Config) error {
		c.Auth.PasswordHash = string(hash)
		c.Auth.EnablePassword = true
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password updated from %s", session.ClientIP(r))
	if !wasOn {
		// The page's CSRF token is the anon one, so it reloads onto the new session.
		sessID := s.sessions.Create(s.sessionLifetimeDays())
		s.sessions.ClearExcept(sessID)
		s.setSessionCookie(w, r, sessID)
		setFlashHeader(w, "Password updated.", "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		return
	}
	s.sessions.ClearExcept(session.FromContext(r.Context()))
	writeInlineFlash(w, "ok", "Password updated.")
	s.renderAuthPasswordOOB(w, r)
}

func (s *Server) settingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := apitoken.ValidateName(name); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	tok, secret := apitoken.New(name, config.AllScopes)
	if err := s.withConfig(func(c *config.Config) error {
		return c.Auth.Tokens.Add(tok)
	}); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	logx.Infof("settings: API token %q created from %s", name, session.ClientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("HX-Trigger", "token-created")
	s.renderTemplate(w, "webui/token_created", map[string]any{"Token": secret})
	s.renderAuthTokensOOB(w, r)
}

func (s *Server) tokenPeer(id string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if i := slices.IndexFunc(s.cfg.Auth.Tokens, func(t apitoken.Token) bool { return t.ID == id }); i >= 0 {
		return s.cfg.Auth.Tokens[i].Paired
	}
	return ""
}

func (s *Server) settingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if app := s.tokenPeer(id); app != "" {
		notifyErr := s.removePeerPairing(r, app)
		logx.Infof("settings: API token %s revoked from %s", id, session.ClientIP(r))
		if notifyErr != nil {
			writeInlineFlash(w, "warn", "Revoked here, but could not reach "+app+" - remove the pairing there too.")
		} else {
			writeInlineFlash(w, "ok", "Token revoked, "+app+" pairing removed.")
		}
		s.renderAuthTokensOOB(w, r)
		s.renderPluginRows(w, r, true)
		return
	}
	var removed bool
	if err := s.withConfig(func(c *config.Config) error {
		removed = c.Auth.Tokens.Remove(id)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !removed {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s revoked from %s", id, session.ClientIP(r))
	writeInlineFlash(w, "ok", "Token revoked.")
	s.renderAuthTokensOOB(w, r)
}

func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	tokens := slices.Clone(s.cfg.Auth.Tokens)
	s.cfgMu.RUnlock()
	s.renderTemplate(w, "webui/auth_tokens", map[string]any{
		"Tokens": tokens,
		"OOB":    true,
	})
}

func (s *Server) settingsTokenPrivilegesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cfgMu.RLock()
	var scopes []string
	var paired bool
	i := slices.IndexFunc(s.cfg.Auth.Tokens, func(t apitoken.Token) bool { return t.ID == id })
	if i >= 0 {
		scopes = slices.Clone(s.cfg.Auth.Tokens[i].Scopes)
		paired = s.cfg.Auth.Tokens[i].Paired != ""
	}
	s.cfgMu.RUnlock()
	if i < 0 {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	descs := map[string]string{
		config.ScopeRead:   "read - all GET endpoints",
		config.ScopeWrite:  "write - create and modify",
		config.ScopeDelete: "delete - destructive actions",
	}
	s.renderTemplate(w, "webui/token_privileges_dialog", map[string]any{
		"ID":     id,
		"Scopes": webui.ScopeRows(config.AllScopes, scopes, descs),
		"Paired": paired,
	})
}

func (s *Server) settingsTokenPrivilegesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.tokenPeer(id) != "" {
		writeInlineFlash(w, "err", "This token is managed by a pairing; its privileges can't be changed.")
		return
	}
	scopes := apitoken.Filter(r.Form["scope"], config.AllScopes)
	var found bool
	if err := s.withConfig(func(c *config.Config) error {
		found = c.Auth.Tokens.SetScopes(id, scopes)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !found {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s privileges updated from %s", id, session.ClientIP(r))
	setDialogSavedTrigger(w, "token-saved", "token-cfg-"+id)
	s.renderTemplate(w, "webui/token_scopes_saved", map[string]string{"ID": id, "Scopes": strings.Join(scopes, " ")})
}

func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if v := s.passwordPinned(); v != "" {
		writeInlineFlash(w, "err", "The password is set by "+v+" in the environment.")
		return
	}
	// Whenever a hash is set, even with the password turned off in the
	// TOML, so a file edit cannot open the remove path.
	currentPass := r.FormValue("current_password")
	if current := s.passwordHash(); current != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(current), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Auth.EnablePassword = false
		c.Auth.PasswordHash = ""
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password removed from %s", session.ClientIP(r))
	// Old sessions would otherwise stay valid when a password is set again.
	s.sessions.Clear()
	writeInlineFlash(w, "ok", "Password removed. Authentication is now disabled.")
	s.renderAuthPasswordOOB(w, r)
}
