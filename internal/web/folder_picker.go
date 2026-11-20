package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/library"
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
}

var errFolderGone = errors.New("folder not found")

// The field for kind on the active gallery; a tree that cannot be read
// leaves it a plain text box.
func (s *Server) folderPicker(r *http.Request, cx *library.Gallery, kind string) webui.FolderPicker {
	p := folderPickerKinds[kind]
	if kind == "upload" {
		p.Blank = "(gallery root)"
		if folder := strings.TrimSpace(s.cfgSnapshot().Gallery.DefaultUploadFolder); folder != "" {
			p.Blank = "(default: " + folder + ")"
		}
	}
	tree, err := cx.FolderTreeUnder(resolveCeiling(r, cx))
	if err != nil {
		logx.Warnf("folder picker: %v", err)
		p.Unavailable = "the folder list failed"
		return p
	}
	p.Listed = true
	p.Held = func(path string) bool { return gallery.FindFolder(tree, path) != nil }
	return p
}

func folderEntryOf(n *gallery.FolderNode) webui.FolderEntry {
	return webui.FolderEntry{Path: n.Path, Name: n.Name, Images: n.Count, Folders: len(n.Children)}
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
		from := min((page-1)*contract.FolderPageSize, len(node.Children))
		kids := node.Children[from:min(from+contract.FolderPageSize, len(node.Children))]
		out := make([]webui.FolderEntry, len(kids))
		for i := range kids {
			out[i] = folderEntryOf(&kids[i])
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
		rows[i] = webui.FolderRowOf(folderEntryOf(n), "")
	}
	s.renderTemplate(w, "webui/folder_picker_suggest", rows)
}
