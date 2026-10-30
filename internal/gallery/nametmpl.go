package gallery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/shared/fsx"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// maxNameBytes leaves room under the usual 255-byte filename limit for an
// extension and a collision counter.
const maxNameBytes = 180

// Scope is the call site a template is parsed for; it decides which
// separators and tokens are allowed.
type Scope int

const (
	ScopeRename Scope = iota
	ScopeRenameBatch
	ScopeMove
	ScopeMoveBatch
	ScopeUploadFolder
	ScopeUploadName
)

func (s Scope) Folder() bool { return s == ScopeMove || s == ScopeMoveBatch || s == ScopeUploadFolder }

func (s Scope) sequence() bool { return s == ScopeRenameBatch || s == ScopeMoveBatch }

func (s Scope) source() bool { return s == ScopeUploadFolder || s == ScopeUploadName }

type nameToken int

const (
	tokLiteral nameToken = iota
	tokName
	tokExt
	tokType
	tokID
	tokHash
	tokMD5
	tokGallery
	tokDate
	tokYear
	tokMonth
	tokDay
	tokTime
	tokImgWidth
	tokImgHeight
	tokSize
	tokOrigin
	tokFolder
	tokSeq
	tokSource
	tokPostID
)

var nameTokens = map[string]nameToken{
	"name": tokName, "ext": tokExt, "type": tokType, "id": tokID,
	"hash": tokHash, "md5": tokMD5, "gallery": tokGallery, "date": tokDate,
	"year": tokYear, "month": tokMonth, "day": tokDay, "time": tokTime,
	"w": tokImgWidth, "h": tokImgHeight, "size": tokSize, "origin": tokOrigin,
	"folder": tokFolder, "n": tokSeq, "source": tokSource, "post_id": tokPostID,
}

var refusedNameTokens = map[string]string{
	"tag":        "tags are not known at ingest time",
	"artist":     "tags are not known at ingest time",
	"collection": "a collection membership is not a fact about the file",
	"rating":     "a rating is a tag by another name",
	"inbox":      "the inbox is a triage state, not an identity",
}

type namePart struct {
	lit   string
	tok   nameToken
	width int
}

type NameTemplate struct {
	parts  []namePart
	scope  Scope
	tokens bool
}

