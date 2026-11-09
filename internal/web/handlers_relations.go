package web

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/relations"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/session"
)

type relatedTile struct {
	ID     int64
	Marker string
	Label  string
}

func (s *Server) relatedEntriesGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("related entries load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	navs, nErr := gallery.CollectionNavs(cx.DB, id, resolveCeiling(r, cx).RankFilter(), relatedPanelCap)
	if nErr != nil {
		logx.Warnf("collection navs load %d: %v", id, nErr)
	}
	tiles := flattenRelationsForPanel(rels, id)
	paths := loadImagePaths(r.Context(), cx.DB, cx.Boundary(), id)
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_entries.html", map[string]any{
		"ImageID":       id,
		"Tiles":         tiles,
		"Collections":   collectionNavViews(navs, id, s.pageSize(r)),
		"ImagePaths":    paths,
		"CSRFToken":     s.sessions.CSRFToken(session.FromContext(r.Context())),
		"ActiveGallery": s.activeGallery(),
		"BackQS":        back.QueryString("?"),
		"BackKVQS":      back.QueryString("&"),
	})
}

const relatedPanelCap = 6

type collectionNavView struct {
	Name, Query      string
	Rank, Total      int
	PrevURL, NextURL string
	Tiles            []collectionTileView
}

type collectionTileView struct {
	ID    int64
	Order *int
	URL   string
}

// Every link opens in the collection's own search, placed on the gallery
// page that holds it, so prev / next then walk the collection.
func collectionNavViews(navs []gallery.CollectionNav, self int64, pageSize int) []collectionNavView {
	var views []collectionNavView
	for _, nav := range navs {
		if nav.Total < 2 {
			continue
		}
		q := `collection:"` + search.QuoteValue(nav.Name) + `"`
		selfAt := slices.IndexFunc(nav.Window, func(m gallery.CollectionSample) bool { return m.ID == self })
		v := collectionNavView{Name: nav.Name, Query: q, Rank: nav.Rank, Total: nav.Total}
		for i, m := range nav.Window {
			u := collectionStepURL(m.ID, q, nav.Rank-selfAt+i, pageSize)
			v.Tiles = append(v.Tiles, collectionTileView{ID: m.ID, Order: m.Order, URL: u})
			switch i {
			case selfAt - 1:
				v.PrevURL = u
			case selfAt + 1:
				v.NextURL = u
			}
		}
		views = append(views, v)
	}
	return views
}

func collectionStepURL(id int64, q string, rank, pageSize int) string {
	v := backContext{Q: q, Sort: "order", Order: "asc", Page: strconv.Itoa((rank-1)/pageSize + 1)}.URLValues()
	v.Set("back_idx", strconv.Itoa((rank-1)%pageSize))
	return fmt.Sprintf("/images/%d?%s", id, v.Encode())
}

type collectionSibling struct {
	ID     int64
	Series string
	Order  *int64
	// On a collection's last shown tile: the members past the cap.
	More    int
	MoreURL string
	total   int
}

const collectionSiblingCap = 200

func loadCollectionSiblings(cx *library.Gallery, imageID int64, ceiling *Ceiling) ([]collectionSibling, error) {
	under, args := "", []any{imageID, imageID}
	if rank, active := ceiling.RankCeiling(); active {
		under, args = " AND i.rating_rank <= ?", append(args, rank)
	}
	return db.QueryAll(cx.DB.Read, db.ScanRow(func(sib *collectionSibling) []any {
		return []any{&sib.ID, &sib.Series, &sib.Order, &sib.total}
	}), `WITH ranked AS (
			SELECT jc.image_id, jc.name, jc.position,
			       ROW_NUMBER() OVER (PARTITION BY jc.name ORDER BY jc.position IS NULL, jc.position, jc.image_id) AS rn,
			       COUNT(*) OVER (PARTITION BY jc.name) AS total
			FROM image_collections self
			JOIN image_collections jc ON jc.name = self.name
			JOIN images i ON i.id = jc.image_id
			WHERE self.image_id = ? AND jc.image_id != ? AND i.is_missing = 0`+under+`
		)
		SELECT image_id, name, position, total FROM ranked
		WHERE rn <= ?
		ORDER BY name, rn`,
		append(args, collectionSiblingCap)...)
}

