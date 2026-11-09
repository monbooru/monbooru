// Package models holds the row types and constants several packages
// share; it imports nothing of ours.
package models

import (
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	FileTypeJPEG = "jpeg"
	FileTypePNG  = "png"
	FileTypeWEBP = "webp"
	FileTypeAVIF = "avif"
	FileTypeJXL  = "jxl"
	FileTypeGIF  = "gif"
	FileTypeMP4  = "mp4"
	FileTypeWEBM = "webm"
	// FileTypeCBZ also covers .zip archives.
	FileTypeCBZ = "cbz"

	SourceTypeA1111   = "a1111"
	SourceTypeComfyUI = "comfyui"
	SourceTypeNone    = "none"
	SourceTypeBoth    = "a1111,comfyui"

	OriginIngest = "ingest"
	OriginUpload = "upload"
	// OriginExtract marks a page extracted from a cbz archive in the reader.
	OriginExtract = "extract"
	// OriginGenerate marks a cbz archive monbooru built from a collection.
	OriginGenerate = "generate"

	// TagSourceMonbooru attributes the meta tags monbooru derives from a
	// file's own properties.
	TagSourceMonbooru = "monbooru"
)

// MediaKinds lists every bucket MediaKind returns.
var MediaKinds = []string{"image", "archive", "animated"}

// FileType is one format ingest stores. Exts[0] is the extension a file
// monbooru names itself takes; Accept are file-picker tokens beside MIME.
type FileType struct {
	Name        string
	Exts        []string
	MIME        string
	Kind        string
	Video       bool
	FFmpegStill bool // a still only ffmpeg decodes
	PageImage   bool // read as a page inside a cbz
	Accept      []string
}

// FileTypes is every format ingest stores; adding one is a row here plus
// its decode path.
var FileTypes = []FileType{
	{Name: FileTypeJPEG, Exts: []string{".jpg", ".jpeg"}, MIME: "image/jpeg", Kind: "image", PageImage: true},
	{Name: FileTypePNG, Exts: []string{".png"}, MIME: "image/png", Kind: "image", PageImage: true},
	{Name: FileTypeWEBP, Exts: []string{".webp"}, MIME: "image/webp", Kind: "image", PageImage: true},
	// The bare extensions reach a picker on an OS with no MIME entry for
	// the format.
	{Name: FileTypeAVIF, Exts: []string{".avif"}, MIME: "image/avif", Kind: "image", FFmpegStill: true, PageImage: true, Accept: []string{".avif"}},
	{Name: FileTypeJXL, Exts: []string{".jxl"}, MIME: "image/jxl", Kind: "image", FFmpegStill: true, PageImage: true, Accept: []string{".jxl"}},
	{Name: FileTypeGIF, Exts: []string{".gif"}, MIME: "image/gif", Kind: "animated", PageImage: true},
	{Name: FileTypeMP4, Exts: []string{".mp4"}, MIME: "video/mp4", Kind: "animated", Video: true},
	{Name: FileTypeWEBM, Exts: []string{".webm"}, MIME: "video/webm", Kind: "animated", Video: true},
	// Served as what the stdlib sniffer already answers for a PK archive.
	{Name: FileTypeCBZ, Exts: []string{".cbz", ".zip"}, MIME: "application/zip", Kind: "archive", Accept: []string{"application/vnd.comicbook+zip", "application/x-cbz"}},
}

func LookupFileType(name string) (FileType, bool) {
	for _, ft := range FileTypes {
		if ft.Name == name {
			return ft, true
		}
	}
	return FileType{}, false
}

func ExtFileType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	for _, ft := range FileTypes {
		if slices.Contains(ft.Exts, ext) {
			return ft.Name
		}
	}
	return ""
}

func MediaKind(fileType string) string {
	ft, _ := LookupFileType(fileType)
	return ft.Kind
}

type Image struct {
	ID             int64
	SHA256         string
	MD5            string // "" until computed
	CanonicalPath  string
	FolderPath     string // relative to the gallery root; "" is the root
	FileType       string
	Width          *int
	Height         *int
	FileSize       int64
	IsMissing      bool
	IsFavorited    bool
	IsInbox        bool
	AutoTaggedAt   *time.Time
	SourceType     string
	Origin         string // an Origin* constant or the API caller's via label
	Source         string
	URL            string   // http(s) only
	Note           string   // the operator's; never written by a push, enrich or import
	OriginalSource string   // read-only: only a gallery transfer or import carries it over
	PageCount      *int     // cbz rows only; nil otherwise
	LastReadPage   *int     // 1-based; nil when unstarted or finished
	DurationSec    *float64 // seconds; nil unless a video probe measured it
	Series         string
	SeriesOrder    *int
	Phash          *int64 // nil until backfilled, or when no thumbnail decodes
	IngestedAt     time.Time
	UploadBatch    *int64 // shared by the files of one web-UI upload; nil otherwise
}

