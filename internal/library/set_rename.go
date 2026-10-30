package library

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// Rename moves the data folder and reopens the gallery under the new name. The
// host saves the config before the new name goes live, since a restart on the
// old one would open it empty and orphan the data; a failed save undoes the move.
func (s *Set) Rename(oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	if err := config.ValidateGalleryName(newName); err != nil {
		return err
	}
	r, err := s.jobs.Reserve()
	if err != nil {
		return ErrJobRunning
	}
	defer r.Release()
	cx, err := s.renameMember(oldName, newName)
	if cx != nil {
		s.start(cx)
	}
	return err
}

// renameMember returns the member to start: the renamed gallery, or the old
// one reopened after a refused rename.
func (s *Set) renameMember(oldName, newName string) (*Gallery, error) {
	s.Lock()
	defer s.Unlock()
	cx := s.Get(oldName)
	if cx == nil {
		return nil, fmt.Errorf("unknown gallery %q", oldName)
	}
	if s.Get(newName) != nil {
		return nil, fmt.Errorf("gallery %q already exists", newName)
	}
	layout := s.host.Layout()
	if dir, what := layout.dataFolderHolds(oldName); what != "" {
		return nil, fmt.Errorf("%s holds %s, which a rename would move; move it out first", dir, what)
	}
	newDB, newThumbs := layout.DerivePaths(newName)
	newDir := filepath.Dir(newDB)
	if _, err := os.Stat(newDir); err == nil {
		return nil, fmt.Errorf("data dir %q already exists", newDir)
	}
	if err := cx.exportBusy(); err != nil {
		return nil, err
	}
	cx.Close()
	oldDir := filepath.Dir(cx.DBPath)
	restoreOld := func(refused error, dbPath, thumbnailsPath string) (*Gallery, error) {
		reopened, err := Open(config.Gallery{
			Name: oldName, GalleryPath: cx.GalleryPath, DBPath: dbPath, ThumbnailsPath: thumbnailsPath,
		})
		if err != nil {
			logx.Errorf("gallery %q: could not reopen after a refused rename: %v", oldName, err)
			return nil, fmt.Errorf("%w; reopening the gallery also failed (a switch to it tries again): %v", refused, err)
		}
		s.publish(func(members map[string]*Gallery, _ *string) { members[oldName] = reopened })
		s.Redraw()
		return reopened, refused
	}
	moved := false
	if err := os.Rename(oldDir, newDir); err != nil {
		if !os.IsNotExist(err) {
			return restoreOld(fmt.Errorf("rename data dir %q -> %q: %w", oldDir, newDir, err), cx.DBPath, cx.ThumbnailsPath)
		}
	} else {
		moved = true
	}
	undo := func(refused error) (*Gallery, error) {
		if moved {
			if err := os.Rename(newDir, oldDir); err != nil {
				logx.Errorf("gallery %q: could not put the data dir back at %q: %v", oldName, oldDir, err)
				// Reopened where the data sits: an Open at the old path would start an empty database.
				return restoreOld(fmt.Errorf("%w; its data folder stays at %q (move it back to %q before the next start): %v",
					refused, newDir, oldDir, err), newDB, newThumbs)
			}
		}
		return restoreOld(refused, cx.DBPath, cx.ThumbnailsPath)
	}
	newCx, err := Open(config.Gallery{Name: newName, GalleryPath: cx.GalleryPath, DBPath: newDB, ThumbnailsPath: newThumbs})
	if err != nil {
		return undo(err)
	}
	if err := s.host.Renamed(oldName, newName, newDB, newThumbs); err != nil {
		newCx.Close()
		return undo(fmt.Errorf("persist gallery rename: %w", err))
	}
	s.publish(func(members map[string]*Gallery, active *string) {
		delete(members, oldName)
		members[newName] = newCx
		if *active == oldName {
			*active = newName
		}
	})
	s.Redraw()
	logx.Infof("gallery: renamed %q to %q", oldName, newName)
	return newCx, nil
}

// NestingNote tells what a gallery just added sits inside or leaves out.
func (s *Set) NestingNote(name string) string {
	cx := s.Get(name)
	if cx == nil {
		return ""
	}
	galleries := s.host.Layout().Galleries
	slices.SortFunc(galleries, func(a, b config.Gallery) int { return strings.Compare(a.Name, b.Name) })
	var around *Gallery
	var inside []config.Gallery
	for _, g := range galleries {
		other := s.Get(g.Name)
		if g.Name == name || other == nil {
			continue
		}
		switch {
		case gallery.Encloses(g.GalleryPath, cx.GalleryPath):
			if around == nil || gallery.Encloses(around.GalleryPath, g.GalleryPath) {
				around = other
			}
		case gallery.Encloses(cx.GalleryPath, g.GalleryPath):
			inside = append(inside, g)
		}
	}
	var outermost []string
	for _, g := range inside {
		if !slices.ContainsFunc(inside, func(o config.Gallery) bool { return gallery.Encloses(o.GalleryPath, g.GalleryPath) }) {
			outermost = append(outermost, g.Name)
		}
	}
	note := ""
	if around != nil {
		note += fmt.Sprintf(" It sits inside gallery %s, which leaves this folder to it from now on", around.Name)
		if n := around.imagesUnder(name); n > 0 {
			note += fmt.Sprintf(": %s's next sync marks the %d %s it holds there missing", around.Name, n, models.Plural(n, "image", "images"))
		}
		note += "."
	}
	switch len(outermost) {
	case 0:
	case 1:
		note += fmt.Sprintf(" It leaves out the folder of gallery %s.", outermost[0])
	default:
		note += fmt.Sprintf(" It leaves out the folders of galleries %s.", strings.Join(outermost, ", "))
	}
	return note
}

func (cx *Gallery) imagesUnder(inner string) int {
	for _, e := range cx.Boundary().Fenced() {
		if e.Kind != gallery.FenceGallery || e.Owner != inner {
			continue
		}
		var n int
		if err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*) FROM images WHERE is_missing = 0
			   AND (folder_path = ? COLLATE NOCASE OR folder_path LIKE ? ESCAPE '\' COLLATE NOCASE)`,
			e.Rel, db.EscapeLike(e.Rel)+"/%",
		).Scan(&n); err != nil {
			logx.Warnf("gallery %q: count images under %q: %v", cx.Name, e.Rel, err)
		}
		return n
	}
	return 0
}

// LeftOutBy counts the indexed images, over every gallery, that ignore leaves
// out: the next sync marks them missing.
func (s *Set) LeftOutBy(ignore []string) int {
	n := 0
	for name, b := range s.Draw(ignore) {
		if cx := s.Get(name); cx != nil {
			n += cx.imagesExcludedBy(b)
		}
	}
	return n
}

func (cx *Gallery) imagesExcludedBy(b *gallery.Boundary) int {
	rows, err := cx.DB.Read.Query(`SELECT canonical_path FROM images WHERE is_missing = 0`)
	if err != nil {
		logx.Warnf("gallery %q: count ignored images: %v", cx.Name, err)
		return 0
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && b.Excludes(p) {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("gallery %q: count ignored images: %v", cx.Name, err)
	}
	return n
}
