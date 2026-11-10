package library

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/search"
)

type testHost struct {
	root      string
	galleries []config.Gallery
	def       string
	saveErr   error

	started func()
	renamed func()

	thumbnails func(name string) string
}

func (h *testHost) dataDir(name string) string { return filepath.Join(h.root, "data", name) }

func (h *testHost) Layout() Layout {
	return Layout{
		Galleries: slices.Clone(h.galleries),
		DataPath:  filepath.Join(h.root, "data"),
		DerivePaths: func(name string) (string, string) {
			if h.thumbnails != nil {
				return filepath.Join(h.dataDir(name), "monbooru.db"), h.thumbnails(name)
			}
			return filepath.Join(h.dataDir(name), "monbooru.db"), filepath.Join(h.dataDir(name), "thumbnails")
		},
	}
}

func (h *testHost) Background(string) Background {
	if h.started != nil {
		h.started()
	}
	return Background{}
}
func (h *testHost) DefaultGallery() string { return h.def }

func (h *testHost) Added(g config.Gallery) error {
	if h.saveErr != nil {
		return h.saveErr
	}
	h.galleries = append(h.galleries, g)
	return nil
}

func (h *testHost) Removed(name string) error {
	if h.saveErr != nil {
		return h.saveErr
	}
	h.galleries = slices.DeleteFunc(h.galleries, func(g config.Gallery) bool { return g.Name == name })
	return nil
}

func (h *testHost) Repointed(name, galleryPath string) error {
	if h.saveErr != nil {
		return h.saveErr
	}
	for i := range h.galleries {
		if h.galleries[i].Name == name {
			h.galleries[i].GalleryPath = galleryPath
		}
	}
	return nil
}

func (h *testHost) Renamed(oldName, newName, dbPath, thumbnailsPath string) error {
	if h.renamed != nil {
		h.renamed()
	}
	if h.saveErr != nil {
		return h.saveErr
	}
	for i := range h.galleries {
		if h.galleries[i].Name == oldName {
			h.galleries[i].Name, h.galleries[i].DBPath, h.galleries[i].ThumbnailsPath = newName, dbPath, thumbnailsPath
		}
	}
	return nil
}