func flattenRelationsForPanel(rels *relations.ImageRelations, self int64) []relatedTile {
	var tiles []relatedTile
	seen := map[int64]bool{self: true}
	add := func(t relatedTile) {
		if len(tiles) >= relatedPanelCap || seen[t.ID] {
			return
		}
		seen[t.ID] = true
		tiles = append(tiles, t)
	}
	if rels.DupGroup != nil {
		for _, m := range rels.DupGroup.Members {
			if m == self {
				continue
			}
			marker := "Duplicate"
			label := "duplicate"
			if m == rels.DupGroup.Original {
				marker = "Original"
				label = "original"
			}
			add(relatedTile{ID: m, Marker: marker, Label: label})
		}
	}
	for _, m := range rels.AltGroupMembers {
		add(relatedTile{ID: m, Marker: "Alternate", Label: "alternate"})
	}
	if rels.VersionParent != nil {
		add(relatedTile{ID: *rels.VersionParent, Marker: "Earlier", Label: "previous version"})
	}
	if rels.VersionChild != nil {
		add(relatedTile{ID: *rels.VersionChild, Marker: "Newer", Label: "newer version"})
	}
	for _, m := range rels.DerivativeSources {
		add(relatedTile{ID: m, Marker: "Source", Label: "source"})
	}
	for _, m := range rels.Derivatives {
		add(relatedTile{ID: m, Marker: "Derivative", Label: "derivative"})
	}
	return tiles
}

func (s *Server) imageRelationsPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	img, err := loadImage(r.Context(), cx.DB, id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}
	rels, err := relations.LoadImageRelations(cx.DB, id)
	if err != nil {
		logx.Warnf("relations page load %d: %v", id, err)
		http.Error(w, "load relations", http.StatusInternalServerError)
		return
	}
	siblings, sErr := loadCollectionSiblings(cx, id, resolveCeiling(r, cx))
	if sErr != nil {
		logx.Warnf("relations page collection siblings %d: %v", id, sErr)
	}
	// Siblings already shown in a relation section above stay: dropping
	// them would leave gaps in the collection's numbering.
	collection := collectionWithSelf(siblings, *img)
	var nextOriginal int64
	if rels.DupGroup != nil && len(rels.DupGroup.Members) >= 3 {
		next, nErr := cx.RelationsSvc.NextOriginalIfRemoved(rels.DupGroup.ID, rels.DupGroup.Original)
		if nErr != nil {
			logx.Warnf("relations page next-original %d: %v", id, nErr)
		}
		nextOriginal = next
	}
	titleName := filepath.Base(img.CanonicalPath)
	if parent := filepath.Base(filepath.Dir(img.CanonicalPath)); parent != "" && parent != "." && parent != "/" {
		titleName = parent + "/" + titleName
	}
	back := parseBackContext(r)
	pageData := relationsImagePageData{
		baseData:                       s.base(r, "gallery", fmt.Sprintf("Relations - %s - %s", titleName, s.booruName())),
		Image:                          *img,
		Relations:                      rels,
		Self:                           id,
		Collection:                     collection,
		NextOriginalIfOriginalUnlinked: nextOriginal,
		AltGroupMembersOrdered:         reorderSelfFirst(rels.AltGroupMembers, id),
		BackQS:                         back.QueryString("?"),
	}
	if rels.DupGroup != nil {
		pageData.DupGroupMembersOrdered = reorderSelfFirst(rels.DupGroup.Members, id)
	}
	if rels.VersionParent != nil || rels.VersionChild != nil {
		gens, vErr := versionChainGensForImage(cx, id)
		if vErr != nil {
			logx.Warnf("relations page version chain %d: %v", id, vErr)
		}
		pageData.VersionChainGens = gens
		pageData.VersionActions = versionActionMap(id, rels)
	}
	if len(rels.DerivativeSources) > 0 || len(rels.Derivatives) > 0 {
		graph, tErr := derivativeGraphForImage(cx, id)
		if tErr != nil {
			logx.Warnf("relations page derivative graph %d: %v", id, tErr)
		}
		pageData.DerivativeGraph = graph
		pageData.DerivativeActions = derivativeActionMap(id, rels)
	}
	s.renderTemplate(w, "relations_image.html", pageData)
}

