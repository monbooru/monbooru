package gallery

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/lookup"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type SyncResult struct {
	Added      int
	Removed    int
	Moved      int
	Duplicates int
	Conflicts  int
	Conflicted string
	// Excluded is the part of Removed the boundary left out rather than
	// found gone.
	Excluded    int
	Reactivated int
	Edited      int
}

func (r SyncResult) Summary() string {
	missing := fmt.Sprintf("%d missing", r.Removed)
	if r.Excluded > 0 {
		missing += fmt.Sprintf(" (%d excluded)", r.Excluded)
	}
	out := fmt.Sprintf("%d added, %s, %d moved", r.Added, missing, r.Moved)
	if r.Reactivated > 0 {
		out += fmt.Sprintf(", %d restored", r.Reactivated)
	}
	if r.Edited > 0 {
		out += fmt.Sprintf(", %d re-read", r.Edited)
	}
	switch {
	case r.Conflicts == 1:
		out += fmt.Sprintf(", 1 conflicted (%s)", r.Conflicted)
	case r.Conflicts > 1:
		out += fmt.Sprintf(", %d conflicted (%s and %d more)", r.Conflicts, r.Conflicted, r.Conflicts-1)
	}
	return out
}

type SourceLabelCount struct {
	Source string
	Count  int
}

type syncFileInfo struct {
	path      string
	sha256    string
	md5       string // "" when the shortcut skipped hashing; the row keeps its stored md5
	size      int64
	mtime     int64
	mtimeNano int64
}

type syncKnownEntry struct {
	size      int64
	sha256    string
	mtime     int64
	mtimeNano int64
	canonical bool
}

func (k syncKnownEntry) unchanged(size, mtime, mtimeNano int64) bool {
	if k.size != size {
		return false
	}
	if k.mtimeNano != 0 {
		return k.mtimeNano == mtimeNano
	}
	return k.mtime != 0 && k.mtime == mtime
}

type syncBySHARow struct {
	id            int64
	canonicalPath string
	isMissing     int
	fileSize      int64
}

// Sync flags missing the rows whose file is gone or outside bound;
// maxFileSizeMB <= 0 means no size cap.
func Sync(ctx context.Context, h Handle, maxFileSizeMB int, naming Naming, progress func(processed, total int, message string), onChange func(), onPhash PhashSink) (SyncResult, error) {
	var result SyncResult
	database, bound := h.DB, h.Boundary()
	galleryPath := bound.Root()

	// The walk skips unreadable directories: an unmounted root would read
	// as an empty tree and phase 3 would flag the whole library missing.
	if _, err := os.ReadDir(galleryPath); err != nil {
		return result, fmt.Errorf("gallery path is unreadable: %w", err)
	}

	progress(0, 0, "Phase 1: scanning filesystem...")
	known, err := loadKnownPaths(database)
	if err != nil {
		return result, err
	}
	// The same one level down, for a linked folder whose target went away.
	if link := danglingLinkOver(bound, known); link != "" {
		return result, fmt.Errorf("linked folder %q does not resolve", link)
	}
	found, skips, err := walkGalleryFiles(ctx, bound, int64(maxFileSizeMB)*1024*1024, known)
	if err != nil {
		return result, err
	}

	total := len(found)
	progress(0, total, "Phase 2: reconciling...")

	foundPaths := make(map[string]struct{}, total)
	for _, fi := range found {
		foundPaths[fi.path] = struct{}{}
	}

	bySHA, err := loadImagesBySHA(database)
	if err != nil {
		return result, err
	}

	// A first sync leaves files where they are: naming them would
	// reorganise a tree the operator arranged before monbooru saw it.
	adopting := len(bySHA) == 0

	var ingested []int64
	changed := 0
	for i, fi := range found {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if i%50 == 0 || i == total-1 {
			// Before the tick, so the refresh it prompts reads what changed.
			if n := result.Added + result.Moved + result.Reactivated + result.Edited; onChange != nil && n != changed {
				changed = n
				onChange()
			}
			progress(i, total, "Phase 2: reconciling...")
		}
		if id := reconcileFile(h, bound, fi, known, bySHA, &result, onPhash); id != 0 {
			ingested = append(ingested, id)
		}
	}

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	if adopting && !naming.Empty() && len(ingested) > 0 {
		logx.Infof("sync: first sync of this library, %d %s left where they are", len(ingested), models.Plural(len(ingested), "file", "files"))
	} else {
		nameIngested(ctx, database, bound, naming, ingested, foundPaths)
	}

	toMark, excluded, err := selectImagesToMarkMissing(database, bound, foundPaths, skips)
	if err != nil {
		return result, err
	}
	removed, err := markImagesMissingChunked(ctx, database, toMark)
	result.Removed = removed
	result.Excluded = min(excluded, removed)
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	// Not on an empty walk: an unmounted gallery would lose every alias row.
	if len(found) > 0 {
		if err := pruneStaleAliasPaths(ctx, database, bound, foundPaths); err != nil {
			return result, err
		}
	}

	progress(0, 0, fmt.Sprintf("Done: %d added, %d missing, %d moved, %d duplicates",
		result.Added, result.Removed, result.Moved, result.Duplicates))

	return result, nil
}

