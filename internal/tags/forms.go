package tags

import (
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// A form is a tag as the tag language spells it: category:name, or the
// bare name for general.

func FormName(category, name string) string {
	if category != "general" {
		return category + ":" + name
	}
	return name
}

// Form is a typed spelling read against the catalog. A prefix before the
// first colon that names a category, in any case, splits off; any other
// prefix stays part of Name and is reported as Namespace.
type Form struct {
	CategoryID int64
	Category   string
	Name       string
	Namespace  string
}

var ErrEmptyFormName = errors.New("empty tag name after category prefix")

// SplitForm answers a failed category read with the error: reading the
// form as general would file character:foo as a general tag.
func SplitForm(database *db.DB, form string) (Form, error) {
	form = strings.TrimSpace(form)
	idx := strings.Index(form, ":")
	if idx <= 0 {
		return Form{Name: form}, nil
	}
	category := strings.ToLower(form[:idx])
	id, ok, err := CategoryIDByName(database, category)
	if err != nil {
		return Form{}, err
	}
	if !ok {
		return Form{Name: form, Namespace: form[:idx]}, nil
	}
	return Form{CategoryID: id, Category: category, Name: form[idx+1:]}, nil
}

// ParseForm reads a prefix that names no category as part of a general
// name.
func (s *Service) ParseForm(form string) (categoryID int64, bare string, ok bool) {
	f, err := SplitForm(s.db, form)
	switch {
	case err != nil || f.Name == "":
		return 0, "", false
	case f.CategoryID != 0:
		return f.CategoryID, f.Name, true
	case s.generalCatID == 0:
		return 0, "", false
	}
	return s.generalCatID, f.Name, true
}

// NormalizeForm validates the name and drops the general prefix.
func (s *Service) NormalizeForm(form string) (string, bool) {
	form = strings.TrimSpace(form)
	catID, bare, ok := s.ParseForm(form)
	if !ok {
		return "", false
	}
	norm, err := ValidateTagName(bare)
	if err != nil {
		return "", false
	}
	if catID == s.generalCatID {
		return norm, true
	}
	return strings.ToLower(form[:len(form)-len(bare)]) + norm, true
}

func (s *Service) FormValid(form string) bool {
	_, bare, ok := s.ParseForm(form)
	if !ok {
		return false
	}
	_, err := ValidateTagName(bare)
	return err == nil
}

// FormExists matches the spelling as given, a tag or an alias.
func (s *Service) FormExists(form string) bool {
	catID, bare, ok := s.ParseForm(form)
	if !ok {
		return false
	}
	exists, err := s.TagNameExists(catID, bare)
	return err == nil && exists
}

// FormFree reports whether the validated spelling names nothing yet, so an
// alias could take it.
func (s *Service) FormFree(form string) bool {
	catID, bare, ok := s.ParseForm(form)
	if !ok {
		return false
	}
	norm, err := ValidateTagName(bare)
	if err != nil {
		return false
	}
	taken, err := s.TagNameExists(catID, norm)
	return err == nil && !taken
}

// TagNameExists matches the name as given, a tag or an alias.
func (s *Service) TagNameExists(catID int64, name string) (bool, error) {
	var n int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE name = ? AND category_id = ?`, name, catID,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// WouldAddImplication reports whether declaring tagID implies form, or
// form implies tagID when implied is false, would add an edge: a spelling
// nothing holds yet always would, an alias never does.
func (s *Service) WouldAddImplication(tagID int64, form string, implied bool) bool {
	catID, bare, ok := s.ParseForm(form)
	if !ok {
		return false
	}
	norm, err := ValidateTagName(bare)
	if err != nil {
		return false
	}
	var id int64
	var isAlias int
	err = s.db.Read.QueryRow(
		`SELECT id, is_alias FROM tags WHERE name = ? AND category_id = ?`, norm, catID,
	).Scan(&id, &isAlias)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil || isAlias == 1 {
		return false
	}
	parent, child := tagID, id
	if !implied {
		parent, child = id, tagID
	}
	var n int
	err = s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tag_implications WHERE parent_tag_id = ? AND implied_tag_id = ?`, parent, child,
	).Scan(&n)
	return err == nil && n == 0
}

