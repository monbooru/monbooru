package api

import (
	"cmp"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/apitoken"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// The booru layer answers like a read-only Danbooru server, so booru client
// apps can browse a gallery. Each gallery has its own address,
// /booru/{gallery}/{key}/, keyed by the one token Settings picks: the apps
// send no credentials when they load images or notes, so the key in the
// path is the only auth.

const (
	BooruPrefix = "/booru/"
	// Danbooru's own page sizes.
	booruPageLimit = 20
	booruMaxLimit  = 200
	booruSuggest   = 10
	booruMaxNames  = 1000
	// LoliSnatcher cuts the last six characters, the zone, before parsing,
	// so UTC is written +00:00 rather than Z.
	booruTimeLayout = "2006-01-02T15:04:05.000-07:00"
)

// BooruKey is derived from the stored hash, so the address lives as long as
// the token; the prefix keeps the key from being the hash itself.
func BooruKey(tokenHash string) string {
	sum := sha256.Sum256([]byte("monbooru-booru\x00" + tokenHash))
	return hex.EncodeToString(sum[:16])
}

// BooruToken says whether Settings may pick a token for the layer: a
// paired token belongs to an app, not to a person adding a booru.
func BooruToken(t apitoken.Token) bool { return t.Paired == "" && t.HasScope(config.ScopeRead) }

func BooruPath(gallery, key string) string { return BooruPrefix + gallery + "/" + key }

// IsBooruPath: the layer checks its own key, ahead of any login.
func IsBooruPath(path string) bool { return strings.HasPrefix(path, BooruPrefix) }

// IsBooruFile: an app's grid loads a thumbnail per post, as noisy as the
// web UI's own. The route is read after the key, so a gallery named file
// is not one.
func IsBooruFile(path string) bool {
	_, route, ok := splitBooruPath(path)
	if !ok {
		return false
	}
	switch first, _, _ := strings.Cut(route, "/"); first {
	case "file", "sample", "thumb":
		return true
	}
	return false
}

// RedactBooruPath keeps the key out of the request log.
func RedactBooruPath(path string) string {
	gallery, route, ok := splitBooruPath(path)
	if !ok {
		return path
	}
	return BooruPrefix + gallery + "/<key>/" + route
}

// splitBooruPath parts a layer path around its key.
func splitBooruPath(path string) (gallery, route string, ok bool) {
	rest, ok := strings.CutPrefix(path, BooruPrefix)
	if !ok {
		return "", "", false
	}
	gallery, rest, _ = strings.Cut(rest, "/")
	_, route, _ = strings.Cut(rest, "/")
	return gallery, route, true
}

// ChosenBooruToken is the token Settings picked for the layer, while it
// still can be one; nil keeps the layer closed.
func ChosenBooruToken(auth config.AuthConfig) *apitoken.Token {
	if auth.BooruTokenID == "" {
		return nil
	}
	if t := auth.Tokens.ByID(auth.BooruTokenID); t != nil && BooruToken(*t) {
		return t
	}
	return nil
}

func booruKeyValid(auth config.AuthConfig, key string) bool {
	t := ChosenBooruToken(auth)
	return t != nil && subtle.ConstantTimeCompare([]byte(BooruKey(t.TokenHash)), []byte(key)) == 1
}

type booruHandler func(http.ResponseWriter, *http.Request, *library.Gallery)

func (h *Handler) mountBooru(mux *http.ServeMux) {
	const at = "GET " + BooruPrefix + "{gallery}/{key}/"
	mux.HandleFunc(at+"posts.json", h.booru(h.booruPosts))
	mux.HandleFunc(at+"posts/{post}", h.booru(h.booruPost))
	mux.HandleFunc(at+"counts/posts.json", h.booru(h.booruCount))
	mux.HandleFunc(at+"autocomplete.json", h.booru(booruAutocomplete))
	mux.HandleFunc(at+"tags.json", h.booru(booruTags))
	mux.HandleFunc(at+"notes.json", h.booru(booruNotes))
	mux.HandleFunc(at+"file/{name}", h.booruFile("file", fileOf))
	mux.HandleFunc(at+"sample/{name}", h.booruFile("sample", booruSampleOf))
	mux.HandleFunc(at+"thumb/{name}", h.booruFile("thumb", thumbnailOf))
	mux.HandleFunc(at+"favicon.ico", h.booru(h.booruFavicon))
	mux.HandleFunc(at+"{rest...}", h.booru(booruEmpty))
}

// An app looks for the site's icon beside its address; it gets the one the
// web UI shows, which the theme picks.
func (h *Handler) booruFavicon(w http.ResponseWriter, r *http.Request, _ *library.Gallery) {
	if h.favicon == nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, h.favicon(), http.StatusFound)
}

// The layer takes the gallery lock itself, so its files can let go of it.
func (h *Handler) booru(next booruHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.lock != nil {
			defer h.lock()()
		}
		if g, ok := h.booruGallery(w, r); ok {
			next(w, r, g)
		}
	}
}

