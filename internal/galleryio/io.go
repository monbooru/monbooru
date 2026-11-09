// Package galleryio exports, imports, merges and transfers whole
// galleries. A restore writes tables directly: the services would fire
// implication fan-outs and recounts the document already carries.
package galleryio

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/galleryio/compatibility"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// SafeArchiveDest rejects an absolute entry or one that escapes root. It uses
// PathInside, not a ".." substring test, so "foo..bar.ext" stays legal.
func SafeArchiveDest(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute archive entry path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dst, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if !gallery.PathInside(rootAbs, dst) {
		return "", fmt.Errorf("archive entry escapes gallery root")
	}
	return dst, nil
}

// Bump ExportVersion when the document gains a field; loadExportStream
// must still read every version down to ExportMinSupported.
const ExportVersion = 15

const ExportMinSupported = 1

// LightManifestVersion does not follow ExportVersion.
const LightManifestVersion = 1

// MaxImportBytes caps an upload and each document an archive carries;
// gallery.max_file_size_mb caps only the gallery files.
const MaxImportBytes = compatibility.MaxDocumentBytes

func checkExportVersion(v int) error {
	if v < ExportMinSupported || v > ExportVersion {
		return fmt.Errorf("unsupported export version %d (supported: %d..%d)", v, ExportMinSupported, ExportVersion)
	}
	return nil
}

var errStopSections = errors.New("stop reading sections")

// each must consume the value dec is positioned at: skipValue for a key
// it ignores.
func decodeSections(r io.Reader, each func(key string, dec *json.Decoder) error) error {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decode json: %w", err)
		}
		key, _ := tok.(string)
		if err := each(key, dec); err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	if tok != want {
		return fmt.Errorf("decode json: got %v, want %v", tok, want)
	}
	return nil
}

func decodeValue(dec *json.Decoder, v any) error {
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

func skipValue(dec *json.Decoder) error {
	return decodeValue(dec, &json.RawMessage{})
}

// streamArray decodes one element at a time; null reads as empty.
func streamArray[T any](dec *json.Decoder, row func(T) error) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	if tok == nil {
		return nil
	}
	if tok != json.Delim('[') {
		return fmt.Errorf("decode json: got %v, want an array", tok)
	}
	for dec.More() {
		var r T
		if err := decodeValue(dec, &r); err != nil {
			return err
		}
		if err := row(r); err != nil {
			return err
		}
	}
	return expectDelim(dec, ']')
}

type Export struct {
	Version          int                  `json:"version"`
	GalleryName      string               `json:"gallery_name"`
	GalleryPath      string               `json:"gallery_path"`
	TagCategories    []tagCategoryRow     `json:"tag_categories"`
	Tags             []tagRow             `json:"tags"`
	TagImplications  []tagImplicationRow  `json:"tag_implications"`
	TagNotes         []tagNoteRow         `json:"tag_notes,omitempty"`
	TagDistinctPairs []tagDistinctPairRow `json:"tag_distinct_pairs,omitempty"`
	Images           []imageRow           `json:"images"`
	ImageCollections []imageCollectionRow `json:"image_collections,omitempty"`
	FindRelations    []findRelationsRow   `json:"collection_find_relations,omitempty"`
	ImageSources     []imageSourceRow     `json:"image_sources,omitempty"`
	ImageAnnotations []imageAnnotationRow `json:"image_annotations,omitempty"`
	ImagePaths       []imagePathRow       `json:"image_paths"`
	ImageTags        []imageTagRow        `json:"image_tags"`
	ImageTagSources  []imageTagSourceRow  `json:"image_tag_sources,omitempty"`
	SDMetadata       []sdMetadataRow      `json:"sd_metadata"`
	ComfyUIMetadata  []comfyMetadataRow   `json:"comfyui_metadata"`
	MangaMetadata    []mangaMetadataRow   `json:"manga_metadata,omitempty"`
	DupGroups        []dupGroupRow        `json:"dup_groups,omitempty"`
	DupGroupMembers  []dupGroupMemberRow  `json:"dup_group_members,omitempty"`
	AltGroups        []altGroupRow        `json:"alt_groups,omitempty"`
	AltGroupMembers  []altGroupMemberRow  `json:"alt_group_members,omitempty"`
	VersionEdges     []versionEdgeRow     `json:"version_edges,omitempty"`
	DerivativeEdges  []derivativeEdgeRow  `json:"derivative_edges,omitempty"`
	NotRelatedPairs  []notRelatedPairRow  `json:"not_related_pairs,omitempty"`
	SavedSearches    []savedSearchRow     `json:"saved_searches"`
	ImageLookups     []imageLookupRow     `json:"image_lookups,omitempty"`
}

type tagCategoryRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsBuiltin int    `json:"is_builtin"`
	Position  int    `json:"position"`
}

type tagRow struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	CategoryID     int64          `json:"category_id"`
	UsageCount     int            `json:"usage_count"`
	IsAlias        int            `json:"is_alias"`
	CanonicalTagID sql.NullInt64  `json:"canonical_tag_id"`
	CreatedAt      string         `json:"created_at"`
	Origin         string         `json:"origin"`
	LastUsedAt     sql.NullString `json:"last_used_at"`
	Stale          int            `json:"stale,omitempty"`
	Rejected       int            `json:"rejected,omitempty"`
	RejectedDrops  int            `json:"rejected_drops,omitempty"`
}

type imageRow struct {
	ID                 int64           `json:"id"`
	SHA256             string          `json:"sha256"`
	MD5                string          `json:"md5,omitempty"`
	CanonicalPath      string          `json:"canonical_path"`
	FolderPath         string          `json:"folder_path"`
	FileType           string          `json:"file_type"`
	Width              sql.NullInt64   `json:"width"`
	Height             sql.NullInt64   `json:"height"`
	FileSize           int64           `json:"file_size"`
	IsMissing          int             `json:"is_missing"`
	IsFavorited        int             `json:"is_favorited"`
	IsInbox            int             `json:"is_inbox"`
	AutoTaggedAt       sql.NullString  `json:"auto_tagged_at"`
	SourceType         string          `json:"source_type"`
	Origin             string          `json:"origin"`
	Source             string          `json:"source"`
	URL                string          `json:"url"`
	PageCount          sql.NullInt64   `json:"page_count,omitempty"`
	DurationSeconds    sql.NullFloat64 `json:"duration_seconds,omitempty"`
	Series             string          `json:"collection,omitempty"`
	SeriesOrder        sql.NullInt64   `json:"collection_order,omitempty"`
	Note               string          `json:"note,omitempty"`
	OriginalSource     string          `json:"original_source,omitempty"`
	Phash              sql.NullInt64   `json:"phash,omitempty"`
	LastReadPage       sql.NullInt64   `json:"last_read_page,omitempty"`
	UploadBatch        sql.NullInt64   `json:"upload_batch,omitempty"`
	IngestedAt         string          `json:"ingested_at"`
	ScheduledLookup    int             `json:"scheduled_lookup"`
	ScheduledLookupPTR int             `json:"scheduled_lookup_ptr"`
}

