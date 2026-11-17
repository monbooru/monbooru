package api

import (
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	activeName := ""
	if active, ok := h.resolver(""); ok {
		activeName = active.Name
	}
	configured := h.cfg().Galleries
	out := make([]contract.Gallery, 0, len(configured))
	for _, gc := range configured {
		g, ok := h.resolver(gc.Name)
		if !ok {
			continue
		}
		images, imagesOK := g.VisibleCount()
		tags, tagsOK := g.TagCount()
		if !imagesOK || !tagsOK {
			contract.WriteError(w, http.StatusInternalServerError, "internal_error", "could not count gallery "+gc.Name)
			return
		}
		out = append(out, contract.Gallery{Name: gc.Name, Images: images, Tags: tags, Active: gc.Name == activeName})
	}
	contract.WriteJSON(w, http.StatusOK, out)
}

// One level at a time, as the sidebar: a {hash:4} layout holds tens of
// thousands of folders.
const folderPageSize = 500

const (
	folderSearchLimit = 10
	folderSearchMax   = 100
)

// Folders come from the images, so an empty folder on disk is not listed;
// a send to a folder that does not exist yet still creates it.
func (h *Handler) folderTree(w http.ResponseWriter, r *http.Request) ([]gallery.FolderNode, bool) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return nil, false
	}
	tree, err := g.FolderTreeUnder(nil)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "could not read the folders of gallery "+g.Name)
		return nil, false
	}
	return tree, true
}

func folderOf(n *gallery.FolderNode) contract.Folder {
	return contract.Folder{Path: n.Path, Name: n.Name, Images: n.Count, Folders: len(n.Children)}
}

// The subfolders of path, a page at a time, read off the cached tree.
func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	tree, ok := h.folderTree(w, r)
	if !ok {
		return
	}
	path := strings.Trim(strings.TrimSpace(r.URL.Query().Get("path")), "/")
	node := gallery.FindFolder(tree, path)
	if node == nil {
		contract.WriteError(w, http.StatusNotFound, "not_found", "folder not found: "+path)
		return
	}
	offset, limit := parsePage(r, folderPageSize, folderPageSize)
	kids := node.Children
	page := kids[min(offset, len(kids)):min(offset+limit, len(kids))]
	out := make([]contract.Folder, len(page))
	for i := range page {
		out[i] = folderOf(&page[i])
	}
	writePage(w, offset/limit+1, limit, len(kids), out)
}

// Folders whose path holds q, any case, those starting with it first, in
// path order. A walk of the cached tree: no query per keystroke.
func (h *Handler) searchFolders(w http.ResponseWriter, r *http.Request) {
	tree, ok := h.folderTree(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	_, limit := contract.PageParams("", q.Get("limit"), folderSearchLimit, folderSearchMax)
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	starts, holds := []contract.Folder{}, []contract.Folder{}
	var walk func(nodes []gallery.FolderNode) bool
	walk = func(nodes []gallery.FolderNode) bool {
		for i := range nodes {
			n := &nodes[i]
			path := strings.ToLower(n.Path)
			switch {
			case strings.HasPrefix(path, needle):
				starts = append(starts, folderOf(n))
			case len(holds) < limit && strings.Contains(path, needle):
				holds = append(holds, folderOf(n))
			}
			if len(starts) >= limit || !walk(n.Children) {
				return false
			}
		}
		return true
	}
	if root := gallery.FindFolder(tree, ""); needle != "" && root != nil {
		walk(root.Children)
	}
	out := append(starts, holds...)
	contract.WriteJSON(w, http.StatusOK, map[string]any{"results": out[:min(len(out), limit)]})
}