// The key is checked before the gallery, so a wrong key learns nothing
// about which galleries exist, and a gallery not turned on answers as one
// that does not.
func (h *Handler) booruGallery(w http.ResponseWriter, r *http.Request) (*library.Gallery, bool) {
	cfg := h.cfg()
	if !booruKeyValid(cfg.Auth, r.PathValue("key")) {
		contract.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid key")
		return nil, false
	}
	name := r.PathValue("gallery")
	g, ok := h.resolver(name)
	if gc := cfg.FindGallery(name); gc == nil || !gc.Booru {
		ok = false
	}
	if !ok {
		contract.WriteError(w, http.StatusNotFound, "not_found", "unknown gallery")
		return nil, false
	}
	return g, true
}

// The lock is let go before the first byte, as the web UI's files do: a
// gallery switch queued behind a phone's long video would stall every
// request after it. The extension is only there for the apps that read
// the type off the URL. The ETag is the web UI's: SQLite reuses the
// highest id once its row is deleted, so a URL can come back with new
// bytes.
func (h *Handler) booruFile(kind string, plan func(http.ResponseWriter, *library.Gallery, int64) (string, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, ok := func() (string, bool) {
			if h.lock != nil {
				defer h.lock()()
			}
			g, ok := h.booruGallery(w, r)
			if !ok {
				return "", false
			}
			stem, _, _ := strings.Cut(r.PathValue("name"), ".")
			id, err := strconv.ParseInt(stem, 10, 64)
			if err != nil || id <= 0 {
				contract.WriteError(w, http.StatusNotFound, "not_found", "image not found")
				return "", false
			}
			path, ok := plan(w, g, id)
			if ok {
				SetGalleryScopedCache(w, g.Name, kind+"-"+strconv.FormatInt(id, 10), path)
			}
			return path, ok
		}()
		if ok {
			http.ServeFile(w, r, path)
		}
	}
}

// The apps take the file URLs as they come, so they are absolute, on the
// host the app reached.
func (h *Handler) booruBase(r *http.Request) string {
	return ReachedOrigin(h.cfg().Server.BaseURL, r) + BooruPath(r.PathValue("gallery"), r.PathValue("key"))
}

func (h *Handler) booruPosts(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	q := r.URL.Query()
	// As on Danbooru, an md5 looks one post up and answers it alone.
	if md5 := strings.TrimSpace(q.Get("md5")); md5 != "" {
		h.booruOne(w, r, g, search.FilterExpr{Key: "md5", Val: md5}, "newest", false)
		return
	}
	bq := translateBooruQuery(q.Get("tags"), booruVocabularyOf(g))
	pageNum := q.Get("page")
	// Danbooru also pages by id, b123 before it and a123 after, in id
	// order whatever the search's; a page after comes back newest first.
	before, after := booruIDPage(pageNum)
	switch {
	case before != "":
		bq.text, bq.sort, bq.order, pageNum = strings.TrimSpace(bq.text+" id:<"+before), "id", "desc", "1"
	case after != "":
		bq.text, bq.sort, bq.order, pageNum = strings.TrimSpace(bq.text+" id:>"+after), "id", "asc", "1"
	}
	// As on Danbooru, the limit parameter wins over the limit: metatag.
	pageSize := q.Get("limit")
	if pageSize == "" && bq.limit > 0 {
		pageSize = strconv.Itoa(bq.limit)
	}
	page, limit := contract.PageParams(pageNum, pageSize, booruPageLimit, booruMaxLimit)
	sq := search.Query{
		Expr: search.Parse(bq.text), Sort: bq.sort, Order: bq.order, Page: page, Limit: limit, SkipCount: true,
	}
	if sq.Sort == "order" {
		sq.OrderCollection = search.PinnedCollectionName(sq.Expr)
	}
	result, err := search.Execute(r.Context(), h.searchSource(g), sq)
	if err != nil && r.Context().Err() != nil {
		return
	}
	if serverError(w, err) {
		return
	}
	images := result.Results
	// Danbooru pages through the posts random: drew, so the pages stop
	// after that many.
	if bq.random > 0 {
		images = images[:min(len(images), max(bq.random-(page-1)*limit, 0))]
	}
	if after != "" {
		slices.Reverse(images)
	}
	posts, err := booruPostsOf(h.booruBase(r), g, images)
	if serverError(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusOK, posts)
}

// booruIDPage reads page=b123 or a123 into the id it pages before or
// after.
func booruIDPage(page string) (before, after string) {
	if len(page) < 2 {
		return "", ""
	}
	if _, err := strconv.ParseInt(page[1:], 10, 64); err != nil {
		return "", ""
	}
	switch page[0] {
	case 'b':
		return page[1:], ""
	case 'a':
		return "", page[1:]
	}
	return "", ""
}

// Through the search, so a post the listing would hide stays hidden here.
// posts/random is one post of those its tags find, as on Danbooru. The
// post's page, without .json, answers a missing post empty rather than 404,
// like the routes the layer has no data for: one app tests a new site
// with posts/100, which a small gallery does not hold.
func (h *Handler) booruPost(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	name, isJSON := strings.CutSuffix(r.PathValue("post"), ".json")
	if name == "random" {
		bq := translateBooruQuery(r.URL.Query().Get("tags"), booruVocabularyOf(g))
		h.booruOne(w, r, g, search.Parse(bq.text), "random", false)
		return
	}
	id, err := strconv.ParseInt(name, 10, 64)
	if err != nil || id <= 0 {
		contract.WriteError(w, http.StatusNotFound, "not_found", "post not found")
		return
	}
	h.booruOne(w, r, g, search.FilterExpr{Key: "id", Val: strconv.FormatInt(id, 10)}, "newest", !isJSON)
}