type findRelationsRow struct {
	Name string `json:"name"`
}

type imageLookupRow struct {
	ImageID    int64          `json:"image_id"`
	Backend    string         `json:"backend"`
	Attempts   int            `json:"attempts"`
	QueuedAt   sql.NullString `json:"queued_at"`
	JobID      sql.NullInt64  `json:"job_id"`
	LastAt     sql.NullString `json:"last_at"`
	LastResult string         `json:"last_result"`
	NextDueAt  sql.NullString `json:"next_due_at"`
	PTRCursor  sql.NullInt64  `json:"ptr_cursor"`
}

type imageCollectionRow struct {
	ImageID  int64         `json:"image_id"`
	Name     string        `json:"name"`
	Position sql.NullInt64 `json:"position"`
}

type imageSourceRow struct {
	ImageID     int64   `json:"image_id"`
	Site        string  `json:"site"`
	PostID      string  `json:"post_id"`
	URL         string  `json:"url"`
	MD5         string  `json:"md5"`
	Commentary  string  `json:"commentary"`
	Translated  string  `json:"commentary_translated,omitempty"`
	Original    string  `json:"original,omitempty"`
	Similarity  float64 `json:"similarity,omitempty"`
	MD5Match    string  `json:"md5_match,omitempty"`
	ParentURL   string  `json:"parent_url,omitempty"`
	UpgradeKept int     `json:"upgrade_kept,omitempty"`
	PostWidth   int64   `json:"post_width,omitempty"`
	PostHeight  int64   `json:"post_height,omitempty"`
	PostSize    int64   `json:"post_size,omitempty"`
	PostExt     string  `json:"post_ext,omitempty"`
	FetchedAt   string  `json:"fetched_at"`
}

type imageAnnotationRow struct {
	ImageID   int64  `json:"image_id"`
	Site      string `json:"site"`
	PostID    string `json:"post_id"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	Body      string `json:"body"`
	Manual    int    `json:"manual,omitempty"`
	FetchedAt string `json:"fetched_at"`
}

type imagePathRow struct {
	ID          int64  `json:"id"`
	ImageID     int64  `json:"image_id"`
	Path        string `json:"path"`
	IsCanonical int    `json:"is_canonical"`
	MtimeUnix   int64  `json:"mtime_unix,omitempty"`
	MtimeNsec   int64  `json:"mtime_nsec,omitempty"`
}

type imageTagRow struct {
	ImageID    int64           `json:"image_id"`
	TagID      int64           `json:"tag_id"`
	IsAuto     int             `json:"is_auto"`
	IsImplied  int             `json:"is_implied,omitempty"`
	Confidence sql.NullFloat64 `json:"confidence"`
	TaggerName sql.NullString  `json:"tagger_name"`
	CreatedAt  string          `json:"created_at"`
	Stale      int             `json:"stale,omitempty"`
}

// imageTagSourceRow travels on its own: image_tags.tagger_name keeps only
// the first source, so the ledger can't be rebuilt from it.
type imageTagSourceRow struct {
	ImageID   int64  `json:"image_id"`
	TagID     int64  `json:"tag_id"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

type tagImplicationRow struct {
	ParentTagID  int64  `json:"parent_tag_id"`
	ImpliedTagID int64  `json:"implied_tag_id"`
	CreatedAt    string `json:"created_at"`
	Origin       string `json:"origin"`
	Stale        int    `json:"stale,omitempty"`
}

type tagDistinctPairRow struct {
	ATagID    int64  `json:"a_tag_id"`
	BTagID    int64  `json:"b_tag_id"`
	CreatedAt string `json:"created_at"`
}

type tagNoteRow struct {
	TagID int64  `json:"tag_id"`
	Body  string `json:"body"`
	Links string `json:"links"`
}

type sdMetadataRow struct {
	ImageID        int64           `json:"image_id"`
	Prompt         sql.NullString  `json:"prompt"`
	NegativePrompt sql.NullString  `json:"negative_prompt"`
	Model          sql.NullString  `json:"model"`
	Seed           sql.NullInt64   `json:"seed"`
	Sampler        sql.NullString  `json:"sampler"`
	Steps          sql.NullInt64   `json:"steps"`
	CFGScale       sql.NullFloat64 `json:"cfg_scale"`
	RawParams      sql.NullString  `json:"raw_params"`
	GenerationHash sql.NullString  `json:"generation_hash"`
}

type comfyMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Prompt          sql.NullString  `json:"prompt"`
	ModelCheckpoint sql.NullString  `json:"model_checkpoint"`
	Seed            sql.NullInt64   `json:"seed"`
	Sampler         sql.NullString  `json:"sampler"`
	Steps           sql.NullInt64   `json:"steps"`
	CFGScale        sql.NullFloat64 `json:"cfg_scale"`
	RawWorkflow     sql.NullString  `json:"raw_workflow"`
	GenerationHash  sql.NullString  `json:"generation_hash"`
}

type mangaMetadataRow struct {
	ImageID         int64           `json:"image_id"`
	Title           sql.NullString  `json:"title"`
	Series          sql.NullString  `json:"series"`
	Number          sql.NullString  `json:"number"`
	Volume          sql.NullString  `json:"volume"`
	Count           sql.NullInt64   `json:"count"`
	Summary         sql.NullString  `json:"summary"`
	Notes           sql.NullString  `json:"notes"`
	Year            sql.NullInt64   `json:"year"`
	Month           sql.NullInt64   `json:"month"`
	Day             sql.NullInt64   `json:"day"`
	Writer          sql.NullString  `json:"writer"`
	Penciller       sql.NullString  `json:"penciller"`
	Inker           sql.NullString  `json:"inker"`
	Colorist        sql.NullString  `json:"colorist"`
	Letterer        sql.NullString  `json:"letterer"`
	CoverArtist     sql.NullString  `json:"cover_artist"`
	Editor          sql.NullString  `json:"editor"`
	Publisher       sql.NullString  `json:"publisher"`
	Imprint         sql.NullString  `json:"imprint"`
	Genre           sql.NullString  `json:"genre"`
	Web             sql.NullString  `json:"web"`
	LanguageISO     sql.NullString  `json:"language_iso"`
	Format          sql.NullString  `json:"format"`
	Manga           sql.NullString  `json:"manga"`
	AgeRating       sql.NullString  `json:"age_rating"`
	CommunityRating sql.NullFloat64 `json:"community_rating"`
	XMLPageCount    sql.NullInt64   `json:"xml_page_count"`
	RawXML          sql.NullString  `json:"raw_xml"`
}

type savedSearchRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	Sort      string `json:"sort"`
	Order     string `json:"sort_order"`
	Seed      string `json:"seed"`
	CreatedAt string `json:"created_at"`
}