type Collection struct {
	Name  string
	Order *int // nil = unordered
}

type ImageSource struct {
	Site                 string
	PostID               string // "" for a manually added origin
	URL                  string
	Commentary           string
	CommentaryTranslated string
	Original             string  // the artist source the post declared; several are newline-joined
	Similarity           float64 // lookup match score; 0 = exact or manual
	MD5                  string  // the md5 the source last claimed
	MD5Match             string  // claimed md5 vs the file: "" unknown, "match" or "differ"
	// UpgradeKept is the operator's "keep my file" ruling on this origin.
	UpgradeKept bool
	// The file as the post describes it; zero where the source published
	// nothing.
	PostWidth, PostHeight int
	PostSize              int64
	PostExt               string
}

// Annotation coordinates are original-image pixels; a Manual box has no
// Site or PostID.
type Annotation struct {
	ID     int64
	Site   string
	PostID string
	X      int
	Y      int
	W      int
	H      int
	Body   string
	Manual bool
}

// MangaMetadata is parsed from ComicInfo.xml; Image.PageCount, not
// XMLPageCount, is the page count to trust.
type MangaMetadata struct {
	ImageID         int64
	Title           string
	Series          string
	Number          string
	Volume          string
	Count           *int
	Summary         string
	Notes           string
	Year            *int
	Month           *int
	Day             *int
	Writer          string
	Penciller       string
	Inker           string
	Colorist        string
	Letterer        string
	CoverArtist     string
	Editor          string
	Publisher       string
	Imprint         string
	Genre           string
	Web             string
	LanguageISO     string
	Format          string
	Manga           string // "Yes" | "YesAndRightToLeft" | "No" | "Unknown"
	AgeRating       string
	CommunityRating *float64
	XMLPageCount    *int
	RawXML          string // verbatim, truncated at 64 KiB
}

type ImagePath struct {
	ID          int64
	ImageID     int64
	Path        string
	IsCanonical bool
}

type Tag struct {
	ID                     int64
	Name                   string
	CategoryID             int64
	CategoryName           string
	CategoryColor          string
	UsageCount             int
	IsAlias                bool
	CanonicalTagID         *int64
	CanonicalName          string // alias rows only, from the queries that join the canonical
	CanonicalCategoryName  string
	CanonicalCategoryColor string
	CreatedAt              time.Time
	Origin                 string    // creation provenance label; "" when not recorded
	LastUsedAt             time.Time // zero when never applied to an image
	Stale                  bool      // alias rows: the PTR's latest refresh no longer listed this spelling
	StaleUsage             int       // count of this tag's image_tags rows a source dropped (stale=1)
	FoldedInto             string    // corrected spelling on the folded-duplicates view; "" otherwise
	Rejected               bool      // a name mapped to nothing: every add path drops it
	RejectedDrops          int       // arrivals dropped since the reject
}

type TagCategory struct {
	ID        int64
	Name      string
	Color     string
	IsBuiltin bool
	Position  int
}

type ImageTag struct {
	ImageID          int64
	TagID            int64
	TagName          string
	Category         string
	Color            string
	CategoryPosition int
	UsageCount       int
	IsAuto           bool
	IsImplied        bool // row was fanned out from a parent tag's implication graph
	Confidence       *float64
	TaggerName       string // the auto-tagger or source that applied it; "" for a tag added by hand
	Stale            bool   // the attributed source's latest fetch no longer carried this tag
	CreatedAt        time.Time
}

// Adding ParentID to an image also adds ImpliedID as an implied row.
type Implication struct {
	ParentID             int64
	ImpliedID            int64
	ParentName           string
	ParentCategoryName   string
	ParentCategoryColor  string
	ImpliedName          string
	ImpliedCategoryName  string
	ImpliedCategoryColor string
	CreatedAt            time.Time
	Origin               string // creation provenance label; "" when not recorded
	Stale                bool   // the PTR's latest refresh no longer carried the edge
}

type SDParam struct {
	Key string
	Val string
}

type SDMetadata struct {
	ImageID        int64
	Prompt         string
	NegativePrompt string
	Model          string
	Seed           *int64
	Sampler        string
	Steps          *int
	CFGScale       *float64
	RawParams      string
	ParsedParams   []SDParam
	GenerationHash string // hex digest of the generation settings, seed excluded
}

