package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

type catTag struct {
	catID int64
	name  string
	raw   string
}

func (s *Server) parseTagInput(tagInput string) ([]catTag, []string, string) {
	tokens, err := splitTagTokens(tagInput)
	if err != nil {
		return nil, nil, err.Error()
	}

	var generalID int64
	if cx := s.active(); cx != nil {
		generalID = cx.GeneralCategoryID
	}

	var catTags []catTag
	var rejected []string
	// Unknown prefixes stay in the name and get reported: they may be typos.
	var unknownCats []string
	for _, name := range tokens {
		f, err := tags.SplitForm(s.db(), name)
		switch {
		case err != nil:
			return nil, nil, err.Error()
		case f.CategoryID != 0 && f.Name == "":
			rejected = append(rejected, "rejected: "+name+": "+tags.ErrEmptyFormName.Error())
		case f.CategoryID != 0:
			catTags = append(catTags, catTag{f.CategoryID, f.Name, name})
		default:
			if f.Namespace != "" && !slices.Contains(unknownCats, f.Namespace) {
				unknownCats = append(unknownCats, f.Namespace)
			}
			catTags = append(catTags, catTag{generalID, f.Name, name})
		}
	}

	return catTags, unknownCats, strings.Join(rejected, "; ")
}

func unknownCategoryNote(unknownCats []string) string {
	return joinLabeled("no category named ", ", ", unknownCats)
}

func splitTagTokens(s string) ([]string, error) {
	var tokens []string
	var buf strings.Builder
	quoted := false
	inToken := false

	flush := func() {
		if !inToken {
			return
		}
		tokens = append(tokens, buf.String())
		buf.Reset()
		inToken = false
	}

	for _, r := range s {
		if r == '"' {
			quoted = !quoted
			inToken = true
			continue
		}
		if quoted {
			if r == ' ' || r == '\t' {
				buf.WriteRune('_')
			} else {
				buf.WriteRune(r)
			}
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		buf.WriteRune(r)
		inToken = true
	}
	if quoted {
		return nil, fmt.Errorf("unterminated quote in tag input")
	}
	flush()
	return tokens, nil
}

func (s *Server) addTagToImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	tagInput := strings.TrimSpace(r.FormValue("tag"))
	if tagInput == "" {
		http.Error(w, "tag required", http.StatusBadRequest)
		return
	}

	catTags, unknownCats, parseErrMsg := s.parseTagInput(tagInput)

	var added, rejected, dupes []string
	var promotedTokens []string
	var displacedRatings []string
	mutated := false

	// Resolved first so the inserts share one transaction.
	type resolved struct {
		name string
		tag  *models.Tag
	}
	prepared := make([]resolved, 0, len(catTags))
	seen := make(map[int64]bool, len(catTags))
	resolvedRaw := map[string]int{}
	for _, ct := range catTags {
		tag, err := s.tagSvc().GetOrCreateTag(ct.name, ct.catID)
		if err != nil {
			logx.Warnf("add tag %q: %v", ct.name, err)
			rejected = append(rejected, ct.name+": "+err.Error())
			continue
		}
		resolvedRaw[ct.raw]++
		if seen[tag.ID] {
			continue
		}
		seen[tag.ID] = true
		prepared = append(prepared, resolved{name: ct.name, tag: tag})
	}

	if len(prepared) > 0 {
		tagIDs := make([]int64, len(prepared))
		for i, p := range prepared {
			tagIDs[i] = p.tag.ID
		}
		results, err := s.tagSvc().AddTagsToOneImage(id, tagIDs, "")
		if err != nil {
			logx.Warnf("batch add tags to image %d: %v", id, err)
			for _, p := range prepared {
				rejected = append(rejected, p.name+": "+err.Error())
			}
		} else {
			for i, res := range results {
				name := prepared[i].tag.Name
				if res.Added || res.Promoted {
					mutated = true
				}
				if res.Added && !res.Promoted {
					added = append(added, name)
				}
				if !res.Added && !res.Promoted {
					dupes = append(dupes, name)
				}
				if res.Promoted {
					promotedTokens = append(promotedTokens, name)
				}
				displacedRatings = append(displacedRatings, res.DisplacedRatings...)
			}
		}
	}

	if mutated {
		w.Header().Set("HX-Trigger", "tags-changed")
	}

	addedPart := joinLabeled("added: ", ", ", added)
	promotedPart := joinLabeled("promoted to user tag: ", ", ", promotedTokens)
	dupesPart := ""
	if mutated {
		dupesPart = joinLabeled("already on image: ", ", ", dupes)
	}
	displacedPart := joinLabeled("replaced rating ", ", ", displacedRatings)
	rejectedPart := joinLabeled("rejected: ", "; ", rejected)
	unknownPart := unknownCategoryNote(unknownCats)

	joinNonEmpty := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "; ")
	}

	var addErrMsg, addWarnMsg, addOkMsg string
	switch {
	case parseErrMsg != "" && !mutated && len(rejected) == 0:
		addErrMsg = parseErrMsg
	case mutated && (len(rejected) > 0 || parseErrMsg != ""):
		addWarnMsg = joinNonEmpty(addedPart, promotedPart, dupesPart, displacedPart, unknownPart, parseErrMsg, rejectedPart)
	case len(rejected) > 0:
		addErrMsg = joinNonEmpty(parseErrMsg, rejectedPart)
	case len(dupes) > 0 && !mutated && parseErrMsg == "":
		addErrMsg = "tag already on image: " + strings.Join(dupes, ", ")
	default:
		addOkMsg = joinNonEmpty(addedPart, promotedPart, dupesPart, displacedPart, unknownPart)
	}
	var input *string
	switch {
	case len(rejected) == 0 && parseErrMsg == "":
		input = new(string)
	case mutated:
		// Only what failed stays to be fixed: resubmitting the rest would add it again.
		tokens, _ := splitTagTokens(tagInput)
		var left []string
		for _, t := range tokens {
			if resolvedRaw[t] > 0 {
				resolvedRaw[t]--
				continue
			}
			left = append(left, t)
		}
		refill := strings.Join(left, " ")
		input = &refill
	}
	s.renderTagListWithSidebar(w, r, id, addErrMsg, addWarnMsg, addOkMsg, input)
}