type dupGroupRow struct {
	ID              int64  `json:"id"`
	OriginalImageID int64  `json:"original_image_id"`
	CreatedAt       string `json:"created_at"`
}

type dupGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type altGroupRow struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
}

type altGroupMemberRow struct {
	ImageID   int64  `json:"image_id"`
	GroupID   int64  `json:"group_id"`
	CreatedAt string `json:"created_at"`
}

type versionEdgeRow struct {
	ChildImageID  int64  `json:"child_image_id"`
	ParentImageID int64  `json:"parent_image_id"`
	CreatedAt     string `json:"created_at"`
}

type derivativeEdgeRow struct {
	DerivativeImageID int64  `json:"derivative_image_id"`
	SourceImageID     int64  `json:"source_image_id"`
	CreatedAt         string `json:"created_at"`
}

type notRelatedPairRow struct {
	AImageID  int64  `json:"a_image_id"`
	BImageID  int64  `json:"b_image_id"`
	CreatedAt string `json:"created_at"`
}

// VACUUM INTO gives a consistent, WAL-folded snapshot of the live gallery,
// and it only reads, so the read pool runs it and writes carry on.
func ExportGalleryDB(cx gallery.Handle, w io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Dir(cx.DBPath), "export-*.db")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := cx.DB.Read.Exec("VACUUM INTO ?", tmpPath); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// The ids ride as one json_each parameter, since a scope is a whole search
// and SQLite caps parameters at 32766. nil means the whole gallery.
type exportScope struct {
	ids  []int64
	json string
}

func newExportScope(ids []int64) exportScope {
	if ids == nil {
		return exportScope{}
	}
	b, _ := json.Marshal(ids)
	return exportScope{ids: ids, json: string(b)}
}

func (sc exportScope) where(col string) string {
	if sc.ids == nil {
		return ""
	}
	return " WHERE " + col + " IN (SELECT value FROM json_each(?))"
}

func (sc exportScope) args() []any {
	if sc.ids == nil {
		return nil
	}
	return []any{sc.json}
}

// nil ids exports the whole gallery. A scoped export still carries the
// whole tag catalog, which its images reference.
func ExportGalleryJSON(cx gallery.Handle, w io.Writer, ids []int64) error {
	// One snapshot: a row committed between two sections would dangle in the import.
	tx, err := cx.DB.Read.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	sc := newExportScope(ids)

	bw := newJSONWriter(w)
	bw.objStart()
	bw.field("version", ExportVersion)
	bw.field("gallery_name", cx.Name)
	bw.field("gallery_path", cx.GalleryPath)

	for _, t := range exportTables {
		t.export(bw, tx, sc)
	}
	bw.objEnd()
	return bw.err
}

type tableIO interface {
	key() string
	export(j *jsonWriter, tx *sql.Tx, sc exportScope)
	load(tx *sql.Tx, dec *json.Decoder) error
}

// cols is the SELECT and the INSERT column list, in fields' and args'
// order; scope names the column a scoped export filters on.
type table[T any] struct {
	name, label, cols, scope, order string
	orIgnore                        bool
	fields                          func(*T) []any
	args                            func(T) []any
}

func (t table[T]) key() string { return t.name }

func (t table[T]) export(j *jsonWriter, tx *sql.Tx, sc exportScope) {
	query, args := `SELECT `+t.cols+` FROM `+t.name, []any(nil)
	if t.scope != "" {
		query, args = query+sc.where(t.scope), sc.args()
	}
	if t.order != "" {
		query += ` ORDER BY ` + t.order
	}
	streamRows(j, t.name, tx, query, db.ScanRow(t.fields), args...)
}