// nameIngested runs before the missing sweep and adds each new path to
// foundPaths, or the sweep would flag every renamed file gone.
func nameIngested(ctx context.Context, database *db.DB, bound *Boundary, naming Naming, ids []int64, foundPaths map[string]struct{}) {
	if naming.Empty() {
		return
	}
	for _, id := range ids {
		newPath, err := naming.ApplyExisting(ctx, database, bound, id)
		if err != nil {
			logx.Warnf("sync: name image %d: %v", id, err)
			continue
		}
		if newPath != "" {
			foundPaths[newPath] = struct{}{}
		}
	}
}

// A deleted folder is not one of these: its rows are meant to go missing.
func danglingLinkOver(bound *Boundary, known map[string]syncKnownEntry) string {
	galleryPath := bound.Root()
	checked := map[string]struct{}{}
	for path := range known {
		if bound.Excludes(path) {
			continue
		}
		for dir := filepath.Dir(path); dir != galleryPath && PathInside(galleryPath, dir); dir = filepath.Dir(dir) {
			if _, seen := checked[dir]; seen {
				break
			}
			checked[dir] = struct{}{}
			info, err := os.Lstat(dir)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if _, err := os.Stat(dir); err != nil {
				return dir
			}
		}
	}
	return ""
}

func loadKnownPaths(database *db.DB) (map[string]syncKnownEntry, error) {
	known := map[string]syncKnownEntry{}
	rows, err := database.Read.Query(
		`SELECT ip.path, i.file_size, i.sha256, ip.mtime_unix, ip.mtime_nsec, ip.is_canonical
		   FROM image_paths ip JOIN images i ON i.id = ip.image_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading known paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p, sha string
		var sz, mt, mtNano int64
		var canonical bool
		if err := rows.Scan(&p, &sz, &sha, &mt, &mtNano, &canonical); err != nil {
			return nil, fmt.Errorf("scanning known paths: %w", err)
		}
		known[p] = syncKnownEntry{size: sz, sha256: sha, mtime: mt, mtimeNano: mtNano, canonical: canonical}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating known paths: %w", err)
	}
	return known, nil
}

// walkSkips is what a walk passed over without reading: those rows keep
// their state rather than read as gone.
type walkSkips struct {
	files map[string]struct{}
	dirs  []string
}

func (s walkSkips) covers(path string) bool {
	if _, ok := s.files[path]; ok {
		return true
	}
	return slices.ContainsFunc(s.dirs, func(dir string) bool { return PathInside(dir, path) })
}

func walkGalleryFiles(ctx context.Context, bound *Boundary, maxBytes int64, known map[string]syncKnownEntry) ([]syncFileInfo, walkSkips, error) {
	var found []syncFileInfo
	skips := walkSkips{files: map[string]struct{}{}}
	err := WalkTree(bound, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				skips.dirs = append(skips.dirs, path)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if _, typeErr := DetectFileType(path); typeErr != nil {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || (maxBytes > 0 && info.Size() > maxBytes) {
			skips.files[path] = struct{}{}
			return nil
		}
		mtimeUnix, mtimeNano := info.ModTime().Unix(), info.ModTime().UnixNano()
		var hash, sum string
		if k, ok := known[path]; ok && k.unchanged(info.Size(), mtimeUnix, mtimeNano) {
			hash = k.sha256
		} else {
			h, m, hashErr := hashFileDigests(path)
			if hashErr != nil {
				logx.Warnf("hash failed for %q: %v", path, hashErr)
				skips.files[path] = struct{}{}
				return nil
			}
			hash, sum = h, m
			claimOwnership(bound.Root(), path)
		}
		found = append(found, syncFileInfo{path: path, sha256: hash, md5: sum, size: info.Size(), mtime: mtimeUnix, mtimeNano: mtimeNano})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, skips, ctx.Err()
		}
		return nil, skips, fmt.Errorf("walking gallery: %w", err)
	}
	return found, skips, nil
}

// One full scan beats a SELECT per walked file.
func loadImagesBySHA(database *db.DB) (map[string]syncBySHARow, error) {
	bySHA := map[string]syncBySHARow{}
	rows, err := database.Read.Query(
		`SELECT id, sha256, canonical_path, is_missing, file_size FROM images`,
	)
	if err != nil {
		return nil, fmt.Errorf("preloading SHA index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r syncBySHARow
		var sha string
		if err := rows.Scan(&r.id, &sha, &r.canonicalPath, &r.isMissing, &r.fileSize); err != nil {
			return nil, fmt.Errorf("scanning SHA index: %w", err)
		}
		bySHA[sha] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating SHA index: %w", err)
	}
	return bySHA, nil
}

// Every branch updates bySHA, or a later file with the same SHA acts on
// the stale entry: a moved image would be moved again.
func reconcileFile(h Handle, bound *Boundary, fi syncFileInfo, known map[string]syncKnownEntry, bySHA map[string]syncBySHARow, result *SyncResult, onPhash PhashSink) int64 {
	database := h.DB
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 && !k.canonical {
		// A rewritten copy: its image keeps its own file, and the path is
		// classified afresh, as the watcher's ingest does.
		if _, err := database.Write.Exec(`DELETE FROM image_paths WHERE path = ? AND is_canonical = 0`, fi.path); err != nil {
			logx.Warnf("sync: drop the rewritten copy %q: %v", fi.path, err)
			return 0
		}
		delete(known, fi.path)
	}
	if k, knownPath := known[fi.path]; knownPath && k.sha256 != fi.sha256 {
		// images.sha256 is UNIQUE, so the UPDATE would fail on every
		// sync; which row the operator wants is not ours to guess.
		if other, held := bySHA[fi.sha256]; held {
			logx.Warnf("sync: %q was rewritten with the bytes of image %d at %q; row left as it is",
				fi.path, other.id, other.canonicalPath)
			if result.Conflicts == 0 {
				result.Conflicted = path.Join(FolderPath(h.GalleryPath, fi.path), filepath.Base(fi.path))
			}
			result.Conflicts++
			return 0
		}
		phash, err := applyInPlaceEdit(h, fi.path, fi.sha256, fi.md5, fi.mtime, fi.mtimeNano, fi.size)
		if err != nil {
			logx.Warnf("sync: in-place edit %q: %v", fi.path, err)
			return 0
		}
		result.Edited++
		if imgID, err := ImageIDBySHA(database.Read, fi.sha256); err == nil {
			bySHA[fi.sha256] = syncBySHARow{id: imgID, canonicalPath: fi.path, isMissing: 0, fileSize: fi.size}
			onPhash.Stored(imgID, phash)
		}
		delete(bySHA, k.sha256)
		known[fi.path] = syncKnownEntry{size: fi.size, sha256: fi.sha256, mtime: fi.mtime}
		return 0
	}

	row, ok := bySHA[fi.sha256]
	if !ok {
		return reconcileNewFile(h, fi, bySHA, result, onPhash)
	}
	reconcileExistingSHA(database, bound, fi, row, bySHA, known, result)
	// file_size 0 marks a row whose bytes were never read: an import placeholder.
	if row.fileSize == 0 {
		readPlaceholder(h, fi, bySHA, onPhash)
	}
	return 0
}

func readPlaceholder(h Handle, fi syncFileInfo, bySHA map[string]syncBySHARow, onPhash PhashSink) {
	phash, err := applyInPlaceEdit(h, fi.path, fi.sha256, fi.md5, fi.mtime, fi.mtimeNano, fi.size)
	if err != nil {
		logx.Warnf("sync: read restored %q: %v", fi.path, err)
		return
	}
	row := bySHA[fi.sha256]
	row.fileSize = fi.size
	bySHA[fi.sha256] = row
	onPhash.Stored(row.id, phash)
}

// ReadPlaceholder reads an import placeholder's file, now on disk at its
// path, into the row.
func ReadPlaceholder(h Handle, path string) (*int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sha, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	sum, err := Md5File(path)
	if err != nil {
		return nil, err
	}
	return applyInPlaceEdit(h, path, sha, sum, fi.ModTime().Unix(), fi.ModTime().UnixNano(), fi.Size())
}

func reconcileNewFile(h Handle, fi syncFileInfo, bySHA map[string]syncBySHARow, result *SyncResult, onPhash PhashSink) int64 {
	// The size and mtime shortcut carries no md5: the row it matched keeps
	// its own, but a split-off alias becomes a new row needing one.
	if fi.md5 == "" {
		sum, err := Md5File(fi.path)
		if err != nil {
			logx.Warnf("sync: md5 %q: %v", fi.path, err)
			return 0
		}
		fi.md5 = sum
	}
	img, _, ingestErr := ingestWithHash(h, fi.path, fi.sha256, fi.md5, "")
	if ingestErr != nil {
		logx.Warnf("ingest failed for %q: %v", fi.path, ingestErr)
		return 0
	}
	result.Added++
	if img == nil {
		return 0
	}
	onPhash.Landed(img)
	bySHA[fi.sha256] = syncBySHARow{id: img.ID, canonicalPath: fi.path, isMissing: 0, fileSize: img.FileSize}
	return img.ID
}

func reconcileExistingSHA(database *db.DB, bound *Boundary, fi syncFileInfo, row syncBySHARow, bySHA map[string]syncBySHARow, known map[string]syncKnownEntry, result *SyncResult) {
	galleryPath := bound.Root()
	if k, ok := known[fi.path]; ok && (k.mtime != fi.mtime || k.mtimeNano != fi.mtimeNano) {
		if _, wErr := database.Write.Exec(
			`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, fi.mtime, fi.mtimeNano, fi.path,
		); wErr != nil {
			logx.Warnf("sync: persist mtime for %q: %v", fi.path, wErr)
		}
	}

	if row.canonicalPath == fi.path {
		if row.isMissing == 1 {
			reactivate(database, fi.sha256, row, bySHA, result)
		}
		return
	}

	respelled := respelling(galleryPath, row.canonicalPath, fi.path)
	if k, knownAlias := known[fi.path]; knownAlias && k.sha256 == fi.sha256 {
		if respelled || CanonicalGone(bound, row.canonicalPath) {
			repointRow(database, galleryPath, fi, row, bySHA, result)
		} else if row.isMissing == 1 {
			reactivate(database, fi.sha256, row, bySHA, result)
		}
		return
	}

	if respelled || CanonicalGone(bound, row.canonicalPath) {
		repointRow(database, galleryPath, fi, row, bySHA, result)
		return
	}
	if _, wErr := database.Write.Exec(
		`INSERT OR IGNORE INTO image_paths (image_id, path, is_canonical, mtime_unix, mtime_nsec) VALUES (?, ?, 0, ?, ?)`,
		row.id, fi.path, fi.mtime, fi.mtimeNano,
	); wErr != nil {
		logx.Warnf("sync: insert alias path %d: %v", row.id, wErr)
	}
	result.Duplicates++
}

