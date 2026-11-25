package globaltags

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/tags"
)

type Counts struct {
	Tags, Aliases, Merged, Implications, Removed, Notes int
}

func (c Counts) Any() bool { return c != Counts{} }

type Skip struct {
	Gallery string
	Tag     Key
	Reason  string
}

func phase(k opKind) int {
	switch k {
	case opCreate, opRename, opMark, opLocal:
		return 0
	case opMerge, opAlias, opUnalias:
		return 1
	case opImply, opUnimply, opFanOut:
		return 2
	}
	return 3
}

func count(ops []op) Counts {
	var c Counts
	for _, o := range ops {
		switch o.kind {
		case opCreate:
			c.Tags++
		case opAlias:
			c.Aliases++
		case opMerge:
			c.Merged++
		case opImply:
			c.Implications++
		case opUnalias, opUnimply:
			c.Removed++
		case opNote:
			c.Notes++
		}
	}
	return c
}

type applier struct {
	dst     Side
	renamed map[string]string
}

func (a applier) id(k Key) (int64, error) {
	var id int64
	err := a.dst.DB.Write.QueryRow(
		`SELECT t.id FROM tags t JOIN tag_categories c ON c.id = t.category_id WHERE c.name = ? AND t.name = ?`,
		k.Category, k.Name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("%s is not a tag in %s", k, a.dst.Name)
	}
	return id, nil
}

func (a applier) category(name, color string) (int64, error) {
	if id, ok, err := tags.CategoryIDByName(a.dst.DB, name); err != nil || ok {
		return id, err
	}
	if color == "" {
		color = "#888888"
	}
	cat, err := a.dst.Tags.CreateCategory(name, color)
	if err != nil {
		return 0, err
	}
	return cat.ID, nil
}

func (a applier) renameCategory(from, to string) (bool, error) {
	if _, taken, err := tags.CategoryIDByName(a.dst.DB, to); err != nil || taken {
		return false, err
	}
	id, _, err := tags.CategoryIDByName(a.dst.DB, from)
	if err != nil {
		return false, err
	}
	var local int
	if err := a.dst.DB.Write.QueryRow(`SELECT COUNT(*) FROM tags t WHERE t.category_id = ? AND t.is_global = 0
		AND NOT EXISTS (SELECT 1 FROM tags g WHERE g.id = t.canonical_tag_id AND g.is_global = 1)`, id).Scan(&local); err != nil || local > 0 {
		return false, err
	}
	if err := a.dst.Tags.RenameCategory(id, to); err != nil {
		if errors.Is(err, tags.ErrBuiltinCategoryName) {
			return false, nil
		}
		return false, err
	}
	a.renamed[from] = to
	return true, nil
}

func (a applier) ensure(k Key, origin, color string) (int64, error) {
	if id, err := a.id(k); err == nil {
		return id, nil
	}
	catID, err := a.category(k.Category, color)
	if err != nil {
		return 0, err
	}
	t, err := a.dst.Tags.GetOrCreateTagFrom(k.Name, catID, origin)
	if err != nil {
		return 0, err
	}
	return t.ID, a.dst.Tags.SetGlobal([]int64{t.ID}, true)
}

type sweep struct {
	parent  int64
	closure []int64
}

type walks struct {
	propagate map[int64]bool
	sweeps    []sweep
}

