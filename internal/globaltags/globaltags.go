package globaltags

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

type GalleryPlan struct {
	Name string
	Counts
}

type Report struct {
	Global     []Key
	ComesAlong []Key
	Galleries  []GalleryPlan
	Local      []Conflict
}

type ErrImplied struct {
	Tag     Key
	Parents []Key
}

func (e *ErrImplied) Error() string {
	names := make([]string, len(e.Parents))
	for i, p := range e.Parents {
		names[i] = p.String()
	}
	return fmt.Sprintf("%s stays global while %s implies it", e.Tag, strings.Join(names, ", "))
}

func sorted(sides []Side) []Side {
	out := slices.Clone(sides)
	slices.SortFunc(out, func(a, b Side) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func loadAll(sides []Side) ([]*catalog, error) {
	cats := make([]*catalog, 0, len(sides))
	for _, s := range sorted(sides) {
		c, err := load(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		cats = append(cats, c)
	}
	return cats, nil
}

func sortKeys(keys []Key) {
	slices.SortFunc(keys, func(a, b Key) int {
		return cmp.Or(cmp.Compare(a.Category, b.Category), cmp.Compare(a.Name, b.Name))
	})
}

func live(c *catalog, k Key) *row {
	if r := c.byKey[k]; r != nil && !r.alias && !r.rejected {
		return r
	}
	return nil
}

type opID struct {
	kind  opKind
	tag   Key
	other Key
}

func check(cats []*catalog, from string, ids []int64, merge bool) (*Report, map[string][]op, error) {
	var src *catalog
	for _, c := range cats {
		if c.side.Name == from {
			src = c
		}
	}
	if src == nil {
		return nil, nil, fmt.Errorf("unknown gallery %q", from)
	}

	asked := map[Key]bool{}
	var start []int64
	for _, id := range ids {
		t := src.byID[id]
		if t != nil && t.alias {
			t = src.byID[t.canon]
		}
		if t == nil || t.rejected || asked[t.key] {
			continue
		}
		asked[t.key] = true
		start = append(start, t.id)
	}

	set := map[Key]bool{}
	for _, id := range src.closure(start) {
		set[src.byID[id].key] = true
	}
	for grew := true; grew; {
		grew = false
		for _, c := range cats {
			var have []int64
			for k := range set {
				if r := live(c, k); r != nil {
					have = append(have, r.id)
				}
			}
			for _, id := range c.closure(have) {
				if k := c.byID[id].key; !set[k] {
					set[k] = true
					grew = true
				}
			}
		}
	}

	rep := &Report{}
	bad := map[Key]bool{}
	seen := map[string]bool{}
	refuse := func(c Conflict) {
		bad[c.Tag] = true
		if id := c.Gallery + "\x00" + c.Tag.String() + "\x00" + c.Reason; !seen[id] {
			seen[id] = true
			rep.Local = append(rep.Local, c)
		}
	}
	plans := map[string]map[opID]op{}
	for _, c := range cats {
		plans[c.side.Name] = map[opID]op{}
	}
	keys := make([]Key, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sortKeys(keys)

	for _, x := range cats {
		for _, k := range keys {
			t := live(x, k)
			if t == nil {
				continue
			}
			for _, y := range cats {
				if y == x {
					continue
				}
				ops, conflicts := planTag(x, y, t, planOpts{merge: merge})
				for _, c := range conflicts {
					refuse(c)
				}
				for _, o := range ops {
					if o.kind != opNote {
						plans[y.side.Name][opID{o.kind, o.tag, o.other}] = o
					}
				}
			}
		}
	}
	for _, k := range keys {
		var winner note
		order := append([]*catalog{src}, cats...)
		for _, c := range order {
			if r := live(c, k); r != nil && c.notes[r.id] != (note{}) {
				winner = c.notes[r.id]
				break
			}
		}
		for _, c := range cats {
			r := live(c, k)
			if r != nil && !r.global {
				plans[c.side.Name][opID{opMark, k, Key{}}] = op{kind: opMark, tag: k}
			}
			var has note
			if r != nil {
				has = c.notes[r.id]
			}
			if winner != has {
				plans[c.side.Name][opID{opNote, k, Key{}}] = op{kind: opNote, tag: k, note: winner}
			}
		}
	}

	for grew := true; grew; {
		grew = false
		for _, c := range cats {
			for _, k := range keys {
				r := live(c, k)
				if r == nil || bad[k] {
					continue
				}
				for v := range c.implies[r.id] {
					if implied := c.byID[v]; implied != nil && bad[implied.key] {
						refuse(Conflict{Gallery: c.side.Name, Tag: k, Reason: fmt.Sprintf("it implies %s, which stays local", implied.key)})
						grew = true
						break
					}
				}
			}
		}
	}

	out := map[string][]op{}
	for _, c := range cats {
		var ops []op
		for id, o := range plans[c.side.Name] {
			if bad[id.tag] || (o.kind == opImply && bad[o.other]) {
				continue
			}
			ops = append(ops, o)
		}
		slices.SortFunc(ops, func(a, b op) int {
			return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.tag.String(), b.tag.String()), cmp.Compare(a.other.String(), b.other.String()))
		})
		out[c.side.Name] = ops
		rep.Galleries = append(rep.Galleries, GalleryPlan{Name: c.side.Name, Counts: count(ops)})
	}
	for _, k := range keys {
		if bad[k] {
			continue
		}
		rep.Global = append(rep.Global, k)
		if r := live(src, k); !asked[k] && (r == nil || !r.global) {
			rep.ComesAlong = append(rep.ComesAlong, k)
		}
	}
	slices.SortFunc(rep.Local, func(a, b Conflict) int {
		return cmp.Or(cmp.Compare(a.Tag.String(), b.Tag.String()), cmp.Compare(a.Gallery, b.Gallery), cmp.Compare(a.Reason, b.Reason))
	})
	return rep, out, nil
}

func Check(sides []Side, from string, ids []int64, merge bool) (*Report, error) {
	cats, err := loadAll(sides)
	if err != nil {
		return nil, err
	}
	rep, _, err := check(cats, from, ids, merge)
	return rep, err
}

func MakeGlobal(ctx context.Context, sides []Side, from string, ids []int64, merge bool) (*Report, []Skip, error) {
	skipped, err := Push(ctx, sides)
	if err != nil {
		return nil, skipped, err
	}
	cats, err := loadAll(sides)
	if err != nil {
		return nil, skipped, err
	}
	rep, plans, err := check(cats, from, ids, merge)
	if err != nil {
		return nil, skipped, err
	}
	for i, c := range cats {
		done, skips, err := apply(ctx, c.side, plans[c.side.Name])
		rep.Galleries[i].Counts = done
		skipped = append(skipped, skips...)
		if err != nil {
			return rep, skipped, err
		}
	}
	return rep, skipped, nil
}

func MakeLocal(side Side, ids []int64) error {
	c, err := load(side)
	if err != nil {
		return err
	}
	var local []int64
	for _, id := range ids {
		t := c.byID[id]
		if t != nil && t.alias {
			t = c.byID[t.canon]
		}
		if t == nil || !t.global {
			continue
		}
		var parents []Key
		for parent, implied := range c.implies {
			if _, ok := implied[t.id]; ok && c.byID[parent] != nil && c.byID[parent].global && !slices.Contains(ids, parent) {
				parents = append(parents, c.byID[parent].key)
			}
		}
		if len(parents) > 0 {
			sortKeys(parents)
			return &ErrImplied{Tag: t.key, Parents: parents}
		}
		local = append(local, t.id)
	}
	return side.Tags.SetGlobal(local, false)
}

func Pending(sides []Side) bool {
	for _, s := range sides {
		var n int
		if err := s.DB.Read.QueryRow(`SELECT EXISTS (SELECT 1 FROM global_tag_changes)`).Scan(&n); err == nil && n == 1 {
			return true
		}
	}
	return false
}

func Reset(sides []Side) {
	for _, s := range sides {
		_, _ = s.DB.Write.Exec(`DELETE FROM global_tag_applying`)
	}
}

func settle(c *catalog) (bool, error) {
	var mark []int64
	var stray []int64
	for _, t := range c.byID {
		if t.alias && t.global {
			stray = append(stray, t.id)
			if canon := c.byID[t.canon]; canon != nil && !canon.global {
				mark = append(mark, canon.id)
			}
		}
	}
	var roots []int64
	for _, t := range c.byID {
		if t.global && !t.alias {
			roots = append(roots, t.id)
		}
	}
	for _, id := range c.closure(append(roots, mark...)) {
		if t := c.byID[id]; !t.global && !t.alias && !t.rejected && !slices.Contains(mark, id) {
			mark = append(mark, id)
		}
	}
	if len(mark) == 0 && len(stray) == 0 {
		return false, nil
	}
	slices.Sort(mark)
	if err := c.side.Tags.SetGlobal(mark, true); err != nil {
		return false, err
	}
	_, err := c.side.DB.Write.Exec(`UPDATE tags SET is_global = 0 WHERE is_alias = 1 AND is_global = 1`)
	return true, err
}

func Push(ctx context.Context, sides []Side) ([]Skip, error) {
	var skipped []Skip
	ordered := sorted(sides)
	for _, s := range ordered {
		cx, err := load(s)
		if err != nil {
			return skipped, err
		}
		if len(cx.known) == 0 {
			continue
		}
		if changed, err := settle(cx); err != nil {
			return skipped, err
		} else if changed {
			if cx, err = load(s); err != nil {
				return skipped, err
			}
		}
		changed := make([]int64, 0, len(cx.known))
		for id := range cx.known {
			changed = append(changed, id)
		}
		slices.Sort(changed)
		for _, other := range ordered {
			if other.Name == s.Name {
				continue
			}
			cy, err := load(other)
			if err != nil {
				return skipped, err
			}
			var ops []op
			for _, id := range changed {
				t := cx.byID[id]
				switch {
				case t != nil && t.alias:
				case t == nil || !t.global:
					d := cy.byKey[cx.known[id]]
					if d == nil || d.alias || !d.global {
						continue
					}
					ops = append(ops, op{kind: opLocal, tag: d.key})
					if t != nil {
						continue
					}
					for parent, implied := range cy.implies {
						if _, ok := implied[d.id]; ok && cy.byID[parent] != nil && cy.byID[parent].global {
							ops = append(ops, op{kind: opUnimply, tag: cy.byID[parent].key, other: d.key})
						}
					}
				default:
					planned, conflicts := planTag(cx, cy, t, planOpts{removes: true})
					ops = append(ops, planned...)
					for _, c := range conflicts {
						skipped = append(skipped, Skip{Gallery: c.Gallery, Tag: c.Tag, Reason: c.Reason})
					}
				}
			}
			_, skips, err := apply(ctx, other, ops)
			skipped = append(skipped, skips...)
			if err != nil {
				return skipped, err
			}
		}
		if err := forget(s, changed); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func forget(s Side, ids []int64) error {
	tx, err := s.DB.Write.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM global_tag_changes WHERE tag_id = ?`, id); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		if _, err := tx.Exec(`DELETE FROM global_tag_removals WHERE tag_id = ?`, id); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}

func Pull(ctx context.Context, sides []Side, into string) ([]Skip, error) {
	ordered := sorted(sides)
	var dst, src *Side
	for i := range ordered {
		switch {
		case ordered[i].Name == into:
			dst = &ordered[i]
		case src == nil:
			src = &ordered[i]
		}
	}
	if dst == nil || src == nil {
		return nil, nil
	}
	for _, table := range []string{"global_tag_changes", "global_tag_removals"} {
		if _, err := dst.DB.Write.Exec(`DELETE FROM ` + table); err != nil {
			return nil, err
		}
	}
	cs, err := load(*src)
	if err != nil {
		return nil, err
	}
	ci, err := load(*dst)
	if err != nil {
		return nil, err
	}
	var ops []op
	var skipped []Skip
	ids := make([]int64, 0, len(cs.byID))
	for id := range cs.byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t := cs.byID[id]
		if !t.global || t.alias {
			continue
		}
		planned, conflicts := planTag(cs, ci, t, planOpts{})
		ops = append(ops, planned...)
		for _, c := range conflicts {
			skipped = append(skipped, Skip{Gallery: c.Gallery, Tag: c.Tag, Reason: c.Reason})
		}
	}
	_, skips, err := apply(ctx, *dst, ops)
	skipped = append(skipped, skips...)
	if err != nil {
		return skipped, err
	}
	var own []int64
	for _, d := range ci.byID {
		if d.global && !d.alias {
			own = append(own, d.id)
		}
	}
	if len(own) == 0 {
		return skipped, nil
	}
	slices.Sort(own)
	_, skips, err = MakeGlobal(ctx, sides, into, own, false)
	return append(skipped, skips...), err
}

func Any(sides []Side) bool {
	for _, s := range sides {
		var n int
		if err := s.DB.Read.QueryRow(`SELECT EXISTS (SELECT 1 FROM tags WHERE is_global = 1)`).Scan(&n); err == nil && n == 1 {
			return true
		}
	}
	return false
}