func (h *testHost) folder(t *testing.T, rel string) string {
	t.Helper()
	dir := filepath.Join(h.root, "folders", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (h *testHost) gallery(t *testing.T, name string) config.Gallery {
	t.Helper()
	db, thumbs := h.Layout().DerivePaths(name)
	return config.Gallery{Name: name, GalleryPath: h.folder(t, name), DBPath: db, ThumbnailsPath: thumbs}
}

func openTestSet(t *testing.T, names ...string) (*Set, *testHost, *jobs.Manager) {
	t.Helper()
	h := &testHost{root: t.TempDir(), def: names[0]}
	for _, n := range names {
		h.galleries = append(h.galleries, h.gallery(t, n))
	}
	jm := jobs.NewManager()
	s, err := OpenSet(h, jm, h.galleries, names[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, h, jm
}

func TestOpenSet_FailsOnAnUnopenableGallery(t *testing.T) {
	h := &testHost{root: t.TempDir()}
	ok, bad := h.gallery(t, "ok"), h.gallery(t, "bad")
	if err := os.MkdirAll(filepath.Dir(bad.DBPath), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, bad.ThumbnailsPath, []byte("not a folder"), 0o644)
	if s, err := OpenSet(h, jobs.NewManager(), []config.Gallery{ok, bad}, "ok"); err == nil {
		s.Close()
		t.Fatal("a set with an unopenable gallery opened")
	}
}

func TestSet_PublishLeavesEarlierReadersTheirSnapshot(t *testing.T) {
	s, _, _ := openTestSet(t, "b", "a")
	if got := s.Names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("names = %v, want sorted", got)
	}
	if s.ActiveName() != "b" || s.Active() != s.Get("b") {
		t.Fatalf("active = %q, want b", s.ActiveName())
	}
	before := s.state.Load()
	s.Lock()
	s.publish(func(members map[string]*Gallery, active *string) {
		delete(members, "b")
		*active = "a"
	})
	s.Unlock()
	if s.Get("b") != nil || s.ActiveName() != "a" {
		t.Errorf("after the change: members %v, active %q", s.Names(), s.ActiveName())
	}
	if before.members["b"] == nil || before.active != "b" {
		t.Error("the change reached a snapshot published before it")
	}
	before.members["b"].Close()
}

func TestSet_ChangesRefuseWhileAJobHoldsTheLane(t *testing.T) {
	s, h, jm := openTestSet(t, "a", "b")
	if err := jm.Start("tag"); err != nil {
		t.Fatal(err)
	}
	defer jm.Complete("done")
	checks := map[string]func() error{
		"switch": func() error { return s.Switch("b") },
		"add":    func() error { return s.Add("c", h.folder(t, "c")) },
		"remove": func() error { _, err := s.Remove("b", false); return err },
		"repoint": func() error {
			_, err := s.Repoint("b", h.folder(t, "elsewhere"))
			return err
		},
	}
	for name, change := range checks {
		if err := change(); !errors.Is(err, ErrJobRunning) {
			t.Errorf("%s: err %v, want ErrJobRunning", name, err)
		}
	}
	if s.ActiveName() != "a" || len(s.Names()) != 2 {
		t.Errorf("a refused change moved the set: active %q, members %v", s.ActiveName(), s.Names())
	}
}

func TestSet_AddFencesTheNewMemberOffAnEnclosingOne(t *testing.T) {
	s, h, _ := openTestSet(t, "outer")
	inner := h.folder(t, "outer/inner")
	if err := s.Add("inner", inner); err != nil {
		t.Fatal(err)
	}
	if s.Get("inner") == nil || !slices.ContainsFunc(h.galleries, func(g config.Gallery) bool { return g.Name == "inner" }) {
		t.Fatal("the added gallery is missing from the set or the host")
	}
	if err := s.Get("outer").Boundary().Check(filepath.Join(inner, "a.png")); err == nil {
		t.Error("the enclosing gallery still reaches into the added one's folder")
	}
	if err := s.Add("twin", inner); err == nil || !strings.Contains(err.Error(), "already the folder of gallery inner") {
		t.Errorf("a second gallery on the same folder: err %v", err)
	}
}

func TestSet_RemoveRefusesTheActiveTheDefaultAndTheLast(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b", "c")
	h.def = "b"
	for _, name := range []string{"a", "b"} {
		if _, err := s.Remove(name, false); err == nil {
			t.Errorf("removing %s was allowed", name)
		}
	}
	dataDir := filepath.Dir(s.Get("c").DBPath)
	if _, err := s.Remove("c", false); err != nil {
		t.Fatal(err)
	}
	if s.Get("c") != nil || slices.ContainsFunc(h.galleries, func(g config.Gallery) bool { return g.Name == "c" }) {
		t.Error("the removed gallery is still in the set or the host")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("the removed gallery's data folder stayed: %v", err)
	}
	if !exists(h.folder(t, "c")) {
		t.Error("the gallery folder went without removeFolder")
	}
	one, _, _ := openTestSet(t, "only")
	if _, err := one.Remove("only", false); err == nil {
		t.Error("the last gallery was removed")
	}
}

func TestSet_RemoveKeepsAFolderAnotherGalleryShares(t *testing.T) {
	h := &testHost{root: t.TempDir(), def: "a"}
	a, b, c := h.gallery(t, "a"), h.gallery(t, "b"), h.gallery(t, "c")
	c.GalleryPath = b.GalleryPath
	h.galleries = []config.Gallery{a, b, c}
	s, err := OpenSet(h, jobs.NewManager(), h.galleries, "a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	shared := filepath.Join(b.GalleryPath, "b-image.png")
	mustWriteFile(t, shared, []byte("b's file"), 0o644)
	if _, err := s.Remove("c", true); err == nil || !strings.Contains(err.Error(), "indexes the same folder") {
		t.Fatalf("Remove err = %v, want a refusal naming the shared folder", err)
	}
	if !exists(shared) {
		t.Error("the shared folder's file went")
	}
	if s.Get("c") == nil {
		t.Error("the refused gallery left the set")
	}
	if _, err := s.Remove("c", false); err != nil {
		t.Fatalf("removing without the folder: %v", err)
	}
	if !exists(shared) {
		t.Error("removing without the folder took the file")
	}
}

func TestSet_RepointReopensOnlyOnAnotherFolder(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	old := s.Get("b")
	if changed, err := s.Repoint("b", old.GalleryPath); err != nil || changed {
		t.Fatalf("repoint onto the same folder: changed %t, err %v", changed, err)
	}
	moved := h.folder(t, "moved")
	if changed, err := s.Repoint("b", moved); err != nil || !changed {
		t.Fatalf("repoint: changed %t, err %v", changed, err)
	}
	if s.Get("b") == old || s.Get("b").GalleryPath != moved || h.galleries[1].GalleryPath != moved {
		t.Error("the repoint did not reopen the gallery on the new folder")
	}
}

func TestAdd_AFailedSaveLeavesNoDataDir(t *testing.T) {
	s, h, _ := openTestSet(t, "a")
	h.saveErr = errors.New("disk full")
	if err := s.Add("b", h.folder(t, "b")); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v, want the save failure", err)
	}
	if s.Get("b") != nil || len(h.galleries) != 1 {
		t.Errorf("after the refused add: members %v, host %d gallery(ies)", s.Names(), len(h.galleries))
	}
	if exists(h.dataDir("b")) {
		t.Error("the refused add left its data folder behind")
	}
	h.saveErr = nil
	if err := s.Add("b", h.folder(t, "b")); err != nil {
		t.Fatalf("adding the name again: %v", err)
	}
}

func TestAdd_AGalleryThatFailsToOpenLeavesNoDataDir(t *testing.T) {
	s, h, _ := openTestSet(t, "a")
	blocker := filepath.Join(h.root, "blocker")
	mustWriteFile(t, blocker, []byte("a file, not a folder"), 0o644)
	h.thumbnails = func(name string) string { return filepath.Join(blocker, name) }
	if err := s.Add("b", h.folder(t, "b")); err == nil || !strings.Contains(err.Error(), "thumbnails") {
		t.Fatalf("err %v, want the failed open", err)
	}
	if s.Get("b") != nil || len(h.galleries) != 1 {
		t.Errorf("after the failed add: members %v, host %d gallery(ies)", s.Names(), len(h.galleries))
	}
	if exists(h.dataDir("b")) {
		t.Error("the failed add left its data folder behind")
	}
	if !exists(blocker) {
		t.Error("the failed add removed a file it did not write")
	}
}

func TestAdd_RefusesARelativeGalleryPath(t *testing.T) {
	s, h, _ := openTestSet(t, "a")
	h.folder(t, "rel")
	t.Chdir(h.root)
	if err := s.Add("b", filepath.Join("folders", "rel")); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("err %v, want the relative path refused", err)
	}
	if s.Get("b") != nil || exists(h.dataDir("b")) {
		t.Error("the refused add left a gallery or its data behind")
	}
}

func TestRemove_AFailedSaveKeepsTheGalleryAndItsData(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	cx := s.Get("b")
	h.saveErr = errors.New("disk full")
	if _, err := s.Remove("b", true); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v, want the save failure", err)
	}
	if s.Get("b") != cx || len(h.galleries) != 2 {
		t.Fatalf("after the refused remove: members %v, host %d gallery(ies)", s.Names(), len(h.galleries))
	}
	if err := cx.DB.Read.Ping(); err != nil {
		t.Errorf("the kept gallery was closed: %v", err)
	}
	if !exists(cx.DBPath) || !exists(cx.GalleryPath) {
		t.Error("the refused remove deleted the gallery's data or folder")
	}
}

func TestRepoint_AFailedSaveKeepsTheOldFolder(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	cx := s.Get("b")
	old := cx.GalleryPath
	h.saveErr = errors.New("disk full")
	if _, err := s.Repoint("b", h.folder(t, "moved")); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v, want the save failure", err)
	}
	if s.Get("b") != cx || cx.GalleryPath != old || h.galleries[1].GalleryPath != old {
		t.Fatal("the refused repoint moved the gallery")
	}
	if err := cx.DB.Read.Ping(); err != nil {
		t.Errorf("the kept gallery was closed: %v", err)
	}
}

