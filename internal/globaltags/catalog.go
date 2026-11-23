package globaltags

import (
	"database/sql"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

type Side struct {
	Name string
	DB   *db.DB
	Tags *tags.Service
}

type Key struct{ Category, Name string }

func (k Key) String() string {
	if k.Category == "general" {
		return k.Name
	}
	return k.Category + ":" + k.Name
}

type row struct {
	id       int64
	key      Key
	alias    bool
	canon    int64
	global   bool
	rejected bool
	carried  bool
	usage    int
	origin   string
}

type note struct{ body, links string }

type removal struct {
	kind string
	key  Key
}

type catalog struct {
	side     Side
	byID     map[int64]*row
	byKey    map[Key]*row
	aliases  map[int64][]*row
	implies  map[int64]map[int64]string
	notes    map[int64]note
	colors   map[string]string
	known    map[int64]Key
	removals map[int64][]removal
}

func load(side Side) (*catalog, error) {
	c := &catalog{
		side:     side,
		byID:     map[int64]*row{},
		byKey:    map[Key]*row{},
		aliases:  map[int64][]*row{},
		implies:  map[int64]map[int64]string{},
		notes:    map[int64]note{},
		colors:   map[string]string{},
		known:    map[int64]Key{},
		removals: map[int64][]removal{},
	}
	q := side.DB.Read
	rows, err := db.QueryAll(q, func(r *sql.Rows) (*row, error) {
		t := &row{}
		return t, r.Scan(&t.id, &t.key.Name, &t.key.Category, &t.alias, &t.canon, &t.global, &t.rejected, &t.usage, &t.origin, &t.carried)
	}, `SELECT t.id, t.name, c.name, t.is_alias, COALESCE(t.canonical_tag_id, 0), t.is_global, t.rejected, t.usage_count, t.origin,
	           EXISTS (SELECT 1 FROM image_tags it WHERE it.tag_id = t.id)
	      FROM tags t JOIN tag_categories c ON c.id = t.category_id ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		c.byID[t.id] = t
		c.byKey[t.key] = t
		if t.alias {
			c.aliases[t.canon] = append(c.aliases[t.canon], t)
		}
	}
	type edge struct {
		parent, implied int64
		origin          string
	}
	edges, err := db.QueryAll(q, db.ScanRow(func(e *edge) []any { return []any{&e.parent, &e.implied, &e.origin} }),
		`SELECT parent_tag_id, implied_tag_id, origin FROM tag_implications ORDER BY parent_tag_id, implied_tag_id`)
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		if c.implies[e.parent] == nil {
			c.implies[e.parent] = map[int64]string{}
		}
		c.implies[e.parent][e.implied] = e.origin
	}
	type noteRow struct {
		id int64
		n  note
	}
	notes, err := db.QueryAll(q, db.ScanRow(func(n *noteRow) []any { return []any{&n.id, &n.n.body, &n.n.links} }),
		`SELECT tag_id, body, links FROM tag_notes`)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		c.notes[n.id] = n.n
	}
	type cat struct{ name, color string }
	cats, err := db.QueryAll(q, db.ScanRow(func(x *cat) []any { return []any{&x.name, &x.color} }), `SELECT name, color FROM tag_categories`)
	if err != nil {
		return nil, err
	}
	for _, x := range cats {
		c.colors[x.name] = x.color
	}
	type change struct {
		id  int64
		key Key
	}
	changes, err := db.QueryAll(q, db.ScanRow(func(x *change) []any { return []any{&x.id, &x.key.Name, &x.key.Category} }),
		`SELECT tag_id, name, category FROM global_tag_changes ORDER BY tag_id`)
	if err != nil {
		return nil, err
	}
	for _, x := range changes {
		c.known[x.id] = x.key
	}
	type gone struct {
		id int64
		r  removal
	}
	removed, err := db.QueryAll(q, db.ScanRow(func(x *gone) []any { return []any{&x.id, &x.r.kind, &x.r.key.Name, &x.r.key.Category} }),
		`SELECT tag_id, kind, name, category FROM global_tag_removals ORDER BY tag_id, kind, category, name`)
	if err != nil {
		return nil, err
	}
	for _, x := range removed {
		c.removals[x.id] = append(c.removals[x.id], x.r)
	}
	return c, nil
}

func (c *catalog) knownAs(t *row) Key {
	if k, ok := c.known[t.id]; ok {
		return k
	}
	return t.key
}

func (c *catalog) reaches(from, to int64) bool {
	seen := map[int64]bool{from: true}
	queue := []int64{from}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == to {
			return true
		}
		for next := range c.implies[id] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func (c *catalog) closure(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	queue := append([]int64(nil), ids...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		for next := range c.implies[id] {
			queue = append(queue, next)
		}
	}
	return out
}