// The walk names nothing outside the root's spelling, so a canonical there
// that is the walked file is that file under an older spelling of the root.
func respelling(root, canonical, walked string) bool {
	if PathInside(root, canonical) {
		return false
	}
	info, err := os.Stat(walked)
	return err == nil && SameFile(info, canonical)
}

// CanonicalGone also holds for a path the boundary refuses.
func CanonicalGone(bound *Boundary, path string) bool {
	if bound.Check(path) != nil {
		return true
	}
	_, err := os.Stat(path)
	return err != nil
}

func reactivate(database *db.DB, sha string, row syncBySHARow, bySHA map[string]syncBySHARow, result *SyncResult) {
	if _, err := SetMissing(database, row.id, false); err != nil {
		logx.Warnf("sync: reactivate %d: %v", row.id, err)
		return
	}
	row.isMissing = 0
	bySHA[sha] = row
	result.Reactivated++
}

func repointRow(database *db.DB, galleryPath string, fi syncFileInfo, row syncBySHARow, bySHA map[string]syncBySHARow, result *SyncResult) {
	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return repointCanonical(tx, row.id, fi.path, FolderPath(galleryPath, fi.path), row.canonicalPath, fi.mtime, fi.mtimeNano)
	}); err != nil {
		logx.Warnf("sync: move %d onto %q: %v", row.id, fi.path, err)
		return
	}
	bySHA[fi.sha256] = syncBySHARow{id: row.id, canonicalPath: fi.path, isMissing: 0, fileSize: row.fileSize}
	if row.isMissing == 1 {
		result.Reactivated++
	} else {
		result.Moved++
	}
}

