package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/api"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/shared/webui"
)

// The folder fields list the active gallery's folders as its sidebar does,
// under the viewer's rating ceiling, so a hidden folder is not named.
const folderPickerRoute = "/internal/folder-picker"

var folderPickerKinds = map[string]webui.FolderPicker{
	"upload": {
		Route: folderPickerRoute, Kind: "upload", ID: "upload-folder", Name: "folder", Label: "Folder",
		NewHint: "Not in the gallery yet; the upload creates it.",
	},
	// The move dialogs take a name template, so a folder they do not hold
	// yet may be one the tokens build: no hint for it.
	"batch-place": {
		Route: folderPickerRoute, Kind: "batch-place", ID: "batch-place-folder", Name: "folder", Label: "folder", Blank: "(gallery root)",
	},
	"place-image": {
		Route: folderPickerRoute, Kind: "place-image", ID: "place-image-folder", Name: "folder", Label: "folder", Blank: "(gallery root)",
	},
}

var errFolderGone = errors.New("folder not found")

// The field for kind. It reads no tree: the panel's own requests do, once
// opened, as the sidebar reads none while its section is closed, and a page
// render after every upload would otherwise rebuild it.
func (s *Server) folderPicker(kind string) webui.FolderPicker {
	p := folderPickerKinds[kind]
	p.Listed = true
	if kind == "upload" {
		p.Blank = "(gallery root)"
		if folder := strings.TrimSpace(s.cfgSnapshot().Gallery.DefaultUploadFolder); folder != "" {
			p.Blank = "(default: " + folder + ")"
		}
	}
	return p
}

// The picker for the request's kind and the active gallery's tree; false
// once answered.
func (s *Server) folderPickerTree(w http.ResponseWriter, r *http.Request) (webui.FolderPicker, []gallery.FolderNode, bool) {
	p, ok := folderPickerKinds[r.URL.Query().Get("kind")]
	if !ok {
		http.NotFound(w, r)
		return p, nil, false
	}
	cx := s.active()
	if cx == nil {
		return p, nil, false
	}
	tree, err := cx.FolderTreeUnder(resolveCeiling(r, cx))
	if err != nil {
		logx.Errorf("folder picker: %v", err)
		hxRefuse(w, r, http.StatusInternalServerError, "The folder list failed.")
		return p, nil, false
	}
	return p, tree, true
}

// A page of the panel; a path no visible image sits under answers no rows,
// as the folder can go away between the render and the click.
func (s *Server) folderPickerLevel(w http.ResponseWriter, r *http.Request) {
	p, tree, ok := s.folderPickerTree(w, r)
	if !ok {
		return
	}
	read := func(path string, page int) (webui.FolderPage, error) {
		node := gallery.FindFolder(tree, path)
		if node == nil {
			return webui.FolderPage{}, errFolderGone
		}
		kids := node.ChildrenPage((page-1)*contract.FolderPageSize, contract.FolderPageSize)
		out := make([]webui.FolderEntry, len(kids))
		for i := range kids {
			out[i] = webui.FolderEntry(api.FolderOf(&kids[i]))
		}
		return webui.FolderPage{Total: len(node.Children), Limit: contract.FolderPageSize, Results: out}, nil
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	lv, err := p.FolderLevelOf(read, strings.Trim(q.Get("path"), "/"), max(page, 1), strings.TrimSpace(r.FormValue(p.Name)))
	if err != nil {
		return
	}
	s.renderTemplate(w, "webui/folder_picker_rows", lv)
}

func (s *Server) folderPickerSuggest(w http.ResponseWriter, r *http.Request) {
	p, tree, ok := s.folderPickerTree(w, r)
	if !ok {
		return
	}
	found := gallery.SearchFolders(tree, r.FormValue(p.Name), contract.FolderSearchLimit)
	rows := make([]webui.FolderRow, len(found))
	for i, n := range found {
		rows[i] = webui.FolderRowOf(webui.FolderEntry(api.FolderOf(n)), "")
	}
	s.renderTemplate(w, "webui/folder_picker_suggest", rows)
}