type ComfyUIMetadata struct {
	ImageID         int64
	Prompt          string
	ModelCheckpoint string
	Seed            *int64
	Sampler         string
	Steps           *int
	CFGScale        *float64
	RawWorkflow     string
	GenerationHash  string // hex digest of the generation settings, seed excluded
}

type ComfyNode struct {
	Key       string
	Title     string
	ClassType string
	Params    []ComfyNodeParam
	// SearchTerm is the `comfyui:` term the node class is indexed under.
	SearchTerm string
}

type ComfyNodeParam struct {
	Name  string
	Value string
	IsRef bool // true if the value is a reference to another node
	// SearchTerm is "" when the value isn't indexed; a link on it would
	// land on an empty result.
	SearchTerm string
}

type SavedSearch struct {
	ID        int64
	Name      string
	Query     string
	Sort      string
	Order     string
	Seed      string
	CreatedAt time.Time
}

// HRef must match the query parameters the gallery handler reads.
func (s SavedSearch) HRef() string {
	out := "/?q=" + url.QueryEscape(s.Query)
	if s.Sort != "" {
		out += "&sort=" + url.QueryEscape(s.Sort)
	}
	if s.Order != "" {
		out += "&order=" + url.QueryEscape(s.Order)
	}
	if s.Seed != "" {
		out += "&seed=" + url.QueryEscape(s.Seed)
	}
	return out
}

const (
	JobTypeSync          = "sync"
	JobTypeAutotag       = "autotag"
	JobTypeReExtract     = "re-extract"
	JobTypeDelete        = "delete"
	JobTypeRebuildThumbs = "rebuild-thumbs"
	JobTypeMove          = "move"
	JobTypeTransfer      = "transfer"
	JobTypeTag           = "tag"
	JobTypeWatcher       = "watcher"
	JobTypePruneThumbs   = "prune-thumbs"
	JobTypePruneDirs     = "prune-dirs"
	JobTypeVacuum        = "vacuum"
	JobTypeFreeMemory    = "free-memory"
	JobTypeHashes        = "hashes"
	JobTypeMetaTags      = "meta-tags"
	JobTypeIndexWork     = "index-workflows"
	JobTypeRelations     = "relations"
	JobTypeFold          = "fold"
	JobTypeLookup        = "lookup"
	JobTypeCheck         = "check"
	JobTypeMerge         = "merge"
)

type JobState struct {
	Running    bool
	JobType    string // a JobType* constant
	Total      int
	Processed  int
	Message    string
	StartedAt  time.Time
	FinishedAt *time.Time
	Summary    string
	Partial    bool
	Error      string
	// Changes counts the gallery changes a sync or the watcher made while a
	// job runs; the client refreshes on it without touching the progress line.
	Changes int
	// The galleries the job writes to; none recorded stands for all of them.
	Galleries []string
}

type SearchResult struct {
	Page         int
	Limit        int
	Total        int
	TotalUnknown bool
	Results      []Image
}

type RowScanner interface {
	Scan(dest ...any) error
}

// Callers alias images as i, and the order must match ScanImageRow's
// positional Scan.
const ImageRowColumns = `i.id, i.sha256, i.md5, i.canonical_path, i.folder_path, i.file_type,
	        i.width, i.height, i.file_size, i.is_missing, i.is_favorited,
	        i.is_inbox, i.auto_tagged_at, i.source_type, i.origin, i.source, i.url, i.note, i.original_source,
	        i.page_count, i.last_read_page, i.duration_seconds, i.series, i.series_order, i.phash, i.ingested_at, i.upload_batch`

func ScanImageRow(row RowScanner) (Image, error) {
	var img Image
	var isMissing, isFav, isInbox int
	var autoTaggedAt *string
	var ingestedAt string
	if err := row.Scan(
		&img.ID, &img.SHA256, &img.MD5, &img.CanonicalPath, &img.FolderPath, &img.FileType,
		&img.Width, &img.Height, &img.FileSize, &isMissing, &isFav,
		&isInbox, &autoTaggedAt, &img.SourceType, &img.Origin, &img.Source, &img.URL, &img.Note, &img.OriginalSource,
		&img.PageCount, &img.LastReadPage, &img.DurationSec, &img.Series, &img.SeriesOrder, &img.Phash, &ingestedAt, &img.UploadBatch,
	); err != nil {
		return Image{}, err
	}
	img.IsMissing = isMissing == 1
	img.IsFavorited = isFav == 1
	img.IsInbox = isInbox == 1
	if autoTaggedAt != nil {
		t, _ := time.Parse(time.RFC3339, *autoTaggedAt)
		img.AutoTaggedAt = &t
	}
	img.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return img, nil
}

func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
