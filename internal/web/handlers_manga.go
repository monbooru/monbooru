package web

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

type readerData struct {
	baseData
	Image       models.Image
	Page        int
	PageCount   int
	NextPage    int
	BackQS      template.URL
	BackKVQS    template.URL
	BackToPages bool
}

type pagesGridData struct {
	baseData
	Image        models.Image
	Filename     string
	PageCount    int
	LastReadPage int
	TagSidebar   tagSidebar
	BackQuery    string
	BackQS       template.URL
	BackKVQS     template.URL
}

// The stored page is re-checked: a re-ingested archive can shrink, and
// the first or last page means unstarted or finished.
func resumePage(img *models.Image) int {
	if img.LastReadPage == nil || img.PageCount == nil {
		return 0
	}
	page := *img.LastReadPage
	if page <= 1 || page >= *img.PageCount {
		return 0
	}
	return page
}

// On false the 404 is already written.
func (s *Server) loadMangaImage(w http.ResponseWriter, r *http.Request) (*models.Image, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return nil, false
	}
	img, err := loadImage(r.Context(), s.db(), id)
	if err != nil || img.FileType != models.FileTypeCBZ || img.PageCount == nil || *img.PageCount < 1 {
		s.notFoundHandler(w, r)
		return nil, false
	}
	return img, true
}

func (s *Server) readerHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	pageCount := *img.PageCount
	// A bare URL resumes; page 1 would clear the bookmark on every visit.
	page := max(resumePage(img), 1)
	rawPage := ""
	if v := r.URL.Query().Get("page"); v != "" {
		rawPage = v
		if n, err := strconv.Atoi(v); err == nil {
			page = n
		}
	}
	page = min(max(page, 1), pageCount)
	// Redirect before recording, so only canonical URLs move the bookmark.
	if rawPage != "" && rawPage != strconv.Itoa(page) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page))
		http.Redirect(w, r, r.URL.Path+"?"+q.Encode(), http.StatusSeeOther)
		return
	}
	s.recordReaderPosition(img, page, pageCount)
	next := 0
	if page < pageCount {
		next = page + 1
	}
	back := parseBackContext(r)
	backToPages := r.URL.Query().Get("from") == "pages"
	backQS, backKVQS := back.ReaderQS(backToPages)

	data := readerData{
		baseData:    s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Reader - "+s.booruName()),
		Image:       *img,
		Page:        page,
		PageCount:   pageCount,
		NextPage:    next,
		BackQS:      backQS,
		BackKVQS:    backKVQS,
		BackToPages: backToPages,
	}
	s.renderTemplate(w, "reader.html", data)
}

// A write on GET is safe: prefetch fetches page bytes, never this render.
func (s *Server) recordReaderPosition(img *models.Image, page, pageCount int) {
	next := 0
	if page > 1 && page < pageCount {
		next = page
	}
	if next == derefOr(img.LastReadPage) {
		return
	}
	if err := gallery.SetReadPage(s.db(), img.ID, next); err != nil {
		logx.Warnf("record reader position for image %d: %v", img.ID, err)
	}
}

func (s *Server) pagesGridHandler(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	back := parseBackContext(r)
	// false: the grid's back link goes to the detail page, not to itself.
	backQS, backKVQS := back.ReaderQS(false)
	_, imageTags, _ := s.tagSvc().GetImageTags(img.ID)
	base := s.base(r, "gallery", filepath.Base(img.CanonicalPath)+" - Pages - "+s.booruName())
	data := pagesGridData{
		baseData:     base,
		Image:        *img,
		Filename:     filepath.Base(img.CanonicalPath),
		PageCount:    *img.PageCount,
		LastReadPage: resumePage(img),
		TagSidebar:   s.buildTagSidebar(img.ID, tagModePref.read(r), impliedModePref.read(r), imageTags),
		BackQuery:    back.Q,
		BackQS:       backQS,
		BackKVQS:     backKVQS,
	}
	s.renderTemplate(w, "pages.html", data)
}

// Idempotent: a second extract lands on the row already holding the bytes.
func (s *Server) extractMangaPage(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > *img.PageCount {
		s.notFoundHandler(w, r)
		return
	}
	fail := func(stage string, err error) {
		logx.Warnf("extract page %d of image %d: %s: %v", n, img.ID, stage, err)
		http.Error(w, "Could not extract this page.", http.StatusInternalServerError)
	}
	cx := s.active()
	if cx == nil {
		fail("gallery", fmt.Errorf("no active gallery"))
		return
	}
	writeDir, naming := s.receivedNaming(cx.Name)
	destDir, err := cx.Boundary().ResolveSubdir(writeDir)
	if err != nil {
		fail("resolve folder", err)
		return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		fail("create folder", err)
		return
	}
	stem := strings.TrimSuffix(filepath.Base(img.CanonicalPath), filepath.Ext(img.CanonicalPath))
	pageID, _, err := s.extractMangaPageToGallery(r.Context(), cx, img, n, destDir, stem+"_p", naming)
	if err != nil {
		fail("extract", err)
		return
	}
	hxRedirect(w, r, fmt.Sprintf("/images/%d", pageID))
}

func loadMangaMeta(ctx context.Context, database *db.DB, imageID int64) *models.MangaMetadata {
	var m models.MangaMetadata
	err := database.Read.QueryRowContext(ctx, `
		SELECT image_id, COALESCE(title, ''), COALESCE(series, ''), COALESCE(number, ''), COALESCE(volume, ''), count,
		       COALESCE(summary, ''), COALESCE(notes, ''), year, month, day,
		       COALESCE(writer, ''), COALESCE(penciller, ''), COALESCE(inker, ''), COALESCE(colorist, ''),
		       COALESCE(letterer, ''), COALESCE(cover_artist, ''), COALESCE(editor, ''), COALESCE(publisher, ''),
		       COALESCE(imprint, ''), COALESCE(genre, ''), COALESCE(web, ''), COALESCE(language_iso, ''),
		       COALESCE(format, ''), COALESCE(manga, ''), COALESCE(age_rating, ''), community_rating, xml_page_count,
		       COALESCE(raw_xml, '')
		FROM manga_metadata WHERE image_id = ?`, imageID,
	).Scan(&m.ImageID, &m.Title, &m.Series, &m.Number, &m.Volume, &m.Count, &m.Summary, &m.Notes,
		&m.Year, &m.Month, &m.Day, &m.Writer, &m.Penciller, &m.Inker, &m.Colorist, &m.Letterer, &m.CoverArtist, &m.Editor, &m.Publisher,
		&m.Imprint, &m.Genre, &m.Web, &m.LanguageISO, &m.Format, &m.Manga, &m.AgeRating, &m.CommunityRating, &m.XMLPageCount, &m.RawXML)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		logx.Debugf("loadMangaMeta: %v", err)
		return nil
	}
	return &m
}