func (t table[T]) load(tx *sql.Tx, dec *json.Decoder) error {
	verb := `INSERT`
	if t.orIgnore {
		verb = `INSERT OR IGNORE`
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", strings.Count(t.cols, ",")+1), ", ")
	args := t.args
	if args == nil {
		args = func(r T) []any { return t.fields(&r) }
	}
	return insertStream(tx, dec, t.label, verb+` INTO `+t.name+` (`+t.cols+`) VALUES (`+marks+`)`, args)
}

// In the document's section order.
var exportTables = []tableIO{
	table[tagCategoryRow]{name: "tag_categories", label: "tag_category", cols: `id, name, color, is_builtin, position`, order: `id`,
		fields: func(r *tagCategoryRow) []any { return []any{&r.ID, &r.Name, &r.Color, &r.IsBuiltin, &r.Position} },
		args: func(r tagCategoryRow) []any {
			// The color lands in an inline style attribute: don't trust it.
			safeColor := tags.SafeCategoryColor(r.Color)
			if safeColor != r.Color {
				logx.Warnf("import: replaced invalid color %q for tag_category %q with %s", r.Color, r.Name, safeColor)
			}
			return []any{r.ID, r.Name, safeColor, r.IsBuiltin, r.Position}
		}},
	table[tagRow]{name: "tags", label: "tag", cols: `id, name, category_id, usage_count, is_alias, canonical_tag_id, created_at, origin, last_used_at, stale, rejected, rejected_drops`, order: `id`,
		fields: func(r *tagRow) []any {
			return []any{&r.ID, &r.Name, &r.CategoryID, &r.UsageCount, &r.IsAlias, &r.CanonicalTagID, &r.CreatedAt, &r.Origin, &r.LastUsedAt, &r.Stale, &r.Rejected, &r.RejectedDrops}
		}},
	table[tagImplicationRow]{name: "tag_implications", label: "tag_implication", cols: `parent_tag_id, implied_tag_id, created_at, origin, stale`, order: `parent_tag_id, implied_tag_id`,
		fields: func(r *tagImplicationRow) []any {
			return []any{&r.ParentTagID, &r.ImpliedTagID, &r.CreatedAt, &r.Origin, &r.Stale}
		}},
	table[tagNoteRow]{name: "tag_notes", label: "tag_note", cols: `tag_id, body, links`, order: `tag_id`,
		fields: func(r *tagNoteRow) []any { return []any{&r.TagID, &r.Body, &r.Links} }},
	table[tagDistinctPairRow]{name: "tag_distinct_pairs", label: "tag_distinct_pair", cols: `a_tag_id, b_tag_id, created_at`, order: `a_tag_id, b_tag_id`, orIgnore: true,
		fields: func(r *tagDistinctPairRow) []any { return []any{&r.ATagID, &r.BTagID, &r.CreatedAt} },
		args: func(r tagDistinctPairRow) []any {
			return []any{min(r.ATagID, r.BTagID), max(r.ATagID, r.BTagID), r.CreatedAt}
		}},
	table[imageRow]{name: "images", label: "image", scope: "id", order: `id`,
		cols: `id, sha256, md5, canonical_path, folder_path, file_type, width, height,
		       file_size, is_missing, is_favorited, is_inbox, auto_tagged_at, source_type, origin, source, url, page_count, duration_seconds, series, series_order, note, original_source,
		       phash, last_read_page, upload_batch, scheduled_lookup, scheduled_lookup_ptr, ingested_at`,
		fields: func(r *imageRow) []any {
			return []any{&r.ID, &r.SHA256, &r.MD5, &r.CanonicalPath, &r.FolderPath, &r.FileType,
				&r.Width, &r.Height, &r.FileSize, &r.IsMissing, &r.IsFavorited, &r.IsInbox,
				&r.AutoTaggedAt, &r.SourceType, &r.Origin, &r.Source, &r.URL, &r.PageCount, &r.DurationSeconds, &r.Series, &r.SeriesOrder, &r.Note, &r.OriginalSource,
				&r.Phash, &r.LastReadPage, &r.UploadBatch, &r.ScheduledLookup, &r.ScheduledLookupPTR, &r.IngestedAt}
		}},
	table[imageCollectionRow]{name: "image_collections", label: "image_collection", cols: `image_id, name, position`, scope: "image_id", order: `image_id, name`, orIgnore: true,
		fields: func(r *imageCollectionRow) []any { return []any{&r.ImageID, &r.Name, &r.Position} }},
	table[findRelationsRow]{name: "collection_find_relations", label: "collection_find_relations", cols: `name`, order: `name`, orIgnore: true,
		fields: func(r *findRelationsRow) []any { return []any{&r.Name} }},
	table[imageSourceRow]{name: "image_sources", label: "image_source", scope: "image_id", order: `rowid`, orIgnore: true,
		cols: `image_id, site, post_id, url, md5, commentary, commentary_translated, original, similarity,
		       md5_match, parent_url, upgrade_kept, post_width, post_height, post_size, post_ext, fetched_at`,
		fields: func(r *imageSourceRow) []any {
			return []any{&r.ImageID, &r.Site, &r.PostID, &r.URL, &r.MD5, &r.Commentary, &r.Translated, &r.Original, &r.Similarity,
				&r.MD5Match, &r.ParentURL, &r.UpgradeKept, &r.PostWidth, &r.PostHeight, &r.PostSize, &r.PostExt, &r.FetchedAt}
		}},
	table[imageAnnotationRow]{name: "image_annotations", label: "image_annotation (image)", cols: `image_id, site, post_id, x, y, w, h, body, manual, fetched_at`, scope: "image_id", order: `id`,
		fields: func(r *imageAnnotationRow) []any {
			return []any{&r.ImageID, &r.Site, &r.PostID, &r.X, &r.Y, &r.W, &r.H, &r.Body, &r.Manual, &r.FetchedAt}
		}},
	table[imagePathRow]{name: "image_paths", label: "image_path", cols: `id, image_id, path, is_canonical, mtime_unix, mtime_nsec`, scope: "image_id", order: `id`,
		fields: func(r *imagePathRow) []any {
			return []any{&r.ID, &r.ImageID, &r.Path, &r.IsCanonical, &r.MtimeUnix, &r.MtimeNsec}
		}},
	table[imageTagRow]{name: "image_tags", label: "image_tag", cols: `image_id, tag_id, is_auto, is_implied, confidence, tagger_name, created_at, stale`, scope: "image_id",
		fields: func(r *imageTagRow) []any {
			return []any{&r.ImageID, &r.TagID, &r.IsAuto, &r.IsImplied, &r.Confidence, &r.TaggerName, &r.CreatedAt, &r.Stale}
		}},
	table[imageTagSourceRow]{name: "image_tag_sources", label: "image_tag_source", cols: `image_id, tag_id, source, created_at`, scope: "image_id", order: `image_id, tag_id, source`, orIgnore: true,
		fields: func(r *imageTagSourceRow) []any { return []any{&r.ImageID, &r.TagID, &r.Source, &r.CreatedAt} }},
	table[sdMetadataRow]{name: "sd_metadata", label: "sd_metadata", cols: `image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash`, scope: "image_id",
		fields: func(r *sdMetadataRow) []any {
			return []any{&r.ImageID, &r.Prompt, &r.NegativePrompt, &r.Model, &r.Seed, &r.Sampler, &r.Steps, &r.CFGScale, &r.RawParams, &r.GenerationHash}
		}},
	table[comfyMetadataRow]{name: "comfyui_metadata", label: "comfyui_metadata", cols: `image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash`, scope: "image_id",
		fields: func(r *comfyMetadataRow) []any {
			return []any{&r.ImageID, &r.Prompt, &r.ModelCheckpoint, &r.Seed, &r.Sampler, &r.Steps, &r.CFGScale, &r.RawWorkflow, &r.GenerationHash}
		}},
	table[mangaMetadataRow]{name: "manga_metadata", label: "manga_metadata", scope: "image_id",
		cols: `image_id, title, series, number, volume, count, summary, notes,
		       year, month, day, writer, penciller, inker, colorist, letterer, cover_artist, editor, publisher,
		       imprint, genre, web, language_iso, format, manga, age_rating, community_rating, xml_page_count, raw_xml`,
		fields: func(r *mangaMetadataRow) []any {
			return []any{&r.ImageID, &r.Title, &r.Series, &r.Number, &r.Volume, &r.Count, &r.Summary, &r.Notes,
				&r.Year, &r.Month, &r.Day, &r.Writer, &r.Penciller, &r.Inker, &r.Colorist, &r.Letterer, &r.CoverArtist,
				&r.Editor, &r.Publisher, &r.Imprint, &r.Genre, &r.Web, &r.LanguageISO, &r.Format, &r.Manga, &r.AgeRating,
				&r.CommunityRating, &r.XMLPageCount, &r.RawXML}
		}},
	table[dupGroupRow]{name: "dup_groups", label: "dup_group", cols: `id, original_image_id, created_at`, order: `id`,
		fields: func(r *dupGroupRow) []any { return []any{&r.ID, &r.OriginalImageID, &r.CreatedAt} }},
	table[dupGroupMemberRow]{name: "dup_group_members", label: "dup_group_member", cols: `image_id, group_id, created_at`, scope: "image_id", order: `image_id`,
		fields: func(r *dupGroupMemberRow) []any { return []any{&r.ImageID, &r.GroupID, &r.CreatedAt} }},
	table[altGroupRow]{name: "alt_groups", label: "alt_group", cols: `id, created_at`, order: `id`,
		fields: func(r *altGroupRow) []any { return []any{&r.ID, &r.CreatedAt} }},
	table[altGroupMemberRow]{name: "alt_group_members", label: "alt_group_member", cols: `image_id, group_id, created_at`, scope: "image_id", order: `image_id`,
		fields: func(r *altGroupMemberRow) []any { return []any{&r.ImageID, &r.GroupID, &r.CreatedAt} }},
	table[versionEdgeRow]{name: "version_edges", label: "version_edge", cols: `child_image_id, parent_image_id, created_at`, scope: "child_image_id", order: `child_image_id`,
		fields: func(r *versionEdgeRow) []any { return []any{&r.ChildImageID, &r.ParentImageID, &r.CreatedAt} }},
	table[derivativeEdgeRow]{name: "derivative_edges", label: "derivative_edge", cols: `derivative_image_id, source_image_id, created_at`, scope: "derivative_image_id", order: `derivative_image_id, source_image_id`,
		fields: func(r *derivativeEdgeRow) []any { return []any{&r.DerivativeImageID, &r.SourceImageID, &r.CreatedAt} }},
	// The relations service only matches a < b, which an older or
	// hand-written document may not follow; OR IGNORE for one that names
	// both orientations.
	table[notRelatedPairRow]{name: "not_related_pairs", label: "not_related_pair", cols: `a_image_id, b_image_id, created_at`, scope: "a_image_id", order: `a_image_id, b_image_id`, orIgnore: true,
		fields: func(r *notRelatedPairRow) []any { return []any{&r.AImageID, &r.BImageID, &r.CreatedAt} },
		args: func(r notRelatedPairRow) []any {
			return []any{min(r.AImageID, r.BImageID), max(r.AImageID, r.BImageID), r.CreatedAt}
		}},
	table[savedSearchRow]{name: "saved_searches", label: "saved_search", cols: `id, name, query, sort, sort_order, seed, created_at`, order: `id`,
		fields: func(r *savedSearchRow) []any {
			return []any{&r.ID, &r.Name, &r.Query, &r.Sort, &r.Order, &r.Seed, &r.CreatedAt}
		}},
	table[imageLookupRow]{name: "image_lookups", label: "image_lookup", cols: `image_id, backend, attempts, queued_at, job_id, last_at, last_result, next_due_at, ptr_cursor`, scope: "image_id", order: `image_id, backend`,
		fields: func(r *imageLookupRow) []any {
			return []any{&r.ImageID, &r.Backend, &r.Attempts, &r.QueuedAt, &r.JobID, &r.LastAt, &r.LastResult, &r.NextDueAt, &r.PTRCursor}
		}},
}

var tableByKey = func() map[string]tableIO {
	m := make(map[string]tableIO, len(exportTables))
	for _, t := range exportTables {
		m[t.key()] = t
	}
	return m
}()

func ExportGalleryArchive(cx gallery.Handle, format string, w io.Writer) error {
	switch format {
	case "db":
		return writeArchive(w, cx.Boundary(), "monbooru.db", func(inner io.Writer) error { return ExportGalleryDB(cx, inner) })
	case "json":
		return writeArchive(w, cx.Boundary(), "monbooru.json", func(inner io.Writer) error { return ExportGalleryJSON(cx, inner, nil) })
	}
	return fmt.Errorf("unknown archive format %q", format)
}

func writeArchive(w io.Writer, b *gallery.Boundary, inner string, write func(io.Writer) error) error {
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	entry, err := zw.CreateHeader(&zip.FileHeader{Name: inner, Method: zip.Deflate})
	if err != nil {
		return err
	}
	if err := write(entry); err != nil {
		return err
	}
	if err := writeGalleryFilesToZip(zw, b); err != nil {
		return err
	}
	// Close writes the central directory, so its error must reach the
	// caller; the deferred Close covers the error paths.
	return zw.Close()
}

// An unreadable root is skipped so the db/json still exports.
func writeGalleryFilesToZip(zw *zip.Writer, b *gallery.Boundary) error {
	galleryPath := b.Root()
	if _, err := os.Stat(galleryPath); err != nil {
		logx.Warnf("export: gallery path %q unreadable; archive will not include gallery files: %v", galleryPath, err)
		return nil
	}
	return gallery.WalkTree(b, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logx.Warnf("export: skip %q: %v", path, walkErr)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(galleryPath, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			logx.Warnf("export: skip %q: %v", path, err)
			return nil
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			logx.Warnf("export: skip %q: %v", path, err)
			return nil
		}
		return storeEntry(zw, f, "gallery/"+filepath.ToSlash(rel), info)
	})
}

