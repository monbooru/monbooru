package web

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/markup"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The render waits on Back's page; past this it keeps the page in the URL.
var rankPageBudget = 150 * time.Millisecond

type annotationView struct {
	ID    int64
	Body  template.HTML
	Style template.CSS
	doc   markup.Doc
}

type annotationEntry struct {
	models.Annotation
	Text string
}

type sourcePanelView struct {
	models.ImageSource
	Annotations       []annotationEntry
	OriginalLines     []originalLine
	CommentaryHTML    template.HTML
	TranslatedHTML    template.HTML
	HasTranslationRow bool
	doc               markup.Doc
	transDoc          markup.Doc
}

type originalLine struct {
	Text  string
	IsURL bool
}

func buildOriginalLines(original string) []originalLine {
	var out []originalLine
	for _, line := range strings.Split(original, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, originalLine{Text: line, IsURL: gallery.ValidExternalURL(line)})
	}
	return out
}

func buildAnnotationViews(img *models.Image, anns []models.Annotation, refs markup.Refs) []annotationView {
	if img.Width == nil || img.Height == nil || *img.Width <= 0 || *img.Height <= 0 || len(anns) == 0 {
		return nil
	}
	fw, fh := float64(*img.Width), float64(*img.Height)
	out := make([]annotationView, 0, len(anns))
	for _, a := range anns {
		x := min(max(a.X, 0), *img.Width)
		y := min(max(a.Y, 0), *img.Height)
		w := min(max(a.W, 0), *img.Width-x)
		h := min(max(a.H, 0), *img.Height-y)
		style := fmt.Sprintf("left:%.4f%%;top:%.4f%%;width:%.4f%%;height:%.4f%%",
			float64(x)/fw*100, float64(y)/fh*100, float64(w)/fw*100, float64(h)/fh*100)
		doc := markup.Parse(a.Body)
		doc.Collect(refs)
		out = append(out, annotationView{ID: a.ID, Style: template.CSS(style), doc: doc})
	}
	return out
}

func buildAnnotationEntries(anns []models.Annotation) []annotationEntry {
	if len(anns) == 0 {
		return nil
	}
	out := make([]annotationEntry, 0, len(anns))
	for _, a := range anns {
		out = append(out, annotationEntry{Annotation: a, Text: markup.Parse(a.Body).Text()})
	}
	return out
}

type detailData struct {
	baseData
	Image             models.Image
	Filename          string
	ImageTags         []models.ImageTag
	SDMeta            *models.SDMetadata
	ComfyMeta         *models.ComfyUIMetadata
	ComfyNodes        []models.ComfyNode
	GenericMeta       []models.SDParam
	MangaMeta         *models.MangaMetadata
	IsManga           bool
	MisnamedExt       string
	ResumePage        int
	MangaHint         string
	Collections       []models.Collection
	Sources           []models.ImageSource
	Annotations       []annotationView
	SourcePanels      []sourcePanelView
	ManualAnnotations []annotationEntry
	NoteHTML          template.HTML
	ExtraPaths        int
	PrevID            *int64
	NextID            *int64
	RefURL            string
	Ref               string
	BackQuery         string
	BackSort          string
	BackOrder         string
	BackPage          string
	BackSeed          string
	PrevBackPage      string
	PrevBackIdx       string
	NextBackPage      string
	NextBackIdx       string
	// template.URL, or html/template would escape the & separators.
	BackQS         template.URL
	BackKVQS       template.URL
	EnabledTaggers []tagger.TaggerStatus
	TaggersPresent bool
	TaggerReason   string
	ImageTaggers   []string
	ImageSources   []string
	HasUserTags    bool
	HasStaleTags   bool
	TagSidebar     tagSidebar
	Lookup         lookupView
	PhashDistance  int
	NoPreview      bool
	PreviewNote    string
	PreviewScaled  bool
	PreviewMaxDim  int
	PluginSlot     pluginSlotView
	RewrittenBy    int64
	// Set for a row marked missing whose file is still there: LeftOut says
	// why the gallery leaves it out, empty when the next sync restores it.
	OnDisk  bool
	LeftOut string
	// The box the image is drawn in, held before its bytes arrive.
	BoxW, BoxH int
	// The thumbnail fills the box meanwhile, unless it would show turned.
	Placeholder bool
}

