package gallery

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func countRows(t *testing.T, database *db.DB, from string, args ...any) int {
	t.Helper()
	var n int
	if err := database.Read.QueryRow(`SELECT COUNT(*) FROM `+from, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", from, err)
	}
	return n
}

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func mustRemoveAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove all %s: %v", path, err)
	}
}

func mustChtimes(t *testing.T, path string, atime, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, atime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("rename %s -> %s: %v", from, to, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	return f
}

func mustIngest(t *testing.T, database *db.DB, galleryPath, thumbnailsPath, path, origin string) (*models.Image, bool) {
	t.Helper()
	img, dup, err := Ingest(testHandle(database, galleryPath, thumbnailsPath), path, origin)
	if err != nil {
		t.Fatalf("Ingest %s: %v", path, err)
	}
	return img, dup
}

func imageIDByPath(t *testing.T, database *db.DB, path string) int64 {
	t.Helper()
	var id int64
	if err := database.Read.QueryRow(`SELECT id FROM images WHERE canonical_path = ?`, path).Scan(&id); err != nil {
		t.Fatalf("image at %s: %v", path, err)
	}
	return id
}

func mustParseNameTemplate(t *testing.T, s string, sc Scope) *NameTemplate {
	t.Helper()
	tmpl, err := ParseNameTemplate(s, sc)
	if err != nil {
		t.Fatalf("ParseNameTemplate(%q): %v", s, err)
	}
	return tmpl
}

func mustPlaceImage(t *testing.T, database *db.DB, galleryPath string, id int64, targetFolder, newName *string) *MoveImageResult {
	t.Helper()
	res, err := PlaceImage(database, NewBoundary(galleryPath, nil, nil), id, targetFolder, newName)
	if err != nil {
		t.Fatalf("PlaceImage(%d): %v", id, err)
	}
	return res
}

func mustOpenManga(t *testing.T, path string) *Manga {
	t.Helper()
	m, err := OpenManga(path)
	if err != nil {
		t.Fatalf("OpenManga %s: %v", path, err)
	}
	return m
}

func mustAddSource(t *testing.T, database *db.DB, imageID int64, site, postID, url string) {
	t.Helper()
	if err := AddSourceMembership(database, imageID, site, postID, url); err != nil {
		t.Fatalf("AddSourceMembership(%d, %q): %v", imageID, site, err)
	}
}

func mustExec(t *testing.T, ex *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := ex.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
	return res
}

func testHandle(database *db.DB, galleryPath, thumbnailsPath string) Handle {
	return Handle{GalleryPath: galleryPath, ThumbnailsPath: thumbnailsPath, DB: database, TagSvc: tags.New(database, nil)}
}

func boundHandle(database *db.DB, b *Boundary, thumbnailsPath string) Handle {
	h := testHandle(database, b.Root(), thumbnailsPath)
	h.Bounds = new(atomic.Pointer[Boundary])
	h.Bounds.Store(b)
	return h
}

type planRecorder struct {
	drv   driver.Driver
	dsn   string
	mu    sync.Mutex
	plans map[string]string
}

func (r *planRecorder) Connect(context.Context) (driver.Conn, error) {
	c, err := r.drv.Open(r.dsn)
	if err != nil {
		return nil, err
	}
	return planConn{Conn: c, r: r}, nil
}

func (r *planRecorder) Driver() driver.Driver { return r.drv }

type planConn struct {
	driver.Conn
	r *planRecorder
}

func (c planConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q := c.Conn.(driver.QueryerContext)
	rows, err := q.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args)
	if err != nil {
		return nil, err
	}
	var plan strings.Builder
	vals := make([]driver.Value, len(rows.Columns()))
	for rows.Next(vals) == nil {
		fmt.Fprintln(&plan, vals[len(vals)-1])
	}
	_ = rows.Close()
	c.r.mu.Lock()
	if c.r.plans == nil {
		c.r.plans = map[string]string{}
	}
	c.r.plans[query] = plan.String()
	c.r.mu.Unlock()
	return q.QueryContext(ctx, query, args)
}

func recordPlans(t *testing.T, database *db.DB, path string) (*db.DB, *planRecorder) {
	t.Helper()
	r := &planRecorder{drv: database.Read.Driver(), dsn: "file:" + path}
	read := sql.OpenDB(r)
	t.Cleanup(func() { _ = read.Close() })
	return &db.DB{Read: read, Write: database.Write}, r
}