// A non-nil input replaces the add box's text; nil leaves it as typed.
func (s *Server) renderTagListWithSidebar(w http.ResponseWriter, r *http.Request, id int64, errMsg, warnMsg, okMsg string, input *string) {
	folderPath, imageTags, _ := s.tagSvc().GetImageTags(id)
	tagMode := tagModePref.resolve(w, r, "tagmode")
	impliedMode := impliedModePref.resolve(w, r, "implied")
	hasUserTags, hasStaleTags := userAndStaleTags(imageTags)
	// The tag routes carry no back context; the page they were sent from does.
	pageQuery := r.URL.Query()
	if u, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && u.RawQuery != "" {
		pageQuery = u.Query()
	}
	back := backContextFrom(pageQuery)
	ref := ""
	if refID, err := strconv.ParseInt(pageQuery.Get("ref"), 10, 64); err == nil && refID != id {
		ref = strconv.FormatInt(refID, 10)
	}
	tagCount := 0
	if cx := s.active(); cx != nil {
		tagCount, _ = cx.TagCount()
	}
	var canonicalPath string
	_ = s.db().Read.QueryRow(`SELECT canonical_path FROM images WHERE id = ?`, id).Scan(&canonicalPath)
	extraPaths := extraImagePaths(loadImagePaths(r.Context(), s.db(), s.boundary(), id))
	filename := ""
	if canonicalPath != "" {
		filename = filepath.Base(canonicalPath)
	}
	inputText := ""
	if input != nil {
		inputText = *input
	}
	s.renderTemplate(w, "partials/tag_list.html", map[string]any{
		"ImageID":          id,
		"ImageTags":        imageTags,
		"TagSidebar":       s.buildTagSidebar(id, tagMode, impliedMode, imageTags),
		"SidebarTags":      true,
		"SidebarCollapsed": sidebarCollapsed(r),
		"DangerZone":       true,
		"HasUserTags":      hasUserTags,
		"HasStaleTags":     hasStaleTags,
		"ImageTaggers":     distinctTaggerNames(imageTags, true),
		"ImageSources":     distinctTaggerNames(imageTags, false),
		"CanTransfer":      len(s.galleryList()) > 1,
		"BackQuery":        back.Q,
		"BackKVQS":         back.QueryString("&"),
		"Ref":              ref,
		"ErrMsg":           errMsg,
		"WarnMsg":          warnMsg,
		"OkMsg":            okMsg,
		"SetInput":         input != nil,
		"Input":            inputText,
		"CurrentFolder":    folderPath,
		"Filename":         filename,
		"ExtraPaths":       extraPaths,
		"RewrittenBy":      gallery.RewrittenBy(s.db().Read, id),
		"TagCount":         tagCount,
	})
}

