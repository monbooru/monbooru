package gallery

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

const MaxAnnotationBodyLen = 4000

// ValidExternalURL accepts only http(s): html/template and the link
// allowlist render anything else inert.
func ValidExternalURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// col is spliced into the SQL and must be a constant. Callers skip empty
// values first: this writes whatever it is given.
func updateSourceField(database *db.DB, imageID int64, site, postID, col string, val any) error {
	_, err := database.Write.Exec(
		fmt.Sprintf(`UPDATE image_sources SET %s = ? WHERE image_id = ? AND site = ? AND post_id = ?`, col),
		val, imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// images.source and images.url mirror the primary origin, the
// lowest-rowid image_sources row. A write that can change that row, or
// its site or url, runs in sourceTx.

func SourcesForImage(database *db.DB, imageID int64) ([]models.ImageSource, error) {
	return db.QueryAll(database.Read, db.ScanRow(func(s *models.ImageSource) []any {
		return []any{&s.Site, &s.PostID, &s.URL, &s.Commentary, &s.CommentaryTranslated, &s.Original,
			&s.Similarity, &s.MD5, &s.MD5Match,
			&s.UpgradeKept, &s.PostWidth, &s.PostHeight, &s.PostSize, &s.PostExt}
	}),
		`SELECT site, post_id, url, commentary, commentary_translated, original, similarity, md5, md5_match,
		        upgrade_kept, post_width, post_height, post_size, post_ext
		 FROM image_sources WHERE image_id = ? ORDER BY rowid`, imageID)
}

// AddSourceMembership never clears a stored url with an empty one: a
// url-less re-push must not wipe known provenance.
func AddSourceMembership(database *db.DB, imageID int64, site, postID, url string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		if postID != "" && url != "" {
			// Adopt an older (site, "") row with this url, or a refetch
			// would leave a twin row for the post.
			if _, err := tx.Exec(
				`UPDATE OR IGNORE image_sources SET post_id = ?
				 WHERE image_id = ? AND site = ? AND post_id = '' AND url = ?`,
				postID, imageID, site, url); err != nil {
				return err
			}
		}
		_, err := tx.Exec(
			`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, site, post_id) DO UPDATE SET
			   url = CASE WHEN excluded.url != '' THEN excluded.url ELSE url END`,
			imageID, site, postID, url)
		return err
	})
}

func setSourceFieldIfSet(database *db.DB, imageID int64, site, postID, col, val string) error {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil
	}
	return updateSourceField(database, imageID, site, postID, col, val)
}

func ImageSHAAndType(q db.RowQuerier, id int64) (sha, fileType string, err error) {
	err = q.QueryRow(`SELECT sha256, file_type FROM images WHERE id = ?`, id).Scan(&sha, &fileType)
	return sha, fileType, err
}

// FileTypes leaves out the ids with no row.
func FileTypes(database *db.DB, ids []int64) (map[int64]string, error) {
	return fileTypes(database, ids, "")
}

func PresentFileTypes(database *db.DB, ids []int64) (map[int64]string, error) {
	return fileTypes(database, ids, "is_missing = 0 AND ")
}

func fileTypes(database *db.DB, ids []int64, filter string) (map[int64]string, error) {
	type typeRow struct {
		id       int64
		fileType string
	}
	types := make(map[int64]string, len(ids))
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		in, args := db.InPlaceholders(chunk)
		rows, err := db.QueryAll(database.Read, db.ScanRow(func(r *typeRow) []any { return []any{&r.id, &r.fileType} }),
			`SELECT id, file_type FROM images WHERE `+filter+`id IN (`+in+`)`, args...)
		if err != nil {
			return err
		}
		for _, r := range rows {
			types[r.id] = r.fileType
		}
		return nil
	})
	return types, err
}

// ImageIDBySHA answers sql.ErrNoRows when no row holds the bytes.
func ImageIDBySHA(q db.RowQuerier, sha string) (int64, error) {
	var id int64
	err := q.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id)
	return id, err
}

