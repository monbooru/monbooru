package monloader

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

func ContribTag(tagSvc func() *tags.Service, id int64) (*models.Tag, bool) {
	tag, err := tagSvc().GetTag(id)
	if err != nil || tag.IsAlias || tag.CategoryName == "rating" {
		return nil, false
	}
	return tag, true
}

func (l *Link) ImageContribPreview(rctx context.Context, tagSvc func() *tags.Service, id int64, sha string) (*ContribPreview, error) {
	_, imageTags, err := tagSvc().GetImageTags(id)
	if err != nil {
		return nil, err
	}
	ledger, err := tagSvc().TagSourcesForImage(id)
	if err != nil {
		logx.Warnf("TagSourcesForImage: %v", err)
	}
	storage, implied := tags.StorageForms(imageTags, ledger)
	byTag, byAlias := imageTagAliases(tagSvc, imageTags)
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	sent, local := l.ptrSubmitSpellings(ctx, storage, byTag)
	preview, err := l.previewContrib(ctx, sha, sent, implied)
	if err != nil {
		return nil, err
	}
	for i, t := range preview.ToAdd {
		preview.ToAdd[i].Sent = t.Tag
		if own, ok := local[t.Tag]; ok {
			preview.ToAdd[i].Tag = own
		}
	}
	foldAliasedPTRTags(preview, byAlias)
	return preview, nil
}

// The graph endpoint refuses while the index syncs, when the panel still
// renders: an error means no substitution, not a failure.
func (l *Link) ptrSubmitSpellings(ctx context.Context, storage []string, byTag map[string][]string) ([]string, map[string]string) {
	var names []string
	for _, form := range storage {
		if len(byTag[form]) > 0 {
			names = append(names, form)
			names = append(names, byTag[form]...)
		}
	}
	if len(names) == 0 {
		return storage, nil
	}
	graph := map[string]contract.PTRTagInfo{}
	for start := 0; start < len(names); start += contract.MaxPTRTags {
		part, err := l.PTRTagLookup(ctx, names[start:min(start+contract.MaxPTRTags, len(names))])
		if err != nil {
			return storage, nil
		}
		maps.Copy(graph, part)
	}
	sent := make([]string, len(storage))
	taken := make(map[string]bool, len(storage))
	for i, form := range storage {
		sent[i], taken[form] = form, true
	}
	local := map[string]string{}
	for i, form := range storage {
		if len(byTag[form]) == 0 {
			continue
		}
		// A spelling another tag on the image already occupies would go
		// up twice and pair the wrong row with it.
		if spelling, _, ok := resolvePTRSpelling(graph, form, byTag[form]); ok && !taken[spelling] {
			sent[i], taken[spelling], local[spelling] = spelling, true, form
		}
	}
	return sent, local
}

// Without this, a PTR spelling pulled here as an alias shows as a petition
// and the operator's own spelling as a new upload of the same tag.
func foldAliasedPTRTags(preview *ContribPreview, byAlias map[string]string) {
	if len(byAlias) == 0 {
		return
	}
	carried := map[string]bool{}
	kept := preview.PTROnly[:0]
	for _, p := range preview.PTROnly {
		if local, ok := byAlias[p.Tag]; ok {
			carried[local] = true
			continue
		}
		kept = append(kept, p)
	}
	preview.PTROnly = kept
	for i, t := range preview.ToAdd {
		if t.Status == contract.PreviewNew && carried[t.Tag] {
			preview.ToAdd[i].Status = contract.PreviewKnown
		}
	}
}

func imageTagAliases(tagSvc func() *tags.Service, imageTags []models.ImageTag) (byTag map[string][]string, byAlias map[string]string) {
	ids := make([]int64, 0, len(imageTags))
	for _, t := range imageTags {
		ids = append(ids, t.TagID)
	}
	byTag, byAlias, err := tagSvc().AliasForms(ids)
	if err != nil {
		logx.Warnf("AliasesForTagIDs: %v", err)
	}
	return byTag, byAlias
}