func (a applier) run(o op, w *walks) error {
	svc := a.dst.Tags
	switch o.kind {
	case opCreate:
		_, err := a.ensure(o.tag, o.origin, o.color)
		return err
	case opRename:
		from := o.from
		if to, ok := a.renamed[from.Category]; ok {
			from.Category = to
		}
		id, err := a.id(from)
		if err != nil {
			return err
		}
		if from.Category != o.tag.Category && o.categoryGone {
			moved, err := a.renameCategory(from.Category, o.tag.Category)
			if err != nil {
				return err
			}
			if moved {
				from.Category = o.tag.Category
			}
		}
		if from == o.tag {
			return nil
		}
		if from.Category == o.tag.Category {
			return svc.RenameTag(id, o.tag.Name)
		}
		catID, err := a.category(o.tag.Category, o.color)
		if err != nil {
			return err
		}
		if from.Name == o.tag.Name {
			return svc.ChangeTagCategory(id, catID)
		}
		return svc.RenameAndMoveTag(id, o.tag.Name, catID)
	case opMark, opLocal:
		id, err := a.id(o.tag)
		if err != nil {
			return err
		}
		return svc.SetGlobal([]int64{id}, o.kind == opMark)
	case opAlias:
		canon, err := a.id(o.tag)
		if err != nil {
			return err
		}
		catID, err := a.category(o.other.Category, o.color)
		if err != nil {
			return err
		}
		alias, err := svc.CreateAliasFrom(o.other.Name, catID, canon, o.origin)
		if err != nil {
			return err
		}
		_, err = a.dst.DB.Write.Exec(`UPDATE tags SET is_global = 0 WHERE id = ?`, alias.ID)
		return err
	case opMerge:
		canon, err := a.id(o.tag)
		if err != nil {
			return err
		}
		alias, err := a.id(o.other)
		if err != nil {
			return err
		}
		if err := svc.MergeTags(alias, canon); err != nil {
			return err
		}
		_, err = a.dst.DB.Write.Exec(`UPDATE tags SET is_global = 0 WHERE id = ?`, alias)
		return err
	case opUnalias:
		alias, err := a.id(o.other)
		if err != nil {
			return err
		}
		return svc.DeleteTag(alias)
	case opImply:
		parent, err := a.id(o.tag)
		if err != nil {
			return err
		}
		implied, err := a.ensure(o.other, o.origin, o.color)
		if err != nil {
			return err
		}
		if _, err := svc.AddImplicationFrom(parent, implied, o.origin); err != nil {
			return err
		}
		w.propagate[parent] = true
		return nil
	case opFanOut:
		parent, err := a.id(o.tag)
		if err != nil {
			return nil
		}
		implied, err := a.id(o.other)
		if err != nil {
			return nil
		}
		unapplied, err := svc.ImplicationUnapplied(parent, implied)
		if unapplied {
			w.propagate[parent] = true
		}
		return err
	case opUnimply:
		parent, err := a.id(o.tag)
		if err != nil {
			return err
		}
		implied, err := a.id(o.other)
		if err != nil {
			return err
		}
		closure, err := svc.ImplicationClosure(implied)
		if err != nil {
			return err
		}
		if err := svc.RemoveImplication(parent, implied); err != nil {
			if errors.Is(err, tags.ErrImplicationNotFound) {
				return nil
			}
			return err
		}
		w.sweeps = append(w.sweeps, sweep{parent, closure})
		return nil
	case opNote:
		id, err := a.id(o.tag)
		if err != nil {
			return err
		}
		var links []string
		if o.note.links != "" {
			links = strings.Split(o.note.links, "\n")
		}
		return svc.SetTagNote(id, o.note.body, links)
	}
	return nil
}

func apply(ctx context.Context, dst Side, ops []op) (Counts, []Skip, error) {
	var done Counts
	if len(ops) == 0 {
		return done, nil, nil
	}
	if _, err := dst.DB.Write.Exec(`INSERT OR IGNORE INTO global_tag_applying (id) VALUES (1)`); err != nil {
		return done, nil, err
	}
	quiet := true
	release := func() {
		if quiet {
			_, _ = dst.DB.Write.Exec(`DELETE FROM global_tag_applying`)
			quiet = false
		}
	}
	defer release()

	ops = slices.Clone(ops)
	slices.SortStableFunc(ops, func(x, y op) int { return phase(x.kind) - phase(y.kind) })
	a := applier{dst: dst, renamed: map[string]string{}}
	w := &walks{propagate: map[int64]bool{}}
	var skipped []Skip
	var applied []op
	for _, o := range ops {
		if err := ctx.Err(); err != nil {
			return count(applied), skipped, err
		}
		if err := a.run(o, w); err != nil {
			skipped = append(skipped, Skip{Gallery: dst.Name, Tag: o.tag, Reason: err.Error()})
			continue
		}
		applied = append(applied, o)
	}
	release()
	for _, sw := range w.sweeps {
		if p, err := dst.Tags.SweepImplicationRemoval(ctx, sw.parent, sw.closure, nil); err != nil {
			return count(applied), skipped, err
		} else if p.Cancelled {
			return count(applied), skipped, context.Canceled
		}
	}
	parents := make([]int64, 0, len(w.propagate))
	for id := range w.propagate {
		parents = append(parents, id)
	}
	slices.Sort(parents)
	for _, id := range parents {
		if p, err := dst.Tags.PropagateImplication(ctx, id, nil); err != nil {
			return count(applied), skipped, err
		} else if p.Cancelled {
			return count(applied), skipped, context.Canceled
		}
	}
	return count(applied), skipped, nil
}
