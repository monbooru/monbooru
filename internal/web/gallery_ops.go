package web

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

func (s *Server) switchGallery(name string) error { return s.set.Switch(name) }

func (s *Server) setDefault(name string) error {
	s.set.Lock()
	if s.set.Get(name) == nil {
		s.set.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	if s.defaultGallery() == name {
		s.set.Unlock()
		return nil
	}
	err := s.withConfig(func(c *config.Config) error {
		c.DefaultGallery = name
		return nil
	})
	s.set.Unlock()
	if err != nil {
		return fmt.Errorf("persist default gallery: %w", err)
	}
	logx.Infof("gallery: default set to %q", name)
	return nil
}

func (s *Server) addGallery(name, galleryPath string) error {
	name, galleryPath = strings.TrimSpace(name), strings.TrimSpace(galleryPath)
	if err := s.set.Add(name, galleryPath); err != nil {
		return err
	}
	logx.Infof("gallery: added %q (path=%q)", name, galleryPath)
	s.queueGlobalPull(name)
	return nil
}

func (s *Server) removeGallery(name string, removeFolder bool) ([]string, error) {
	kept, err := s.set.Remove(name, removeFolder)
	if err != nil {
		return nil, err
	}
	logx.Infof("gallery: removed %q (folder removed=%t)", name, removeFolder)
	return kept, nil
}

func (s *Server) renameGallery(oldName, newName string) error {
	return s.set.Rename(strings.TrimSpace(oldName), strings.TrimSpace(newName))
}

func renameGalleryInConfig(cfg *config.Config, oldName, newName, newDB, newThumbs string) {
	if g := cfg.FindGallery(oldName); g != nil {
		g.Name, g.DBPath, g.ThumbnailsPath = newName, newDB, newThumbs
	}
	if cfg.DefaultGallery == oldName {
		cfg.DefaultGallery = newName
	}
	cfg.RenameGalleryRefs(oldName, newName)
}

func (s *Server) repointGallery(name, galleryPath string) error {
	galleryPath = strings.TrimSpace(galleryPath)
	changed, err := s.set.Repoint(name, galleryPath)
	if err != nil || !changed {
		return err
	}
	logx.Infof("gallery: %q now points at %q", name, filepath.Clean(galleryPath))
	return nil
}

func (s *Server) galleryList() []config.Gallery {
	out := s.galleries()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type galleryRow struct {
	config.Gallery
	Images    int
	Tags      int
	LeavesOut []string
}

// The active row takes the caller's counts, which the footer shows: a
// second cache read could straddle an invalidation and disagree.
func (s *Server) galleryRowsWithSnapshot(activeName string, activeImages, activeTags int) []galleryRow {
	galleries := s.galleryList()
	out := make([]galleryRow, len(galleries))
	for i, g := range galleries {
		out[i].Gallery = g
		cx := s.get(g.Name)
		if cx != nil {
			for _, e := range cx.Boundary().Fenced() {
				out[i].LeavesOut = append(out[i].LeavesOut, e.Rel+" ("+e.OwnerName()+")")
			}
		}
		if g.Name == activeName {
			out[i].Images = activeImages
			out[i].Tags = activeTags
			continue
		}
		if cx == nil || cx.DB == nil {
			continue
		}
		if n, ok := cx.VisibleCount(); ok {
			out[i].Images = n
		}
		if n, ok := cx.TagCount(); ok {
			out[i].Tags = n
		}
	}
	return out
}

func (s *Server) gallerySwitchHandler(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.switchGallery(name); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		// Home, not a refresh: the ids in the current URL mean other rows
		// in the new gallery.
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) settingsGalleriesPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, galleryio.MaxImportBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "bad form data: "+err.Error())
		return
	}
	fields, filePart, err := readFieldsToFile(mr)
	if err != nil {
		writeInlineFlash(w, "err", "bad form data: "+err.Error())
		return
	}
	if filePart != nil {
		defer func() { _ = filePart.Close() }()
	}
	var file *bufio.Reader
	var format string
	if filePart != nil {
		file = bufio.NewReader(filePart)
		if _, err := file.Peek(1); err == io.EOF {
			file = nil
		}
	}
	if file != nil {
		format = galleryio.FormatFromExt(filePart.FileName())
		if format == "" {
			writeInlineFlash(w, "err", "Import file must be .db, .json, or .zip.")
			return
		}
		if format == "zip" {
			galleryPath := strings.TrimSpace(fields["gallery_path"])
			if entries, err := os.ReadDir(galleryPath); err == nil && len(entries) > 0 {
				writeInlineFlash(w, "err", galleryPath+" is not empty: an imported .zip replaces what a gallery folder holds, so it needs an empty folder. Add the gallery without the file, then use its Import, which asks you to confirm.")
				return
			}
		}
	}
	name := fields["name"]
	if err := s.addGallery(name, fields["gallery_path"]); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}

	nesting := s.set.NestingNote(name)
	if file == nil {
		if switchErr := s.switchGallery(name); switchErr != nil {
			logx.Infof("gallery %q: post-add switch skipped: %v", name, switchErr)
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" added and now active."+nesting)
		return
	}
	leftOut, err := s.importGallery(name, format, file)
	if err != nil {
		writeInlineFlash(w, "err", "Gallery created. Import failed: "+err.Error())
		return
	}
	writeInlineFlash(w, "ok", "Gallery "+name+" added and imported."+nesting+leftOutNote(leftOut))
}