// booruOne answers the first post the search finds, as an object; none is
// a 404, or an empty object when emptyIfNone.
func (h *Handler) booruOne(w http.ResponseWriter, r *http.Request, g *library.Gallery, expr search.Expr, sort string, emptyIfNone bool) {
	result, err := search.Execute(r.Context(), h.searchSource(g), search.Query{
		Expr: expr, Sort: sort, Page: 1, Limit: 1, SkipCount: true,
	})
	if err != nil && r.Context().Err() != nil {
		return
	}
	if serverError(w, err) {
		return
	}
	posts, err := booruPostsOf(h.booruBase(r), g, result.Results)
	if serverError(w, err) {
		return
	}
	switch {
	case len(posts) > 0:
		contract.WriteJSON(w, http.StatusOK, posts[0])
	case emptyIfNone:
		contract.WriteJSON(w, http.StatusOK, map[string]any{})
	default:
		contract.WriteError(w, http.StatusNotFound, "not_found", "post not found")
	}
}

func (h *Handler) booruCount(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	bq := translateBooruQuery(r.URL.Query().Get("tags"), booruVocabularyOf(g))
	n, err := search.Count(r.Context(), h.searchSource(g), search.Query{Expr: search.Parse(bq.text)})
	if err != nil && r.Context().Err() != nil {
		return
	}
	if serverError(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"counts": map[string]int{"posts": n}})
}

// Danbooru's category numbers. monbooru's other categories read as general,
// and rating tags go to the rating field instead of the tag strings.
var booruCategories = map[string]int{"general": 0, "artist": 1, "copyright": 3, "character": 4, "meta": 5}

// monbooru's ratings are Danbooru's spelled out, so the first letter is
// Danbooru's, mildest first.
var booruRatings = []string{"general", "sensitive", "questionable", "explicit"}

// The types are a real Danbooru post's: a client that casts strictly
// empties a post with a field of another type.
type booruPost struct {
	ID                  int64           `json:"id"`
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
	UploaderID          int             `json:"uploader_id"`
	ApproverID          *int            `json:"approver_id"`
	Score               int             `json:"score"`
	UpScore             int             `json:"up_score"`
	DownScore           int             `json:"down_score"`
	FavCount            int             `json:"fav_count"`
	Source              string          `json:"source"`
	MD5                 string          `json:"md5"`
	Rating              string          `json:"rating"`
	ImageWidth          int             `json:"image_width"`
	ImageHeight         int             `json:"image_height"`
	FileExt             string          `json:"file_ext"`
	FileSize            int64           `json:"file_size"`
	TagString           string          `json:"tag_string"`
	TagStringGeneral    string          `json:"tag_string_general"`
	TagStringArtist     string          `json:"tag_string_artist"`
	TagStringCharacter  string          `json:"tag_string_character"`
	TagStringCopyright  string          `json:"tag_string_copyright"`
	TagStringMeta       string          `json:"tag_string_meta"`
	TagCount            int             `json:"tag_count"`
	TagCountGeneral     int             `json:"tag_count_general"`
	TagCountArtist      int             `json:"tag_count_artist"`
	TagCountCharacter   int             `json:"tag_count_character"`
	TagCountCopyright   int             `json:"tag_count_copyright"`
	TagCountMeta        int             `json:"tag_count_meta"`
	ParentID            *int64          `json:"parent_id"`
	HasChildren         bool            `json:"has_children"`
	HasActiveChildren   bool            `json:"has_active_children"`
	HasVisibleChildren  bool            `json:"has_visible_children"`
	HasLarge            bool            `json:"has_large"`
	IsPending           bool            `json:"is_pending"`
	IsFlagged           bool            `json:"is_flagged"`
	IsDeleted           bool            `json:"is_deleted"`
	IsBanned            bool            `json:"is_banned"`
	PixivID             *int            `json:"pixiv_id"`
	BitFlags            int             `json:"bit_flags"`
	LastCommentBumpedAt *string         `json:"last_comment_bumped_at"`
	LastCommentedAt     *string         `json:"last_commented_at"`
	LastNotedAt         *string         `json:"last_noted_at"`
	FileURL             string          `json:"file_url"`
	LargeFileURL        string          `json:"large_file_url"`
	PreviewFileURL      string          `json:"preview_file_url"`
	MediaAsset          booruMediaAsset `json:"media_asset"`
}

// A video's length and the sized variants are only read from here.
type booruMediaAsset struct {
	ID          int64         `json:"id"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`
	MD5         string        `json:"md5"`
	FileExt     string        `json:"file_ext"`
	FileSize    int64         `json:"file_size"`
	ImageWidth  int           `json:"image_width"`
	ImageHeight int           `json:"image_height"`
	Duration    *booruSeconds `json:"duration"`
	Status      string        `json:"status"`
	FileKey     string        `json:"file_key"`
	IsPublic    bool          `json:"is_public"`
	PixelHash   string        `json:"pixel_hash"`
	// One app opens a post on its 720x720 variant and, with none listed,
	// on the thumbnail.
	Variants []booruVariant `json:"variants"`
}

