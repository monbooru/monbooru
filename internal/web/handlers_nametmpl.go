package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// The click-for-more is bounded too: a {md5} template hashes each file
// that has no digest yet.
const (
	namePreviewRows    = 5
	namePreviewRowsMax = 25
	// What fits the narrower column at the dialog's declared width, so
	// the CSS ellipsis never adds a second one.
	namePreviewFrom = 36
)

// Each surface's submit parses with these same scopes, so the preview
// refuses what the submit would.
var namePreviewScopes = map[string]struct{ folder, name gallery.Scope }{
	"place":       {gallery.ScopeMove, gallery.ScopeRename},
	"place-batch": {gallery.ScopeMoveBatch, gallery.ScopeRenameBatch},
}

// An empty body, not a 204, when there is nothing to show: htmx does not
// swap on No Content and would leave the last render standing.
func (s *Server) namePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("scope") == "upload" {
		s.uploadDestPreview(r.Context(), w, q.Get("folder"), q.Get("name"))
		return
	}
	scope, known := namePreviewScopes[q.Get("scope")]
	if !known {
		return
	}
	rawFolder, wantFolder := strings.TrimSpace(q.Get("folder")), q.Has("folder")
	rawName := strings.TrimSpace(q.Get("name"))
	folderTmpl, err := gallery.ParseNameTemplate(rawFolder, scope.folder)
	if err != nil {
		s.renderNamePreviewError(w, "folder", err.Error())
		return
	}
	var nameTmpl *gallery.NameTemplate
	if scope.name == gallery.ScopeRenameBatch {
		// The job's own parser, so a plain base name gets the implicit
		// {n} here too.
		nameTmpl, err = gallery.ParseBatchRenameTemplate(rawName)
	} else {
		nameTmpl, err = gallery.ParseNameTemplate(rawName, scope.name)
	}
	if err != nil {
		s.renderNamePreviewError(w, "name", err.Error())
		return
	}
	if !wantFolder && nameTmpl == nil {
		return
	}

	want := namePreviewRows
	if n, _ := strconv.Atoi(q.Get("rows")); n > want {
		want = min(n, namePreviewRowsMax)
	}
	var ids []int64
	if q.Get("kind") == "search" {
		ids = s.searchPreviewIDs(r, want)
	} else {
		ids = s.namePreviewIDs(q["ids"], want)
	}
	if len(ids) == 0 {
		return
	}
	total, _ := strconv.Atoi(q.Get("total"))
	total = max(total, len(ids))

	rows := make([]namePreviewRow, 0, len(ids))
	var rowErr error
	var changed bool
	md5Cap := s.previewMD5Cap()
	cx := s.active()
	// Rows arrive in scope order, so the run's collision numbering is
	// replayed: a preview the run then renames differently would mislead.
	planned := make(map[string]bool, len(ids))
	for i, id := range ids {
		facts, factErr := cx.NameFacts(r.Context(), id, md5Cap, folderTmpl, nameTmpl)
		if factErr != nil {
			rowErr = factErr
			continue
		}
		facts.N, facts.NWidth = i+1, max(len(strconv.Itoa(total)), 2)

		var folder, name *string
		if wantFolder {
			rendered, renderErr := renderOrLiteral(folderTmpl, rawFolder, facts)
			if renderErr != nil {
				rowErr = renderErr
				continue
			}
			// Checked here because a refused submit answers with a status
			// htmx discards, leaving the dialog silent.
			if _, resolveErr := s.boundary().ResolveSubdir(rendered); resolveErr != nil {
				s.renderNamePreviewError(w, "folder", resolveErr.Error())
				return
			}
			folder = &rendered
		}
		if nameTmpl != nil {
			rendered, renderErr := renderOrLiteral(nameTmpl, rawName, facts)
			if renderErr != nil {
				rowErr = renderErr
				continue
			}
			name = &rendered
		}
		dest, _, destErr := gallery.PlannedPath(s.db(), s.boundary(), id, folder, name, planned)
		if destErr != nil {
			rowErr = destErr
			continue
		}
		from := namePath(facts.Folder, facts.Base)
		to := namePath(gallery.FolderPath(cx.GalleryPath, dest), filepath.Base(dest))
		changed = changed || from != to
		rows = append(rows, namePreviewRow{From: elideMiddle(from, namePreviewFrom), FromFull: from, To: to})
	}
	if len(rows) == 0 {
		if rowErr != nil {
			s.renderNamePreviewError(w, "", rowErr.Error())
		}
		return
	}
	if !changed {
		return
	}
	caption := ""
	if total > len(rows) {
		caption = fmt.Sprintf("first %d of %d, in scope order", len(rows), total)
	}
	more := len(rows) == want && total > len(rows) && want < namePreviewRowsMax
	note := ""
	if total > 1 && wantFolder && folderTmpl.PerImage() {
		note = fmt.Sprintf("a folder per image, up to %d new ones", total)
	}
	s.renderNamePreview(w, caption, note, rows, more)
}