// External links go by sha because an image id can be reused after a delete.
func (s *Server) imageByHashHandler(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if d := s.db(); d != nil {
		if id, err := gallery.ImageIDBySHA(d.Read, sha); err == nil {
			http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
			return
		}
	}

	for _, cx := range s.allContexts() {
		if cx.DB == nil {
			continue
		}
		id, err := gallery.ImageIDBySHA(cx.DB.Read, sha)
		if err != nil {
			continue
		}
		if err := s.switchGallery(cx.Name); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
		return
	}
	s.notFoundHandler(w, r)
}

func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	ctx := r.Context()
	img, err := loadImage(ctx, s.db(), id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	back := parseBackContext(r)
	backQ, backSort, backOrder, backPage, backSeed := back.Q, back.Sort, back.Order, back.Page, back.Seed
	backIdx := r.URL.Query().Get("back_idx")

	// After a Similar-images click, back_q may not match: no prev/next.
	refURL := ""
	refStrValid := ""
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if refID, err := strconv.ParseInt(refStr, 10, 64); err == nil && refID != id {
			refURL = back.DetailURL(refID)
			refStrValid = strconv.FormatInt(refID, 10)
		}
	}

	wantAdjacent := refURL == "" && (backSort != "" || backQ != "")
	if wantAdjacent {
		backSort = cmp.Or(backSort, "newest")
		backOrder = cmp.Or(backOrder, search.DefaultOrder(backSort))
	}
	ceiling := resolveCeiling(r, s.active())

	// Back must land on the page holding this image, wherever prev/next walked.
	// back_idx answers it free; each fallback is a query the render waits on.
	var rankPage string
	rankReady := make(chan struct{})
	rankFired := false
	var pending *search.Query
	pageSize := s.pageSize(r)
	posPage, posIdx, havePos := 0, 0, false
	if p, err := strconv.Atoi(backPage); err == nil && p > 0 && pageSize > 0 {
		if i, err := strconv.Atoi(backIdx); err == nil && i >= 0 && i < pageSize {
			posPage, posIdx, havePos = p, i, true
		}
	}
	if havePos {
		backPage = strconv.Itoa(posPage)
	}
	if wantAdjacent && pageSize > 0 && !havePos {
		sq := adjacentSearchQuery(backQ, backSort, backOrder, backSeed, ceiling)
		switch page, listed := search.PageOf(s.source(), sq, id, pageSize); {
		case listed:
			// A miss here means back_q no longer matches; don't rank it.
			if page > 0 {
				backPage = strconv.Itoa(page)
			}
		case backSort == "similarity":
			// A rank by score would re-run the fan the adjacency lookup pays;
			// the page comes off that lookup's cached list afterwards.
			pending = &sq
		default:
			rankFired = true
			go func() {
				defer close(rankReady)
				ctx, cancel := context.WithTimeout(r.Context(), rankPageBudget)
				defer cancel()
				if arrived, err := strconv.Atoi(backPage); err == nil {
					if page, ok := s.pageHoldingMatch(ctx, sq, id, arrived, pageSize); ok {
						rankPage = strconv.Itoa(page)
						return
					}
				}
				rank, err := search.RankInQuery(ctx, s.source(), sq, id)
				if err != nil || rank < 0 {
					return
				}
				rankPage = strconv.Itoa(rank/pageSize + 1)
			}()
		}
	}
	if !rankFired {
		close(rankReady)
	}

	// Related images load lazily; their tag join would dominate this render.
	var (
		imageTags   []models.ImageTag
		sdMeta      *models.SDMetadata
		comfyMeta   *models.ComfyUIMetadata
		genericMeta []models.SDParam
		mangaMeta   *models.MangaMetadata
		imagePaths  []models.ImagePath
		collections []models.Collection
		sources     []models.ImageSource
		annotations []models.Annotation
		sidebar     tagSidebar
		lookupState lookupView
		prevID      *int64
		nextID      *int64
	)
	tagMode := tagModePref.read(r)
	isManga := img.FileType == models.FileTypeCBZ
	misnamedExt := ""
	if claimed := models.ExtFileType(img.CanonicalPath); claimed != "" && claimed != img.FileType {
		misnamedExt = strings.ToLower(filepath.Ext(img.CanonicalPath))
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		_, imageTags, _ = s.tagSvc().GetImageTags(id)
		sidebar = s.buildTagSidebar(id, tagMode, impliedModePref.read(r), imageTags)
	})
	wg.Go(func() { sdMeta = loadSDMeta(ctx, s.db(), id) })
	wg.Go(func() { comfyMeta = loadComfyMeta(ctx, s.db(), id) })
	wg.Go(func() { imagePaths = loadImagePaths(ctx, s.db(), s.boundary(), id) })
	wg.Go(func() { collections, _ = gallery.CollectionsForImage(s.db(), id) })
	wg.Go(func() { sources, _ = gallery.SourcesForImage(s.db(), id) })
	wg.Go(func() { lookupState = s.lookupViewFor(s.active(), id) })
	wg.Go(func() { annotations, _ = gallery.AnnotationsForImage(s.db(), id) })
	wg.Go(func() {
		if !isManga {
			genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
		}
	})
	if isManga {
		wg.Go(func() { mangaMeta = loadMangaMeta(ctx, s.db(), id) })
	}
	if wantAdjacent {
		wg.Go(func() {
			prevID, nextID = s.findAdjacentImages(ctx, id, backQ, backSort, backOrder, backSeed, ceiling)
		})
	}
	wg.Wait()
	<-rankReady
	if rankPage != "" {
		backPage = rankPage
	} else if pending != nil {
		if page, _ := search.PageOf(s.source(), *pending, id, pageSize); page > 0 {
			backPage = strconv.Itoa(page)
		}
	}

	prevBackPage, prevBackIdx, nextBackPage, nextBackIdx := backPage, "", backPage, ""
	if havePos {
		if posIdx > 0 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx-1)
		} else if posPage > 1 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage-1), strconv.Itoa(pageSize-1)
		}
		if posIdx+1 < pageSize {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx+1)
		} else {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage+1), "0"
		}
	}

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	enabledTaggers, taggersPresent, taggerReason := tagger.GalleryTaggers(s.cfgSnapshot(), s.activeGallery())
	imageTaggers := distinctTaggerNames(imageTags, true)
	imageSources := distinctTaggerNames(imageTags, false)
	hasUserTags, hasStaleTags := userAndStaleTags(imageTags)

	var manualAnnotations []models.Annotation
	annBySource := map[[2]string][]models.Annotation{}
	for _, a := range annotations {
		if a.Manual {
			manualAnnotations = append(manualAnnotations, a)
			continue
		}
		k := [2]string{a.Site, a.PostID}
		annBySource[k] = append(annBySource[k], a)
	}
	// All bodies Collect first; resolveMarkup looks the refs up in one batch.
	mkRefs := markup.NewRefs()
	noteDoc := markup.Parse(img.Note)
	noteDoc.Collect(mkRefs)
	annotationViews := buildAnnotationViews(img, annotations, mkRefs)
	var sourcePanels []sourcePanelView
	for _, src := range sources {
		boxes := annBySource[[2]string{src.Site, src.PostID}]
		if src.Commentary == "" && src.CommentaryTranslated == "" && src.Original == "" && len(boxes) == 0 {
			continue
		}
		doc := markup.Parse(src.Commentary)
		doc.Collect(mkRefs)
		transDoc := markup.Parse(src.CommentaryTranslated)
		transDoc.Collect(mkRefs)
		sourcePanels = append(sourcePanels, sourcePanelView{
			ImageSource:       src,
			Annotations:       buildAnnotationEntries(boxes),
			OriginalLines:     buildOriginalLines(src.Original),
			HasTranslationRow: src.Commentary != "" || src.CommentaryTranslated != "",
			doc:               doc,
			transDoc:          transDoc,
		})
	}
	mkRes := s.resolveMarkup(mkRefs)
	for i := range annotationViews {
		annotationViews[i].Body = annotationViews[i].doc.Render(mkRes)
	}
	for i := range sourcePanels {
		sourcePanels[i].CommentaryHTML = sourcePanels[i].doc.Render(mkRes)
		sourcePanels[i].TranslatedHTML = sourcePanels[i].transDoc.Render(mkRes)
	}

	noPreview, previewNote := false, ""
	if _, statErr := os.Stat(gallery.ThumbnailPath(s.thumbnailsPath(), img.ID)); statErr != nil {
		noPreview = true
		previewNote = gallery.PreviewRefusal(img.CanonicalPath, img.FileType)
	}
	var pxW, pxH int
	if img.Width != nil && img.Height != nil {
		pxW, pxH = *img.Width, *img.Height
	}
	previewScaled := !isManga && !gallery.IsVideoType(img.FileType) && gallery.NeedsViewRendition(pxW, pxH)
	boxW, boxH := pxW, pxH
	o, ok := meta.EXIFOrientation(img.CanonicalPath, img.FileType)
	// The size is the stored pixels; a browser draws them turned.
	if ok && o >= 5 && o <= 8 {
		boxW, boxH = pxH, pxW
	}
	placeholder := boxW > 0 && !noPreview && (!ok || o == 1)

	baseName := filepath.Base(img.CanonicalPath)
	// FolderPath, not the disk dir: a root-level file has no folder to name.
	titleName := baseName
	if img.FolderPath != "" {
		titleName = path.Base(img.FolderPath) + "/" + baseName
	}
	mangaHint := ""
	if isManga {
		mangaHint = strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if mangaMeta != nil {
			mangaHint = cmp.Or(strings.TrimSpace(mangaMeta.Series), strings.TrimSpace(mangaMeta.Title), mangaHint)
		}
	}
	var onDisk bool
	var leftOut string
	if img.IsMissing {
		if _, err := os.Stat(img.CanonicalPath); err == nil {
			onDisk = true
			if err := s.active().Boundary().Check(img.CanonicalPath); err != nil {
				leftOut = err.Error()
			}
		}
	}
	data := detailData{
		baseData:          s.base(r, "gallery", fmt.Sprintf("%s - %s", titleName, s.booruName())),
		Image:             *img,
		Filename:          baseName,
		ImageTags:         imageTags,
		SDMeta:            sdMeta,
		ComfyMeta:         comfyMeta,
		ComfyNodes:        comfyNodes,
		GenericMeta:       genericMeta,
		MangaMeta:         mangaMeta,
		IsManga:           isManga,
		MisnamedExt:       misnamedExt,
		MangaHint:         mangaHint,
		ResumePage:        resumePage(img),
		Collections:       collections,
		Sources:           sources,
		Annotations:       annotationViews,
		SourcePanels:      sourcePanels,
		ManualAnnotations: buildAnnotationEntries(manualAnnotations),
		NoteHTML:          noteDoc.Render(mkRes),
		ExtraPaths:        extraImagePaths(imagePaths),
		PrevID:            prevID,
		NextID:            nextID,
		RefURL:            refURL,
		Ref:               refStrValid,
		BackQuery:         backQ,
		BackSort:          backSort,
		BackOrder:         backOrder,
		BackPage:          backPage,
		BackSeed:          backSeed,
		PrevBackPage:      prevBackPage,
		PrevBackIdx:       prevBackIdx,
		NextBackPage:      nextBackPage,
		NextBackIdx:       nextBackIdx,
		BackQS:            back.QueryString("?"),
		BackKVQS:          back.QueryString("&"),
		EnabledTaggers:    enabledTaggers,
		TaggersPresent:    taggersPresent,
		TaggerReason:      taggerReason,
		ImageTaggers:      imageTaggers,
		ImageSources:      imageSources,
		HasUserTags:       hasUserTags,
		HasStaleTags:      hasStaleTags,
		TagSidebar:        sidebar,
		Lookup:            lookupState,
		PhashDistance:     s.findPairsDistance(),
		NoPreview:         noPreview,
		PreviewNote:       previewNote,
		PreviewScaled:     previewScaled,
		PreviewMaxDim:     gallery.ViewMaxDim,
		BoxW:              boxW,
		BoxH:              boxH,
		Placeholder:       placeholder,
		RewrittenBy:       gallery.RewrittenBy(s.db().Read, img.ID),
		OnDisk:            onDisk,
		LeftOut:           leftOut,
		PluginSlot:        s.pluginSlot(r, config.SlotDetailActions, id, img.FileType),
	}
	s.renderTemplate(w, "detail.html", data)
}