type booruVariant struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FileExt string `json:"file_ext"`
}

// Danbooru's sizes, each served by the nearest monbooru has: the thumbnail
// for the small ones, the sample or the file for 720x720, but a video's
// thumbnail, as Danbooru's is a still.
func booruVariants(p booruPost, video bool) []booruVariant {
	w, h := p.ImageWidth, p.ImageHeight
	tw, th := gallery.ThumbSize(w, h)
	thumb := booruVariant{URL: p.PreviewFileURL, Width: tw, Height: th, FileExt: "jpg"}
	large := booruVariant{URL: p.FileURL, Width: w, Height: h, FileExt: p.FileExt}
	if p.HasLarge {
		sw, sh := gallery.ViewRenditionSize(w, h)
		large = booruVariant{URL: p.LargeFileURL, Width: sw, Height: sh, FileExt: "jpg"}
	}
	view := large
	if video {
		view = thumb
	}
	out := []booruVariant{thumb, thumb, view}
	out[0].Type, out[1].Type, out[2].Type = "180x180", "360x360", "720x720"
	if p.HasLarge {
		large.Type = "sample"
		out = append(out, large)
	}
	return append(out, booruVariant{Type: "original", URL: p.FileURL, Width: w, Height: h, FileExt: p.FileExt})
}

// booruSeconds always carries a decimal point: Go writes 5.0 as 5, which
// an app declaring the field a double reads as an int and fails the page.
type booruSeconds float64

func (s booruSeconds) MarshalJSON() ([]byte, error) {
	out := strconv.FormatFloat(float64(s), 'f', -1, 64)
	if !strings.Contains(out, ".") {
		out += ".0"
	}
	return []byte(out), nil
}

func booruPostsOf(base string, g *library.Gallery, images []models.Image) ([]booruPost, error) {
	ids := make([]int64, len(images))
	for i, img := range images {
		ids[i] = img.ID
	}
	tagsByID, err := loadTagsForImages(g, ids)
	if err != nil {
		return nil, err
	}
	noted, err := notedImages(g, ids)
	if err != nil {
		return nil, err
	}
	posts := make([]booruPost, len(images))
	for i, img := range images {
		posts[i] = booruPostOf(base, img, tagsByID[img.ID], noted[img.ID])
	}
	return posts, nil
}

func notedImages(g *library.Gallery, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	in, args := db.InPlaceholders(ids)
	err := db.QueryIDsFunc(g.DB.Read, func(id int64) bool { out[id] = true; return true },
		`SELECT DISTINCT image_id FROM image_annotations WHERE image_id IN (`+in+`)`, args...)
	return out, err
}

func booruPostOf(base string, img models.Image, imageTags []imageTagJSON, noted bool) booruPost {
	created := img.IngestedAt.Format(booruTimeLayout)
	id := strconv.FormatInt(img.ID, 10)
	ext := booruExt(img.FileType)
	p := booruPost{
		ID: img.ID, CreatedAt: created, UpdatedAt: created,
		Source: img.URL, MD5: img.MD5,
		ImageWidth: derefInt(img.Width), ImageHeight: derefInt(img.Height),
		FileExt: ext, FileSize: img.FileSize,
		FileURL:        base + "/file/" + id + "." + ext,
		PreviewFileURL: base + "/thumb/" + id + ".jpg",
	}
	p.LargeFileURL = p.FileURL
	if booruSampled(img.FileType, p.ImageWidth, p.ImageHeight) {
		p.LargeFileURL, p.HasLarge = base+"/sample/"+id+".jpg", true
	}
	if noted {
		p.LastNotedAt = &created
	}
	p.MediaAsset = booruMediaAsset{
		ID: img.ID, CreatedAt: created, UpdatedAt: created, MD5: img.MD5, FileExt: ext, FileSize: img.FileSize,
		ImageWidth: p.ImageWidth, ImageHeight: p.ImageHeight, Duration: (*booruSeconds)(img.DurationSec), Status: "active", IsPublic: true,
		Variants: booruVariants(p, gallery.IsVideoType(img.FileType)),
	}

	byCategory := map[int][]string{}
	var all []string
	rating := -1
	for _, t := range imageTags {
		if t.Category == "rating" {
			// The highest rating wins, as monbooru's own rating: filter reads it.
			rating = max(rating, slices.Index(booruRatings, t.Name))
			continue
		}
		c := booruCategories[t.Category]
		byCategory[c] = append(byCategory[c], t.Name)
		all = append(all, t.Name)
	}
	if rating >= 0 {
		p.Rating = booruRatings[rating][:1]
	}
	slices.Sort(all)
	p.TagString, p.TagCount = strings.Join(all, " "), len(all)
	for c, s := range map[int]*string{0: &p.TagStringGeneral, 1: &p.TagStringArtist, 3: &p.TagStringCopyright, 4: &p.TagStringCharacter, 5: &p.TagStringMeta} {
		*s = strings.Join(byCategory[c], " ")
	}
	p.TagCountGeneral, p.TagCountArtist = len(byCategory[0]), len(byCategory[1])
	p.TagCountCopyright, p.TagCountCharacter, p.TagCountMeta = len(byCategory[3]), len(byCategory[4]), len(byCategory[5])
	return p
}

