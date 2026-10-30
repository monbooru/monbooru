package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
)

// applyDestinationScan bounds the rows one preview reads looking for some
// that change: on a library that already follows, it would read them all.
const applyDestinationScan = 1000

const noDestination = `Set "Where received files go" in General first.`

// The watcher's naming whether or not it applies it to found files.
func (s *Server) applyDestination() (n gallery.Naming, folder, name string) {
	g := s.cfgSnapshot().Gallery
	folder, name = strings.TrimSpace(g.DefaultUploadFolder), strings.TrimSpace(g.DefaultUploadName)
	return gallery.IngestNaming(s.activeGallery(), folder, name), folder, name
}

// The ids a run walks, oldest first, leaving out what a sync would set
// aside; limit < 0 is all of them.
func (s *Server) applyDestinationIDs(limit int) ([]int64, error) {
	rows, err := s.db().Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	b := s.boundary()
	var ids []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		if !b.Excludes(path) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func existingDestination(ctx context.Context, cx *library.Gallery, b *gallery.Boundary, n gallery.Naming, id, md5Cap int64) (facts gallery.NameFacts, folder, name *string, follows bool, err error) {
	facts, err = cx.NameFacts(ctx, id, md5Cap, n.Folder, n.Name)
	if err != nil {
		return facts, nil, nil, false, err
	}
	folder, name = n.Existing(facts)
	follows, err = gallery.Follows(cx.DB, b, id, folder, name)
	return facts, folder, name, follows, err
}

func (s *Server) applyDestinationGet(w http.ResponseWriter, r *http.Request) {
	if !isHTMXRequest(r) {
		http.Redirect(w, r, "/settings#maintenance", http.StatusSeeOther)
		return
	}
	n, rawFolder, rawName := s.applyDestination()
	data := map[string]any{"Folder": rawFolder, "Name": rawName}
	if n.Empty() {
		data["Message"] = noDestination
		s.renderTemplate(w, "partials/apply_destination.html", data)
		return
	}
	ids, err := s.applyDestinationIDs(applyDestinationScan)
	if err != nil {
		data["Message"] = "Error: " + err.Error()
		s.renderTemplate(w, "partials/apply_destination.html", data)
		return
	}
	want := namePreviewRows
	if v, _ := strconv.Atoi(r.URL.Query().Get("rows")); v > want {
		want = min(v, namePreviewRowsMax)
	}

	cx := s.active()
	b := s.boundary()
	md5Cap := s.previewMD5Cap()
	planned := map[string]bool{}
	var rows []namePreviewRow
	var rowErr error
	for _, id := range ids {
		if len(rows) == want {
			break
		}
		facts, folder, name, follows, err := existingDestination(r.Context(), cx, b, n, id, md5Cap)
		if err != nil {
			rowErr = err
			continue
		}
		if follows {
			continue
		}
		dest, _, err := gallery.PlannedPath(cx.DB, b, id, folder, name, planned)
		if err != nil {
			rowErr = err
			continue
		}
		from := namePath(facts.Folder, facts.Base)
		rows = append(rows, namePreviewRow{
			From:     elideMiddle(from, namePreviewFrom),
			FromFull: from,
			To:       namePath(gallery.FolderPath(cx.GalleryPath, dest), filepath.Base(dest)),
		})
	}

	whole := len(ids) < applyDestinationScan
	switch {
	case len(rows) == 0 && rowErr != nil:
		data["Preview"] = map[string]any{"Error": rowErr.Error()}
	case len(ids) == 0:
		data["Message"] = "The gallery holds no image."
	case len(rows) == 0 && whole:
		data["Message"] = "Every image already follows the destination."
	case len(rows) == 0:
		data["Message"] = fmt.Sprintf("The oldest %d images already follow the destination; the check covers the rest.", len(ids))
	default:
		caption := fmt.Sprintf("%d that change", len(rows))
		if len(rows) == want {
			caption = fmt.Sprintf("first %d that change, oldest first", len(rows))
		} else if !whole {
			caption = fmt.Sprintf("%d that change among the oldest %d", len(rows), len(ids))
		}
		data["Preview"] = map[string]any{
			"Caption":  caption,
			"Note":     applyDestinationNotes(n, rawFolder),
			"Rows":     rows,
			"More":     len(rows) == want && want < namePreviewRowsMax,
			"NextRows": namePreviewRowsMax,
		}
	}
	s.renderTemplate(w, "partials/apply_destination.html", data)
}

func applyDestinationNotes(n gallery.Naming, rawFolder string) string {
	var notes []string
	switch {
	case n.Folder == nil:
	case !n.Folder.Uses("folder"):
		notes = append(notes, fmt.Sprintf("Images in every folder move under %s, subfolders included.", rawFolder))
	case rawFolder != "{folder}":
		notes = append(notes, "A folder built on {folder} nests one level deeper on every run.")
	}
	if n.Name.Uses("name") {
		notes = append(notes, "{name} is the name an image has now: a second run applies the template to it again.")
	}
	for _, t := range []*gallery.NameTemplate{n.Folder, n.Name} {
		if t.Uses("source") || t.Uses("post_id") {
			notes = append(notes, "{source} and {post_id} come with a received file: images already in the gallery render them empty.")
			break
		}
	}
	return strings.Join(notes, " ")
}

func (s *Server) applyDestinationPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	n, _, _ := s.applyDestination()
	if n.Empty() {
		writeInlineFlash(w, "err", noDestination)
		return
	}
	ids, err := s.applyDestinationIDs(-1)
	if err != nil {
		writeInlineFlash(w, "err", "Error: "+err.Error())
		return
	}
	if len(ids) == 0 {
		writeInlineFlash(w, "ok", "The gallery holds no image.")
		return
	}
	// The check takes its own job type: nothing is written, so the watcher
	// has nothing to suppress.
	if r.FormValue("check") == "1" {
		s.goJob(w, models.JobTypeCheck, "Checking the whole gallery...", func(context.Context) {
			s.runApplyDestinationCheck(n, ids)
		})
		return
	}
	s.goJob(w, models.JobTypeMove, "Apply destination started.", func(context.Context) {
		s.runApplyDestination(n, ids)
	})
}

