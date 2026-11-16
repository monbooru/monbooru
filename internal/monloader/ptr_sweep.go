package monloader

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tags"
)

// Both halves share the job slot the caller already holds.
// The spelling is exact, a bare one meaning general, as the PTR writes it.
func (l *Link) PTRMergeSweep(svc *tags.Service, id int64, spelling string) {
	catID, bare, ok := svc.ParseForm(spelling)
	if !ok {
		l.jobs.Fail("not a tag name: " + spelling)
		return
	}
	target, err := svc.GetOrCreateTag(bare, catID)
	if err != nil {
		l.jobs.Fail(err.Error())
		return
	}
	l.jobs.Update(0, 1, "aliasing tag…")
	if err := svc.MergeTags(id, target.ID); err != nil {
		l.jobs.Fail(err.Error())
		return
	}
	l.PTRTagSweep(svc, []int64{target.ID}, "")
}

type ptrLookupCand struct {
	id   int64
	name string
}

// Rating tags are immutable, so they are never swept.
func ptrLookupCands(svc *tags.Service, ids []int64) ([]ptrLookupCand, error) {
	forms, err := svc.PlainForms(ids)
	if err != nil {
		return nil, err
	}
	cands := make([]ptrLookupCand, len(forms))
	for i, f := range forms {
		cands[i] = ptrLookupCand{id: f.ID, name: f.Form}
	}
	return cands, nil
}

// An orphan spelling answers known with nothing behind it, so a connected
// candidate beats it wherever it turns up; otherwise the tag's own orphan
// name would shadow an alias holding the whole cluster.
func resolvePTRSpelling(graph map[string]contract.PTRTagInfo, own string, aliases []string) (string, contract.PTRTagInfo, bool) {
	bareName, bareInfo, haveBare := "", contract.PTRTagInfo{}, false
	connected := func(name string, info contract.PTRTagInfo) bool {
		if ptrConnected(name, info) {
			return true
		}
		if !haveBare {
			bareName, bareInfo, haveBare = name, info, true
		}
		return false
	}
	if info, ok := graph[own]; ok && info.Known && connected(own, info) {
		return own, info, true
	}
	var first string
	for _, a := range aliases {
		info, ok := graph[a]
		if !ok || !info.Known || !connected(a, info) {
			continue
		}
		if info.Ideal == a {
			return a, info, true
		}
		first = cmp.Or(first, a)
	}
	if first != "" {
		return first, graph[first], true
	}
	if haveBare {
		return bareName, bareInfo, true
	}
	return "", contract.PTRTagInfo{}, false
}

func ptrConnected(name string, info contract.PTRTagInfo) bool {
	return len(info.Aliases)+len(info.Implications)+len(info.ImpliedBy) > 0 || (info.Ideal != "" && info.Ideal != name)
}

func ptrAliasForms(svc *tags.Service, ids []int64) (map[int64][]string, error) {
	aliases, err := svc.AliasesForTagIDs(ids)
	if err != nil {
		return nil, err
	}
	forms := make(map[int64][]string, len(aliases))
	for id, rows := range aliases {
		for _, a := range rows {
			forms[id] = append(forms[id], tags.FormName(a.CategoryName, a.Name))
		}
	}
	return forms, nil
}

// A spelling that answered for the tag joins its aliases, so the pull
// adopts it and the staleness sync never retires it. An `as` the operator
// named gets no alias fallback.
func (l *Link) ptrResolveChunk(ctx context.Context, svc *tags.Service, chunk []ptrLookupCand, as string) (map[int64]contract.PTRTagInfo, error) {
	if as != "" && len(chunk) == 1 {
		graph, err := l.PTRTagLookup(ctx, []string{as})
		if err != nil {
			return nil, err
		}
		info := graph[as]
		if !info.Known {
			return map[int64]contract.PTRTagInfo{}, nil
		}
		if as != chunk[0].name {
			info.Aliases = append(info.Aliases, as)
		}
		return map[int64]contract.PTRTagInfo{chunk[0].id: info}, nil
	}
	names := make([]string, len(chunk))
	for i, c := range chunk {
		names[i] = c.name
	}
	graph, err := l.PTRTagLookup(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]contract.PTRTagInfo, len(chunk))
	// An orphan own name retries too, as the tag card resolves it: an
	// alias may carry the cluster.
	var retry []int64
	for _, c := range chunk {
		if info := graph[c.name]; info.Known && ptrConnected(c.name, info) {
			out[c.id] = info
		} else {
			retry = append(retry, c.id)
		}
	}
	if len(retry) == 0 {
		return out, nil
	}
	forms, err := ptrAliasForms(svc, retry)
	if err != nil {
		return nil, err
	}
	var all []string
	for _, id := range retry {
		all = append(all, forms[id]...)
	}
	for start := 0; start < len(all); start += contract.MaxPTRTags {
		part, err := l.PTRTagLookup(ctx, all[start:min(start+contract.MaxPTRTags, len(all))])
		if err != nil {
			return nil, err
		}
		maps.Copy(graph, part)
	}
	for _, c := range chunk {
		if _, done := out[c.id]; done {
			continue
		}
		if spelling, info, ok := resolvePTRSpelling(graph, c.name, forms[c.id]); ok {
			if spelling != c.name {
				info.Aliases = append(info.Aliases, spelling)
			}
			out[c.id] = info
		}
	}
	return out, nil
}