func selectImagesToMarkMissing(database *db.DB, bound *Boundary, foundPaths map[string]struct{}, skips walkSkips) ([]int64, int, error) {
	rows, err := database.Read.Query(
		`SELECT id, canonical_path FROM images WHERE is_missing = 0`,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("querying existing images: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var toMark []int64
	excluded := 0
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			logx.Warnf("sync: scan existing image row: %v", err)
			continue
		}
		if _, seen := foundPaths[path]; !seen && !skips.covers(path) {
			toMark = append(toMark, id)
			if bound.Excludes(path) {
				excluded++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return toMark, excluded, fmt.Errorf("iterating existing images: %w", err)
	}
	return toMark, excluded, nil
}

func markImagesMissingChunked(ctx context.Context, database *db.DB, ids []int64) (int, error) {
	marked := 0
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var n int
		if wErr := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			var err error
			n, err = setMissingTx(tx, chunk, true)
			return err
		}); wErr != nil {
			logx.Warnf("sync: mark missing chunk: %v", wErr)
			return fmt.Errorf("mark missing chunk: %w", wErr)
		}
		marked += n
		return nil
	})
	return marked, err
}

// A path the walk skipped (size cap, unreadable) keeps its row until its
// file is gone. An excluded path goes even when present: that copy is
// another gallery's.
func pruneStaleAliasPaths(ctx context.Context, database *db.DB, bound *Boundary, foundPaths map[string]struct{}) error {
	type aliasPath struct {
		id              int64
		path, canonical string
	}
	// Collected before any stat so the read cursor is not held open
	// across the syscalls.
	aliases, err := db.QueryAll(database.Read, db.ScanRow(func(a *aliasPath) []any {
		return []any{&a.id, &a.path, &a.canonical}
	}), `SELECT ip.id, ip.path, i.canonical_path FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.is_canonical = 0`)
	if err != nil {
		return fmt.Errorf("listing alias paths: %w", err)
	}
	var staleIDs []int64
	for _, a := range aliases {
		if _, ok := foundPaths[a.path]; ok {
			continue
		}
		if bound.Check(a.path) != nil {
			staleIDs = append(staleIDs, a.id)
			continue
		}
		// An unwalked path onto the canonical's own file is a second name
		// the watcher took through a link the walk refuses.
		info, statErr := os.Stat(a.path)
		if os.IsNotExist(statErr) || statErr == nil && SameFile(info, a.canonical) {
			staleIDs = append(staleIDs, a.id)
		}
	}

	return db.Chunked(staleIDs, 500, func(chunk []int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		placeholders, args := db.InPlaceholders(chunk)
		if _, wErr := database.Write.Exec(
			`DELETE FROM image_paths WHERE id IN (`+placeholders+`)`, args...,
		); wErr != nil {
			return fmt.Errorf("prune alias paths chunk: %w", wErr)
		}
		return nil
	})
}