// Counted in runes: a byte cut could split a character.
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max / 3
	return string(r[:head]) + "..." + string(r[len(r)-(max-head-3):])
}

func renderOrLiteral(tmpl *gallery.NameTemplate, literal string, facts gallery.NameFacts) (string, error) {
	if !tmpl.HasTokens() {
		return literal, nil
	}
	return tmpl.Render(facts)
}

type namePreviewRow struct{ From, FromFull, To string }

// Previews run per keystroke, so a file past the size cap is not hashed
// for {md5}.
func (s *Server) previewMD5Cap() int64 { return int64(s.maxFileSizeMB()) * 1024 * 1024 }

func namePath(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func (s *Server) renderNamePreview(w http.ResponseWriter, caption, note string, rows []namePreviewRow, more bool) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Caption":  caption,
		"Note":     note,
		"Rows":     rows,
		"More":     more,
		"NextRows": namePreviewRowsMax,
	})
}

func (s *Server) renderNamePreviewError(w http.ResponseWriter, field, msg string) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Field": field,
		"Error": msg,
	})
}

func (s *Server) uploadDestPreview(ctx context.Context, w http.ResponseWriter, folder, name string) {
	folderTmpl, err := gallery.ParseNameTemplate(folder, gallery.ScopeUploadFolder)
	if err != nil {
		s.renderNamePreviewError(w, "folder", err.Error())
		return
	}
	nameTmpl, err := gallery.ParseNameTemplate(name, gallery.ScopeUploadName)
	if err != nil {
		s.renderNamePreviewError(w, "name", err.Error())
		return
	}
	ids := s.namePreviewIDs(nil, 1)
	if (folderTmpl == nil && nameTmpl == nil) || len(ids) == 0 {
		return
	}
	facts, err := s.active().NameFacts(ctx, ids[0], s.previewMD5Cap(), folderTmpl, nameTmpl)
	if err != nil {
		s.renderNamePreviewError(w, "", err.Error())
		return
	}

	base := facts.Base
	if nameTmpl != nil {
		if base, err = nameTmpl.Render(facts); err != nil {
			s.renderNamePreviewError(w, "name", err.Error())
			return
		}
		if onDisk := filepath.Ext(facts.Base); !strings.EqualFold(filepath.Ext(base), onDisk) {
			base += onDisk
		}
	}
	to := base
	if folderTmpl != nil {
		dir, dirErr := folderTmpl.Render(facts)
		if dirErr != nil {
			s.renderNamePreviewError(w, "folder", dirErr.Error())
			return
		}
		to = namePath(dir, base)
	}
	s.renderNamePreview(w, "a file arriving now", "", []namePreviewRow{{From: facts.Base, FromFull: facts.Base, To: to}}, false)
}

func (s *Server) ingestNaming(galleryName string) gallery.Naming {
	s.cfgMu.RLock()
	on, folder, name := s.cfg.Gallery.RenameOnIngest, s.cfg.Gallery.DefaultUploadFolder, s.cfg.Gallery.DefaultUploadName
	s.cfgMu.RUnlock()
	if !on {
		return gallery.Naming{}
	}
	return gallery.IngestNaming(galleryName, folder, name)
}

// No name half: the extract, the generated pages and the cbz name their
// own output, carrying page order or generation time.
func (s *Server) receivedNaming(galleryName string) (writeDir string, n gallery.Naming) {
	s.cfgMu.RLock()
	folder := s.cfg.Gallery.DefaultUploadFolder
	s.cfgMu.RUnlock()
	return gallery.ReceivedNaming(galleryName, "", strings.TrimSpace(folder), "")
}

// A search scope is read from its first match in view order, as the run
// numbers it, rather than from whatever page the grid shows.
func (s *Server) searchPreviewIDs(r *http.Request, want int) []int64 {
	q := r.URL.Query()
	seed, _ := strconv.ParseInt(q.Get("seed"), 10, 64)
	sc := search.NewScope(search.Parse(q.Get("q")), readRatingCookie(r)).InViewOrder(q.Get("sort"), q.Get("order"), seed)
	ids, err := sc.FirstIDs(s.source(), want)
	if err != nil {
		logx.Warnf("name preview search: %v", err)
	}
	return ids
}

func (s *Server) namePreviewIDs(raw []string, want int) []int64 {
	ids := parseIDList(raw)
	if len(ids) > want {
		ids = ids[:want]
	}
	if len(ids) > 0 {
		return ids
	}
	var id int64
	if err := s.db().Read.QueryRow(
		`SELECT id FROM images WHERE is_missing = 0 ORDER BY id DESC LIMIT 1`,
	).Scan(&id); err != nil {
		return nil
	}
	return []int64{id}
}