func reorderSelfFirst(members []int64, self int64) []int64 {
	out := slices.Clone(members)
	if i := slices.Index(out, self); i > 0 {
		out = append([]int64{self}, slices.Delete(out, i, i+1)...)
	}
	return out
}

func versionChainGensForImage(cx *library.Gallery, imageID int64) ([][]int64, error) {
	root, err := relations.ChainRoot(cx.DB.Read, "version_edges", "parent_image_id", "child_image_id", imageID)
	if err != nil {
		return nil, err
	}
	members := []int64{root}
	cur := root
	for i := 0; i < relations.MaxVersionChainDepth; i++ {
		var c int64
		err := cx.DB.Read.QueryRow(`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, cur).Scan(&c)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		members = append(members, c)
		cur = c
	}
	if len(members) <= 1 {
		return nil, nil
	}
	gens := make([][]int64, len(members))
	for i, m := range members {
		gens[i] = []int64{m}
	}
	return gens, nil
}

func derivativeGraphForImage(cx *library.Gallery, imageID int64) (*derivGraph, error) {
	members, err := relations.DerivativeComponent(cx.DB.Read, imageID)
	if err != nil || len(members) < 2 {
		return nil, err
	}
	sourcesOf := map[int64][]int64{}
	for _, id := range members {
		srcs, err := db.QueryIDs(cx.DB.Read,
			`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ? ORDER BY source_image_id`, id)
		if err != nil {
			return nil, err
		}
		sourcesOf[id] = srcs
	}
	return layOutDerivatives(members, sourcesOf), nil
}

func collectionWithSelf(siblings []collectionSibling, self models.Image) []collectionSibling {
	if len(siblings) == 0 {
		return siblings
	}
	var order *int64
	if self.SeriesOrder != nil {
		v := int64(*self.SeriesOrder)
		order = &v
	}
	merged := make([]collectionSibling, 0, len(siblings)+1)
	merged = append(merged, siblings...)
	merged = append(merged, collectionSibling{ID: self.ID, Series: self.Series, Order: order})
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if !strings.EqualFold(a.Series, b.Series) {
			return strings.ToLower(a.Series) < strings.ToLower(b.Series)
		}
		aNull, bNull := a.Order == nil, b.Order == nil
		if aNull != bNull {
			return !aNull
		}
		if !aNull && !bNull && *a.Order != *b.Order {
			return *a.Order < *b.Order
		}
		return a.ID < b.ID
	})
	markCollectionCuts(merged)
	return merged
}

// markCollectionCuts puts what the cap left out of each collection on its
// last shown tile.
func markCollectionCuts(rows []collectionSibling) {
	shown, total, last := map[string]int{}, map[string]int{}, map[string]int{}
	for i, r := range rows {
		if r.total > 0 {
			shown[r.Series]++
			total[r.Series] = r.total
			last[r.Series] = i
		}
	}
	for name, n := range total {
		if more := n - shown[name]; more > 0 {
			q := url.Values{"q": {`collection:"` + search.QuoteValue(name) + `"`}, "sort": {"order"}, "order": {"asc"}}
			rows[last[name]].More, rows[last[name]].MoreURL = more, "/?"+q.Encode()
		}
	}
}

type relationsImagePageData struct {
	baseData
	Image                          models.Image
	Relations                      *relations.ImageRelations
	Self                           int64
	Collection                     []collectionSibling
	NextOriginalIfOriginalUnlinked int64
	DupGroupMembersOrdered         []int64
	AltGroupMembersOrdered         []int64
	VersionChainGens               [][]int64
	DerivativeGraph                *derivGraph
	DerivativeActions              map[int64]string
	VersionActions                 map[int64]string
	BackQS                         template.URL
}

func derivativeActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	for _, s := range rels.DerivativeSources {
		m[s] = "source"
	}
	for _, d := range rels.Derivatives {
		m[d] = "derivative"
	}
	return m
}

func versionActionMap(self int64, rels *relations.ImageRelations) map[int64]string {
	m := map[int64]string{self: "this"}
	if rels.VersionParent != nil {
		m[*rels.VersionParent] = "earlier"
	}
	if rels.VersionChild != nil {
		m[*rels.VersionChild] = "newer"
	}
	return m
}

func (s *Server) recomputePhashPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	if _, err := cx.RecomputePhash(r.Context(), id); err != nil {
		logx.Warnf("recompute phash %d: %v", id, err)
		setFlashHeader(w, "phash recompute failed (is the thumbnail present?)", "err", nil)
		return
	}
	hxDone(w, r, "phash recomputed.", "", "/images/"+strconv.FormatInt(id, 10))
}

func (s *Server) relationsForm(w http.ResponseWriter, r *http.Request) (*library.Gallery, bool) {
	if !parseFormOK(w, r) {
		return nil, false
	}
	cx := s.active()
	if cx == nil || cx.RelationsSvc == nil {
		externalErr(w, r, "No active gallery.", http.StatusServiceUnavailable)
		return nil, false
	}
	return cx, true
}

func (s *Server) addRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	if r.FormValue("direction") == "ba" {
		a, b = b, a
	}
	relType := r.FormValue("type")
	var err error
	if r.FormValue("force") == "true" {
		err = cx.RelationsSvc.Overwrite(relType, a, b)
	} else {
		err = cx.RelationsSvc.Add(relType, a, b)
	}
	if errors.Is(err, relations.ErrUnknownKind) {
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	setFlashHeader(w, "Relation added.", "ok", nil)
	writeInlineFlash(w, "ok", "Relation added.")
}

type relationRemoveOp struct {
	field string
	msg   string
	run   func(svc *relations.Service, a, b int64) error
}

var relationRemoveOps = map[string]relationRemoveOp{
	"duplicate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveDupMember(id) }},
	"alternate": {field: "image_id", msg: "Relation removed.",
		run: func(svc *relations.Service, id, _ int64) error { return svc.RemoveAltMember(id) }},
	"version":     {msg: "Relation removed.", run: (*relations.Service).RemoveVersionEdge},
	"derivative":  {msg: "Relation removed.", run: (*relations.Service).RemoveDerivativeEdge},
	"not_related": {msg: "Relation removed.", run: (*relations.Service).RemoveNotRelated},
	"dissolve-dup": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveDupGroup(gid) }},
	"dissolve-alt": {field: "group_id", msg: "Group dissolved.",
		run: func(svc *relations.Service, gid, _ int64) error { return svc.DissolveAltGroup(gid) }},
	"dissolve-version": {field: "root_id", msg: "Version chain dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveVersionChain(rid) }},
	"dissolve-derivative": {field: "root_id", msg: "Derivative tree dissolved.",
		run: func(svc *relations.Service, rid, _ int64) error { return svc.DissolveDerivativeTree(rid) }},
}

func (s *Server) removeRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	relType := r.FormValue("type")
	var msg string
	switch op, tabled := relationRemoveOps[relType]; {
	case tabled:
		var a, b int64
		var ok bool
		if op.field != "" {
			a, ok = formInt64(w, r, op.field)
		} else {
			a, b, ok = parseRelationPair(w, r)
		}
		if !ok {
			return
		}
		if err := op.run(cx.RelationsSvc, a, b); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = op.msg
	case relType == "promote-original":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		id, ok := formInt64(w, r, "image_id")
		if !ok {
			return
		}
		if err := cx.RelationsSvc.PromoteToOriginal(gid, id); err != nil {
			writeRelationError(w, err)
			return
		}
		msg = "Original updated."
	case relType == "review-again":
		reviewAgainPost(w, r, cx)
		return
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	setFlashHeader(w, msg, "ok", nil)
	writeInlineFlash(w, "ok", msg)
}

func reviewAgainPost(w http.ResponseWriter, r *http.Request, cx *library.Gallery) {
	subkind := r.FormValue("kind")
	var a, b int64
	switch subkind {
	case "duplicate", "alternate":
		gid, ok := formInt64(w, r, "group_id")
		if !ok {
			return
		}
		// A group dissolved since the page rendered still aggregates to
		// one row, of NULLs.
		var n int
		var ar, br int64
		if err := cx.DB.Read.QueryRow(
			`SELECT COUNT(*), COALESCE(MIN(image_id), 0), COALESCE(MAX(image_id), 0)
			 FROM `+browseGroupKinds[subkind].membersTable+` WHERE group_id = ?`, gid,
		).Scan(&n, &ar, &br); err != nil {
			writeRelationError(w, err)
			return
		}
		if n != 2 {
			flashStatus(w, http.StatusOK, "Group must have exactly two members.")
			return
		}
		a, b = ar, br
		dissolve := cx.RelationsSvc.DissolveAltGroup
		if subkind == "duplicate" {
			dissolve = cx.RelationsSvc.DissolveDupGroup
		}
		if err := dissolve(gid); err != nil {
			writeRelationError(w, err)
			return
		}
	case "version", "derivative", "not_related":
		ar, br, ok := parseRelationPair(w, r)
		if !ok {
			return
		}
		if err := relationRemoveOps[subkind].run(cx.RelationsSvc, ar, br); err != nil {
			writeRelationError(w, err)
			return
		}
		a, b = ar, br
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown review-again kind.")
		return
	}
	if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
		writeRelationError(w, err)
		return
	}
	if err := cx.RelationsSvc.QueueForReview(a, b); err != nil {
		writeRelationError(w, err)
		return
	}
	// Pinned to the pair, or the session opens on whatever the queue
	// sorts first.
	lo, hi := min(a, b), max(a, b)
	dest := "/relations/session?a=" + strconv.FormatInt(lo, 10) + "&b=" + strconv.FormatInt(hi, 10)
	hxRedirect(w, r, dest)
}

type copyTagsPreviewGroup struct {
	Category string
	Color    string
	Tags     []string
}

func (s *Server) copyTagsToOriginalPreview(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.requireActive(w, r)
	if !ok {
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	original, err := relations.GroupOriginal(cx.DB.Read, gid)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ids, err := cx.RelationsSvc.TagsToCopy(gid)
	if err != nil {
		http.Error(w, "load preview", http.StatusInternalServerError)
		return
	}
	type row struct {
		name     string
		category string
		color    string
	}
	var entries []row
	if len(ids) > 0 {
		placeholders, args := db.InPlaceholders(ids)
		entries, err = db.QueryAll(cx.DB.Read, db.ScanRow(func(rec *row) []any {
			return []any{&rec.name, &rec.category, &rec.color}
		}), `
			SELECT t.name, COALESCE(c.name, ''), COALESCE(c.color, '')
			FROM tags t
			LEFT JOIN tag_categories c ON c.id = t.category_id
			WHERE t.id IN (`+placeholders+`)
			ORDER BY c.position, c.name, t.name`, args...)
		if err != nil {
			http.Error(w, "load preview", http.StatusInternalServerError)
			return
		}
	}
	category := func(e row) string { return cmp.Or(e.category, "(uncategorised)") }
	groups := groupOrdered(entries, nil, category,
		func(e row) *copyTagsPreviewGroup {
			return &copyTagsPreviewGroup{Category: category(e), Color: e.color}
		},
		func(g *copyTagsPreviewGroup, e row) { g.Tags = append(g.Tags, e.name) })
	total := 0
	for _, g := range groups {
		total += len(g.Tags)
	}
	s.renderTemplate(w, "partials/copy_tags_preview.html", map[string]any{
		"GroupID":    gid,
		"OriginalID": original,
		"Groups":     groups,
		"Total":      total,
	})
}

func (s *Server) reverseRelationPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	a, b, ok := parseRelationPair(w, r)
	if !ok {
		return
	}
	var err error
	switch r.FormValue("type") {
	case "version":
		err = cx.RelationsSvc.ReverseVersionEdge(a, b)
	case "derivative":
		err = cx.RelationsSvc.ReverseDerivativeEdge(a, b)
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown relation type.")
		return
	}
	if err != nil {
		writeRelationError(w, err)
		return
	}
	writeInlineFlash(w, "ok", "Edge reversed.")
}

func (s *Server) mergeGroupsPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	kind := cmp.Or(r.URL.Query().Get("kind"), r.FormValue("kind"))
	if kind != "alt" && kind != "dup" {
		flashStatus(w, http.StatusBadRequest, "Unknown merge kind.")
		return
	}
	ids := parseIDList(r.Form["group_id"])
	if len(ids) < 2 {
		flashStatus(w, http.StatusBadRequest, "Pick at least two groups to merge.")
		return
	}
	switch kind {
	case "alt":
		if err := cx.RelationsSvc.MergeAltGroups(ids); err != nil {
			writeRelationError(w, err)
			return
		}
	case "dup":
		var keep int64
		if raw := strings.TrimSpace(r.FormValue("keep_original_from")); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				flashStatus(w, http.StatusBadRequest, "Invalid keep_original_from.")
				return
			}
			if !slices.Contains(ids, v) {
				flashStatus(w, http.StatusBadRequest, "keep_original_from must be one of the merging groups.")
				return
			}
			keep = v
		}
		if err := cx.RelationsSvc.MergeDupGroups(ids, keep); err != nil {
			writeRelationError(w, err)
			return
		}
	}
	hxRedirect(w, r, "/relations/browse?kind="+cmp.Or(browseKindByShortName[kind], "duplicate"))
}

var dissolveOpFor = map[string]string{
	"duplicate":  "dissolve-dup",
	"alternate":  "dissolve-alt",
	"version":    "dissolve-version",
	"derivative": "dissolve-derivative",
}

func (s *Server) dissolveGroupsPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	kind := cmp.Or(r.URL.Query().Get("kind"), r.FormValue("kind"))
	switch op, uniform := relationRemoveOps[dissolveOpFor[kind]]; {
	case uniform:
		for _, id := range parseIDList(r.Form[op.field]) {
			if err := op.run(cx.RelationsSvc, id, 0); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	case kind == "not_related":
		for _, raw := range r.Form["pair"] {
			a, b, ok := parsePairValue(raw)
			if !ok {
				continue
			}
			if err := cx.RelationsSvc.RemoveNotRelated(a, b); err != nil {
				writeRelationError(w, err)
				return
			}
		}
	default:
		flashStatus(w, http.StatusBadRequest, "Unknown dissolve kind.")
		return
	}
	target := "/relations/browse?kind=" + kind
	hxRedirect(w, r, target)
}

func parseIDList(raw []string) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(raw))
	for _, field := range raw {
		// One id per field or comma-joined: htmx flattens arrays that
		// way, and the batch bar joins its selection to stay under
		// net/url's 10000-parameter cap.
		for _, s := range strings.Split(field, ",") {
			v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func parsePairValue(raw string) (int64, int64, bool) {
	aStr, bStr, ok := strings.Cut(raw, ":")
	if !ok {
		return 0, 0, false
	}
	a, err := strconv.ParseInt(strings.TrimSpace(aStr), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.ParseInt(strings.TrimSpace(bStr), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

func (s *Server) copyTagsToOriginalPost(w http.ResponseWriter, r *http.Request) {
	cx, ok := s.relationsForm(w, r)
	if !ok {
		return
	}
	gid, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	added, err := cx.RelationsSvc.CopyTagsFromDuplicatesToOriginal(gid)
	if err != nil {
		writeRelationError(w, err)
		return
	}
	writeInlineFlash(w, "ok", fmt.Sprintf("Copied %d tag%s to the original.", added, plural(added)))
}

func parseRelationPair(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	a, ok := formInt64(w, r, "a")
	if !ok {
		return 0, 0, false
	}
	b, ok := formInt64(w, r, "b")
	if !ok {
		return 0, 0, false
	}
	if a == b {
		// 200, or htmx would not swap the message into its target.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		flashStatus(w, http.StatusOK, "Cannot relate an image to itself.")
		return 0, 0, false
	}
	return a, b, true
}

// writeRelationError answers 200 because htmx does not swap a 4xx body
// into the target.
func writeRelationError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if fe := relations.FriendlyErrorFor(err); fe != nil {
		msg = fe.Message
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	flashStatus(w, http.StatusOK, msg)
}