// Each id is swept at most once, which is what ends the follow-up rounds
// over tags the fan-in created.
func (l *Link) PTRTagSweep(svc *tags.Service, ids []int64, as string) {
	ctx := l.jobs.Context()
	lookedUpAs := as

	cands, err := ptrLookupCands(svc, ids)
	if err != nil {
		l.jobs.Fail(err.Error())
		return
	}

	// Implied-by is pulled only for the requested tags: a series is
	// implied by every character carrying it, so recursing would pull in
	// the whole graph.
	seed := make(map[int64]bool, len(cands))
	for _, c := range cands {
		seed[c.id] = true
	}

	total := len(cands)
	aliases, implications, unknown, processed, dropped, aliased, retired := 0, 0, 0, 0, 0, 0, 0
	cancelled := false
	unavailable := false
	fanOutParents := map[int64]struct{}{}
	swept := make(map[int64]struct{}, total)
	l.jobs.Update(0, total, "PTR lookup…")
	for len(cands) > 0 && !cancelled && !unavailable {
		createdTags := map[int64]struct{}{}
		for start := 0; start < len(cands) && !cancelled; start += contract.MaxPTRTags {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			chunk := cands[start:min(start+contract.MaxPTRTags, len(cands))]
			results, err := l.ptrResolveChunk(ctx, svc, chunk, as)
			var support map[int64]map[int64]bool
			if err == nil {
				support, err = l.ptrImpliedBySupport(ctx, svc, chunk, results)
			}
			if errors.Is(err, ErrPTRUnavailable) {
				// The PTR going away mid-sweep is a degraded stop, not a
				// failure: what already applied stays.
				unavailable = true
				break
			}
			if err != nil && ctx.Err() != nil {
				cancelled = true
				break
			}
			if err != nil {
				ptrFanOut(ctx, svc, fanOutParents)
				l.jobs.Fail("PTR lookup failed: " + err.Error())
				return
			}
			for _, c := range chunk {
				swept[c.id] = struct{}{}
				info := results[c.id]
				if !info.Known {
					// Unknown now, so every relation it pulled earlier
					// goes stale.
					unknown++
					retired += syncPTRStaleness(svc, c.id, nil, support[c.id])
				} else {
					a, i, d, al, r := applyPTRTagInfo(svc, c.id, info, support[c.id], fanOutParents, createdTags, seed[c.id])
					aliases += a
					implications += i
					dropped += d
					aliased += al
					retired += r
				}
				processed++
			}
			l.jobs.Update(processed, total, "PTR lookup…")
		}
		if cancelled || unavailable {
			break
		}
		as = ""
		var next []int64
		for id := range createdTags {
			if _, ok := swept[id]; !ok {
				next = append(next, id)
			}
		}
		if cands, err = ptrLookupCands(svc, next); err != nil {
			ptrFanOut(ctx, svc, fanOutParents)
			l.jobs.Fail(err.Error())
			return
		}
		total += len(cands)
	}

	// A cancel stops quickly: the next sweep finds the edges unapplied.
	if !cancelled {
		ptrFanOut(ctx, svc, fanOutParents)
		cancelled = len(fanOutParents) > 0 && ctx.Err() != nil
	}

	msg := fmt.Sprintf("PTR: added %d %s and %d %s across %d %s", aliases, models.Plural(aliases, "alias", "aliases"),
		implications, models.Plural(implications, "implication", "implications"), processed, models.Plural(processed, "tag", "tags"))
	if lookedUpAs != "" {
		msg += ", looked up as " + lookedUpAs
	}
	if unknown > 0 {
		msg += fmt.Sprintf("; %d unknown to the PTR", unknown)
	}
	if dropped > 0 {
		msg += fmt.Sprintf("; %d %s not representable", dropped, models.Plural(dropped, "spelling", "spellings"))
	}
	if aliased > 0 {
		msg += fmt.Sprintf("; %d %s skipped, an alias here points elsewhere", aliased, models.Plural(aliased, "relation", "relations"))
	}
	if retired > 0 {
		msg += fmt.Sprintf("; %d %s no longer on the PTR", retired, models.Plural(retired, "relation", "relations"))
	}
	msg += "."
	if unavailable && !cancelled {
		l.jobs.Complete(fmt.Sprintf("PTR lookup stopped at %d/%d: the PTR became unavailable on monloader. %s", processed, total, msg))
		return
	}
	cancelMsg := fmt.Sprintf("PTR lookup cancelled (%d/%d processed)", processed, total)
	if len(fanOutParents) > 0 {
		cancelMsg += "; run it again to apply its implications"
	}
	l.jobs.Finish(nil, cancelled, cancelMsg, msg)
}