func TestSet_ReplaceLeavesTheGalleryOpenWhenStagingFails(t *testing.T) {
	s, _, _ := openTestSet(t, "a", "b")
	cx := s.Get("b")
	staged := errors.New("upload cut short")
	_, err := s.Replace("b", func(string) error { return staged },
		func(string, string, *gallery.Boundary, func() error) error {
			t.Fatal("applied after a failed stage")
			return nil
		})
	if !errors.Is(err, staged) {
		t.Fatalf("err %v, want the staging error", err)
	}
	if s.Get("b") != cx || cx.DB.Read.Ping() != nil {
		t.Error("a failed stage closed or replaced the gallery")
	}
	applied := errors.New("bad archive")
	reopened, err := s.Replace("b", func(string) error { return nil },
		func(_, _ string, _ *gallery.Boundary, quiesce func() error) error {
			if err := quiesce(); err != nil {
				return err
			}
			return applied
		})
	if !errors.Is(err, applied) || reopened == nil || s.Get("b") != reopened || reopened == cx {
		t.Fatalf("after a failed apply: err %v, reopened %v", err, reopened)
	}
	if err := reopened.DB.Read.Ping(); err != nil {
		t.Errorf("the gallery reopened after a failed apply is not usable: %v", err)
	}
}

func TestSet_ReplaceLeavesTheGalleryOpenWhenApplyRefusesBeforeQuiescing(t *testing.T) {
	s, _, _ := openTestSet(t, "a", "b")
	cx := s.Get("b")
	refused := errors.New("rejecting archive entry")
	got, err := s.Replace("b", func(string) error { return nil },
		func(string, string, *gallery.Boundary, func() error) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("err %v, want the refusal", err)
	}
	if got != cx || s.Get("b") != cx || cx.Closed() || cx.DB.Read.Ping() != nil {
		t.Error("a refusal before quiescing closed or replaced the gallery")
	}
}

