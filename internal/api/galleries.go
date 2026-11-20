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
	offset, limit := parsePage(r, contract.FolderPageSize, contract.FolderPageSize)
	kids := node.Children
	page := kids[min(offset, len(kids)):min(offset+limit, len(kids))]
	out := make([]contract.Folder, len(page))
	for i := range page {
		out[i] = folderOf(&page[i])
	}
	writePage(w, offset/limit+1, limit, len(kids), out)
}

func (h *Handler) searchFolders(w http.ResponseWriter, r *http.Request) {
	tree, ok := h.folderTree(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	_, limit := contract.PageParams("", q.Get("limit"), contract.FolderSearchLimit, contract.FolderSearchMax)
	found := gallery.SearchFolders(tree, q.Get("q"), limit)
	out := make([]contract.Folder, len(found))
	for i, n := range found {
		out[i] = folderOf(n)
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}