func (s *Server) relatedImagesHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !isHTMXRequest(r) {
		dst := "/images/" + idStr
		if r.URL.RawQuery != "" {
			dst += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, dst, http.StatusSeeOther)
		return
	}
	related, _ := s.tagSvc().RelatedImages(id, 6, resolveCeiling(r, s.active()).ExcludedTagIDs())
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_images.html", map[string]any{
		"Images":        related,
		"ActiveGallery": s.activeGallery(),
		"SourceID":      id,
		"BackKVQS":      back.QueryString("&"),
	})
}

// A source row's tagger_name is its source label, so auto=false lists sources.
func distinctTaggerNames(tags []models.ImageTag, auto bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if t.IsAuto != auto || t.TaggerName == "" || seen[t.TaggerName] {
			continue
		}
		seen[t.TaggerName] = true
		out = append(out, t.TaggerName)
	}
	return out
}

// One prev/next step reaches at most a neighbour page, and a page-sized
// read costs the same at any depth, where a rank COUNT grows with it.
func (s *Server) pageHoldingMatch(ctx context.Context, sq search.Query, id int64, arrived, pageSize int) (int, bool) {
	if arrived < 1 || pageSize < 1 {
		return 0, false
	}
	for _, page := range []int{arrived, arrived + 1, arrived - 1} {
		if page < 1 || ctx.Err() != nil {
			continue
		}
		probe := sq
		probe.Page, probe.Limit = page, pageSize
		probe.SkipCount, probe.Cached = true, false
		res, err := search.Execute(ctx, s.source(), probe)
		if err != nil {
			return 0, false
		}
		if slices.ContainsFunc(res.Results, func(img models.Image) bool { return img.ID == id }) {
			return page, true
		}
	}
	return 0, false
}