// A tag the PTR holds that the ledger does not credit to it is still work
// for a pull, which records the attribution.
func (l *Link) PTRUnattributed(rctx context.Context, tagSvc func() *tags.Service, id int64, preview *ContribPreview) []string {
	known := map[string]bool{}
	for _, t := range preview.ToAdd {
		if t.Status == contract.PreviewKnown {
			known[t.Tag] = true
		}
	}
	if len(known) == 0 {
		return nil
	}
	ledger, err := tagSvc().TagSourcesForImage(id)
	if err != nil {
		return nil
	}
	_, imageTags, err := tagSvc().GetImageTags(id)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range imageTags {
		name := tags.FormName(t.Category, t.TagName)
		if !known[name] || slices.ContainsFunc(ledger[t.TagID], isPTRSource) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return l.ownPTRIdeals(rctx, out)
}

// A pull writes a tag the PTR files under another spelling (1girl under
// 1_female) as that spelling, so it can never credit this one. No answer
// leaves the list as it is.
func (l *Link) ownPTRIdeals(rctx context.Context, names []string) []string {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	var kept []string
	for start := 0; start < len(names); start += contract.MaxPTRTags {
		part := names[start:min(start+contract.MaxPTRTags, len(names))]
		graph, err := l.PTRTagLookup(ctx, part)
		if err != nil {
			return names
		}
		for _, name := range part {
			if ideal := graph[name].Ideal; ideal == "" || ideal == name {
				kept = append(kept, name)
			}
		}
	}
	return kept
}

func isPTRSource(src tags.TagSource) bool { return strings.EqualFold(src.Source, contract.SourcePTR) }

func MappingItems(sha string, adds, petitions []string, reason string) []contract.ContribItem {
	var items []contract.ContribItem
	for _, tag := range adds {
		items = append(items, contract.ContribItem{Kind: contract.ContribMappingAdd, SHA256: sha, Tag: tag})
	}
	for _, tag := range petitions {
		items = append(items, contract.ContribItem{Kind: contract.ContribMappingPetition, SHA256: sha, Tag: tag, Reason: reason})
	}
	return items
}

type ContribReceiptRow struct {
	Label string
	Note  string
}

type ContribReceipt struct {
	Sent, Refused                int
	Adds, Petitions, RefusedRows []ContribReceiptRow
}

// Verdicts pair with items by position, so a count mismatch would
// mislabel them: it answers false.
func ReadContribReceipt(items []contract.ContribItem, results []contract.ContribResult) (ContribReceipt, bool) {
	if len(results) != len(items) {
		return ContribReceipt{}, false
	}
	var r ContribReceipt
	r.Adds, r.Petitions, r.RefusedRows = receiptRows(items, results)
	r.Sent, r.Refused = len(r.Adds)+len(r.Petitions), len(r.RefusedRows)
	return r, true
}

func receiptRows(items []contract.ContribItem, results []contract.ContribResult) (adds, petitions, refused []ContribReceiptRow) {
	for i := 0; i < min(len(items), len(results)); i++ {
		kind := items[i].Kind
		var label string
		switch kind {
		case contract.ContribMappingAdd, contract.ContribMappingPetition:
			label = items[i].Tag
		case contract.ContribSibling, contract.ContribSiblingPetition:
			label = items[i].Bad + " -> " + items[i].Good
		default:
			label = items[i].Child + " => " + items[i].Parent
		}
		row := ContribReceiptRow{Label: label}
		switch {
		case results[i].Result != contract.ContribStaged:
			row.Note = results[i].Result
			if results[i].Note != "" {
				row.Note += " - " + results[i].Note
			}
			refused = append(refused, row)
		case kind == contract.ContribMappingPetition || kind == contract.ContribSiblingPetition || kind == contract.ContribParentPetition:
			petitions = append(petitions, row)
		default:
			adds = append(adds, row)
		}
	}
	return adds, petitions, refused
}

// A and B follow hydrus: sibling A=bad (alias), B=good; parent A=child
// (the carrying tag), B=parent (the implied tag).
type TagPairRow struct {
	Kind           string
	A, B           string
	AColor, BColor string
	Rel            string
	Direction      string
	Note           string
}

// Space delimits the fields because the tag charset excludes whitespace,
// unlike '|'.
func (p TagPairRow) Value() string { return p.Kind + " " + p.A + " " + p.B }

func (p TagPairRow) Label() string {
	if p.Kind == contract.ContribSibling {
		return p.A + " -> " + p.B
	}
	return p.A + " => " + p.B
}

type TagContribDiff struct {
	Local, PTROnly []TagPairRow
	Pullable       bool
	Provisional    bool
	KnownAs        string
	Unknown        bool
	Empty          bool
	IdealElsewhere string
}

func (l *Link) TagContribPreview(rctx context.Context, tagSvc func() *tags.Service, id int64, tag *models.Tag) (*TagContribDiff, error) {
	ctx, cancel := context.WithTimeout(rctx, 8*time.Second)
	defer cancel()
	tagForm := tags.FormName(tag.CategoryName, tag.Name)
	aliases, err := tagSvc().AliasesForTagIDs([]int64{id})
	if err != nil {
		return nil, err
	}
	implications, err := tagSvc().ListImplications(id)
	if err != nil {
		return nil, err
	}
	impliedBy, err := tagSvc().ImpliedBy(id)
	if err != nil {
		return nil, err
	}
	d := &TagContribDiff{}
	localAlias, localImplied, localImpliedBy := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var aliasForms []string
	for _, a := range aliases[id] {
		form := tags.FormName(a.CategoryName, a.Name)
		d.Local = append(d.Local, TagPairRow{Kind: contract.ContribSibling, A: form, B: tagForm, Rel: "alias of this"})
		localAlias[form] = true
		aliasForms = append(aliasForms, form)
	}
	for _, im := range impliedBy {
		d.Local = append(d.Local, TagPairRow{Kind: contract.ContribParent, A: tags.FormName(im.ParentCategoryName, im.ParentName), B: tagForm, Rel: "implied by"})
		localImpliedBy[tags.FormName(im.ParentCategoryName, im.ParentName)] = true
	}
	for _, im := range implications {
		d.Local = append(d.Local, TagPairRow{Kind: contract.ContribParent, A: tagForm, B: tags.FormName(im.ImpliedCategoryName, im.ImpliedName), Rel: "implies"})
		localImplied[tags.FormName(im.ImpliedCategoryName, im.ImpliedName)] = true
	}
	names := append([]string{tagForm}, aliasForms...)
	names = names[:min(len(names), contract.MaxPTRTags)]
	graph, err := l.PTRTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	known, info, ok := resolvePTRSpelling(graph, tagForm, aliasForms)
	if ok && known != tagForm {
		d.KnownAs = known
	}
	var toPreview []*TagPairRow
	for i := range d.Local {
		// Known only under another spelling, pair-preview reads every
		// local relation as new; those the cluster already carries would
		// propose flipping the ideal, so they fold as covered.
		if d.KnownAs != "" && heldByCluster(tagSvc, d.Local[i], tagForm, known, info) {
			d.Local[i].Direction = contract.PairCovered
			continue
		}
		toPreview = append(toPreview, &d.Local[i])
	}
	previews, err := l.pairPreviews(ctx, toPreview)
	if err != nil {
		return nil, err
	}
	for i, row := range toPreview {
		row.Direction, row.Note = previews[i].Direction, previews[i].Note
		d.Provisional = d.Provisional || previews[i].Provisional
	}
	if !ok {
		d.Unknown = true
		return d, nil
	}
	d.Pullable = pullWouldAdd(tagSvc, id, info, known)
	d.Empty = len(info.Aliases) == 0 && len(info.Implications) == 0 && len(info.ImpliedBy) == 0 &&
		(info.Ideal == "" || info.Ideal == known)
	// Only pairs the index holds (petition or pending) qualify; the rest
	// is sibling-chain noise. Rows are keyed on the spelling the PTR
	// answered for, the name the index holds them under.
	var candidates []TagPairRow
	for _, a := range info.Aliases {
		if a == "" || a == known || localAlias[localForm(tagSvc, a)] {
			continue
		}
		// A tag of its own here whose PTR ideal is this one reads, on its
		// own card, as a merge into this tag, which a petition contradicts.
		if row, ok := tagSvc().FormRow(a); ok && !row.IsAlias && (info.Ideal == "" || info.Ideal == known) {
			continue
		}
		candidates = append(candidates, TagPairRow{Kind: contract.ContribSibling, A: a, B: known, Rel: "alias of this"})
	}
	// The ideal is never a petition row: a pull adopts it as an alias
	// here, or leaves it alone when another tag here holds the name.
	if info.Ideal != "" && info.Ideal != known && !localAlias[localForm(tagSvc, info.Ideal)] && tagSvc().FormExists(info.Ideal) {
		d.IdealElsewhere = info.Ideal
	}
	undeclared := func(im string, declared map[string]bool) bool {
		if im == "" || im == known || declared[localForm(tagSvc, im)] {
			return false
		}
		// The pull skips an endpoint that is an alias here, so the edge
		// has no local form to disagree with.
		row, ok := tagSvc().FormRow(im)
		return !ok || !row.IsAlias
	}
	for _, im := range info.Implications {
		if undeclared(im, localImplied) {
			candidates = append(candidates, TagPairRow{Kind: contract.ContribParent, A: known, B: im, Rel: "implies"})
		}
	}
	for _, im := range info.ImpliedBy {
		if undeclared(im, localImpliedBy) {
			candidates = append(candidates, TagPairRow{Kind: contract.ContribParent, A: im, B: known, Rel: "implied by"})
		}
	}
	toPreview = toPreview[:0]
	for i := range candidates {
		// A petition needs a local judgement, which an endpoint the
		// catalog has never seen cannot give.
		other := candidates[i].A
		if other == known {
			other = candidates[i].B
		}
		if tagSvc().FormExists(other) {
			toPreview = append(toPreview, &candidates[i])
		}
	}
	previews, err = l.pairPreviews(ctx, toPreview)
	if err != nil {
		return nil, err
	}
	for i, row := range toPreview {
		if previews[i].Direction != contract.PairPetition && previews[i].Direction != contract.PairPending {
			continue
		}
		row.Direction, row.Note = previews[i].Direction, previews[i].Note
		d.PTROnly = append(d.PTROnly, *row)
	}
	return d, nil
}

// Each preview is a monloader round trip and a hub tag has dozens, so a
// few run at once; the first failure stops the rest.
func (l *Link) pairPreviews(ctx context.Context, rows []*TagPairRow) ([]*contract.PairPreviewResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]*contract.PairPreviewResponse, len(rows))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var failed sync.Once
	var firstErr error
	for i, row := range rows {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			preview, err := l.previewPair(ctx, row.Kind, row.A, row.B)
			if err != nil {
				failed.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			out[i] = preview
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, ctx.Err()
}

func localForm(tagSvc func() *tags.Service, name string) string {
	if form, ok := tagSvc().NormalizeForm(name); ok {
		return form
	}
	return name
}

func heldByCluster(tagSvc func() *tags.Service, row TagPairRow, tagForm, known string, info contract.PTRTagInfo) bool {
	names := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return localForm(tagSvc, n) == row.A })
	}
	switch {
	case row.Kind == contract.ContribSibling:
		return row.A == known || row.A == localForm(tagSvc, info.Ideal) || names(info.Aliases)
	case row.A == tagForm:
		return slices.ContainsFunc(info.Implications, func(n string) bool { return localForm(tagSvc, n) == row.B })
	default:
		return names(info.ImpliedBy)
	}
}