func TestSetChanges_StartTheWatcherOutsideTheLock(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	var locked []bool
	h.started = func() {
		free := s.TryRLock()
		if free {
			s.RUnlock()
		}
		locked = append(locked, !free)
	}
	changes := []struct {
		name   string
		change func() error
	}{
		{"add", func() error { return s.Add("c", h.folder(t, "c")) }},
		{"repoint", func() error { _, err := s.Repoint("c", h.folder(t, "moved")); return err }},
		{"replace", func() error {
			_, err := s.Replace("c", func(string) error { return nil }, func(_, _ string, _ *gallery.Boundary, quiesce func() error) error { return quiesce() })
			return err
		}},
		{"rename", func() error { return s.Rename("c", "d") }},
	}
	for _, c := range changes {
		locked = nil
		if err := c.change(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(locked) != 1 || locked[0] {
			t.Errorf("%s: the watcher started with the set locked (%v)", c.name, locked)
		}
	}
}

func TestSetChanges_AShutdownCloseBeforeTheStartLeavesNothingRunning(t *testing.T) {
	s, h, _ := openTestSet(t, "a")
	closed := make(chan struct{})
	h.started = func() {
		h.started = nil
		go func() {
			s.Close()
			close(closed)
		}()

		select {
		case <-closed:
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := s.Add("b", h.folder(t, "b")); err != nil {
		t.Fatal(err)
	}
	<-closed
	if cx := s.Get("b"); cx.ReclaimerRunning() || cx.WatcherRunning() {
		t.Error("the gallery's background work started after the set closed")
	}
}

func TestReplace_AGalleryWhoseReopenFailedCannotBeSwitchedTo(t *testing.T) {
	s, _, _ := openTestSet(t, "a", "b")
	_, err := s.Replace("b", func(string) error { return nil }, func(_, thumbnailsPath string, _ *gallery.Boundary, quiesce func() error) error {
		if err := quiesce(); err != nil {
			return err
		}
		if err := os.RemoveAll(thumbnailsPath); err != nil {
			return err
		}
		return os.WriteFile(thumbnailsPath, []byte("not a folder"), 0o644)
	})
	if err == nil || !strings.Contains(err.Error(), "reopen gallery") {
		t.Fatalf("err %v, want the failed reopen", err)
	}
	if s.Get("b") == nil {
		t.Fatal("the gallery left the set, so its import cannot be retried")
	}
	if err := s.Switch("b"); err == nil {
		t.Fatal("switched to a gallery whose database is closed")
	}
	if s.ActiveName() != "a" {
		t.Errorf("active = %q, want a", s.ActiveName())
	}
}

func TestSet_RenameMovesTheDataAndCarriesTheActiveName(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	oldDir := filepath.Dir(s.Get("a").DBPath)
	if err := s.Rename("a", "z"); err != nil {
		t.Fatal(err)
	}
	if s.ActiveName() != "z" || s.Get("a") != nil || s.Get("z") == nil {
		t.Fatalf("after the rename: active %q, members %v", s.ActiveName(), s.Names())
	}
	if exists(oldDir) || !exists(h.dataDir("z")) {
		t.Errorf("data folder: old kept %t, new present %t", exists(oldDir), exists(h.dataDir("z")))
	}
	if h.galleries[0].Name != "z" {
		t.Errorf("the host still names %q", h.galleries[0].Name)
	}
}

func TestSet_RenameRollsBackAFailedSave(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	h.saveErr = errors.New("disk full")
	if err := s.Rename("b", "c"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v, want the save failure", err)
	}
	cx := s.Get("b")
	if cx == nil || s.Get("c") != nil {
		t.Fatalf("after the refused rename: members %v", s.Names())
	}
	if err := cx.DB.Read.Ping(); err != nil {
		t.Errorf("the reopened gallery is not usable: %v", err)
	}
	if !exists(h.dataDir("b")) || exists(h.dataDir("c")) {
		t.Errorf("data folder: old present %t, new present %t", exists(h.dataDir("b")), exists(h.dataDir("c")))
	}
}

func TestSet_AGalleryARefusedRenameCouldNotReopenComesBackOnASwitch(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	h.saveErr = errors.New("disk full")
	h.renamed = func() {
		thumbs := filepath.Join(h.dataDir("c"), "thumbnails")
		if err := os.RemoveAll(thumbs); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, thumbs, []byte("not a folder"), 0o644)
	}
	err := s.Rename("b", "c")
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "reopen") {
		t.Fatalf("err %v, want the save failure and the failed reopen", err)
	}
	if cx := s.Get("b"); cx == nil || !cx.Closed() {
		t.Fatalf("after the failed reopen: members %v", s.Names())
	}
	thumbs := filepath.Join(h.dataDir("b"), "thumbnails")
	if err := os.Remove(thumbs); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch("b"); err != nil {
		t.Fatalf("switch to the closed gallery: %v", err)
	}
	cx := s.Get("b")
	if cx.Closed() || s.ActiveName() != "b" {
		t.Fatalf("after the switch: closed %t, active %q", cx.Closed(), s.ActiveName())
	}
	if err := cx.DB.Read.Ping(); err != nil {
		t.Errorf("the reopened gallery is not usable: %v", err)
	}
}

func TestSet_ARenameWhoseDataCannotGoBackReopensWhereTheDataSits(t *testing.T) {
	s, h, _ := openTestSet(t, "a", "b")
	mustExec(t, s.Get("b").DB.Write, `INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES ('s', 'x.png', 'png', 1)`)
	h.saveErr = errors.New("disk full")
	h.renamed = func() {
		if err := os.MkdirAll(h.dataDir("b"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(h.dataDir("b"), "stray"), nil, 0o644)
	}
	err := s.Rename("b", "c")
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), h.dataDir("c")) {
		t.Errorf("err %v, want the save failure and where the data stays", err)
	}
	cx := s.Get("b")
	if cx == nil || cx.Closed() || s.Get("c") != nil {
		t.Fatalf("after the refused rename: members %v", s.Names())
	}
	if n := countRows(t, cx.DB, "images"); n != 1 {
		t.Errorf("the reopened gallery holds %d image(s), want its own 1", n)
	}
	if exists(filepath.Join(h.dataDir("b"), "monbooru.db")) {
		t.Error("an empty database was started at the old data folder")
	}
}

func TestSet_SwitchDropsTheOutgoingGallerysLists(t *testing.T) {
	s, _, _ := openTestSet(t, "a", "b")
	q := search.Query{Cached: true, Sort: "newest", Order: "desc"}
	s.Get("a").cache.Seed(q, []int64{1})
	s.Get("b").cache.Seed(q, []int64{2})
	if err := s.Switch("b"); err != nil {
		t.Fatal(err)
	}
	if _, ok := search.PageOf(s.Get("a").Source(), q, 1, 1); ok {
		t.Error("the outgoing gallery kept its lists")
	}
	if page, ok := search.PageOf(s.Get("b").Source(), q, 2, 1); !ok || page != 1 {
		t.Errorf("the incoming gallery's list holds image 2 at page %d (%t), want its own list", page, ok)
	}
}