func (s *Server) settingsGalleryRenamePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := r.PathValue("name")
	newName := strings.TrimSpace(r.FormValue("new_name"))
	if err := s.renameGallery(oldName, newName); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	if oldName != newName {
		if err := s.ml.GalleryRenamed(r.Context(), oldName, newName); err != nil {
			logx.Warnf("gallery: telling monloader about the rename of %q: %v", oldName, err)
			writeInlineFlash(w, "warn", fmt.Sprintf("Renamed %s to %s. monloader could not be told, so its default gallery and site targets still name %s.", oldName, newName, oldName))
			return
		}
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDeletePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	confirm := strings.TrimSpace(r.FormValue("confirm_name"))
	if confirm != name {
		writeInlineFlash(w, "err", "type-to-confirm name does not match")
		return
	}
	removeFolder := r.FormValue("remove_folder") == "on"
	kept, err := s.removeGallery(name, removeFolder)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	if len(kept) > 0 {
		writeInlineFlash(w, "ok", fmt.Sprintf("Deleted %s. Kept what it leaves out: %s.", name, library.KeptList(kept)))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDefaultPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.setDefault(name); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

type setHost struct{ s *Server }

func (h setHost) Layout() library.Layout {
	s := h.s
	c := s.cfgSnapshot()
	return library.Layout{
		Galleries:   c.Galleries,
		DataPath:    c.Paths.DataPath,
		ModelPath:   c.Paths.ModelPath,
		ThemesDir:   s.themesDir(),
		PluginsDir:  s.pluginsDir(),
		Ignore:      slices.Clone(c.Gallery.Ignore),
		DerivePaths: c.DerivePaths,
	}
}

func (h setHost) Background(name string) library.Background { return h.s.background(name) }

func (h setHost) DefaultGallery() string { return h.s.defaultGallery() }

func (h setHost) Added(g config.Gallery) error {
	return h.s.withConfig(func(cfg *config.Config) error {
		cfg.Galleries = append(cfg.Galleries, g)
		return nil
	})
}

func (h setHost) Removed(name string) error {
	return h.s.withConfig(func(cfg *config.Config) error {
		cfg.Galleries = slices.DeleteFunc(cfg.Galleries, func(g config.Gallery) bool { return g.Name == name })
		cfg.DropGalleryRefs(name)
		return nil
	})
}

func (h setHost) Repointed(name, galleryPath string) error {
	return h.s.withConfig(func(cfg *config.Config) error {
		if g := cfg.FindGallery(name); g != nil {
			g.GalleryPath = galleryPath
		}
		return nil
	})
}

func (h setHost) Renamed(oldName, newName, dbPath, thumbnailsPath string) error {
	return h.s.withConfig(func(cfg *config.Config) error {
		renameGalleryInConfig(cfg, oldName, newName, dbPath, thumbnailsPath)
		return nil
	})
}
