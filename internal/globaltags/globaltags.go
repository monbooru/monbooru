package globaltags

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
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

// MakeGlobal records ids in the source until it ends without error, so a
// run stopped part way can be run again.
func MakeGlobal(ctx context.Context, sides []Side, from string, ids []int64, merge bool) (rep *Report, skipped []Skip, err error) {
	if skipped, err = Push(ctx, sides); err != nil {
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
	src := cats[slices.IndexFunc(cats, func(c *catalog) bool { return c.side.Name == from })].side
	if err := making(src, ids, merge); err != nil {
		return rep, skipped, err
	}
	defer func() {
		if err == nil {
			err = made(src, ids)
		}
	}()
	merged := false
	for i, c := range cats {
		done, skips, err := apply(ctx, c.side, plans[c.side.Name])
		rep.Galleries[i].Counts = done
		skipped = append(skipped, skips...)
		if err != nil {
			return rep, skipped, err
		}
		merged = merged || done.Merged > 0
	}
	if !merged {
		return rep, skipped, nil
	}
	if cats, err = loadAll(sides); err != nil {
		return rep, skipped, err
	}
	if _, plans, err = check(cats, from, ids, merge); err != nil {
		return rep, skipped, err
	}
	for _, c := range cats {
		_, skips, err := apply(ctx, c.side, plans[c.side.Name])
		skipped = append(skipped, skips...)
		if err != nil {
			return rep, skipped, err
		}
	}
	return rep, skipped, nil
}

func making(s Side, ids []int64, merge bool) error {
	tx, err := s.DB.Write.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM global_tag_making WHERE tag_id = ?`, id); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO global_tag_making (tag_id, merge) VALUES (?, ?)`, -id, merge); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}

func made(s Side, ids []int64) error {
	tx, err := s.DB.Write.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM global_tag_making WHERE tag_id = ?`, -id); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}

// Resume runs again every Make global a gallery still records.
func Resume(ctx context.Context, sides []Side) ([]Skip, error) {
	var skipped []Skip
	for _, s := range sorted(sides) {
		type run struct {
			id    int64
			merge bool
		}
		runs, err := db.QueryAll(s.DB.Read, db.ScanRow(func(r *run) []any { return []any{&r.id, &r.merge} }),
			`SELECT abs(tag_id), merge FROM global_tag_making ORDER BY 1`)
		if err != nil {
			return skipped, err
		}
		if len(runs) == 0 {
			continue
		}
		ids := make([]int64, len(runs))
		merge := false
		for i, r := range runs {
			ids[i], merge = r.id, merge || r.merge
		}
		_, skips, err := MakeGlobal(ctx, sides, s.Name, ids, merge)
		skipped = append(skipped, skips...)
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
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
		if err := s.DB.Read.QueryRow(`SELECT EXISTS (SELECT 1 FROM global_tag_changes WHERE tag_id > 0)
			OR EXISTS (SELECT 1 FROM global_tag_making WHERE tag_id > 0)`).Scan(&n); err == nil && n == 1 {
			return true
		}
	}
	return false
}

var records = []string{"global_tag_changes", "global_tag_removals"}

func Reset(sides []Side) {
	for _, s := range sides {
		_, _ = s.DB.Write.Exec(`DELETE FROM global_tag_applying`)
		_, _ = s.DB.Write.Exec(`DELETE FROM global_tag_changes AS fresh WHERE tag_id > 0
			AND EXISTS (SELECT 1 FROM global_tag_changes taken WHERE taken.tag_id = -fresh.tag_id)`)
		_, _ = s.DB.Write.Exec(`DELETE FROM global_tag_removals AS taken WHERE tag_id < 0
			AND EXISTS (SELECT 1 FROM global_tag_removals fresh WHERE fresh.tag_id = -taken.tag_id
			            AND fresh.kind = taken.kind AND fresh.name = taken.name AND fresh.category = taken.category)`)
		for _, table := range records {
			_, _ = s.DB.Write.Exec(`UPDATE ` + table + ` SET tag_id = -tag_id WHERE tag_id < 0`)
		}
		_, _ = s.DB.Write.Exec(`UPDATE global_tag_making SET tag_id = -tag_id WHERE tag_id < 0`)
	}
}

// claim takes the gallery's records by negating their tag id, so an edit
// made from here on records a row of its own. A record an earlier run took
// and left holds the older name and wins.
func claim(s Side) ([]int64, error) {
	tx, err := s.DB.Write.Begin()
	if err != nil {
		return nil, err
	}
	for _, table := range records {
		if _, err := tx.Exec(`UPDATE OR IGNORE ` + table + ` SET tag_id = -tag_id WHERE tag_id > 0`); err != nil {
			return nil, errors.Join(err, tx.Rollback())
		}
		if _, err := tx.Exec(`DELETE FROM ` + table + ` WHERE tag_id > 0`); err != nil {
			return nil, errors.Join(err, tx.Rollback())
		}
	}
	ids, err := db.QueryAll(tx, db.ScanRow(func(id *int64) []any { return []any{id} }),
		`SELECT -tag_id FROM global_tag_changes WHERE tag_id < 0 ORDER BY 1`)
	if err != nil {
		return nil, errors.Join(err, tx.Rollback())
	}
	return ids, tx.Commit()
}

func plan(cx, cy *catalog, changed []int64) ([]op, []Skip) {
	var ops []op
	var skipped []Skip
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
	return ops, skipped
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
		changed, err := claim(s)
		if err != nil {
			return skipped, err
		}
		if len(changed) == 0 {
			continue
		}
		cx, err := load(s)
		if err != nil {
			return skipped, err
		}
		if settled, err := settle(cx); err != nil {
			return skipped, err
		} else if settled {
			if changed, err = claim(s); err != nil {
				return skipped, err
			}
			if cx, err = load(s); err != nil {
				return skipped, err
			}
		}
		left := map[Key]bool{}
		for _, other := range ordered {
			if other.Name == s.Name {
				continue
			}
			out, err := pushTo(ctx, cx, other, changed)
			skipped = append(skipped, out...)
			if err != nil {
				return skipped, err
			}
			for _, sk := range out {
				left[sk.Tag] = true
			}
		}
		var done []int64
		for _, id := range changed {
			if t := cx.byID[id]; t == nil || !left[t.key] {
				done = append(done, id)
			}
		}
		if err := forget(s, done); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

// pushTo plans a gallery that left something out once more, against what
// the first round wrote there, so a name the same push freed is taken.
func pushTo(ctx context.Context, cx *catalog, dst Side, changed []int64) ([]Skip, error) {
	var out []Skip
	for round := range 2 {
		cy, err := load(dst)
		if err != nil {
			return out, err
		}
		ops, refused := plan(cx, cy, changed)
		if round > 0 && !writes(ops) {
			return refused, nil
		}
		_, skips, err := apply(ctx, dst, ops)
		out = append(refused, skips...)
		if err != nil || len(out) == 0 {
			return out, err
		}
	}
	return out, nil
}

func writes(ops []op) bool {
	return slices.ContainsFunc(ops, func(o op) bool { return o.kind != opFanOut })
}

func forget(s Side, ids []int64) error {
	tx, err := s.DB.Write.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		for _, table := range records {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE tag_id = ?`, -id); err != nil {
				return errors.Join(err, tx.Rollback())
			}
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
	for _, table := range records {
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