type FormRow struct {
	IsAlias       bool
	CanonicalID   int64
	CanonicalName string
	Usage         int
}

func (s *Service) FormRow(form string) (FormRow, bool) {
	catID, bare, _ := s.ParseForm(form)
	norm, _ := ValidateTagName(bare)
	var row FormRow
	var alias int
	var canon sql.NullInt64
	var canonName sql.NullString
	if err := s.db.Read.QueryRow(
		`SELECT t.is_alias, t.canonical_tag_id, c.name, t.usage_count
		 FROM tags t LEFT JOIN tags c ON c.id = t.canonical_tag_id
		 WHERE t.name = ? AND t.category_id = ?`, norm, catID,
	).Scan(&alias, &canon, &canonName, &row.Usage); err != nil {
		return FormRow{}, false
	}
	row.IsAlias, row.CanonicalID, row.CanonicalName = alias == 1, canon.Int64, canonName.String
	return row, true
}

type TagForm struct {
	ID   int64
	Form string
}

// PlainForms leaves out the aliases and the rating tags, which nothing may
// rewrite, and the rejected tags, which map to nothing.
func (s *Service) PlainForms(ids []int64) ([]TagForm, error) {
	var out []TagForm
	err := db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, args := db.InPlaceholders(chunk)
		batch, err := db.QueryAll(s.db.Read, func(rows *sql.Rows) (TagForm, error) {
			var f TagForm
			var name, cat string
			err := rows.Scan(&f.ID, &name, &cat)
			f.Form = FormName(cat, name)
			return f, err
		},
			`SELECT t.id, t.name, c.name FROM tags t JOIN tag_categories c ON c.id = t.category_id
			 WHERE t.id IN (`+placeholders+`) AND t.is_alias = 0 AND t.rejected = 0 AND c.name != 'rating'`, args...)
		out = append(out, batch...)
		return err
	})
	return out, err
}

// AliasForms maps each canonical spelling to its aliases' and each alias
// spelling back to its canonical, a name a pre-widening catalog folded
// under both shapes.
func (s *Service) AliasForms(tagIDs []int64) (byTag map[string][]string, byAlias map[string]string, err error) {
	byCanonical, err := s.AliasesForTagIDs(tagIDs)
	if err != nil {
		return nil, nil, err
	}
	byTag, byAlias = map[string][]string{}, map[string]string{}
	for _, list := range byCanonical {
		for _, a := range list {
			form := FormName(a.CategoryName, a.Name)
			canonical := FormName(a.CanonicalCategoryName, a.CanonicalName)
			byTag[canonical] = append(byTag[canonical], form)
			byAlias[form] = canonical
			byAlias[LegacyFold(form)] = canonical
		}
	}
	return byTag, byAlias, nil
}

// StorageForms keeps apart the tags an image's owner may speak for: an
// implied row, a rating and a tag only derived from the file are context.
func StorageForms(imageTags []models.ImageTag, ledger map[int64][]TagSource) (storage, context []string) {
	for _, t := range imageTags {
		if t.IsImplied || t.Category == "rating" || derivedOnly(t, ledger[t.TagID]) {
			context = append(context, FormName(t.Category, t.TagName))
			continue
		}
		storage = append(storage, FormName(t.Category, t.TagName))
	}
	return storage, context
}

func derivedOnly(t models.ImageTag, ledger []TagSource) bool {
	if len(ledger) == 0 {
		return t.TaggerName == models.TagSourceMonbooru
	}
	return !slices.ContainsFunc(ledger, func(src TagSource) bool { return src.Source != models.TagSourceMonbooru })
}