// Media is already compressed; a deflate pass would spend CPU for nothing.
func storeEntry(zw *zip.Writer, src io.Reader, name string, info fs.FileInfo) error {
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: info.ModTime()})
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, src)
	return err
}

// quiesce closes the target just before the first write, so a refusal
// before it leaves the gallery open and no watcher ingests what is
// extracted; the caller reopens it after. maxFileSizeMB <= 0 lifts the
// per-entry cap; the count is the entries b excludes.
func ApplyImport(format, tmpPath, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int, quiesce func() error) (int, error) {
	switch format {
	case "db":
		return 0, replaceDBFromFile(tmpPath, dbPath, thumbsPath, b, quiesce)
	case "json":
		isLight, err := isLightManifestJSON(tmpPath)
		if err != nil {
			return 0, fmt.Errorf("inspect json: %w", err)
		}
		if isLight {
			return 0, replaceFromLightManifest(tmpPath, dbPath, thumbsPath, b, quiesce)
		}
		return 0, replaceDBFromJSON(tmpPath, dbPath, thumbsPath, b, quiesce)
	case "zip":
		return replaceFromArchive(tmpPath, dbPath, thumbsPath, b, maxFileSizeMB, quiesce)
	}
	return 0, fmt.Errorf("unknown import format %q", format)
}

// The first key that tells the two apart decides: a light manifest leads
// with its images, a full export with its name and categories.
func isLightManifestJSON(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	light := true
	err = decodeSections(f, func(key string, dec *json.Decoder) error {
		switch key {
		case "images":
			return errStopSections
		case "gallery_name", "tag_categories":
			light = false
			return errStopSections
		}
		return skipValue(dec)
	})
	if err != nil && !errors.Is(err, errStopSections) {
		return false, err
	}
	return light, nil
}

func replaceDBFromFile(srcPath, dbPath, thumbsPath string, b *gallery.Boundary, quiesce func() error) error {
	if err := validateSQLiteFile(srcPath); err != nil {
		return fmt.Errorf("uploaded file is not a valid monbooru database: %w", err)
	}
	return loadThenInstall(srcPath, dbPath, thumbsPath, "", quiesce, func(database *db.DB) error {
		// Bootstrap first: a sanitiser may read a column an older snapshot lacks.
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap imported db: %w", err)
		}
		if err := sanitizeImportedCategoryColors(database); err != nil {
			return fmt.Errorf("sanitize colors: %w", err)
		}
		if err := sanitizeImportedAliasChains(database); err != nil {
			return fmt.Errorf("sanitize alias chains: %w", err)
		}
		return rebaseImagePaths(database, b)
	})
}