// Uses applyPTRTagInfo's normalization and row lookups: comparing
// spellings as strings would keep offering a pull that adopts nothing.
func pullWouldAdd(tagSvc func() *tags.Service, tagID int64, info contract.PTRTagInfo, tagForm string) bool {
	svc := tagSvc()
	free := func(n string) bool { return n != "" && n != tagForm && svc.FormFree(n) }
	implies := func(forward bool) func(string) bool {
		return func(im string) bool { return im != "" && im != tagForm && svc.WouldAddImplication(tagID, im, forward) }
	}
	return slices.ContainsFunc(info.Aliases, free) || free(info.Ideal) ||
		slices.ContainsFunc(info.Implications, implies(true)) || slices.ContainsFunc(info.ImpliedBy, implies(false))
}

// Each value is a TagPairRow's Value: "kind a b".
func PairItems(values []string, direction, reason string) []contract.ContribItem {
	var items []contract.ContribItem
	for _, v := range values {
		parts := strings.SplitN(v, " ", 3)
		if len(parts) != 3 {
			continue
		}
		if item, ok := pairSendItem(parts[0], direction, parts[1], parts[2], reason); ok {
			items = append(items, item)
		}
	}
	return items
}

func pairSendItem(kind, direction, a, b, reason string) (contract.ContribItem, bool) {
	item := contract.ContribItem{Reason: reason}
	if kind == contract.ContribParent {
		item.Child, item.Parent = a, b
	} else {
		item.Bad, item.Good = a, b
	}
	switch {
	case direction == contract.PairSuggest && kind == contract.ContribParent:
		item.Kind = contract.ContribParent
	case direction == contract.PairPetition && kind == contract.ContribParent:
		item.Kind = contract.ContribParentPetition
	case direction == contract.PairSuggest:
		item.Kind = contract.ContribSibling
	case direction == contract.PairPetition:
		item.Kind = contract.ContribSiblingPetition
	default:
		return contract.ContribItem{}, false
	}
	return item, true
}