func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// An archive is a zip, the extension the apps know to skip for the sample.
func booruExt(fileType string) string {
	if fileType == models.FileTypeCBZ {
		return "zip"
	}
	if ft, ok := models.LookupFileType(fileType); ok && len(ft.Exts) > 0 {
		return strings.TrimPrefix(ft.Exts[0], ".")
	}
	return fileType
}

// A sample is what an app can show of a file it cannot: the first page of
// an archive, or the web UI's own cached JPEG of a format phones do not
// decode or of an image too large to.
func booruSampled(fileType string, width, height int) bool {
	return fileType == models.FileTypeCBZ || gallery.IsFFmpegStill(fileType) ||
		!gallery.IsVideoType(fileType) && gallery.NeedsViewRendition(width, height)
}

func booruSampleOf(w http.ResponseWriter, g *library.Gallery, id int64) (string, bool) {
	canonPath, fileType, ok := containedCanonical(w, g, id)
	if !ok {
		return "", false
	}
	if fileType == models.FileTypeCBZ {
		page, err := gallery.EnsureMangaPage(g.ThumbnailsPath, canonPath, id, 1)
		if err != nil {
			contract.WriteError(w, http.StatusNotFound, "not_found", "page not found")
			return "", false
		}
		return page, true
	}
	var width, height int
	err := g.DB.Read.QueryRow(`SELECT COALESCE(width, 0), COALESCE(height, 0) FROM images WHERE id = ?`, id).Scan(&width, &height)
	if serverError(w, err) {
		return "", false
	}
	if !booruSampled(fileType, width, height) {
		return fileOf(w, g, id)
	}
	// As the web UI does, a rendition that fails falls back to the file.
	rendition, err := gallery.ViewRenditionFor(canonPath, g.ThumbnailsPath, id, fileType, width, height)
	if err != nil {
		logx.Warnf("booru sample for image %d: %v", id, err)
		return fileOf(w, g, id)
	}
	w.Header().Set("Content-Type", "image/jpeg")
	return rendition, true
}

type booruAutocompleteJSON struct {
	Type      string `json:"type"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	Category  int    `json:"category"`
	PostCount int    `json:"post_count"`
}

// One app wraps the term in wildcards; the suggestions already take the
// names starting with it first, then those containing it.
func booruAutocomplete(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	q := r.URL.Query()
	out := []booruAutocompleteJSON{}
	term := strings.Trim(q.Get("search[query]"), "*")
	if term == "" {
		contract.WriteJSON(w, http.StatusOK, out)
		return
	}
	_, limit := contract.PageParams("", q.Get("limit"), booruSuggest, booruMaxLimit)
	found, err := g.TagSvc.SuggestTags(term, "", limit)
	if serverError(w, err) {
		return
	}
	for _, t := range found {
		if t.CategoryName == "rating" {
			continue
		}
		out = append(out, booruAutocompleteJSON{
			Type: "tag", Label: strings.ReplaceAll(t.Name, "_", " "), Value: t.Name,
			Category: booruCategories[t.CategoryName], PostCount: t.UsageCount,
		})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

type booruTagJSON struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	PostCount    int      `json:"post_count"`
	Category     int      `json:"category"`
	CreatedAt    *string  `json:"created_at"`
	UpdatedAt    *string  `json:"updated_at"`
	IsDeprecated bool     `json:"is_deprecated"`
	Words        []string `json:"words"`
}

// The apps look tags up by name, for their categories and counts, and list
// them, the popular ones first under order=count. name_matches is a glob,
// so one without a * is a name.
func booruTags(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	q := r.URL.Query()
	order := q.Get("search[order]")
	pattern := tags.NormalizeTagName(q.Get("search[name_matches]"))
	listed := q.Get("search[name_comma]") + "," + q.Get("search[name]")
	if !strings.Contains(pattern, "*") {
		listed += "," + pattern
	}
	// Capped while collecting, so a long list costs no more than the cap.
	var names []string
	for _, n := range strings.Split(listed, ",") {
		if len(names) == booruMaxNames {
			break
		}
		if n = tags.NormalizeTagName(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var found []models.Tag
	var err error
	if len(names) > 0 {
		found, err = g.TagSvc.NamedTags(names)
		slices.SortStableFunc(found, booruTagOrder(order))
	} else {
		page, limit := contract.PageParams(q.Get("page"), q.Get("limit"), booruSuggest, booruMaxLimit)
		found, _, err = g.TagSvc.ListTags(tags.TagFilter{
			Prefix: pattern, Type: "tag", Sort: booruTagSort(order), PageIndex: page - 1, Limit: limit, SkipCount: true,
		})
	}
	if serverError(w, err) {
		return
	}
	// Danbooru's names are unique; of monbooru's same name in several
	// categories, the most used answers.
	best := map[string]models.Tag{}
	for _, t := range found {
		if b, ok := best[t.Name]; t.CategoryName != "rating" && (!ok || t.UsageCount > b.UsageCount) {
			best[t.Name] = t
		}
	}
	out := []booruTagJSON{}
	for _, t := range found {
		if best[t.Name].ID != t.ID {
			continue
		}
		out = append(out, booruTagJSON{
			ID: t.ID, Name: t.Name, PostCount: t.UsageCount, Category: booruCategories[t.CategoryName],
			Words: strings.Split(t.Name, "_"),
		})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

// Danbooru's tag orders: count is the most used first, name A to Z, and
// date, the default, the newest first.
func booruTagSort(order string) string {
	switch order {
	case "count":
		return "usage"
	case "name":
		return "name"
	}
	return "created"
}

// booruTagOrder sorts tags looked up by name in Danbooru's order, date by
// id as Danbooru's is.
func booruTagOrder(order string) func(a, b models.Tag) int {
	return func(a, b models.Tag) int {
		switch booruTagSort(order) {
		case "usage":
			return cmp.Or(b.UsageCount-a.UsageCount, strings.Compare(a.Name, b.Name))
		case "name":
			return strings.Compare(a.Name, b.Name)
		}
		return cmp.Compare(b.ID, a.ID)
	}
}

type booruNoteJSON struct {
	ID        int64   `json:"id"`
	CreatedAt *string `json:"created_at"`
	UpdatedAt *string `json:"updated_at"`
	X         int     `json:"x"`
	Y         int     `json:"y"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	IsActive  bool    `json:"is_active"`
	PostID    int64   `json:"post_id"`
	Body      string  `json:"body"`
	Version   int     `json:"version"`
}

