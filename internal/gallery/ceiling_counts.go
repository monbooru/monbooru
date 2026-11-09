package gallery

import (
	"cmp"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
)

// RankCeiling's zero value hides nothing.
type RankCeiling struct {
	Rank   int
	Active bool
}

func (c RankCeiling) and(imagesAlias string) (string, []any) {
	if !c.Active {
		return "", nil
	}
	return ` AND ` + imagesAlias + `.rating_rank <= ?`, []any{c.Rank}
}

func scalarCountUnder(database *db.DB, baseWhere string, ceiling RankCeiling) (int, error) {
	where, args := ceiling.and("i")
	var n int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM images i WHERE `+baseWhere+where, args...,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func InboxCountUnder(database *db.DB, ceiling RankCeiling) (int, error) {
	return scalarCountUnder(database, "i.is_missing = 0 AND i.is_inbox = 1", ceiling)
}

func PhashMissingUnder(database *db.DB, ceiling RankCeiling) (int, error) {
	return scalarCountUnder(database, "i.phash IS NULL AND i.is_missing = 0", ceiling)
}

// SourceLabelCountsUnderQuery counts secondary origins too, since the
// source: filter matches any of an image's sources.
func SourceLabelCountsUnderQuery(database *db.DB, limit int, ceiling RankCeiling) ([]SourceLabelCount, error) {
	under, args := ceiling.and("i")
	args = append(args, limit)
	return db.QueryAll(database.Read, db.ScanRow(func(c *SourceLabelCount) []any {
		return []any{&c.Source, &c.Count}
	}),
		`SELECT s.site, COUNT(DISTINCT s.image_id) c FROM image_sources s
		 WHERE s.site != '' AND EXISTS (SELECT 1 FROM images i WHERE i.id = s.image_id AND i.is_missing = 0`+under+`)
		 GROUP BY s.site ORDER BY c DESC, s.site ASC LIMIT ?`,
		args...)
}

type FolderNode struct {
	Path     string
	Name     string
	Count    int
	Depth    int
	Children []FolderNode
}

// FolderTree's counts include every descendant's, and a folder holding
// only subfolders still gets a node.
func FolderTree(database *db.DB) ([]FolderNode, error) {
	return FolderTreeUnder(database, RankCeiling{})
}

func buildFolderTree(flat []folderCount) []FolderNode {
	known := map[string]bool{"": true}
	for _, fc := range flat {
		known[fc.path] = true
	}
	var toAdd []folderCount
	for _, fc := range flat {
		if fc.path == "" {
			continue
		}
		segments := strings.Split(fc.path, "/")
		for i := 1; i < len(segments); i++ {
			ancestor := strings.Join(segments[:i], "/")
			if !known[ancestor] {
				known[ancestor] = true
				toAdd = append(toAdd, folderCount{path: ancestor, count: 0})
			}
		}
	}
	flat = append(flat, toAdd...)

	// Pointer-tree intermediate so parent-child wiring survives mutations.
	type pnode struct {
		FolderNode
		children []*pnode
	}

	rootP := &pnode{FolderNode: FolderNode{Path: "", Name: "(root)", Depth: 0}}
	pnodeMap := map[string]*pnode{"": rootP}

	// Sort lexicographically so parents always exist before children.
	slices.SortFunc(flat, func(a, b folderCount) int {
		return cmp.Compare(a.path, b.path)
	})

	for _, fc := range flat {
		if fc.path == "" {
			rootP.Count = fc.count
			continue
		}
		// folder_path is "/"-separated on every platform, which
		// filepath.Dir is not.
		name := fc.path
		parentPath := ""
		if i := strings.LastIndex(fc.path, "/"); i >= 0 {
			name = fc.path[i+1:]
			parentPath = fc.path[:i]
		}
		n := &pnode{FolderNode: FolderNode{
			Path:  fc.path,
			Name:  name,
			Count: fc.count,
			Depth: strings.Count(fc.path, "/") + 1,
		}}
		pnodeMap[fc.path] = n

		parent, ok := pnodeMap[parentPath]
		if !ok {
			parent = rootP
		}
		parent.children = append(parent.children, n)
	}

	var rollup func(p *pnode)
	rollup = func(p *pnode) {
		for _, c := range p.children {
			rollup(c)
			p.Count += c.Count
		}
	}
	rollup(rootP)

	var toValue func(p *pnode) FolderNode
	toValue = func(p *pnode) FolderNode {
		n := p.FolderNode
		for _, c := range p.children {
			n.Children = append(n.Children, toValue(c))
		}
		return n
	}

	return []FolderNode{toValue(rootP)}
}

// FindFolder returns the node at path in a tree from FolderTreeUnder, the
// root for "", or nil when no visible image sits at or under it.
func FindFolder(tree []FolderNode, path string) *FolderNode {
	if len(tree) == 0 {
		return nil
	}
	n := &tree[0]
	if path == "" {
		return n
	}
	for end := 0; end < len(path); {
		if i := strings.IndexByte(path[end+1:], '/'); i >= 0 {
			end += 1 + i
		} else {
			end = len(path)
		}
		i, ok := slices.BinarySearchFunc(n.Children, path[:end], func(c FolderNode, p string) int {
			return strings.Compare(c.Path, p)
		})
		if !ok {
			return nil
		}
		n = &n.Children[i]
	}
	return n
}

func FolderTreeUnder(database *db.DB, ceiling RankCeiling) ([]FolderNode, error) {
	where, args := ceiling.and("i")
	flat, err := db.QueryAll(database.Read, db.ScanRow(func(fc *folderCount) []any {
		return []any{&fc.path, &fc.count}
	}),
		`SELECT COALESCE(i.folder_path, ''), COUNT(*) FROM images i
		 WHERE i.is_missing = 0`+where+`
		 GROUP BY i.folder_path ORDER BY i.folder_path`,
		args...)
	if err != nil {
		return nil, err
	}
	return buildFolderTree(flat), nil
}

type folderCount struct {
	path  string
	count int
}