// Inline, since the propagation job cannot start while the sweep holds the
// runner; one pass per parent applies its full implied closure. A parent
// stays in the set until its pass completes.
func ptrFanOut(ctx context.Context, svc *tags.Service, parents map[int64]struct{}) {
	for parentID := range parents {
		if ctx.Err() != nil {
			return
		}
		p, err := svc.PropagateImplication(ctx, parentID, nil)
		if err != nil {
			logx.Warnf("ptr lookup fan-out for tag %d: %v", parentID, err)
			continue
		}
		if !p.Cancelled {
			delete(parents, parentID)
		}
	}
}

// An edge that already existed still fans out while some carrier of the
// parent lacks the child, which a failed or cancelled sweep leaves behind.
func markFanOut(svc *tags.Service, fanOutParents map[int64]struct{}, parentID, impliedID int64, isNew bool) {
	if _, ok := fanOutParents[parentID]; ok {
		return
	}
	if !isNew {
		unapplied, err := svc.ImplicationUnapplied(parentID, impliedID)
		if err != nil {
			logx.Warnf("ptr lookup fan-out probe for tag %d: %v", parentID, err)
			return
		}
		if !unapplied {
			return
		}
	}
	fanOutParents[parentID] = struct{}{}
}

func ptrResolve(svc *tags.Service, name string) (catID int64, normalized string, ok bool, err error) {
	catID, bare, ok := svc.ParseForm(name)
	if !ok {
		return 0, "", false, nil
	}
	normalized, err = tags.ValidateTagName(bare)
	return catID, normalized, true, err
}