func (s *Server) findAdjacentImages(ctx context.Context, currentID int64, queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	prevID, nextID, err := search.ExecuteAdjacent(ctx, s.source(), sq, currentID)
	if err != nil {
		logx.Warnf("findAdjacentImages: %v", err)
	}
	return
}

func adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) search.Query {
	sq := viewQuery(search.Parse(queryStr), queryStr, sortStr, orderStr, ceiling)
	if sortStr == "random" && seedStr != "" {
		if seed, err := strconv.ParseInt(seedStr, 10, 64); err == nil {
			sq.RandomSeed = seed
		}
	}
	return sq
}

// The collection pin is read before the ceiling's terms join the expr.
func viewQuery(expr search.Expr, text, sortStr, orderStr string, ceiling *Ceiling) search.Query {
	sq := search.Query{
		Expr:         ceiling.Apply(expr),
		Sort:         sortStr,
		Order:        orderStr,
		Cached:       true,
		Text:         text,
		CeilingLevel: ceiling.Level(),
	}
	if sortStr == "order" {
		sq.OrderCollection = search.PinnedCollectionName(expr)
	}
	return sq
}

// A file over the ingest cap is left to the backfill: too big to hash per view.
func (s *Server) md5CellGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if maxMB := s.maxFileSizeMB(); maxMB > 0 {
		var size int64
		if err := cx.DB.Read.QueryRow(`SELECT file_size FROM images WHERE id = ?`, id).Scan(&size); err == nil &&
			size > int64(maxMB)*1024*1024 {
			s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"Deferred": true})
			return
		}
	}
	sum, err := cx.StoreMD5(r.Context(), id)
	if err != nil {
		logx.Debugf("md5 cell %d: %v", id, err)
	}
	s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"MD5": sum})
}
