package galleryio

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio/compatibility"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type LightManifestImage = compatibility.ManifestImage

type LightManifest struct {
	Version int                  `json:"version"`
	Images  []LightManifestImage `json:"images"`
}

func readLightManifest(path string) (LightManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return LightManifest{}, fmt.Errorf("open tags.json: %w", err)
	}
	defer func() { _ = f.Close() }()
	var mf LightManifest
	if err := json.NewDecoder(f).Decode(&mf); err != nil {
		return mf, fmt.Errorf("decode tags.json: %w", err)
	}
	if mf.Version != LightManifestVersion {
		return mf, fmt.Errorf("unsupported light export version %d (expected %d)", mf.Version, LightManifestVersion)
	}
	return mf, nil
}

// ImportSourceNative is the tagger_name of native imports; compat imports
// use their format name.
const ImportSourceNative = "import"

func ExportGalleryLight(cx gallery.Handle, w io.Writer) error {
	return writeArchive(w, cx.Boundary(), "tags.json", func(inner io.Writer) error {
		return ExportGalleryLightManifest(cx, inner)
	})
}

func ExportGalleryLightManifest(cx gallery.Handle, w io.Writer) error {
	return writeLightManifest(cx.DB.Read, w, exportScope{}, nil)
}

// DownloadManifest names each image by its path in the zip, not in the gallery.
func DownloadManifest(cx gallery.Handle, names map[int64]string) ([]byte, error) {
	ids := make([]int64, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	var buf bytes.Buffer
	err := writeLightManifest(cx.DB.Read, &buf, newExportScope(ids), names)
	return buf.Bytes(), err
}

func writeLightManifest(read *sql.DB, w io.Writer, sc exportScope, names map[int64]string) error {
	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", LightManifestVersion)
	bw.arrayStart("images")
	first := true
	err := walkLightRows(read, sc, false, func(id int64, sha, relPath string, missing bool, tags []lightTag) error {
		if missing {
			return nil
		}
		// A tag-less image ships an empty array, not null.
		tokens := make([]string, len(tags))
		for i, tg := range tags {
			tokens[i] = tg.token
		}
		if names != nil {
			relPath = names[id]
		}
		bw.arrayItem(&first, LightManifestImage{SHA256: sha, Path: relPath, Tags: tokens})
		return nil
	})
	bw.arrayEnd()
	bw.objEnd()
	if err != nil {
		return err
	}
	return bw.err
}

type lightTag struct {
	token      string
	isAuto     bool
	taggerName string
	confidence *float64
}

// Grouping depends on the ORDER BY i.id: an image closes when the id changes.
func walkLightRows(read *sql.DB, sc exportScope, directOnly bool, emit func(id int64, sha, relPath string, missing bool, tags []lightTag) error) error {
	q := lightRowsQuery
	if directOnly {
		q = lightDirectRowsQuery
	}
	rows, err := read.Query(q+sc.where("i.id")+" ORDER BY i.id, tc.name, t.name", sc.args()...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var curID int64
	var curSHA, curPath string
	var curMissing bool
	var tags []lightTag
	open := false
	for rows.Next() {
		var id int64
		var sha, folder, canonical string
		var missing bool
		var tname, tcat, tagger sql.NullString
		var isAuto sql.NullBool
		var conf *float64
		if err := rows.Scan(&id, &sha, &folder, &canonical, &missing, &tname, &tcat, &isAuto, &tagger, &conf); err != nil {
			return err
		}
		if !open || id != curID {
			if open {
				if err := emit(curID, curSHA, curPath, curMissing, tags); err != nil {
					return err
				}
			}
			curID, open = id, true
			curSHA, curMissing = sha, missing
			curPath = filepath.ToSlash(filepath.Join(folder, storedBasename(canonical)))
			tags = nil
		}
		if tname.Valid {
			tags = append(tags, lightTag{token: compatibility.TagToken(tcat.String, tname.String), isAuto: isAuto.Bool, taggerName: tagger.String, confidence: conf})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if open {
		return emit(curID, curSHA, curPath, curMissing, tags)
	}
	return nil
}

// is_alias and is_implied sit in the joins, not a WHERE, so a skipped
// row can't drop its image.
const (
	lightRowsSelect = `
	SELECT i.id, i.sha256, i.folder_path, i.canonical_path, i.is_missing, t.name, tc.name, it.is_auto, it.tagger_name, it.confidence
	FROM images i
	LEFT JOIN image_tags it ON it.image_id = i.id`
	lightRowsJoins = `
	LEFT JOIN tags t ON t.id = it.tag_id AND t.is_alias = 0
	LEFT JOIN tag_categories tc ON tc.id = t.category_id`
	lightRowsQuery       = lightRowsSelect + lightRowsJoins
	lightDirectRowsQuery = lightRowsSelect + ` AND it.is_implied = 0` + lightRowsJoins
)

func replaceFromLightArchive(manifest *zip.File, files []translatedFile, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int, quiesce func() error) (int, error) {
	mf, err := extractLightManifest(manifest, filepath.Dir(dbPath))
	if err != nil {
		return 0, err
	}
	return ApplyLightReplace(mf, files, dbPath, thumbsPath, b, ImportSourceNative, maxFileSizeMB, quiesce)
}

type translatedFile struct {
	rel  string
	file *zip.File
}

// ApplyLightReplace rebuilds the gallery from mf and files; the count is
// the files b leaves out.
func ApplyLightReplace(mf LightManifest, files []translatedFile, dbPath, thumbsPath string, b *gallery.Boundary, source string, maxFileSizeMB int, quiesce func() error) (int, error) {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	dsts, err := planExtraction(b, files, maxBytes)
	if err != nil {
		return 0, err
	}
	if err := quiesce(); err != nil {
		return 0, err
	}
	leftOut, err := extractPlanned(b, files, dsts, maxBytes)
	if rebuildErr := rebuildFromLightManifest(dbPath, b, thumbsPath, mf.Images, source); err == nil {
		err = rebuildErr
	}
	return leftOut, err
}

// planExtraction runs before anything is reset or wiped, so an archive it
// refuses leaves the gallery whole: it reads every entry it will write.
func planExtraction(b *gallery.Boundary, files []translatedFile, maxBytes int64) ([]string, error) {
	if other := b.SharedWith(); other != "" && len(files) > 0 {
		return nil, fmt.Errorf("gallery %q indexes the same folder; a replace from an archive would delete its files", other)
	}
	root := filepath.Clean(b.Root())
	dsts := make([]string, len(files))
	written := make(map[string]string, len(files))
	lowered := make(map[string]string, len(files))
	folds := sync.OnceValue(func() bool { return foldsCase(root) })
	entryAt := func(dst string) (string, bool) {
		if name, ok := written[dst]; ok {
			return name, true
		}
		name, ok := lowered[strings.ToLower(dst)]
		return name, ok && folds()
	}
	for i, tf := range files {
		dst, err := SafeArchiveDest(root, tf.rel)
		if err != nil {
			return nil, fmt.Errorf("rejecting archive entry %q: %w", tf.file.Name, err)
		}
		if dst == root {
			return nil, fmt.Errorf("rejecting archive entry %q: it names the gallery root", tf.file.Name)
		}
		dsts[i] = dst
		if b.Excludes(dst) {
			continue
		}
		if first, ok := entryAt(dst); ok {
			return nil, fmt.Errorf("rejecting archive entry %q: it names the same file as %q", tf.file.Name, first)
		}
		if err := checkEntrySize(tf.file, maxBytes); err != nil {
			return nil, err
		}
		if err := copyZipEntry(io.Discard, tf.file, maxBytes); err != nil {
			return nil, err
		}
		written[dst] = tf.file.Name
		lowered[strings.ToLower(dst)] = tf.file.Name
	}
	for _, dst := range dsts {
		for dir := filepath.Dir(dst); dir != root; dir = filepath.Dir(dir) {
			if name, ok := entryAt(dir); ok {
				return nil, fmt.Errorf("rejecting archive entry %q: it is both a file and a folder", name)
			}
		}
	}
	return dsts, nil
}

// foldsCase tells whether dir's filesystem takes two names differing only
// in case for one file, by probing it; where it cannot, it goes by the OS.
var foldsCase = func(dir string) bool {
	f, err := os.CreateTemp(dir, ".monbooru-case-*")
	if err != nil {
		return runtime.GOOS == "windows"
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()
	_, err = os.Stat(filepath.Join(dir, strings.ToUpper(filepath.Base(name))))
	return err == nil
}

// extractPlanned carries on past a file it cannot write and returns the
// first error, so the caller's DB step still sees every file that landed.
func extractPlanned(b *gallery.Boundary, files []translatedFile, dsts []string, maxBytes int64) (leftOut int, err error) {
	// No files means a rebuild against what is on disk; wiping would
	// delete the files the manifest names.
	if len(files) == 0 {
		return 0, nil
	}
	if _, err := gallery.RemoveOwned(b); err != nil {
		return 0, fmt.Errorf("wipe gallery: %w", err)
	}
	for i, tf := range files {
		if b.Excludes(dsts[i]) {
			leftOut++
			continue
		}
		writeErr := os.MkdirAll(filepath.Dir(dsts[i]), 0o755)
		if writeErr == nil {
			writeErr = copyZipFile(tf.file, dsts[i], maxBytes)
		}
		if err == nil {
			err = writeErr
		}
	}
	return leftOut, err
}

func removeSidecars(dbPath string) error {
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

func resetThumbs(thumbsPath string) error {
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	// The rebuilt database numbers ids from 1, which the cached pages are keyed by.
	if err := os.RemoveAll(gallery.MangaCacheDir(thumbsPath)); err != nil {
		return fmt.Errorf("clear manga cache: %w", err)
	}
	if err := os.MkdirAll(thumbsPath, 0o755); err != nil {
		return fmt.Errorf("recreate thumbnails dir: %w", err)
	}
	return nil
}

func replaceFromLightManifest(srcPath, dbPath, thumbsPath string, b *gallery.Boundary, quiesce func() error) error {
	mf, err := readLightManifest(srcPath)
	if err != nil {
		return err
	}
	if err := quiesce(); err != nil {
		return err
	}
	return rebuildFromLightManifest(dbPath, b, thumbsPath, mf.Images, ImportSourceNative)
}

// The caller quiesces the gallery first: the build's manga pages land in the
// live cache under the new ids, so it is cleared before and after a failure.
func rebuildFromLightManifest(dbPath string, b *gallery.Boundary, thumbsPath string, images []LightManifestImage, source string) (err error) {
	mangaCache := gallery.MangaCacheDir(thumbsPath)
	if err := os.RemoveAll(mangaCache); err != nil {
		return fmt.Errorf("clear manga cache: %w", err)
	}
	defer func() {
		if err != nil {
			gallery.WaitPageThumbs(time.Minute)
			_ = os.RemoveAll(mangaCache)
		}
	}()
	tmp, err := os.CreateTemp(filepath.Dir(dbPath), "import-*.db")
	if err != nil {
		return fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	newThumbs, err := os.MkdirTemp(filepath.Dir(thumbsPath), "thumbnails-import-*")
	if err != nil {
		return fmt.Errorf("create temp thumbnails dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(newThumbs) }()
	return loadThenInstall(tmpPath, dbPath, thumbsPath, newThumbs, func() error { return nil }, func(database *db.DB) error {
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		ingestLightManifestEntries(database, b, newThumbs, images, source)
		return nil
	})
}

// An entry with no file on disk still gets an is_missing row to keep its tags.
func ingestLightManifestEntries(database *db.DB, b *gallery.Boundary, thumbsPath string, entries []LightManifestImage, source string) {
	galleryPath := b.Root()
	tagSvc := tags.New(database, nil)
	h := gallery.Handle{GalleryPath: galleryPath, ThumbnailsPath: thumbsPath, DB: database, TagSvc: tagSvc}
	generalID := LookupCategoryID(database, "general")
	for _, r := range entries {
		path, err := SafeArchiveDest(galleryPath, r.Path)
		if err != nil {
			logx.Warnf("light import: skipping entry %q: %v", r.Path, err)
			continue
		}
		_, statErr := os.Stat(path)
		if statErr != nil && models.ExtFileType(path) == "" {
			logx.Warnf("light import: skipping %q: its type cannot be told without the file, so its tags are not kept", r.Path)
			continue
		}
		if statErr != nil || b.Excludes(path) {
			imgID, err := insertMissingImageRow(database, r.SHA256, path, galleryPath, source)
			if err != nil {
				logx.Warnf("light import: record missing %q: %v", r.Path, err)
				continue
			}
			applyImportTagsToImage(database, tagSvc, imgID, r.Tags, generalID, source)
			continue
		}
		if _, err := gallery.DetectFileType(path); err != nil {
			logx.Warnf("light import: unsupported file %q: %v", r.Path, err)
			continue
		}
		img, _, err := gallery.Ingest(h, path, source)
		if err != nil {
			logx.Warnf("light import: ingest %q: %v", r.Path, err)
			continue
		}
		applyImportTagsToImage(database, tagSvc, img.ID, r.Tags, generalID, source)
	}
}

func insertMissingImageRow(database *db.DB, sha, path, galleryPath, origin string) (int64, error) {
	ft, err := gallery.DetectFileType(path)
	if err != nil {
		return 0, err
	}
	if sha == "" {
		return 0, fmt.Errorf("manifest entry has empty sha256")
	}
	origin = cmp.Or(origin, models.OriginIngest)
	folder := gallery.FolderPath(galleryPath, path)
	var id int64
	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		err := tx.QueryRow(
			`INSERT INTO images (sha256, canonical_path, folder_path, file_type, file_size,
			                    is_missing, source_type, origin)
			 VALUES (?, ?, ?, ?, 0, 1, ?, ?)
			 ON CONFLICT(sha256) DO NOTHING
			 RETURNING id`,
			sha, path, folder, ft, models.SourceTypeNone, origin,
		).Scan(&id)
		if err == sql.ErrNoRows {
			if id, err = gallery.ImageIDBySHA(tx, sha); err != nil {
				return fmt.Errorf("fetch existing sha: %w", err)
			}
		} else if err != nil {
			return err
		}
		_, err = tx.Exec(
			`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical) VALUES (?, ?, 1)`,
			id, path,
		)
		return err
	}); err != nil {
		return 0, err
	}
	return id, nil
}

type mergeRecord struct {
	SHA256     string
	Tags       []string
	groups     []transferTagGroup // a native upload's rows, in place of Tags
	SourcePath string             // relative to gallery/
	zipEntry   *zip.File          // when set, extract into galleryPath/<unique SourcePath>
}

type MergeResult struct {
	Added     int // images ingested from files the archive carried
	Tagged    int // images already in the gallery that gained a tag
	Unchanged int // images already in the gallery that already had every tag named
	Skipped   int // records whose sha the gallery does not hold and no file backed
	LeftOut   int // files the archive carried into a folder the gallery leaves out
}

// MergeGallery wipes nothing: db and json uploads only tag images matched
// by sha, and a zip also ingests the images it carries files for. A
// cancelled ctx stops it before the next record, with what it did so far.
func MergeGallery(ctx context.Context, cx *library.Gallery, format string, upload io.Reader, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	var res MergeResult
	dataDir := filepath.Dir(cx.DBPath)

	tmp, err := os.CreateTemp(dataDir, "merge-*.upload")
	if err != nil {
		return res, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		return res, fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	var mergeErr error
	switch format {
	case "db":
		res, mergeErr = mergeDBFile(ctx, cx, tmpPath, "uploaded file is not a valid monbooru database", nil, maxFileSizeMB, progress)
	case "json":
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			mergeErr = fmt.Errorf("inspect json: %w", err)
			break
		}
		if isLight {
			res, mergeErr = mergeFromLightJSON(ctx, cx, tmpPath, maxFileSizeMB, progress)
		} else {
			res, mergeErr = mergeJSONFile(ctx, cx, tmpPath, nil, maxFileSizeMB, progress)
		}
	case "zip":
		res, mergeErr = mergeFromZip(ctx, cx, tmpPath, maxFileSizeMB, progress)
	default:
		mergeErr = fmt.Errorf("unknown merge format %q", format)
	}
	if mergeErr == nil {
		logx.Infof("gallery: merged into %q (format=%s): +%d %s, %d tagged, %d skipped",
			cx.Name, format, res.Added, models.Plural(res.Added, "image", "images"), res.Tagged, res.Skipped)
	}
	return res, mergeErr
}

// Summary states Added even at 0: the dialog promises new images, which
// only a .zip can bring.
func (r MergeResult) Summary() string {
	parts := []string{fmt.Sprintf("%d %s added", r.Added, models.Plural(r.Added, "image", "images")), fmt.Sprintf("%d tagged", r.Tagged)}
	if r.Unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", r.Unchanged))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d not in this gallery", r.Skipped))
	}
	if r.LeftOut > 0 {
		parts = append(parts, fmt.Sprintf("%d in folders this gallery leaves out", r.LeftOut))
	}
	return strings.Join(parts, ", ")
}

func mergeDBFile(ctx context.Context, cx *library.Gallery, path, invalidMsg string, files map[string]*zip.File, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	if err := validateSQLiteFile(path); err != nil {
		return MergeResult{}, fmt.Errorf("%s: %w", invalidMsg, err)
	}
	src, err := db.Open(path)
	if err != nil {
		return MergeResult{}, err
	}
	defer func() { _ = src.Close() }()
	return mergeFromSourceDB(ctx, cx, src, files, maxFileSizeMB, progress)
}

// The document is loaded into a temp db by the replace's loader, so the
// merge walks its rows rather than holding the whole document.
func mergeJSONFile(ctx context.Context, cx *library.Gallery, path string, files map[string]*zip.File, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(cx.DBPath), "merge-*.db")
	if err != nil {
		return MergeResult{}, fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() {
		for _, p := range []string{tmpPath, tmpPath + "-wal", tmpPath + "-shm"} {
			_ = os.Remove(p)
		}
	}()
	src, err := db.Open(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open temp db: %w", err)
	}
	defer func() { _ = src.Close() }()
	if err := db.Bootstrap(src); err != nil {
		return MergeResult{}, fmt.Errorf("bootstrap: %w", err)
	}
	if err := stripScratchSchema(src); err != nil {
		return MergeResult{}, err
	}
	if err := loadExportStream(src, f); err != nil {
		return MergeResult{}, err
	}
	return mergeFromSourceDB(ctx, cx, src, files, maxFileSizeMB, progress)
}

// The temp db is walked once, by image id, so the live schema's
// triggers and secondary indexes would only slow its load.
func stripScratchSchema(d *db.DB) error {
	drops, err := db.QueryAll(d.Read, func(rows *sql.Rows) (string, error) {
		var typ, name string
		err := rows.Scan(&typ, &name)
		return "DROP " + typ + ` "` + name + `"`, err
	}, `SELECT type, name FROM sqlite_master WHERE type = 'trigger' OR (type = 'index' AND sql IS NOT NULL)`)
	if err != nil {
		return fmt.Errorf("list temp db schema: %w", err)
	}
	for _, s := range drops {
		if _, err := d.Write.Exec(s); err != nil {
			return fmt.Errorf("strip temp db: %w", err)
		}
	}
	return nil
}

// A missing row brings its tags but no file: a gallery made inside
// another still takes the outer one's tags after its sync, and an
// archive's file at that path is some other image's. Implied rows stay
// behind: the target's own implications re-derive them.
func mergeFromSourceDB(ctx context.Context, cx *library.Gallery, src *db.DB, files map[string]*zip.File, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	var total int
	if err := src.Read.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&total); err != nil {
		return MergeResult{}, fmt.Errorf("count images: %w", err)
	}
	// An uploaded .db is not upgraded, and one older than the column has
	// only direct rows.
	var hasImplied bool
	if err := src.Read.QueryRow(`SELECT COUNT(*) > 0 FROM pragma_table_xinfo('image_tags') WHERE name = 'is_implied'`).Scan(&hasImplied); err != nil {
		return MergeResult{}, fmt.Errorf("probe image_tags: %w", err)
	}
	if err := createMissingCategories(src, cx); err != nil {
		return MergeResult{}, err
	}
	m := newMerger(ctx, cx, ImportSourceNative, maxFileSizeMB)
	done := 0
	err := walkLightRows(src.Read, exportScope{}, hasImplied, func(_ int64, sha, relPath string, missing bool, tags []lightTag) error {
		if missing {
			relPath = ""
		}
		if progress != nil {
			progress(done, total)
		}
		done++
		return m.apply(mergeRecord{SHA256: sha, SourcePath: relPath, groups: mergeTagGroups(tags), zipEntry: files[relPath]})
	})
	return m.res, err
}

func createMissingCategories(src *db.DB, cx *library.Gallery) error {
	rows, err := src.Read.Query(`SELECT name, color FROM tag_categories ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read categories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, color string
		if err := rows.Scan(&name, &color); err != nil {
			return fmt.Errorf("read categories: %w", err)
		}
		if LookupCategoryID(cx.DB, name) != 0 {
			continue
		}
		if _, err := cx.TagSvc.CreateCategory(name, color); err != nil {
			logx.Warnf("merge: create category %q: %v", name, err)
		}
	}
	return rows.Err()
}

// Manual rows land as the import's; auto rows keep their tagger and score,
// so a target's own auto row is not promoted to a manual one.
func mergeTagGroups(tags []lightTag) []transferTagGroup {
	groups := []transferTagGroup{{taggerName: ImportSourceNative}}
	byTagger := map[string]int{}
	for _, tg := range tags {
		if !tg.isAuto {
			groups[0].tokens = append(groups[0].tokens, tg.token)
			continue
		}
		i, ok := byTagger[tg.taggerName]
		if !ok {
			i = len(groups)
			byTagger[tg.taggerName] = i
			groups = append(groups, transferTagGroup{isAuto: true, taggerName: tg.taggerName})
		}
		groups[i].tokens = append(groups[i].tokens, tg.token)
		groups[i].confs = append(groups[i].confs, tg.confidence)
	}
	return groups
}

func mergeFromLightJSON(ctx context.Context, cx *library.Gallery, tmpPath string, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	mf, err := readLightManifest(tmpPath)
	if err != nil {
		return MergeResult{}, err
	}
	records := make([]mergeRecord, 0, len(mf.Images))
	for _, img := range mf.Images {
		records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags})
	}
	return applyMergeRecords(ctx, cx, records, ImportSourceNative, maxFileSizeMB, progress)
}

func mergeFromZip(ctx context.Context, cx *library.Gallery, tmpPath string, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		return MergeResult{}, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	innerDB, innerJSON, innerLight, inGallery := classifyArchive(zr.File)
	// Keyed by the path under gallery/, which is what a light manifest names.
	galleryFiles := make(map[string]*zip.File, len(inGallery))
	for _, tf := range inGallery {
		galleryFiles[tf.rel] = tf.file
	}

	dataDir := filepath.Dir(cx.DBPath)
	switch {
	case innerLight != nil && innerDB == nil && innerJSON == nil:
		mf, err := extractLightManifest(innerLight, dataDir)
		if err != nil {
			return MergeResult{}, err
		}
		records := make([]mergeRecord, 0, len(mf.Images))
		for _, img := range mf.Images {
			records = append(records, mergeRecord{SHA256: img.SHA256, Tags: img.Tags, SourcePath: img.Path, zipEntry: galleryFiles[img.Path]})
		}
		return applyMergeRecords(ctx, cx, records, ImportSourceNative, maxFileSizeMB, progress)
	case innerDB != nil:
		inner, err := extractZipEntryToTemp(innerDB, dataDir)
		if err != nil {
			return MergeResult{}, err
		}
		defer func() { _ = os.Remove(inner) }()
		return mergeDBFile(ctx, cx, inner, "inner db invalid", galleryFiles, maxFileSizeMB, progress)
	case innerJSON != nil:
		inner, err := extractZipEntryToTemp(innerJSON, dataDir)
		if err != nil {
			return MergeResult{}, err
		}
		defer func() { _ = os.Remove(inner) }()
		return mergeJSONFile(ctx, cx, inner, galleryFiles, maxFileSizeMB, progress)
	}
	if format := compatibility.Detect(zr.File); format != "" {
		return mergeFromCompatArchive(ctx, cx, zr.File, format, maxFileSizeMB, progress)
	}
	return MergeResult{}, fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
}

func applyMergeRecords(ctx context.Context, cx *library.Gallery, records []mergeRecord, source string, maxFileSizeMB int, progress func(done, total int)) (MergeResult, error) {
	m := newMerger(ctx, cx, source, maxFileSizeMB)
	for i, r := range records {
		if progress != nil {
			progress(i, len(records))
		}
		if err := m.apply(r); err != nil {
			return m.res, err
		}
	}
	return m.res, nil
}

type merger struct {
	ctx       context.Context
	cx        *library.Gallery
	source    string
	generalID int64
	maxBytes  int64
	res       MergeResult
}

func newMerger(ctx context.Context, cx *library.Gallery, source string, maxFileSizeMB int) *merger {
	return &merger{ctx: ctx, cx: cx, source: source, generalID: LookupCategoryID(cx.DB, "general"), maxBytes: int64(maxFileSizeMB) * 1024 * 1024}
}

// apply returns only the ctx's error, which stops the merge before r.
func (m *merger) apply(r mergeRecord) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	cx := m.cx
	imgID, err := gallery.ImageIDBySHA(cx.DB.Read, r.SHA256)
	if err == nil {
		m.tagMatched(imgID, r)
		return nil
	}
	if err != sql.ErrNoRows {
		logx.Warnf("merge: lookup sha %s: %v", r.SHA256, err)
		m.res.Skipped++
		return nil
	}
	if r.zipEntry == nil || r.SourcePath == "" {
		m.res.Skipped++
		return nil
	}
	safeBase, err := SafeArchiveDest(cx.GalleryPath, r.SourcePath)
	if err != nil {
		logx.Warnf("merge: skipping entry %q: %v", r.SourcePath, err)
		m.res.Skipped++
		return nil
	}
	// Suffix collisions within the entry's folder, not the root.
	entry := r.zipEntry
	landed, err := cx.Land(m.ctx, filepath.Dir(safeBase), filepath.Base(safeBase),
		func(dst string) error { return copyZipFile(entry, dst, m.maxBytes) }, library.LandSpec{Origin: m.source})
	if err != nil {
		if library.LandStageOf(err) == library.LandPlace {
			m.res.LeftOut++
		} else {
			logx.Warnf("merge: land %q: %v", r.SourcePath, err)
			m.res.Skipped++
		}
		return nil
	}
	// A record without a sha finds its image only once extracted.
	if landed.Duplicate {
		m.tagMatched(landed.Image.ID, r)
		return nil
	}
	m.tag(landed.Image.ID, r)
	m.res.Added++
	return nil
}

func (m *merger) tagMatched(imageID int64, r mergeRecord) {
	if m.tag(imageID, r) {
		m.res.Tagged++
	} else {
		m.res.Unchanged++
	}
}

func (m *merger) tag(imageID int64, r mergeRecord) (gained bool) {
	if r.groups == nil {
		return applyImportTagsToImage(m.cx.DB, m.cx.TagSvc, imageID, r.Tags, m.generalID, m.source)
	}
	gained, err := applyTransferTags(m.cx.DB, m.cx.TagSvc, imageID, r.groups, m.generalID)
	if err != nil {
		logx.Warnf("merge: %v", err)
	}
	return gained
}

// Merged tags go through the tag service so aliases resolve and usage
// counts move. Tokens resolve before the insert transaction:
// GetOrCreateTag needs the single write connection itself.
func applyImportTagsToImage(database *db.DB, tagSvc *tags.Service, imageID int64, tokens []string, generalID int64, source string) (gained bool) {
	// Native keeps an unknown namespace verbatim so a round-trip is
	// lossless; foreign imports drop it.
	tagIDs, _ := resolveTokenTagIDsConf(database, tagSvc, tokens, nil, generalID, source != ImportSourceNative, source)
	added, err := tagSvc.AddTagsToImageFromTaggerConf(imageID, tagIDs, nil, false, source)
	if err != nil {
		logx.Warnf("import tags to image %d: %v", imageID, err)
	}
	return added > 0
}

// confs, when not nil, pairs with tokens; the returned slices stay
// aligned as tokens drop out.
func resolveTokenTagIDsConf(database *db.DB, tagSvc *tags.Service, tokens []string, confs []*float64, generalID int64, dropUnknownNamespace bool, origin string) ([]int64, []*float64) {
	tagIDs := make([]int64, 0, len(tokens))
	outConfs := make([]*float64, 0, len(tokens))
	for i, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		catID, bareName, err := resolveImportTag(database, token, generalID, dropUnknownNamespace)
		var t *models.Tag
		if err == nil {
			t, err = tagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		}
		if err != nil {
			logx.Warnf("import tag %q: %v", token, err)
			continue
		}
		tagIDs = append(tagIDs, t.ID)
		if confs != nil {
			outConfs = append(outConfs, confs[i])
		}
	}
	return tagIDs, outConfs
}

// Returns the error: a move deletes the source only once the tags have landed.
func applyTransferTags(database *db.DB, tagSvc *tags.Service, imageID int64, groups []transferTagGroup, generalID int64) (gained bool, err error) {
	for _, g := range groups {
		// An unlabelled group was a UI add on the source side, hence "user".
		origin := cmp.Or(g.taggerName, "user")
		tagIDs, confs := resolveTokenTagIDsConf(database, tagSvc, g.tokens, g.confs, generalID, false, origin)
		added, err := tagSvc.AddTagsToImageFromTaggerConf(imageID, tagIDs, confs, g.isAuto, g.taggerName)
		if err != nil {
			return gained, fmt.Errorf("add tags to image %d: %w", imageID, err)
		}
		gained = gained || added > 0
	}
	return gained, nil
}

func resolveImportTag(database *db.DB, token string, generalID int64, dropUnknownNamespace bool) (int64, string, error) {
	f, err := tags.SplitForm(database, token)
	switch {
	case err != nil:
		return 0, "", err
	case f.CategoryID != 0:
		return f.CategoryID, f.Name, nil
	case dropUnknownNamespace && f.Namespace != "":
		return generalID, f.Name[len(f.Namespace)+1:], nil
	}
	return generalID, f.Name, nil
}

func LookupCategoryID(database *db.DB, name string) int64 {
	id, ok, err := tags.CategoryIDByName(database, name)
	if !ok || err != nil {
		return 0
	}
	return id
}

// The cap counts decompressed bytes, so a zip bomb can't fill the disk;
// maxBytes <= 0 means no cap.
func copyZipEntry(dst io.Writer, f *zip.File, maxBytes int64) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("archive entry %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	var src io.Reader = rc
	if maxBytes > 0 {
		src = io.LimitReader(rc, maxBytes+1)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("archive entry %q: %w", f.Name, err)
	}
	if maxBytes > 0 && n > maxBytes {
		return entryTooLarge(f.Name, maxBytes)
	}
	return nil
}

// A declared size can lie, so copyZipEntry still counts the bytes.
func checkEntrySize(f *zip.File, maxBytes int64) error {
	if maxBytes > 0 && f.UncompressedSize64 > uint64(maxBytes) {
		return entryTooLarge(f.Name, maxBytes)
	}
	return nil
}

func entryTooLarge(name string, maxBytes int64) error {
	return fmt.Errorf("archive entry %q exceeds per-file limit of %d bytes", name, maxBytes)
}

func copyZipFile(f *zip.File, dst string, maxBytes int64) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = copyZipEntry(out, f, maxBytes)
	_ = out.Close()
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

func extractLightManifest(f *zip.File, dataDir string) (LightManifest, error) {
	path, err := extractZipEntryToTemp(f, dataDir)
	if err != nil {
		return LightManifest{}, err
	}
	defer func() { _ = os.Remove(path) }()
	return readLightManifest(path)
}

func extractZipEntryToTemp(f *zip.File, dataDir string) (string, error) {
	tmp, err := os.CreateTemp(dataDir, "merge-inner-*")
	if err != nil {
		return "", err
	}
	copyErr := copyZipEntry(tmp, f, MaxImportBytes)
	_ = tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmp.Name())
		return "", copyErr
	}
	return tmp.Name(), nil
}