// The PTR ideal becomes an alias too, since locally this tag stays the
// canonical. An alias only takes an unused name, so the operator's
// catalog wins; AddImplicationFrom guards cycles and aliases.
func applyPTRTagInfo(svc *tags.Service, tagID int64, info contract.PTRTagInfo, supported map[int64]bool, fanOutParents, createdTags map[int64]struct{}, pullImpliedBy bool) (aliases, implications, dropped, aliased, retired int) {
	freshAliases := map[tags.AliasKey]bool{}
	freshImplied := maps.Clone(supported)
	if freshImplied == nil {
		freshImplied = map[int64]bool{}
	}
	aliasNames := info.Aliases
	if info.Ideal != "" {
		aliasNames = append([]string{info.Ideal}, aliasNames...)
	}
	for _, name := range aliasNames {
		catID, normalized, ok, err := ptrResolve(svc, name)
		if !ok || err != nil {
			dropped++
			continue
		}
		freshAliases[tags.AliasKey{CategoryID: catID, Name: normalized}] = true
		exists, err := svc.TagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		if exists {
			continue
		}
		if _, err := svc.CreateAliasFrom(normalized, catID, tagID, "ptr"); err != nil {
			logx.Warnf("ptr alias %q: %v", name, err)
			continue
		}
		aliases++
	}
	for _, name := range info.Implications {
		catID, normalized, ok, err := ptrResolve(svc, name)
		if !ok {
			continue
		}
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		exists, err := svc.TagNameExists(catID, normalized)
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		implied, err := svc.GetOrCreateTagFrom(normalized, catID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if redirected(implied, normalized, catID) {
			aliased++
			continue
		}
		freshImplied[implied.ID] = true
		if !exists {
			createdTags[implied.ID] = struct{}{}
		}
		isNew, err := svc.AddImplicationFrom(tagID, implied.ID, "ptr")
		if err != nil {
			logx.Warnf("ptr implication %q: %v", name, err)
			continue
		}
		if isNew {
			implications++
		}
		markFanOut(svc, fanOutParents, tagID, implied.ID, isNew)
	}
	if pullImpliedBy {
		for _, name := range info.ImpliedBy {
			catID, normalized, ok, err := ptrResolve(svc, name)
			if !ok {
				continue
			}
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			// The parent is not swept: its implications reach this tag's
			// siblings (solo_futanari implies futanari) and would drag
			// the cluster in.
			parent, err := svc.GetOrCreateTagFrom(normalized, catID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if redirected(parent, normalized, catID) {
				aliased++
				continue
			}
			isNew, err := svc.AddImplicationFrom(parent.ID, tagID, "ptr")
			if err != nil {
				logx.Warnf("ptr implied-by %q: %v", name, err)
				continue
			}
			if isNew {
				implications++
			}
			markFanOut(svc, fanOutParents, parent.ID, tagID, isNew)
		}
	}
	retired = syncPTRStaleness(svc, tagID, freshAliases, freshImplied)
	return aliases, implications, dropped, aliased, retired
}

// A child's own answer and its target's implied_by spell a cluster
// differently whenever its ideal is a separate tag here, so an edge stays
// current while either answer lists it.
func (l *Link) ptrImpliedBySupport(ctx context.Context, svc *tags.Service, chunk []ptrLookupCand, results map[int64]contract.PTRTagInfo) (map[int64]map[int64]bool, error) {
	support := map[int64]map[int64]bool{}
	mark := func(child, target int64) {
		if support[child] == nil {
			support[child] = map[int64]bool{}
		}
		support[child][target] = true
	}
	inChunk := make(map[string]int64, len(chunk))
	for _, c := range chunk {
		inChunk[c.name] = c.id
	}
	// The chunk's own implied_by pulls create edges after this runs, so
	// their answers vouch for them directly.
	for _, c := range chunk {
		for _, child := range results[c.id].ImpliedBy {
			if id, ok := inChunk[child]; ok {
				mark(id, c.id)
			}
		}
	}
	type edge struct {
		child, target int64
		childForm     string
	}
	byTarget := map[string][]edge{}
	for _, c := range chunk {
		rows, err := svc.ListImplications(c.id)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			form := tags.FormName(r.ImpliedCategoryName, r.ImpliedName)
			if _, swept := inChunk[form]; r.Origin == contract.SourcePTR && !swept && !slices.Contains(results[c.id].Implications, form) {
				byTarget[form] = append(byTarget[form], edge{c.id, r.ImpliedID, c.name})
			}
		}
	}
	targets := slices.Sorted(maps.Keys(byTarget))
	for start := 0; start < len(targets); start += contract.MaxPTRTags {
		graph, err := l.PTRTagLookup(ctx, targets[start:min(start+contract.MaxPTRTags, len(targets))])
		if err != nil {
			return nil, err
		}
		for form, info := range graph {
			for _, e := range byTarget[form] {
				if slices.Contains(info.ImpliedBy, e.childForm) {
					mark(e.child, e.target)
				}
			}
		}
	}
	return support, nil
}

// A redirected name is an alias here: storing the edge on its canonical
// would assert one neither source declares, which the contribution dialog
// would then offer back as new.
func redirected(got *models.Tag, name string, categoryID int64) bool {
	return got.Name != name || got.CategoryID != categoryID
}

func syncPTRStaleness(svc *tags.Service, tagID int64, freshAliases map[tags.AliasKey]bool, freshImplied map[int64]bool) int {
	retired := 0
	n, err := svc.SyncAliasStaleness(tagID, "ptr", freshAliases)
	if err != nil {
		logx.Warnf("ptr alias staleness for tag %d: %v", tagID, err)
	}
	retired += n
	n, err = svc.SyncImplicationStaleness(tagID, "ptr", freshImplied)
	if err != nil {
		logx.Warnf("ptr implication staleness for tag %d: %v", tagID, err)
	}
	return retired + n
}