func ImageIDBySourceURL(database *db.DB, url string) (int64, bool) {
	var id int64
	err := database.Read.QueryRow(
		`SELECT image_id FROM image_sources WHERE url = ? ORDER BY image_id LIMIT 1`, url).Scan(&id)
	return id, err == nil
}

func ChildIDsByParentURL(database *db.DB, url string) ([]int64, error) {
	return db.QueryIDs(database.Read,
		`SELECT DISTINCT image_id FROM image_sources WHERE parent_url = ? ORDER BY image_id`, url)
}

// SetSourceSimilarity keeps the stored score on zero: the mark spares
// later refetches an md5 verify this file fails by design.
func SetSourceSimilarity(database *db.DB, imageID int64, site, postID string, score float64) error {
	if score <= 0 {
		return nil
	}
	return updateSourceField(database, imageID, site, postID, "similarity", score)
}

// SetSourceMD5Match takes "match", "differ" or "" for unknown. Until the
// merge adopts the post id the origin is keyed (site, ""), so a miss on
// the exact key falls back to that row.
func SetSourceMD5Match(database *db.DB, imageID int64, site, postID, verdict string) error {
	site = strings.TrimSpace(site)
	res, err := database.Write.Exec(
		`UPDATE image_sources SET md5_match = ? WHERE image_id = ? AND site = ? AND post_id = ?`,
		verdict, imageID, site, strings.TrimSpace(postID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 || strings.TrimSpace(postID) == "" {
		return nil
	}
	_, err = database.Write.Exec(
		`UPDATE image_sources SET md5_match = ? WHERE image_id = ? AND site = ? AND post_id = ''`,
		verdict, imageID, site)
	return err
}

// MarkSourceExact is for an origin that now serves the local bytes; it
// writes past the setters' keep-stored guards on purpose.
func MarkSourceExact(database *db.DB, imageID int64, site, postID, md5 string) error {
	_, err := database.Write.Exec(
		`UPDATE image_sources SET similarity = 0, md5 = ?, md5_match = 'match'
		 WHERE image_id = ? AND site = ? AND post_id = ?`,
		strings.TrimSpace(md5), imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// MarkSourceRescued keeps the post's md5 on a re-encoded file's origin; the
// similarity mark waives a verified refresh, and the keep ruling, set after
// the md5 whose trigger lapses it, stops an upgrade re-rescuing the same bytes.
func MarkSourceRescued(database *db.DB, imageID int64, site, postID, upstreamMD5 string) error {
	site, postID = strings.TrimSpace(site), strings.TrimSpace(postID)
	if _, err := database.Write.Exec(
		`UPDATE image_sources SET similarity = 100, md5 = ?
		 WHERE image_id = ? AND site = ? AND post_id = ?`,
		strings.TrimSpace(upstreamMD5), imageID, site, postID); err != nil {
		return err
	}
	_, err := database.Write.Exec(
		`UPDATE image_sources SET upgrade_kept = 1 WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, site, postID)
	return err
}

// PostFile is what a post claims its file is; a zero field was not published.
type PostFile struct {
	Width, Height int
	Size          int64
	Ext           string
}

// Per field: a site that publishes dimensions but no size must not wipe a
// size an earlier fetch found.
func setSourcePostFile(database *db.DB, imageID int64, site, postID string, f PostFile) error {
	if f.Width <= 0 && f.Height <= 0 && f.Size <= 0 && strings.TrimSpace(f.Ext) == "" {
		return nil
	}
	_, err := database.Write.Exec(
		`UPDATE image_sources SET
		   post_width  = CASE WHEN ? > 0 THEN ? ELSE post_width END,
		   post_height = CASE WHEN ? > 0 THEN ? ELSE post_height END,
		   post_size   = CASE WHEN ? > 0 THEN ? ELSE post_size END,
		   post_ext    = CASE WHEN ? != '' THEN ? ELSE post_ext END
		 WHERE image_id = ? AND site = ? AND post_id = ?`,
		f.Width, f.Width, f.Height, f.Height, f.Size, f.Size,
		strings.ToLower(strings.TrimSpace(f.Ext)), strings.ToLower(strings.TrimSpace(f.Ext)),
		imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return err
}

// SetSourceUpgradeKept's flag is cleared by a trigger when the post
// claims a new md5.
func SetSourceUpgradeKept(database *db.DB, imageID int64, site, postID string, kept bool) error {
	res, err := database.Write.Exec(
		`UPDATE image_sources SET upgrade_kept = ? WHERE image_id = ? AND site = ? AND post_id = ?`,
		kept, imageID, strings.TrimSpace(site), strings.TrimSpace(postID))
	return requireAffected(res, err, ErrSourceNotFound)
}

func SourceSimilarityMatched(database *db.DB, imageID int64, site, postID string) bool {
	var matched bool
	err := database.Read.QueryRow(
		`SELECT similarity > 0 FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
		imageID, strings.TrimSpace(site), strings.TrimSpace(postID)).Scan(&matched)
	return err == nil && matched
}

// RenameSourceMembership keeps the row's age, so a primary stays primary.
// Onto an identity the image already has, the rows merge and the target
// keeps its own text fields unless empty.
func RenameSourceMembership(database *db.DB, imageID int64, prevSite, prevPost, site, postID, url string) error {
	prevSite = strings.TrimSpace(prevSite)
	prevPost = strings.TrimSpace(prevPost)
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	url = strings.TrimSpace(url)
	if site == "" && url == "" {
		return errors.New("source label or url required")
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		var prevRid int64
		switch err := tx.QueryRow(
			`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, prevSite, prevPost).Scan(&prevRid); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)
				 ON CONFLICT(image_id, site, post_id) DO UPDATE SET url = excluded.url`,
				imageID, site, postID, url); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			var targetRid int64
			switch err := tx.QueryRow(
				`SELECT rowid FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
				imageID, site, postID).Scan(&targetRid); {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := tx.Exec(
					`UPDATE image_sources SET site = ?, post_id = ?, url = ? WHERE rowid = ?`,
					site, postID, url, prevRid); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				if _, err := tx.Exec(
					`UPDATE image_sources SET url = ?,
					        commentary = CASE WHEN commentary = '' THEN (SELECT commentary FROM image_sources WHERE rowid = ?) ELSE commentary END,
					        commentary_translated = CASE WHEN commentary_translated = '' THEN (SELECT commentary_translated FROM image_sources WHERE rowid = ?) ELSE commentary_translated END,
					        original = CASE WHEN original = '' THEN (SELECT original FROM image_sources WHERE rowid = ?) ELSE original END
					 WHERE rowid = ?`,
					url, prevRid, prevRid, prevRid, targetRid); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, prevRid); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(
				`UPDATE image_annotations SET site = ?, post_id = ? WHERE image_id = ? AND site = ? AND post_id = ? AND manual = 0`,
				site, postID, imageID, prevSite, prevPost); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveSourceMembership drops the origin's pulled annotations too:
// nothing else removes them.
func RemoveSourceMembership(database *db.DB, imageID int64, site, postID string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID)
		if err := requireAffected(res, err, ErrSourceNotFound); err != nil {
			return err
		}
		return deletePulledAnnotationsTx(tx, imageID, site, postID)
	})
}

// col is spliced into the SQL and must be a constant. A value creates the
// origin if absent; an empty one never does.
func setSourceTextField(database *db.DB, imageID int64, site, postID, col, value string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	value = strings.TrimSpace(value)
	if site == "" {
		return ErrSourceLabelRequired
	}
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		if value == "" {
			_, err := tx.Exec(
				fmt.Sprintf(`UPDATE image_sources SET %s = '' WHERE image_id = ? AND site = ? AND post_id = ?`, col),
				imageID, site, postID)
			return err
		}
		_, err := tx.Exec(
			fmt.Sprintf(`INSERT INTO image_sources (image_id, site, post_id, %[1]s) VALUES (?, ?, ?, ?)
			 ON CONFLICT(image_id, site, post_id) DO UPDATE SET %[1]s = excluded.%[1]s`, col),
			imageID, site, postID, value)
		return err
	})
}

func SetSourceCommentary(database *db.DB, imageID int64, site, postID, commentary string) error {
	return setSourceTextField(database, imageID, site, postID, "commentary", commentary)
}

func SetSourceCommentaryTranslated(database *db.DB, imageID int64, site, postID, translated string) error {
	return setSourceTextField(database, imageID, site, postID, "commentary_translated", translated)
}

func SetSourceOriginal(database *db.DB, imageID int64, site, postID, original string) error {
	return setSourceTextField(database, imageID, site, postID, "original", original)
}

var (
	ErrSourceIdentityExists = errors.New("another source with that label already exists on this image")
	ErrSourceLabelRequired  = errors.New("source label required")
	ErrSourceNotFound       = errors.New("source not found on this image")
)

// SetPrimarySource removes the primary origin when both are empty. Editing
// by rowid keeps its age, so it stays primary through a relabel.
func SetPrimarySource(database *db.DB, imageID int64, site, url string) error {
	site = strings.TrimSpace(site)
	url = strings.TrimSpace(url)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		var rid int64
		var curSite, curPost string
		err := tx.QueryRow(`SELECT rowid, site, post_id FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&rid, &curSite, &curPost)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if site != "" || url != "" {
				if _, err := tx.Exec(
					`INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, '', ?)`,
					imageID, site, url); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case site == "" && url == "":
			if _, err := tx.Exec(`DELETE FROM image_sources WHERE rowid = ?`, rid); err != nil {
				return err
			}
			if err := deletePulledAnnotationsTx(tx, imageID, curSite, curPost); err != nil {
				return err
			}
		default:
			var clash bool
			if err := tx.QueryRow(
				`SELECT EXISTS(SELECT 1 FROM image_sources WHERE image_id = ? AND site = ? AND post_id = ? AND rowid != ?)`,
				imageID, site, curPost, rid).Scan(&clash); err != nil {
				return err
			}
			if clash {
				return ErrSourceIdentityExists
			}
			if _, err := tx.Exec(`UPDATE image_sources SET site = ?, url = ? WHERE rowid = ?`,
				site, url, rid); err != nil {
				return err
			}
		}
		return nil
	})
}

// MakeSourcePrimary takes the table-wide MIN, not the image's, so the new
// rowid stays unique.
func MakeSourcePrimary(database *db.DB, imageID int64, site, postID string) error {
	site = strings.TrimSpace(site)
	postID = strings.TrimSpace(postID)
	return sourceTx(database, imageID, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE image_sources SET rowid = (SELECT MIN(rowid) - 1 FROM image_sources)
			 WHERE image_id = ? AND site = ? AND post_id = ?`,
			imageID, site, postID)
		return requireAffected(res, err, ErrSourceNotFound)
	})
}

func sourceTx(database *db.DB, imageID int64, work func(*sql.Tx) error) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if err := work(tx); err != nil {
			return err
		}
		return RebindPrimarySourceTx(tx, imageID)
	})
}

// RebindPrimarySourceTx is for a write outside sourceTx that adds or drops
// an image's origins.
func RebindPrimarySourceTx(tx *sql.Tx, imageID int64) error {
	var site, url string
	err := tx.QueryRow(`SELECT site, url FROM image_sources WHERE image_id = ? ORDER BY rowid LIMIT 1`, imageID).Scan(&site, &url)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET source = '', url = '' WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	_, e := tx.Exec(`UPDATE images SET source = ?, url = ? WHERE id = ?`, site, url, imageID)
	return e
}