func SameFile(info os.FileInfo, path string) bool {
	other, err := os.Stat(path)
	return err == nil && os.SameFile(info, other)
}

// applyInPlaceEdit keeps the row's id, so its tags survive the rewrite.
func applyInPlaceEdit(h Handle, path, newSHA, newMD5 string, newMtime, newMtimeNano, newSize int64) (*int64, error) {
	database, thumbnailsPath := h.DB, h.ThumbnailsPath
	var imageID int64
	var oldSHA string
	if err := database.Read.QueryRow(
		`SELECT ip.image_id, i.sha256 FROM image_paths ip JOIN images i ON i.id = ip.image_id WHERE ip.path = ?`, path,
	).Scan(&imageID, &oldSHA); err != nil {
		return nil, fmt.Errorf("locate image for path %q: %w", path, err)
	}
	// Unchanged bytes are an import placeholder's file, read for the first time.
	logCtx := "in-place edit"
	if oldSHA == newSHA {
		logCtx = "placeholder read"
	}

	// Sniffed, as a rewrite can change the type under the same name;
	// bytes that are no longer media leave the row as it was.
	fileType, err := MagicFileType(path)
	if err != nil {
		return nil, fmt.Errorf("contents of %q are not a supported media type: %w", path, err)
	}

	facts, err := deriveFileFacts(path, fileType, logCtx)
	if err != nil {
		logx.Warnf("%s: manga %q: %v", logCtx, path, err)
	}

	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			// duration_seconds is written for every type, or a former video
			// keeps matching the duration: filter. A placeholder read keeps
			// its auto-tag stamp: its bytes did not change.
			`UPDATE images SET sha256 = ?, md5 = ?, file_size = ?, file_type = ?, width = ?, height = ?, page_count = ?, duration_seconds = ?, source_type = ?, phash = NULL,
			        auto_tagged_at = CASE WHEN sha256 = ? THEN auto_tagged_at END WHERE id = ?`,
			newSHA, newMD5, newSize, fileType, facts.width, facts.height, facts.pageCount, facts.durationSec, facts.sourceType, newSHA, imageID,
		); err != nil {
			return fmt.Errorf("update images row: %w", err)
		}
		if _, err := setMissingTx(tx, []int64{imageID}, false); err != nil {
			return fmt.Errorf("mark row present: %w", err)
		}
		if _, err := tx.Exec(
			`UPDATE image_paths SET mtime_unix = ?, mtime_nsec = ? WHERE path = ?`, newMtime, newMtimeNano, path,
		); err != nil {
			return fmt.Errorf("update image_paths mtime: %w", err)
		}
		// The recorded lookup misses are about bytes this row no longer has.
		if err := lookup.DeleteForImage(tx, imageID); err != nil {
			return fmt.Errorf("clear lookup history: %w", err)
		}
		if err := replaceGenerationMetadata(context.Background(), tx, imageID, facts.sd, facts.comfy); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM manga_metadata WHERE image_id = ?`, imageID); err != nil {
			return fmt.Errorf("clear manga_metadata: %w", err)
		}
		if facts.manga != nil {
			facts.manga.ImageID = imageID
			if err := insertMangaMeta(tx, facts.manga); err != nil {
				return fmt.Errorf("insert manga_metadata: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// The reader serves cached pages without revalidating them, so the
	// old contents' cache must go.
	if fileType == models.FileTypeCBZ {
		removeMangaCache(thumbnailsPath, imageID)
	}

	phash := regenerateDerived(database, thumbnailsPath, path, imageID, fileType, logCtx)
	if err := ApplyMetaTags(h, imageID); err != nil {
		logx.Warnf("%s: meta tags for %q: %v", logCtx, path, err)
	}
	if oldSHA == newSHA {
		logx.Infof("placeholder read: image id=%d at %q read from its file (%d bytes)", imageID, path, newSize)
	} else {
		logx.Infof("in-place edit: image id=%d at %q now carries sha %s", imageID, path, newSHA)
	}
	return phash, nil
}