// The database at path is loaded and swapped in for dbPath only on
// success, so a failed load leaves the gallery untouched. newThumbs, when
// set, replaces thumbsPath after the database; otherwise thumbsPath is emptied.
func loadThenInstall(path, dbPath, thumbsPath, newThumbs string, quiesce func() error, load func(*db.DB) error) error {
	defer func() {
		for _, p := range []string{path + "-wal", path + "-shm"} {
			_ = os.Remove(p)
		}
	}()
	database, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("open new db: %w", err)
	}
	loadErr := load(database)
	if loadErr == nil {
		// Fold the WAL in so the rename below carries every committed page.
		if _, err := database.Write.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			loadErr = fmt.Errorf("checkpoint: %w", err)
		}
	}
	if err := database.Close(); err != nil && loadErr == nil {
		loadErr = fmt.Errorf("close new db: %w", err)
	}
	if loadErr != nil {
		return loadErr
	}
	if err := quiesce(); err != nil {
		return err
	}
	if err := removeSidecars(dbPath); err != nil {
		return err
	}
	if newThumbs == "" {
		if err := resetThumbs(thumbsPath); err != nil {
			return err
		}
	}
	// Rename replaces the old file, so a failure here keeps it.
	if err := os.Rename(path, dbPath); err != nil {
		return fmt.Errorf("install db: %w", err)
	}
	if newThumbs == "" {
		return nil
	}
	if err := os.RemoveAll(thumbsPath); err != nil {
		return fmt.Errorf("clear thumbnails: %w", err)
	}
	if err := os.Rename(newThumbs, thumbsPath); err != nil {
		return fmt.Errorf("install thumbnails: %w", err)
	}
	return nil
}

