package api

import (
	"net/http"

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
func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	tree, err := g.FolderTreeUnder(nil)
	if err != nil {
		contract.WriteError(w, http.StatusInternalServerError, "internal_error", "could not read the folders of gallery "+g.Name)
		return
	}
	root := contract.Folder{Children: []contract.Folder{}}
	if len(tree) > 0 {
		root = folderOf(tree[0])
		root.Name = "" // the sidebar's "(root)" is a label, not a name
	}
	contract.WriteJSON(w, http.StatusOK, root)
}

func folderOf(n gallery.FolderNode) contract.Folder {
	f := contract.Folder{Path: n.Path, Name: n.Name, Images: n.Count, Children: make([]contract.Folder, 0, len(n.Children))}
	for _, c := range n.Children {
		f.Children = append(f.Children, folderOf(c))
	}
	return f
}