// Repeated params, not comma-joined: a label can carry a comma.
func trimmedValues(raw []string) []string {
	var out []string
	for _, v := range raw {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) removeAutoTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	names := trimmedValues(r.URL.Query()["taggers"])
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		return s.tagSvc().RemoveAutoTagsFromImage(id, names)
	})
}

func (s *Server) removeSourceTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	names := trimmedValues(r.URL.Query()["sources"])
	stale := r.URL.Query().Get("stale")
	if stale != "0" && stale != "1" {
		stale = ""
	}
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		return s.tagSvc().RemoveSourceTagsFromImage(id, names, stale)
	})
}

func (s *Server) removeCategoryTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimSpace(r.URL.Query().Get("cat"))
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		if category == "" {
			return 0, nil
		}
		return s.tagSvc().RemoveCategoryTagsFromImage(id, category)
	})
}

func (s *Server) dropSourceContributionHandler(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	tagIDs := parseIDList(r.URL.Query()["tag"])
	s.removeImageTagsWithMsg(w, r, func(id int64) (string, error) {
		if source == "" {
			return "", nil
		}
		covered, removed, err := s.tagSvc().DropSourceFromImageTags(id, source, tagIDs)
		return droppedSourceMsg(source, covered, removed), err
	})
}

// covered: tags the source let go; removed: those that lost their last source.
func droppedSourceMsg(source string, covered, removed int) string {
	switch {
	case covered == 0:
		return ""
	case removed == covered:
		return removedTagsMsg(removed)
	case removed == 0:
		return fmt.Sprintf("dropped %s from %d tag%s", source, covered, plural(covered))
	default:
		return fmt.Sprintf("dropped %s from %d tag%s, %d removed", source, covered, plural(covered), removed)
	}
}

func (s *Server) removeUserTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	s.removeImageTagsHandler(w, r, s.tagSvc().RemoveUserTagsFromImage)
}

func (s *Server) removeAllTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	s.removeImageTagsHandler(w, r, func(id int64) (int, error) {
		var n int
		if err := s.db().Read.QueryRow(`SELECT COUNT(*) FROM image_tags WHERE image_id = ?`, id).Scan(&n); err != nil {
			return 0, err
		}
		return n, s.tagSvc().RemoveAllTagsFromImage(id)
	})
}

func (s *Server) removeStaleTagsFromImageHandler(w http.ResponseWriter, r *http.Request) {
	s.removeImageTagsHandler(w, r, s.tagSvc().RemoveStaleTagsFromImage)
}

func removedTagsMsg(removed int) string {
	if removed == 0 {
		return ""
	}
	return fmt.Sprintf("removed %d tag%s", removed, plural(removed))
}

func (s *Server) removeImageTagsHandler(w http.ResponseWriter, r *http.Request, remove func(int64) (int, error)) {
	s.removeImageTagsWithMsg(w, r, func(id int64) (string, error) {
		removed, err := remove(id)
		return removedTagsMsg(removed), err
	})
}

func (s *Server) removeImageTagsWithMsg(w http.ResponseWriter, r *http.Request, remove func(int64) (string, error)) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	msg, err := remove(id)
	if err != nil {
		s.renderTagListWithSidebar(w, r, id, err.Error(), "", "", nil)
		return
	}
	if msg != "" {
		w.Header().Set("HX-Trigger", "tags-changed")
	}
	s.renderTagListWithSidebar(w, r, id, "", "", msg, nil)
}