// A .db import skips every validator, so its colors are coerced after the fact.
func sanitizeImportedCategoryColors(database *db.DB) error {
	type row struct {
		id    int64
		color string
	}
	all, err := db.QueryAll(database.Read, db.ScanRow(func(r *row) []any {
		return []any{&r.id, &r.color}
	}), `SELECT id, color FROM tag_categories`)
	if err != nil {
		return err
	}
	for _, r := range all {
		if tags.IsValidCategoryColor(r.color) {
			continue
		}
		safe := tags.SafeCategoryColor(r.color)
		logx.Warnf("import: replaced invalid color %q on tag_category id=%d with %s", r.color, r.id, safe)
		if _, err := database.Write.Exec(
			`UPDATE tag_categories SET color = ? WHERE id = ?`, safe, r.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// Resolvers follow COALESCE(canonical_tag_id, id) once, without an
// is_alias check, so an imported chain drops out of search and a plain
// tag's pointer misdirects it.
func sanitizeImportedAliasChains(database *db.DB) error {
	type node struct {
		alias     bool
		canonical int64
	}
	type idNode struct {
		id int64
		node
	}
	all, err := db.QueryAll(database.Read, func(rows *sql.Rows) (idNode, error) {
		var n idNode
		var alias int
		err := rows.Scan(&n.id, &alias, &n.canonical)
		n.alias = alias == 1
		return n, err
	}, `SELECT id, is_alias, COALESCE(canonical_tag_id, 0) FROM tags`)
	if err != nil {
		return err
	}
	nodes := make(map[int64]node, len(all))
	ids := make([]int64, 0, len(all))
	for _, n := range all {
		nodes[n.id] = n.node
		ids = append(ids, n.id)
	}
	// Sorted walk order keeps the promoted cycle member deterministic.
	slices.Sort(ids)

	// root[id] is where id's chain ends; a promoted row is its own root.
	root := make(map[int64]int64)
	promoted := make(map[int64]bool)
	for _, id := range ids {
		if n := nodes[id]; !n.alias {
			continue
		}
		if _, done := root[id]; done {
			continue
		}
		var path []int64
		onPath := make(map[int64]bool)
		cur := id
		var end int64
		for {
			n := nodes[cur]
			if !n.alias {
				end = cur
				break
			}
			if r, ok := root[cur]; ok {
				end = r
				break
			}
			if _, ok := nodes[n.canonical]; !ok || onPath[cur] {
				end = cur
				promoted[cur] = true
				break
			}
			onPath[cur] = true
			path = append(path, cur)
			cur = n.canonical
		}
		root[end] = end
		for _, p := range path {
			root[p] = end
		}
	}

	for _, id := range ids {
		n := nodes[id]
		switch {
		case !n.alias && n.canonical != 0:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: cleared the canonical pointer on plain tag id=%d", id)
		case !n.alias:
		case promoted[id]:
			if _, err := database.Write.Exec(
				`UPDATE tags SET is_alias = 0, canonical_tag_id = NULL WHERE id = ?`, id,
			); err != nil {
				return err
			}
			logx.Warnf("import: promoted alias id=%d to a plain tag (alias cycle or missing canonical)", id)
		case root[id] != n.canonical:
			if _, err := database.Write.Exec(
				`UPDATE tags SET canonical_tag_id = ? WHERE id = ?`, root[id], id,
			); err != nil {
				return err
			}
			logx.Warnf("import: re-pointed alias id=%d through its alias chain to tag id=%d", id, root[id])
		}
	}
	return nil
}

func replaceDBFromJSON(srcPath, dbPath, thumbsPath string, b *gallery.Boundary, quiesce func() error) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open json: %w", err)
	}
	defer func() { _ = f.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dbPath), "import-*.db")
	if err != nil {
		return fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	return loadThenInstall(tmpPath, dbPath, thumbsPath, "", quiesce, func(database *db.DB) error {
		if err := db.Bootstrap(database); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if err := loadExportStream(database, f); err != nil {
			return err
		}
		if err := sanitizeImportedAliasChains(database); err != nil {
			return fmt.Errorf("sanitize alias chains: %w", err)
		}
		return rebaseImagePaths(database, b)
	})
}

func classifyArchive(files []*zip.File) (innerDB, innerJSON, innerLight *zip.File, gallery []translatedFile) {
	for _, f := range files {
		switch {
		case f.Name == "monbooru.db":
			innerDB = f
		case f.Name == "monbooru.json":
			innerJSON = f
		case f.Name == "tags.json":
			innerLight = f
		case strings.HasPrefix(f.Name, "gallery/") && !strings.HasSuffix(f.Name, "/"):
			gallery = append(gallery, translatedFile{rel: strings.TrimPrefix(f.Name, "gallery/"), file: f})
		}
	}
	return innerDB, innerJSON, innerLight, gallery
}

func replaceFromArchive(srcPath, dbPath, thumbsPath string, b *gallery.Boundary, maxFileSizeMB int, quiesce func() error) (int, error) {
	maxBytes := int64(maxFileSizeMB) * 1024 * 1024
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return 0, fmt.Errorf("open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	innerDB, innerJSON, innerLight, galleryFiles := classifyArchive(zr.File)
	if innerDB == nil && innerJSON == nil && innerLight == nil {
		if format := compatibility.Detect(zr.File); format != "" {
			return replaceFromCompatArchive(zr.File, format, dbPath, thumbsPath, b, maxFileSizeMB, quiesce)
		}
		return 0, fmt.Errorf("archive missing monbooru.db, monbooru.json, or tags.json")
	}
	if innerDB == nil && innerJSON == nil {
		return replaceFromLightArchive(innerLight, galleryFiles, dbPath, thumbsPath, b, maxFileSizeMB, quiesce)
	}

	dsts, err := planExtraction(b, galleryFiles, maxBytes)
	if err != nil {
		return 0, err
	}

	var innerFile *zip.File
	var applyInner func(string, string, string, *gallery.Boundary, func() error) error
	if innerDB != nil {
		innerFile = innerDB
		applyInner = replaceDBFromFile
	} else {
		innerFile = innerJSON
		applyInner = replaceDBFromJSON
	}
	innerTmpPath, err := extractZipEntryToTemp(innerFile, filepath.Dir(dbPath))
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(innerTmpPath) }()
	if err := applyInner(innerTmpPath, dbPath, thumbsPath, b, quiesce); err != nil {
		return 0, err
	}

	if len(galleryFiles) == 0 {
		return 0, nil
	}

	leftOut, err := extractPlanned(b, galleryFiles, dsts, maxBytes)
	// applyInner reconciled before these files existed, so reconcile again,
	// after a failed extraction too: the archive's rows are installed.
	database, openErr := db.Open(dbPath)
	if openErr != nil {
		if err == nil {
			err = fmt.Errorf("reopen db for reconcile: %w", openErr)
		}
		return leftOut, err
	}
	defer func() { _ = database.Close() }()
	if reconcileErr := reconcileMissingFiles(database, b); err == nil {
		err = reconcileErr
	}
	return leftOut, err
}

func slashed(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// Not filepath.Base: a stored path carries the writing machine's separators.
func storedBasename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func rebaseImagePaths(database *db.DB, b *gallery.Boundary) error {
	root := strings.TrimRight(b.Root(), "/")
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// A library written on Windows stores backslash folders: normalise
	// them before the scan reads folder_path, and before the rewrite
	// erases the backslashes that mark a Windows row.
	if _, err := tx.Exec(db.NormalizeWindowsFolderPathSQL); err != nil {
		return fmt.Errorf("normalize folder paths: %w", err)
	}

	// The source root, inferred from the first canonical row, lets an alias
	// keep its own folder instead of collapsing into the canonical's. It is
	// matched in slash form, whichever OS wrote the paths.
	sourceRoot := ""
	rootKnown := false
	err = eachImagePathChunk(tx, func(imgs []imgPathRow) error {
		for _, r := range imgs {
			if rootKnown || r.canonical == "" {
				continue
			}
			canonical := slashed(r.canonical)
			suffix := path.Join(r.folder, storedBasename(r.canonical))
			if src, ok := strings.CutSuffix(canonical, "/"+suffix); ok {
				sourceRoot = src
				rootKnown = true
			} else if canonical == suffix {
				rootKnown = true
			}
		}
		for _, r := range imgs {
			newCanonical := filepath.Join(root, r.folder, storedBasename(r.canonical))
			if newCanonical == r.canonical {
				continue
			}
			if _, err := tx.Exec(
				`UPDATE images SET canonical_path = ? WHERE id = ?`, newCanonical, r.id,
			); err != nil {
				return fmt.Errorf("update image %d: %w", r.id, err)
			}
		}
		return reconcileMissingFlags(tx, b, imgs)
	})
	if err != nil {
		return err
	}

	type pathRow struct {
		id, imageID int64
		path        string
		isCanonical bool
		folder      string
	}
	for after := int64(0); ; {
		paths, err := db.QueryAll(tx, func(rows *sql.Rows) (pathRow, error) {
			var r pathRow
			var isCanon int
			err := rows.Scan(&r.id, &r.imageID, &r.path, &isCanon, &r.folder)
			r.isCanonical = isCanon == 1
			return r, err
		},
			`SELECT ip.id, ip.image_id, ip.path, ip.is_canonical, i.folder_path
			 FROM image_paths ip
			 JOIN images i ON i.id = ip.image_id
			 WHERE ip.id > ? ORDER BY ip.id LIMIT ?`, after, rebaseChunk)
		if err != nil {
			return fmt.Errorf("scan image_paths for rebase: %w", err)
		}
		if len(paths) == 0 {
			break
		}
		for _, p := range paths {
			// Canonical rows rebuild exactly as images.canonical_path did.
			var newPath string
			if p.isCanonical || sourceRoot == "" {
				newPath = filepath.Join(root, p.folder, storedBasename(p.path))
			} else if rel, ok := strings.CutPrefix(slashed(p.path), sourceRoot+"/"); ok {
				newPath = filepath.Join(root, filepath.FromSlash(rel))
			} else {
				newPath = filepath.Join(root, p.folder, storedBasename(p.path))
			}
			if newPath == p.path {
				continue
			}
			if _, err := tx.Exec(
				`UPDATE image_paths SET path = ? WHERE id = ?`, newPath, p.id,
			); err != nil {
				return fmt.Errorf("update image_path %d: %w", p.id, err)
			}
		}
		after = paths[len(paths)-1].id
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
}

// The export's usage counts include images the reconcile just marked
// missing; left as they are, tags claim matches the gallery can't show.
func recalcImportedTagCounts(database *db.DB) error {
	if _, err := tags.RecalcDBCount(database); err != nil {
		return fmt.Errorf("recalculate tag counts: %w", err)
	}
	return nil
}

type imgPathRow struct {
	id        int64
	folder    string
	canonical string
}

const rebaseChunk = 1000

func eachImagePathChunk(q db.Querier, fn func([]imgPathRow) error) error {
	for after := int64(0); ; {
		imgs, err := db.QueryAll(q, db.ScanRow(func(r *imgPathRow) []any {
			return []any{&r.id, &r.folder, &r.canonical}
		}), `SELECT id, folder_path, canonical_path FROM images WHERE id > ? ORDER BY id LIMIT ?`, after, rebaseChunk)
		if err != nil {
			return fmt.Errorf("scan images: %w", err)
		}
		if len(imgs) == 0 {
			return nil
		}
		if err := fn(imgs); err != nil {
			return err
		}
		after = imgs[len(imgs)-1].id
	}
}

// Both directions: an absent file is flagged, and a row exported missing
// but present here is cleared, since an import queues no Sync.
func reconcileMissingFlags(x db.Execer, b *gallery.Boundary, imgs []imgPathRow) error {
	for _, r := range imgs {
		flag := fileMissingFlag(b, r.folder, r.canonical)
		if _, err := x.Exec(`UPDATE images SET is_missing = ? WHERE id = ?`, flag, r.id); err != nil {
			return fmt.Errorf("reconcile is_missing for image %d: %w", r.id, err)
		}
	}
	return nil
}

func fileMissingFlag(b *gallery.Boundary, folder, canonical string) int {
	path := filepath.Join(b.Root(), folder, storedBasename(canonical))
	if _, err := os.Stat(path); err == nil && !b.Excludes(path) {
		return 0
	}
	return 1
}

func reconcileMissingFiles(database *db.DB, b *gallery.Boundary) error {
	err := eachImagePathChunk(database.Read, func(imgs []imgPathRow) error {
		return reconcileMissingFlags(database.Write, b, imgs)
	})
	if err != nil {
		return err
	}
	return recalcImportedTagCounts(database)
}

func validateSQLiteFile(path string) error {
	database, err := db.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	for _, tbl := range []string{"tag_categories", "tags", "images", "image_tags"} {
		var n int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("missing table %q", tbl)
		}
	}
	return nil
}

// The error names the first two arguments: the pair-keyed tables are the
// ones that collide.
func insertStream[T any](tx *sql.Tx, dec *json.Decoder, label, query string, args func(T) []any) error {
	stmt, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", label, err)
	}
	defer func() { _ = stmt.Close() }()
	return streamArray(dec, func(r T) error {
		a := args(r)
		if _, err := stmt.Exec(a...); err != nil {
			key := make([]any, min(2, len(a)))
			for i := range key {
				key[i] = reflect.Indirect(reflect.ValueOf(a[i])).Interface()
			}
			return fmt.Errorf("insert %s %v: %w", label, key, err)
		}
		return nil
	})
}

func loadExportStream(database *db.DB, r io.Reader) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error { return loadExportTx(tx, r) })
}

