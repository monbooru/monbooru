package globaltags

import (
	"fmt"
	"slices"
)

type opKind int

const (
	opCreate opKind = iota
	opRename
	opMark
	opLocal
	opMerge
	opAlias
	opUnalias
	opImply
	opUnimply
	opFanOut
	opNote
)

type op struct {
	kind   opKind
	tag    Key
	from   Key
	other  Key
	origin string
	color  string
	note   note
}

type Conflict struct {
	Gallery   string
	Tag       Key
	Reason    string
	Mergeable bool
}

type planOpts struct {
	merge   bool
	removes bool
}

func planTag(src, dst *catalog, t *row, opts planOpts) ([]op, []Conflict) {
	var ops []op
	var conflicts []Conflict
	refuse := func(mergeable bool, format string, args ...any) {
		conflicts = append(conflicts, Conflict{Gallery: dst.side.Name, Tag: t.key, Reason: fmt.Sprintf(format, args...), Mergeable: mergeable})
	}
	in := dst.side.Name

	d, now := dst.byKey[src.knownAs(t)], dst.byKey[t.key]
	if d == nil || (!d.global && (now != nil || (!d.alias && !d.rejected))) {
		d = now
	}
	renamed := false
	switch {
	case d == nil:
		ops = append(ops, op{kind: opCreate, tag: t.key, origin: t.origin, color: src.colors[t.key.Category]})
	case d.alias:
		refuse(false, "%s is an alias of %s in %s", d.key, canonKey(dst, d), in)
		return nil, conflicts
	case d.rejected:
		refuse(false, "%s is rejected in %s", d.key, in)
		return nil, conflicts
	default:
		if d.key != t.key {
			if taken := dst.byKey[t.key]; taken != nil && taken != d {
				refuse(false, "%s is already a name in %s", t.key, in)
				return nil, conflicts
			}
			ops = append(ops, op{kind: opRename, tag: t.key, from: d.key, color: src.colors[t.key.Category]})
			renamed = true
		}
		if !d.global {
			ops = append(ops, op{kind: opMark, tag: t.key})
		}
	}

	for _, a := range src.aliases[t.id] {
		x := dst.byKey[a.key]
		switch {
		case x == nil, x == d && renamed:
			ops = append(ops, op{kind: opAlias, tag: t.key, other: a.key, origin: a.origin, color: src.colors[a.key.Category]})
		case x.alias && d != nil && x.canon == d.id:
		case x.alias:
			refuse(false, "its alias %s is an alias of %s in %s", a.key, canonKey(dst, x), in)
		case x.rejected:
			refuse(false, "its alias %s is rejected in %s", a.key, in)
		case x == d:
			refuse(false, "its alias %s is the tag itself in %s", a.key, in)
		case (x.carried || owns(dst, x)) && (x.global || opts.merge):
			ops = append(ops, op{kind: opMerge, tag: t.key, other: a.key})
		case x.carried:
			refuse(true, "its alias %s is a tag on %s in %s", a.key, images(x.usage), in)
		case owns(dst, x):
			refuse(true, "its alias %s is a tag with aliases, implications or a note of its own in %s", a.key, in)
		default:
			ops = append(ops, op{kind: opAlias, tag: t.key, other: a.key, origin: a.origin, color: src.colors[a.key.Category]})
		}
	}

	for _, v := range sortedIDs(src.implies[t.id]) {
		vr := src.byID[v]
		if vr == nil {
			continue
		}
		y := dst.byKey[src.knownAs(vr)]
		if y == nil {
			y = dst.byKey[vr.key]
		}
		switch {
		case y != nil && y.alias:
			refuse(false, "it implies %s, an alias of %s in %s", vr.key, canonKey(dst, y), in)
		case y != nil && y.rejected:
			refuse(false, "it implies %s, rejected in %s", vr.key, in)
		case y != nil && d != nil && hasEdge(dst, d.id, y.id):
			ops = append(ops, op{kind: opFanOut, tag: t.key, other: vr.key})
		case y != nil && d != nil && dst.reaches(y.id, d.id):
			refuse(false, "implying %s would close a loop in %s", vr.key, in)
		default:
			ops = append(ops, op{kind: opImply, tag: t.key, other: vr.key, origin: src.implies[t.id][v], color: src.colors[vr.key.Category]})
		}
	}

	sn := src.notes[t.id]
	var dn note
	if d != nil {
		dn = dst.notes[d.id]
	}
	if sn != dn && (sn != (note{}) || (d != nil && d.global)) {
		ops = append(ops, op{kind: opNote, tag: t.key, note: sn})
	}

	if opts.removes && d != nil {
		for _, rm := range src.removals[t.id] {
			switch rm.kind {
			case "alias":
				if s := src.byKey[rm.key]; s != nil && s.alias && s.canon == t.id {
					continue
				}
				if x := dst.byKey[rm.key]; x != nil && x.alias && x.canon == d.id {
					ops = append(ops, op{kind: opUnalias, tag: t.key, other: rm.key})
				}
			case "implies":
				if s := src.byKey[rm.key]; s != nil && hasEdge(src, t.id, s.id) {
					continue
				}
				if y := dst.byKey[rm.key]; y != nil && hasEdge(dst, d.id, y.id) {
					ops = append(ops, op{kind: opUnimply, tag: t.key, other: rm.key})
				}
			}
		}
	}
	return ops, conflicts
}

func owns(c *catalog, t *row) bool {
	if len(c.aliases[t.id]) > 0 || len(c.implies[t.id]) > 0 || c.notes[t.id] != (note{}) {
		return true
	}
	for _, implied := range c.implies {
		if _, ok := implied[t.id]; ok {
			return true
		}
	}
	return false
}

func hasEdge(c *catalog, parent, implied int64) bool {
	_, ok := c.implies[parent][implied]
	return ok
}

func canonKey(c *catalog, alias *row) Key {
	if canon := c.byID[alias.canon]; canon != nil {
		return canon.key
	}
	return Key{}
}

func images(n int) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}

func sortedIDs(m map[int64]string) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