func (s *Server) removeTagFromImage(w http.ResponseWriter, r *http.Request) {
	tagID, ok := pathInt64(w, r, "tagID")
	if !ok {
		return
	}
	s.removeImageTagsWithMsg(w, r, func(id int64) (string, error) {
		var name string
		_ = s.db().Read.QueryRow(`SELECT name FROM tags WHERE id = ?`, tagID).Scan(&name)
		removed, err := s.tagSvc().RemoveTagFromImage(id, tagID)
		if err != nil {
			return "", err
		}
		if removed > 0 {
			return "removed: " + name, nil
		}
		parents, err := db.QueryStrings(s.db().Read,
			`SELECT p.name FROM tag_implications ti
			 JOIN image_tags it ON it.image_id = ? AND it.tag_id = ti.parent_tag_id
			 JOIN tags p ON p.id = ti.parent_tag_id
			 WHERE ti.implied_tag_id = ? AND EXISTS (SELECT 1 FROM image_tags WHERE image_id = ? AND tag_id = ?)
			 ORDER BY p.name`, id, tagID, id, tagID)
		if err != nil || len(parents) == 0 {
			return "", err
		}
		verb := "implies"
		if len(parents) > 1 {
			verb = "imply"
		}
		return fmt.Sprintf("kept: %s (%s %s it)", name, strings.Join(parents, ", "), verb), nil
	})
}

// Always category-qualified: a miss is usually a wrong category.
func (s *Server) tagTokenLabel(ct catTag) string {
	categories, err := tags.CategoryIDsByName(s.db().Read)
	if err != nil {
		return ct.name
	}
	for name, id := range categories {
		if id == ct.catID {
			return name + ":" + ct.name
		}
	}
	return ct.name
}

func joinLabeled(label, sep string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return label + strings.Join(items, sep)
}

func tagCategoryStatus(err error) int {
	var coll *tags.ErrCategoryCollision
	switch {
	case errors.Is(err, tags.ErrTagNotFound):
		return http.StatusNotFound
	case errors.Is(err, tags.ErrCategoryNotFound), errors.Is(err, tags.ErrRatingCategoryClosed),
		errors.Is(err, tags.ErrRatingTagImmutable):
		return http.StatusBadRequest
	case errors.As(err, &coll):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func (s *Server) changeTagCategory(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	catIDStr := r.FormValue("category_id")
	catID, err := strconv.ParseInt(catIDStr, 10, 64)
	if err != nil {
		http.Error(w, "bad category_id", http.StatusBadRequest)
		return
	}
	before, err := s.tagSvc().GetTag(id)
	if err != nil {
		externalErr(w, r, err.Error(), tagCategoryStatus(err))
		return
	}
	keep := r.FormValue("keep_alias") == "1"
	var svcErr error
	merged := false
	switch {
	case r.FormValue("merge") == "1":
		merged, svcErr = s.tagSvc().ChangeTagCategoryMerge(id, catID, keep)
	case keep:
		svcErr = s.tagSvc().ChangeTagCategoryKeepAlias(id, catID)
	default:
		svcErr = s.tagSvc().ChangeTagCategory(id, catID)
	}
	if svcErr != nil {
		var coll *tags.ErrCategoryCollision
		if errors.As(svcErr, &coll) && isHTMXRequest(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w,
				`<div class="flash flash-err">%s <button type="button" class="btn-sm" onclick="mergeCategoryCollision(%d, %d)">Merge into it</button></div>`,
				template.HTMLEscapeString(svcErr.Error()), id, catID)
			return
		}
		externalErr(w, r, svcErr.Error(), tagCategoryStatus(svcErr))
		return
	}
	if isHTMXRequest(r) {
		if merged {
			writeInlineFlash(w, "ok", "Merged into the existing tag.")
			return
		}
		if keep || before.IsAlias || before.CategoryID == catID {
			writeInlineFlash(w, "ok", "Category updated.")
			return
		}
		// The move already happened, so keeping the old spelling is an alias
		// create on it rather than a second move.
		old := before.CategoryName + ":" + before.Name
		writeInlineFlashHTML(w, "ok", fmt.Sprintf(
			`Category updated. <button type="button" class="btn-sm" hx-post="/tags/%d/aliases?name=%s" hx-target="#tag-flash">Keep %s as an alias</button>`,
			id, template.HTMLEscapeString(url.QueryEscape(old)), template.HTMLEscapeString(old)))
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) getImageTagsHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	s.renderTagListWithSidebar(w, r, id, "", "", "", nil)
}