func (s *Server) runApplyDestination(n gallery.Naming, ids []int64) {
	ctx := s.jobs.Context()
	cx := s.active()
	b := s.boundary()
	var followed int
	tally := newPlaceTally()
	done, failed, cancelled := s.perImageLoop(ids, "apply destination", "applying the destination", func(_ int, id int64) error {
		_, folder, name, follows, err := existingDestination(ctx, cx, b, n, id, 0)
		if err != nil {
			return err
		}
		if follows {
			followed++
			return nil
		}
		return tally.place(cx, id, folder, name)
	})

	changed := done - followed
	var summary string
	if changed == 0 && failed == 0 {
		summary = fmt.Sprintf("Nothing to do: %d %s the destination.", followed, models.Plural(followed, "image already follows", "images already follow"))
	} else {
		past := tally.past()
		summary = fmt.Sprintf("%s %d image%s.", titleCase(past), changed, plural(changed))
		if failed > 0 {
			summary = fmt.Sprintf("%s %d image%s, %d failed.", titleCase(past), changed, plural(changed), failed)
		}
		if followed > 0 {
			summary += fmt.Sprintf(" %d already followed the destination.", followed)
		}
		summary += tally.tail()
	}
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("apply destination cancelled (%d/%d done)", done, len(ids)), summary)
}

func (s *Server) runApplyDestinationCheck(n gallery.Naming, ids []int64) {
	ctx := s.jobs.Context()
	cx := s.active()
	b := s.boundary()
	planned := make(map[string]bool, len(ids))
	var changes, followed, collisions int
	done, failed, cancelled := s.perImageLoop(ids, "check", "checking", func(_ int, id int64) error {
		_, folder, name, follows, err := existingDestination(ctx, cx, b, n, id, 0)
		if err != nil {
			return err
		}
		if follows {
			followed++
			return nil
		}
		_, numbered, err := gallery.PlannedPath(cx.DB, b, id, folder, name, planned)
		if err != nil {
			return err
		}
		changes++
		if numbered {
			collisions++
		}
		return nil
	})

	summary := fmt.Sprintf("Checked %d image%s: %d would change, %d already %s the destination; no name is already taken.",
		done, plural(done), changes, followed, models.Plural(followed, "follows", "follow"))
	if collisions > 0 {
		summary = fmt.Sprintf("Checked %d image%s: %d would change, %d already %s the destination; %d would be numbered, %s already taken.",
			done, plural(done), changes, followed, models.Plural(followed, "follows", "follow"), collisions, models.Plural(collisions, "its name is", "their names are"))
	}
	if failed > 0 {
		summary += fmt.Sprintf(" %d could not be checked.", failed)
	}
	s.jobs.Finish(nil, cancelled, fmt.Sprintf("check cancelled (%d/%d checked)", done, len(ids)), summary)
}