// ParseNameTemplate compiles a blank template to nil, which every caller
// reads as "leave the name alone".
func ParseNameTemplate(s string, sc Scope) (*NameTemplate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t := &NameTemplate{scope: sc}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, namePart{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			lit.WriteByte('{')
			i += 2
		case strings.HasPrefix(s[i:], "}}"):
			lit.WriteByte('}')
			i += 2
		case s[i] == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed { in the template")
			}
			part, err := parseNameToken(s[i+1:i+end], sc)
			if err != nil {
				return nil, err
			}
			flush()
			t.parts = append(t.parts, part)
			t.tokens = true
			i += end + 1
		case s[i] == '/' && !sc.Folder():
			return nil, errors.New("a name carries no folder - set that in the folder field")
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return t, nil
}

func parseNameToken(inner string, sc Scope) (namePart, error) {
	name, arg, hasArg := strings.Cut(inner, ":")
	name = strings.TrimSpace(name)
	tok, known := nameTokens[name]
	if !known {
		if why, refused := refusedNameTokens[name]; refused {
			return namePart{}, fmt.Errorf("unknown token {%s}: %s", inner, why)
		}
		return namePart{}, fmt.Errorf("unknown token {%s}", inner)
	}
	if tok == tokSeq && !sc.sequence() {
		return namePart{}, fmt.Errorf("{n} needs a batch - use {id} to name one file")
	}
	if tok == tokFolder && !sc.Folder() {
		return namePart{}, fmt.Errorf("{folder} is a destination - put it in the folder field")
	}
	if (tok == tokSource || tok == tokPostID) && !sc.source() {
		return namePart{}, fmt.Errorf("{%s} is only available for received files", name)
	}
	p := namePart{tok: tok}
	if hasArg {
		n, convErr := strconv.Atoi(strings.TrimSpace(arg))
		switch tok {
		case tokHash:
			if convErr != nil || n < 4 || n > 64 {
				return namePart{}, fmt.Errorf("{hash:N} takes a width between 4 and 64")
			}
		case tokSeq:
			if convErr != nil || n < 1 || n > 12 {
				return namePart{}, fmt.Errorf("{n:N} takes a width between 1 and 12")
			}
		default:
			return namePart{}, fmt.Errorf("{%s} takes no width", name)
		}
		p.width = n
	}
	return p, nil
}

func (t *NameTemplate) HasTokens() bool { return t != nil && t.tokens }

func readsMD5(tmpls []*NameTemplate) bool {
	return slices.ContainsFunc(tmpls, func(t *NameTemplate) bool { return t.uses(tokMD5) })
}

func (t *NameTemplate) Uses(token string) bool {
	tok, known := nameTokens[token]
	return known && t.uses(tok)
}

func (t *NameTemplate) uses(tok nameToken) bool {
	return t != nil && slices.ContainsFunc(t.parts, func(p namePart) bool { return p.tok == tok })
}

type NameFacts struct {
	Name string
	// Ext is lower-cased so {name}.{ext} normalises the extension; Base
	// keeps the on-disk spelling a rename starts from.
	Ext        string
	Base       string
	Folder     string
	Type       string
	Gallery    string
	ID         int64
	SHA256     string
	MD5        string
	IngestedAt time.Time
	Width      int
	Height     int
	Size       int64
	Origin     string
	Source     string
	PostID     string
	N          int
	NWidth     int
}

// LoadNameFacts leaves Source, PostID and the batch position to the
// caller. A template reading {md5} computes and stores an empty one, unless
// the file is over md5Cap (0: no cap), when {md5} renders empty.
func LoadNameFacts(ctx context.Context, database *db.DB, galleryName string, id int64, md5Cap int64, tmpls ...*NameTemplate) (f NameFacts, stored bool, err error) {
	f = NameFacts{ID: id, Gallery: galleryName}
	var canonical, ingestedAt string
	if err := database.Read.QueryRowContext(ctx,
		`SELECT canonical_path, folder_path, file_type, sha256, md5, origin, file_size,
		        COALESCE(width, 0), COALESCE(height, 0), ingested_at
		 FROM images WHERE id = ?`, id,
	).Scan(&canonical, &f.Folder, &f.Type, &f.SHA256, &f.MD5, &f.Origin, &f.Size, &f.Width, &f.Height, &ingestedAt); err != nil {
		return f, false, fmt.Errorf("name facts for image %d: %w", id, err)
	}
	ext := filepath.Ext(canonical)
	f.Base = filepath.Base(canonical)
	f.Name = strings.TrimSuffix(f.Base, ext)
	f.Ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	f.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	if f.MD5 == "" && readsMD5(tmpls) && (md5Cap <= 0 || f.Size <= md5Cap) {
		sum, err := ComputeAndStoreMD5(ctx, database, id)
		if err != nil {
			return f, false, fmt.Errorf("md5 for image %d: %w", id, err)
		}
		f.MD5 = sum
		stored = true
	}
	return f, stored, nil
}

// Render falls back to the root or the id when an arriving file renders
// empty; a rename or move refuses rather than file a scope under ids or
// flatten it.
func (t *NameTemplate) Render(f NameFacts) (string, error) {
	out := t.render(f)
	switch {
	case out != "":
		return out, nil
	// A {folder} template may render the root: that is where a root row
	// already is.
	case t.scope.Folder() && (t.scope.source() || t.uses(tokFolder)):
		return "", nil
	case t.scope.source():
		return strconv.FormatInt(f.ID, 10), nil
	default:
		return "", fmt.Errorf("the template names nothing for image %d", f.ID)
	}
}

func (t *NameTemplate) render(f NameFacts) string {
	var b []byte
	for _, p := range t.parts {
		v := p.lit
		if p.tok != tokLiteral {
			// The row's own directory is already root-bounded, and
			// tidyNamePath cleans each of its segments.
			if v = p.value(f); p.tok != tokFolder {
				v = fsx.SanitizeFilename(v)
			}
			if v == "" {
				// The separator before an empty token goes with it:
				// "{name} - {source}" without a source is the name.
				b = trimSeparatorSuffix(b)
				continue
			}
		}
		b = append(b, v...)
	}
	return tidyNamePath(string(b))
}

// identityTokens tell rows apart; a date, a type or a gallery groups them
// however many values a library holds.
var identityTokens = map[nameToken]bool{
	tokName: true, tokID: true, tokHash: true, tokMD5: true, tokSeq: true,
}

func (t *NameTemplate) PerImage() bool {
	return t != nil && slices.ContainsFunc(t.parts, func(p namePart) bool { return identityTokens[p.tok] })
}

func (p namePart) value(f NameFacts) string {
	switch p.tok {
	case tokName:
		return f.Name
	case tokExt:
		return f.Ext
	case tokType:
		return f.Type
	case tokID:
		return strconv.FormatInt(f.ID, 10)
	case tokHash:
		if p.width > 0 && p.width < len(f.SHA256) {
			return f.SHA256[:p.width]
		}
		return f.SHA256
	case tokMD5:
		return f.MD5
	case tokGallery:
		return f.Gallery
	case tokDate:
		return f.IngestedAt.Format("2006-01-02")
	case tokYear:
		return f.IngestedAt.Format("2006")
	case tokMonth:
		return f.IngestedAt.Format("01")
	case tokDay:
		return f.IngestedAt.Format("02")
	case tokTime:
		return f.IngestedAt.Format("150405")
	case tokImgWidth:
		return sizeOrEmpty(f.Width)
	case tokImgHeight:
		return sizeOrEmpty(f.Height)
	case tokSize:
		return strconv.FormatInt(f.Size, 10)
	case tokOrigin:
		return f.Origin
	case tokFolder:
		return f.Folder
	case tokSeq:
		width := cmp.Or(p.width, f.NWidth)
		return fmt.Sprintf("%0*d", max(width, 1), f.N)
	case tokSource:
		return f.Source
	case tokPostID:
		return f.PostID
	}
	return ""
}

func sizeOrEmpty(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

// Dropping empty segments also keeps out a leading slash, which the
// resolver would read as absolute.
func tidyNamePath(s string) string {
	segs := strings.Split(s, "/")
	kept := segs[:0]
	for _, seg := range segs {
		// Literal text reaches the path too, so each segment is
		// sanitized, not only the tokens.
		seg = strings.Trim(fsx.SanitizeFilename(seg), "-_ ")
		if seg = TruncateFilename(seg, maxNameBytes); seg != "" {
			kept = append(kept, seg)
		}
	}
	return strings.Join(kept, "/")
}

func trimSeparatorSuffix(b []byte) []byte {
	return []byte(strings.TrimRight(string(b), "-_ "))
}

// A browser percent-encodes these in a multipart filename (WHATWG form encoding).
var formFilenameEscapes = strings.NewReplacer("%22", "\"", "%0A", "\n", "%0D", "\r")

// SanitizeReceivedName keeps a sender's leading dots, which the ignore list
// matches dotfiles by.
func SanitizeReceivedName(s string) string {
	return strings.TrimRight(fsx.FoldFilename(formFilenameEscapes.Replace(s)), " .")
}

func TruncateFilename(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Trim(s[:cut], " .")
}

type Naming struct {
	Folder  *NameTemplate
	Name    *NameTemplate
	Gallery string
}

func (n Naming) Empty() bool { return n.Folder == nil && n.Name == nil }

// Apply runs on an ingested row; site and postID are a push's origin,
// empty for every other caller.
func (n Naming) Apply(ctx context.Context, database *db.DB, b *Boundary, id int64, site, postID string) (string, error) {
	if n.Empty() {
		return "", nil
	}
	facts, _, err := LoadNameFacts(ctx, database, n.Gallery, id, 0, n.Folder, n.Name)
	if err != nil {
		return "", err
	}
	facts.Source, facts.PostID = site, postID
	var folder, name *string
	if n.Folder != nil {
		rendered, renderErr := n.Folder.Render(facts)
		if renderErr != nil {
			return "", renderErr
		}
		folder = &rendered
	}
	if n.Name != nil {
		rendered, renderErr := n.Name.Render(facts)
		if renderErr != nil {
			return "", renderErr
		}
		name = &rendered
	}
	res, err := PlaceImage(database, b, id, folder, name)
	if err != nil {
		return "", err
	}
	return res.NewCanonicalPath, nil
}

// Existing renders n for a row already in the gallery. A half that renders
// empty stays nil instead of falling back as an arrival does: the row
// already has a name and a folder.
func (n Naming) Existing(f NameFacts) (folder, name *string) {
	half := func(t *NameTemplate) *string {
		if t == nil {
			return nil
		}
		if out := t.render(f); out != "" {
			return &out
		}
		return nil
	}
	return half(n.Folder), half(n.Name)
}

func (n Naming) ApplyExisting(ctx context.Context, database *db.DB, b *Boundary, id int64) (string, error) {
	if n.Empty() {
		return "", nil
	}
	facts, _, err := LoadNameFacts(ctx, database, n.Gallery, id, 0, n.Folder, n.Name)
	if err != nil {
		return "", err
	}
	folder, name := n.Existing(facts)
	if folder == nil && name == nil {
		return "", nil
	}
	res, err := PlaceImage(database, b, id, folder, name)
	if err != nil {
		return "", err
	}
	return res.NewCanonicalPath, nil
}

// ReceivedNaming lands the bytes in the root when the folder has tokens,
// which need the row, and the move follows the ingest. A folder named on
// the request is taken literally.
func ReceivedNaming(galleryName, requestFolder, defaultFolder, defaultName string) (writeDir string, n Naming) {
	n.Gallery = galleryName
	n.Name = parseSetting(defaultName, ScopeUploadName, "default_upload_name")
	if requestFolder != "" {
		return requestFolder, n
	}
	folder := parseSetting(defaultFolder, ScopeUploadFolder, "default_upload_folder")
	if folder.HasTokens() {
		n.Folder = folder
		return "", n
	}
	return defaultFolder, n
}

// IngestNaming applies the folder as a move, tokens or not, since the file
// is already on disk; a blank one leaves the file where it was dropped.
func IngestNaming(galleryName, folder, name string) Naming {
	return Naming{
		Folder:  parseSetting(folder, ScopeUploadFolder, "default_upload_folder"),
		Name:    parseSetting(name, ScopeUploadName, "default_upload_name"),
		Gallery: galleryName,
	}
}

// ParseBatchRenameTemplate appends {n} to a base with no token, or the
// whole scope would collide on one name. The preview and the job must both
// parse through here.
func ParseBatchRenameTemplate(s string) (*NameTemplate, error) {
	tmpl, err := ParseNameTemplate(s, ScopeRenameBatch)
	if err != nil || tmpl == nil || tmpl.HasTokens() {
		return tmpl, err
	}
	return ParseNameTemplate(s+"{n}", ScopeRenameBatch)
}

func (n Naming) FolderFor(ctx context.Context, database *db.DB, id int64) (string, error) {
	if n.Folder == nil {
		return "", nil
	}
	facts, _, err := LoadNameFacts(ctx, database, n.Gallery, id, 0, n.Folder)
	if err != nil {
		return "", err
	}
	return n.Folder.Render(facts)
}

func parseSetting(raw string, sc Scope, key string) *NameTemplate {
	tmpl, err := ParseNameTemplate(raw, sc)
	if err != nil {
		logx.Warnf("%s %q: %v", key, raw, err)
		return nil
	}
	return tmpl
}