func loadExportTx(tx *sql.Tx, r io.Reader) error {
	// Deferred: an alias created before its canonical has the lower id.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("defer fk: %w", err)
	}

	// Clears what Bootstrap seeded, which would collide with the imported ids.
	for i := len(exportTables) - 1; i >= 0; i-- {
		if _, err := tx.Exec(`DELETE FROM ` + exportTables[i].key()); err != nil {
			return fmt.Errorf("reset table: %w", err)
		}
	}

	version := 0
	seen := map[string]bool{}
	// The image_tags and image_collections insert triggers read images,
	// tags and categories, so rows streamed ahead of those are re-derived.
	rederive := false
	err := decodeSections(r, func(key string, dec *json.Decoder) error {
		seen[key] = true
		switch key {
		case "version":
			if err := decodeValue(dec, &version); err != nil {
				return err
			}
			return checkExportVersion(version)
		case "image_collections":
			rederive = rederive || !seen["images"]
		case "image_tags":
			rederive = rederive || !seen["images"] || !seen["tags"] || !seen["tag_categories"]
		}
		if t, ok := tableByKey[key]; ok {
			return t.load(tx, dec)
		}
		return skipValue(dec)
	})
	if err != nil {
		return err
	}
	if err := checkExportVersion(version); err != nil {
		return err
	}

	if version < 3 {
		if err := seedCollectionsFromSeries(tx); err != nil {
			return fmt.Errorf("seed image_collections: %w", err)
		}
	}
	if version < 10 {
		// Pre-v10 documents lack the opt-ins, which decoded as 0: opted out.
		if _, err := tx.Exec(`UPDATE images SET scheduled_lookup = 1, scheduled_lookup_ptr = 1`); err != nil {
			return fmt.Errorf("opt images in: %w", err)
		}
	}
	if version < 11 {
		// Pre-v11 documents carry no ledger, so derive one.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
			 SELECT image_id, tag_id, COALESCE(NULLIF(tagger_name, ''), 'user'), created_at
			 FROM image_tags WHERE is_implied = 0`,
		); err != nil {
			return fmt.Errorf("backfill image_tag_sources: %w", err)
		}
	}
	if version < 14 {
		if _, err := tx.Exec(db.ResetCategoryPositions); err != nil {
			return fmt.Errorf("order tag_categories: %w", err)
		}
	}
	if rederive {
		if err := db.RederiveTriggerColumns(tx); err != nil {
			return err
		}
	}
	return nil
}

// Pre-v3 documents hold collections only in images.series. A case variant
// of a spelling seeded first joins it, home mirror included.
func seedCollectionsFromSeries(tx *sql.Tx) error {
	spellings, err := db.QueryStrings(tx, `SELECT series FROM images WHERE series != '' GROUP BY series ORDER BY MIN(id)`)
	if err != nil {
		return err
	}
	for _, spelling := range spellings {
		label, err := gallery.ExistingLabelTx(tx, spelling)
		if err != nil {
			return err
		}
		if label != spelling {
			if _, err := tx.Exec(`UPDATE images SET series = ? WHERE series = ?`, label, spelling); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO image_collections (image_id, name, position)
			 SELECT id, ?, series_order FROM images WHERE series = ?`, label, label); err != nil {
			return err
		}
	}
	return nil
}

// jsonWriter streams so an export holds one row in memory, never a whole table.
type jsonWriter struct {
	w     io.Writer
	err   error
	first bool
}

func newJSONWriter(w io.Writer) *jsonWriter { return &jsonWriter{w: w} }

func (j *jsonWriter) writeStr(s string) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write([]byte(s))
}

func (j *jsonWriter) objStart() {
	j.writeStr("{")
	j.first = true
}

func (j *jsonWriter) objEnd() { j.writeStr("}\n") }

func (j *jsonWriter) comma() {
	if j.first {
		j.first = false
	} else {
		j.writeStr(",")
	}
}

func (j *jsonWriter) field(name string, value any) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":")
	j.marshalAndWrite(value)
}

func (j *jsonWriter) arrayStart(name string) {
	j.comma()
	j.marshalAndWrite(name)
	j.writeStr(":[")
}

func (j *jsonWriter) arrayEnd() { j.writeStr("]") }

func (j *jsonWriter) arrayItem(first *bool, value any) {
	if !*first {
		j.writeStr(",")
	}
	*first = false
	j.marshalAndWrite(value)
}

func (j *jsonWriter) marshalAndWrite(value any) {
	if j.err != nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		j.err = err
		return
	}
	_, j.err = j.w.Write(b)
}

func streamRows[T any](j *jsonWriter, key string, q db.Querier, query string, scan func(*sql.Rows) (T, error), args ...any) {
	if j.err != nil {
		return
	}
	j.arrayStart(key)
	first := true
	rows, err := q.Query(query, args...)
	if err != nil {
		j.arrayEnd()
		j.err = err
		return
	}
	defer func() { _ = rows.Close() }()
	defer j.arrayEnd()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			j.err = err
			return
		}
		j.arrayItem(&first, v)
		if j.err != nil {
			return
		}
	}
	if err := rows.Err(); err != nil && j.err == nil {
		j.err = err
	}
}

var importFormats = map[string]string{
	".db":     "db",
	".sqlite": "db",
	".json":   "json",
	".zip":    "zip",
}

func FormatFromExt(filename string) string {
	return importFormats[strings.ToLower(filepath.Ext(filename))]
}