func booruNotes(w http.ResponseWriter, r *http.Request, g *library.Gallery) {
	out := []booruNoteJSON{}
	id, err := strconv.ParseInt(r.URL.Query().Get("search[post_id]"), 10, 64)
	if err != nil || id <= 0 {
		contract.WriteJSON(w, http.StatusOK, out)
		return
	}
	notes, err := gallery.AnnotationsForImage(g.DB, id)
	if serverError(w, err) {
		return
	}
	for _, n := range notes {
		out = append(out, booruNoteJSON{
			ID: n.ID, X: n.X, Y: n.Y, Width: n.W, Height: n.H, IsActive: true, PostID: id, Body: n.Body, Version: 1,
		})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

// Every other Danbooru route answers empty, a list or the object a single
// record's route returns, so the apps' other screens stay empty instead of
// failing.
func booruEmpty(w http.ResponseWriter, r *http.Request, _ *library.Gallery) {
	route, ok := strings.CutSuffix(r.PathValue("rest"), ".json")
	if !ok {
		contract.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if booruObjectRoute(route) {
		contract.WriteJSON(w, http.StatusOK, map[string]any{})
		return
	}
	contract.WriteJSON(w, http.StatusOK, []any{})
}

func booruObjectRoute(route string) bool {
	last := route[strings.LastIndexByte(route, '/')+1:]
	if _, err := strconv.ParseInt(last, 10, 64); err == nil {
		return true
	}
	switch route {
	case "profile", "source", "related_tag":
		return true
	}
	return strings.HasPrefix(route, "wiki_pages/") || strings.HasPrefix(route, "counts/")
}

type booruQuery struct {
	text, sort, order string
	// The limit: metatag's page size and random:'s post count, 0 when
	// none is given.
	limit, random int
}

// A Danbooru metatag monbooru spells differently. read takes one value of
// a comma list, which becomes any of them; nil passes the value whole. An
// empty key drops a metatag whose monbooru namesake means something else.
type booruRewrite struct {
	key  string
	read func(string) string
}

var booruRewrites = map[string]booruRewrite{
	"rating":   {"rating", booruRating},
	"id":       {"id", booruID},
	"filetype": {"mime", booruFileType},
	"filesize": {"size", nil},
	"source":   {"sourceurl", nil},
	"ratio":    {"ratio", booruRatio},
	"status":   {"id", booruStatus},
	"age":      {"date", func(v string) string { return booruAgeAt(v, time.Now()) }},
	// A user's favorites on Danbooru; a gallery keeps one set, whoever is
	// named. monbooru keeps no time a post was favorited, so ordfav lists
	// them newest first.
	"fav":    {"fav", booruFavorite},
	"ordfav": {"fav", booruFavorite},
}

func booruFavorite(string) string { return "true" }

// booruNone is an id no post has, for a metatag that finds nothing here.
const booruNone = "0"

// What monbooru itself reads in a key: term, so nothing Danbooru adds
// needs listing here.
type booruVocabulary struct {
	// A filter or a category, which Danbooru reads alike.
	known func(key string) bool
	// A tag whose name holds a colon, such as re:zero.
	tag func(name string) bool
	// The collection a Danbooru pool name means, as stored; the name
	// itself when none matches.
	collection func(name string) string
}

func booruVocabularyOf(g *library.Gallery) booruVocabulary {
	return booruVocabulary{
		known: func(key string) bool {
			if search.IsFilterKey(key) {
				return true
			}
			_, ok, err := tags.CategoryIDByName(g.DB, key)
			return err == nil && ok
		},
		tag: func(name string) bool {
			found, err := g.TagSvc.NamedTags([]string{name})
			return err == nil && len(found) > 0
		},
		// Danbooru writes a pool name's spaces as underscores.
		collection: func(name string) string {
			for _, n := range []string{name, strings.ReplaceAll(name, "_", " ")} {
				var found sql.NullString
				if err := g.DB.Read.QueryRow(`SELECT MIN(name) FROM image_collections WHERE name = ?`, n).Scan(&found); err == nil && found.Valid {
					return found.String
				}
			}
			return name
		},
	}
}

// translateBooruQuery rewrites a Danbooru search into monbooru's: the ~
// terms form one OR group, order: becomes the sort, the rewrites take
// monbooru's spelling, and a key: that monbooru reads neither as a filter,
// a category nor a tag is a metatag it has nothing for, so it is dropped
// rather than left to match nothing.
func translateBooruQuery(raw string, vocab booruVocabulary) booruQuery {
	q := booruQuery{sort: "newest", order: "desc"}
	var all, anyOf []string
	for _, tok := range strings.Fields(raw) {
		if alt, ok := strings.CutPrefix(tok, "~"); ok && alt != "" {
			if t := q.term(alt, vocab); t != "" {
				anyOf = append(anyOf, t)
			}
			continue
		}
		if t := q.term(tok, vocab); t != "" {
			all = append(all, t)
		}
	}
	if len(anyOf) > 0 {
		all = append(all, booruGroup(anyOf))
	}
	q.text = strings.Join(all, " ")
	return q
}

// term rewrites one Danbooru term; "" drops it.
func (q *booruQuery) term(tok string, vocab booruVocabulary) string {
	body, neg := strings.CutPrefix(tok, "-")
	// A wildcard alone means any post on Danbooru, and one app tests a site
	// with tags=*; monbooru's parser reads it as a tag matching nothing.
	if body != "" && strings.Trim(body, "*") == "" {
		return ""
	}
	key, val, meta := strings.Cut(body, ":")
	if body == "" || !meta {
		return tok
	}
	key = strings.ToLower(key)
	switch key {
	case "order":
		// ordpool's order wins over order:, as on Danbooru.
		if !neg && q.sort != "order" {
			q.sort, q.order = booruOrder(strings.ToLower(val))
		}
		return ""
	case "pool", "ordpool":
		// A pool is a collection; ordpool lists it in its own order.
		out := booruPool(val, vocab)
		if key == "ordpool" && !neg && strings.HasPrefix(out, `collection:"`) {
			q.sort, q.order = "order", search.DefaultOrder("order")
		}
		if neg {
			// -pool:none is in some pool.
			return strings.TrimPrefix("-"+out, "--")
		}
		return out
	case "limit":
		if n, err := strconv.Atoi(val); err == nil && n > 0 && !neg {
			q.limit = n
		}
		return ""
	case "random":
		// N posts drawn at random from those the search finds.
		if n, err := strconv.Atoi(val); err == nil && n > 0 && !neg {
			q.random = n
			q.sort, q.order = "random", "desc"
		}
		return ""
	}
	out := body
	rw, rewritten := booruRewrites[key]
	switch {
	case rewritten && rw.key == "":
		return ""
	case rewritten && rw.read == nil:
		out = rw.key + ":" + val
	case rewritten:
		out = booruValues(rw.key+":", val, rw.read)
	case !vocab.known(key) && !vocab.tag(tags.NormalizeTagName(body)):
		return ""
	}
	if out == "" || !neg {
		return out
	}
	return "-" + out
}

func booruOrder(v string) (sort, order string) {
	switch v {
	case "id", "id_asc", "created_at_asc":
		return "newest", "asc"
	case "filesize", "filesize_desc":
		return "filesize", "desc"
	case "filesize_asc":
		return "filesize", "asc"
	case "random":
		return "random", "desc"
	}
	return "newest", "desc"
}

// Danbooru's pool:any, series and collection are any pool; pool:none is in
// none.
func booruPool(name string, vocab booruVocabulary) string {
	switch strings.ToLower(name) {
	case "any", "series", "collection":
		return "collection:any"
	case "none":
		return "-collection:any"
	}
	return `collection:"` + search.QuoteValue(vocab.collection(name)) + `"`
}

// Danbooru reads a rating by its first letter, so rating:safe is s.
func booruRating(v string) string {
	for _, r := range booruRatings {
		if v != "" && strings.EqualFold(v[:1], r[:1]) {
			return r
		}
	}
	return ""
}

// An id, a comparison or a lo..hi range, either end open, read alike by
// both; anything else is dropped.
func booruID(v string) string {
	num := strings.TrimLeft(v, "<>=")
	if !slices.Contains([]string{"", "<", ">", "<=", ">=", "="}, v[:len(v)-len(num)]) {
		return ""
	}
	ends := []string{num}
	if from, to, ok := strings.Cut(num, ".."); ok {
		if num != v || from == "" && to == "" {
			return ""
		}
		ends = []string{from, to}
	}
	for _, end := range ends {
		if end == "" && len(ends) == 2 {
			continue
		}
		if n, err := strconv.ParseInt(end, 10, 64); err != nil || n <= 0 {
			return ""
		}
	}
	return v
}

// monbooru deletes a post outright and has no moderation queue, so every
// post is active and any other status finds none, as an unknown one does
// on Danbooru.
func booruStatus(v string) string {
	switch strings.ToLower(v) {
	case "active", "any", "all":
		return ""
	}
	return booruNone
}

// Danbooru's age: is how long before now a post was made, so each bound
// becomes that moment and the operators turn around: age:<1w is
// date:>(a week ago). A bare age is the whole day it falls on. The moment
// keeps now's time zone, which the date filter reads its values in.
func booruAgeAt(v string, now time.Time) string {
	at := func(s string) string {
		d, ok := booruDuration(s)
		if !ok {
			return ""
		}
		return now.Add(-d).Format("2006-01-02T15:04:05")
	}
	if from, to, ok := strings.Cut(v, ".."); ok {
		a, b := at(from), at(to)
		switch {
		case from == "" && b != "":
			return ">=" + b
		case to == "" && a != "":
			return "<=" + a
		case a != "" && b != "":
			return min(a, b) + ".." + max(a, b)
		}
		return ""
	}
	for _, op := range []struct{ age, date string }{{"<=", ">="}, {">=", "<="}, {"<", ">"}, {">", "<"}} {
		if rest, ok := strings.CutPrefix(v, op.age); ok {
			if t := at(rest); t != "" {
				return op.date + t
			}
			return ""
		}
	}
	d, ok := booruDuration(v)
	if !ok {
		return ""
	}
	return now.Add(-d).Format(time.DateOnly)
}

// Danbooru's units, smallest first; a month and a year are averages.
var booruDurationUnits = []struct {
	name string
	size time.Duration
}{
	{"seconds", time.Second},
	{"minutes", time.Minute},
	{"hours", time.Hour},
	{"days", 24 * time.Hour},
	{"weeks", 7 * 24 * time.Hour},
	{"months", 8766 * time.Hour / 12},
	{"years", 8766 * time.Hour},
}

// booruDuration reads a duration as Danbooru does: numbers each followed by
// a unit, largest first (1y2mo). A unit may be cut to any prefix that
// names only it, m is minutes, and a number without one is seconds.
func booruDuration(s string) (time.Duration, bool) {
	var total time.Duration
	prev := len(booruDurationUnits)
	for s != "" {
		num := strings.TrimLeft(s, "0123456789.")
		n, err := strconv.ParseFloat(s[:len(s)-len(num)], 64)
		rest := strings.TrimLeftFunc(num, unicode.IsLetter)
		unit := booruDurationUnit(strings.ToLower(num[:len(num)-len(rest)]))
		if err != nil || unit < 0 || unit >= prev {
			return 0, false
		}
		part := n * float64(booruDurationUnits[unit].size)
		if part > float64(1<<62) {
			return 0, false
		}
		total += time.Duration(part)
		prev, s = unit, rest
	}
	return total, prev < len(booruDurationUnits)
}

func booruDurationUnit(abbr string) int {
	switch abbr {
	case "":
		return 0
	case "m":
		return 1
	case "hr":
		return 2
	case "wk":
		return 4
	case "yr":
		return 6
	}
	found := -1
	for i, u := range booruDurationUnits {
		if strings.HasPrefix(u.name, abbr) {
			if found >= 0 {
				return -1
			}
			found = i
		}
	}
	return found
}

// Danbooru names a file type by its extension. One monbooru does not know
// is kept, so the filter matches nothing as Danbooru's would.
func booruFileType(v string) string {
	if ft := models.ExtFileType("." + strings.ToLower(v)); ft != "" {
		return ft
	}
	return v
}

// Danbooru writes a ratio as 16:9, monbooru as the number; the operators
// and ranges around it read alike, so each end is read as the search reads
// a comparison.
func booruRatio(v string) string {
	if from, to, ok := strings.Cut(v, ".."); ok {
		return booruRatioEnd(from) + ".." + booruRatioEnd(to)
	}
	return booruRatioEnd(v)
}

// A value that is not a:b is kept for the search to judge.
func booruRatioEnd(v string) string {
	num := strings.TrimLeft(v, "<>=")
	w, h, ok := strings.Cut(num, ":")
	if !ok {
		return v
	}
	x, errW := strconv.ParseFloat(w, 64)
	y, errH := strconv.ParseFloat(h, 64)
	if errW != nil || errH != nil || y == 0 {
		return v
	}
	return v[:len(v)-len(num)] + strconv.FormatFloat(x/y, 'g', -1, 64)
}

// A comma list is any of its values.
func booruValues(prefix, val string, read func(string) string) string {
	var terms []string
	for _, v := range strings.Split(val, ",") {
		if v = read(strings.TrimSpace(v)); v != "" && !slices.Contains(terms, prefix+v) {
			terms = append(terms, prefix+v)
		}
	}
	return booruGroup(terms)
}

func booruGroup(terms []string) string {
	switch len(terms) {
	case 0:
		return ""
	case 1:
		return terms[0]
	}
	return "( " + strings.Join(terms, " OR ") + " )"
}
