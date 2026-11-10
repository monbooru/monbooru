package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubContribMonloader struct {
	srv        *httptest.Server
	statusBody string
	preview    string
	previewReq string
	sendBody   string
	sendResp   string
}

func newStubContribMonloader(t *testing.T) *stubContribMonloader {
	t.Helper()
	s := &stubContribMonloader{
		statusBody: `{"enabled":true,"state":"ready","contrib":{"account":true,"banned":false}}`,
		preview:    `{"provisional":false,"to_add":[{"tag":"blonde_hair","ptr":"blonde hair","status":"new"},{"tag":"1girl","ptr":"1girl","status":"known"},{"tag":"year:2024","ptr":"","status":"ineligible","note":"unsupported namespace"}],"ptr_only":[{"tag":"hat","ptr":"hat","petitionable":true}]}`,
		sendResp:   `{"results":[{"kind":"mapping_add","result":"staged"}],"job_id":7}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1"}`))
	})
	mux.HandleFunc("/api/v1/queue", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v1/ptr/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(s.statusBody))
	})
	mux.HandleFunc("/api/v1/ptr/contrib/preview", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.previewReq = string(b)
		_, _ = w.Write([]byte(s.preview))
	})
	mux.HandleFunc("/api/v1/ptr/contrib", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.sendBody = string(b)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(s.sendResp))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func pairContribServer(t *testing.T, srv *Server, stub *stubContribMonloader) {
	t.Helper()
	pairPeer(srv, "monloader")
	srv.cfg.Monloader.APIURL = stub.srv.URL
	srv.cfg.Monloader.APIToken = "peer-secret"
	srv.ml.Cache.Expire()
	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
}

func seedContribImage(t *testing.T, srv *Server, sha, fileType string) int64 {
	t.Helper()
	var id int64
	if err := srv.db().Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, ?, 10) RETURNING id`,
		sha, "/g/"+sha+"."+fileType, fileType,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func getBody(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	w := do(srv, "GET", path)
	return w.Code, w.Body.String()
}

func mustGetBody(t *testing.T, srv *Server, path string) string {
	t.Helper()
	code, body := getBody(t, srv, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, code)
	}
	return body
}

func contribImageServer(t *testing.T, stub *stubContribMonloader) *Server {
	t.Helper()
	srv := newTestServer(t)
	seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	pairContribServer(t, srv, stub)
	return srv
}

func contribTagServer(t *testing.T, stub *stubContribMonloader) (*Server, int64) {
	t.Helper()
	srv := newTestServer(t)
	pairContribServer(t, srv, stub)
	return srv, seedContribTag(t, srv)
}

func TestContribPanelGating(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := newTestServer(t)
	seedContribImage(t, srv, strings.Repeat("a", 64), "png")

	_, detail := getBody(t, srv, "/images/1")
	wantNotIn(t, detail, "ptr-contrib-panel")

	pairContribServer(t, srv, stub)
	_, detail = getBody(t, srv, "/images/1")
	wantIn(t, detail, "ptr-contrib-panel")
	code, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, "new to the Public Tag Repository") {
		t.Errorf("panel body = %d %q", code, body)
	}

	seedContribImage(t, srv, strings.Repeat("b", 64), "cbz")
	_, cbzDetail := getBody(t, srv, "/images/2")
	wantNotIn(t, cbzDetail, "ptr-contrib-panel")

	if code, body := getBody(t, srv, "/images/2/ptr-contrib-panel"); code != 200 || strings.Contains(body, "contribute") {
		t.Errorf("cbz panel body = %d %q", code, body)
	}
}

func TestContribCBZDialogAndSendRefused(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := newTestServer(t)
	seedContribImage(t, srv, strings.Repeat("c", 64), "cbz")
	pairContribServer(t, srv, stub)

	if code, _ := getBody(t, srv, "/images/1/ptr-contrib-dialog"); code != http.StatusNotFound {
		t.Errorf("cbz dialog = %d, want 404", code)
	}

	form := url.Values{}
	form.Add("add", "blonde_hair")
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	wantCode(t, w, http.StatusNotFound)
	if stub.sendBody != "" {
		t.Errorf("cbz send reached monloader: %q", stub.sendBody)
	}
}

func TestContribSendShaMismatchRefused(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := contribImageServer(t, stub)

	_, dialog := getBody(t, srv, "/images/1/ptr-contrib-dialog")
	mustIn(t, dialog, `name="sha256" value="`+strings.Repeat("a", 64)+`"`)

	form := url.Values{}
	form.Add("add", "blonde_hair")
	form.Set("sha256", strings.Repeat("f", 64))
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "reopen the dialog") {
		t.Errorf("mismatch send = %d %q", w.Code, w.Body.String())
	}
	if stub.sendBody != "" {
		t.Errorf("mismatched send reached monloader: %q", stub.sendBody)
	}
}

func TestContribSendPetitionNoReasonRefused(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := contribImageServer(t, stub)

	form := url.Values{}
	form.Add("petition", "hat")
	form.Set("petition_reason", "  ")
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "a reason is required") {
		t.Errorf("no-reason petition = %d %q", w.Code, w.Body.String())
	}
	if stub.sendBody != "" {
		t.Errorf("reasonless petition reached monloader: %q", stub.sendBody)
	}
}

func TestContribPanelAllIneligibleCopy(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.preview = `{"provisional":false,"to_add":[{"tag":"year:2024","ptr":"","status":"ineligible","note":"unsupported namespace"}],"ptr_only":[]}`
	srv := contribImageServer(t, stub)

	body := mustGetBody(t, srv, "/images/1/ptr-contrib-panel")
	wantNotIn(t, body, "all already on the Public Tag Repository")
	wantIn(t, body, "nothing to contribute for this image")
}

func TestContribGateBanned(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.statusBody = `{"enabled":true,"state":"ready","contrib":{"account":true,"banned":true}}`
	srv := contribImageServer(t, stub)

	code, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	if code != 200 || strings.Contains(body, `hx-get="/images/1/ptr-contrib-dialog"`) {
		t.Errorf("banned account got an actionable contribute button = %d %q", code, body)
	}
	wantIn(t, body, "the contribution account is banned")
}

func TestContribGateSyncing(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.statusBody = `{"enabled":true,"state":"syncing","contrib":{"account":true,"banned":false}}`
	srv := contribImageServer(t, stub)
	code, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, "ptr-contrib-card") {
		t.Fatalf("syncing index should still render the diff card = %d %q", code, body)
	}
	wantNotIn(t, body, `hx-get="/images/1/ptr-contrib-dialog"`)
	wantIn(t, body, "the Public Tag Repository is still syncing")
	wantIn(t, body, "btn-ptr-pull")
	wantIn(t, body, `disabled title="the Public Tag Repository is still syncing">Pull tags</button>`)
	wantNotIn(t, body, `hx-post="/images/1/lookup"`)
}

func TestContribGateAbsentField(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.statusBody = `{"enabled":true,"state":"ready"}`
	srv := contribImageServer(t, stub)
	code, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	if code != 200 || strings.Contains(body, `hx-get="/images/1/ptr-contrib-dialog"`) {
		t.Errorf("no-account monloader got an actionable contribute button = %d %q", code, body)
	}
	wantIn(t, body, "account in monloader is required")
}

func TestContribDialogFiltersTags(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.preview = `{"provisional":false,"to_add":[{"tag":"blonde_hair","ptr":"blonde hair","status":"new"},{"tag":"1girl","ptr":"1girl","status":"known"},{"tag":"year:2024","ptr":"","status":"ineligible","note":"unsupported namespace"}],"ptr_only":[{"tag":"hat","ptr":"hat","petitionable":true},{"tag":"heart_symbol","ptr":"heart symbol","petitionable":true}]}`
	srv := contribImageServer(t, stub)

	body := mustGetBody(t, srv, "/images/1/ptr-contrib-dialog")

	for _, want := range []string{
		"@@ add",
		`<span class="ptr-live">0/1</span>`,
		`name="add" value="blonde_hair"`,
		`>1girl</span><span class="ptr-note">on the PTR</span>`,
		"unsupported namespace",
		"@@ petition removal",
		`name="petition" value="hat"`,
		`name="petition" value="heart_symbol"`,
		"ptr-contrib-summary",
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, `value="blonde_hair" checked`)
}

func TestContribDialogLongPTRSpellingUnder(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.preview = `{"provisional":false,"to_add":[{"tag":"character:sailor_mercury","ptr":"character:mizuno sailor mercury ami","status":"new"}],"ptr_only":[]}`
	srv := contribImageServer(t, stub)

	body := mustGetBody(t, srv, "/images/1/ptr-contrib-dialog")
	wantIn(t, body, `class="ptr-note ptr-note-under">PTR spelling: character:mizuno sailor mercury ami`)
}

func TestContribPreviewCarriesImpliedContext(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := newTestServer(t)
	imgID := seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "blonde_hair", genID)
	implied := mustTag(t, srv.tagSvc(), "hair", genID)
	mustAddImply(t, srv.tagSvc(), tag.ID, implied.ID)
	mustAddTag(t, srv.tagSvc(), imgID, tag.ID, false, nil)
	pairContribServer(t, srv, stub)

	mustGetBody(t, srv, "/images/1/ptr-contrib-dialog")

	for _, want := range []string{`"tags":["blonde_hair"]`, `"implied":["hair"]`} {
		wantIn(t, stub.previewReq, want)
	}
}

func TestContribPreviewCarriesRatingAsContext(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := newTestServer(t)
	imgID := seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	rating := mustTag(t, srv.tagSvc(), "explicit", srv.tagSvc().RatingCategoryID())
	mustAddTag(t, srv.tagSvc(), imgID, rating.ID, false, nil)
	pairContribServer(t, srv, stub)

	mustGetBody(t, srv, "/images/1/ptr-contrib-dialog")
	wantIn(t, stub.previewReq, `"implied":["rating:explicit"]`)
	wantIn(t, stub.previewReq, `"tags":[]`)
}

func TestContribPanelFoldsAliasedPTRTags(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.preview = `{"provisional":false,"to_add":[{"tag":"character:samus_aran_(metroid)","ptr":"character:samus aran (metroid)","status":"new"}],` +
		`"ptr_only":[{"tag":"character:samus_aran","ptr":"character:samus aran","petitionable":true},{"tag":"hat","ptr":"hat","petitionable":true}]}`
	srv := newTestServer(t)
	imgID := seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	pairContribServer(t, srv, stub)
	charID, _, ok := srv.tagSvc().ParseForm("character:x")
	if !ok {
		t.Fatal("no character category")
	}
	tag := mustTag(t, srv.tagSvc(), "samus_aran_(metroid)", charID)
	mustAddTag(t, srv.tagSvc(), imgID, tag.ID, false, nil)

	_, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	if !strings.Contains(body, "is new to the Public Tag Repository") || !strings.Contains(body, `>2</span> tags this image does not carry`) {
		t.Fatalf("panel = %q", body)
	}

	if _, err := srv.tagSvc().CreateAliasFrom("samus_aran", charID, tag.ID, "ptr"); err != nil {
		t.Fatal(err)
	}
	_, body = getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantNotIn(t, body, "new to the Public Tag Repository")
	wantIn(t, body, `title="hat">1</span> tag this image does not carry`)
	_, dialog := getBody(t, srv, "/images/1/ptr-contrib-dialog")
	if strings.Contains(dialog, `name="add"`) || strings.Contains(dialog, `value="character:samus_aran"`) {
		t.Errorf("dialog still offers the fold: %q", dialog)
	}
}

func TestContribPanelUnattributedSkipsTagsThePTRFilesUnderAnotherSpelling(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	stub.preview = `{"provisional":false,"to_add":[{"tag":"1girl","ptr":"1girl","status":"known"},{"tag":"blonde_hair","ptr":"blonde hair","status":"known"}],"ptr_only":[]}`
	stub.graph = `{"results":{"1girl":{"known":true,"ideal":"1_female"},"blonde_hair":{"known":true,"ideal":"blonde_hair"}}}`
	srv := newTestServer(t)
	imgID := seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	pairContribServer(t, srv, stub.stubContribMonloader)
	generalID, _, _ := srv.tagSvc().ParseForm("x")
	for _, name := range []string{"1girl", "blonde_hair"} {
		mustAddTag(t, srv.tagSvc(), imgID, mustTag(t, srv.tagSvc(), name, generalID).ID, false, nil)
	}

	_, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantIn(t, body, `title="blonde_hair">1</span> of this image's tags without being recorded as a source for it`)

	stub.graphStatus = http.StatusConflict
	_, body = getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantIn(t, body, `>2</span> of this image's tags without being recorded`)
}

func TestContribPreviewSubmitsThePTRSpelling(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv := newTestServer(t)
	imgID := seedContribImage(t, srv, strings.Repeat("a", 64), "png")
	pairContribServer(t, srv, stub.stubContribMonloader)
	charID, _, ok := srv.tagSvc().ParseForm("character:x")
	if !ok {
		t.Fatal("no character category")
	}
	tag := mustTag(t, srv.tagSvc(), "samus_aran_(metroid)", charID)
	mustAddTag(t, srv.tagSvc(), imgID, tag.ID, false, nil)
	if _, err := srv.tagSvc().CreateAliasFrom("samus_aran", charID, tag.ID, "ptr"); err != nil {
		t.Fatal(err)
	}

	stub.graph = `{"results":{"character:samus_aran_(metroid)":{"known":true,"ideal":"character:samus_aran_(metroid)"},` +
		`"character:samus_aran":{"known":true,"ideal":"character:samus_aran","aliases":["character:samus"]}}}`
	stub.preview = `{"provisional":false,"to_add":[{"tag":"character:samus_aran","ptr":"character:samus aran","status":"new"}],"ptr_only":[]}`

	_, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantIn(t, stub.previewReq, `"tags":["character:samus_aran"]`)
	wantIn(t, body, `title="character:samus_aran_(metroid)">1</span> of this image's tags is new`)
	_, dialog := getBody(t, srv, "/images/1/ptr-contrib-dialog")
	for _, want := range []string{
		`value="character:samus_aran">`,
		`>character:samus_aran_(metroid)</span>`,
		`PTR spelling: character:samus aran`,
	} {
		wantIn(t, dialog, want)
	}

	stub.graph = `{"results":{"character:samus_aran_(metroid)":{"known":true,"ideal":"character:samus_aran_(metroid)","aliases":["character:samus_aran"]},` +
		`"character:samus_aran":{"known":true,"ideal":"character:samus_aran_(metroid)"}}}`
	if _, _ = getBody(t, srv, "/images/1/ptr-contrib-panel"); !strings.Contains(stub.previewReq, `"tags":["character:samus_aran_(metroid)"]`) {
		t.Errorf("substituted a spelling the repository already knows the tag by: %q", stub.previewReq)
	}

	stub.graphStatus = http.StatusConflict
	if _, _ = getBody(t, srv, "/images/1/ptr-contrib-panel"); !strings.Contains(stub.previewReq, `"tags":["character:samus_aran_(metroid)"]`) {
		t.Errorf("a refused graph should leave the submitted list alone: %q", stub.previewReq)
	}
}

func TestContribSendProxiesItems(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.sendResp = `{"results":[{"kind":"mapping_add","result":"staged"},{"kind":"mapping_petition","result":"refused","note":"already petitioned"}],"job_id":7}`
	srv := contribImageServer(t, stub)

	form := url.Values{}
	form.Add("add", "blonde_hair")
	form.Add("petition", "hat")
	form.Set("petition_reason", "not in the image")
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	mustCode(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Retarget"); got != "#ptr-contrib-form" {
		t.Errorf("HX-Retarget = %q", got)
	}
	for _, want := range []string{
		"handed 1 to monloader for upload (1 not sent)",
		`<span class="ptr-name">blonde_hair</span><span class="ptr-note">staged</span>`,
		`<span class="ptr-name">hat</span><span class="ptr-note">refused - already petitioned</span>`,
		">close</button>",
	} {
		wantIn(t, w.Body.String(), want)
	}
	var sent struct {
		Commit bool             `json:"commit"`
		Origin string           `json:"origin"`
		Items  []map[string]any `json:"items"`
	}
	mustUnmarshal(t, []byte(stub.sendBody), &sent)
	if !sent.Commit || sent.Origin != "image 1" || len(sent.Items) != 2 {
		t.Fatalf("proxied body = %+v", sent)
	}
	var sawAdd, sawPetition bool
	for _, it := range sent.Items {
		if it["kind"] == "mapping_add" && it["tag"] == "blonde_hair" {
			sawAdd = true
		}
		if it["kind"] == "mapping_petition" && it["tag"] == "hat" && it["reason"] == "not in the image" {
			sawPetition = true
		}
	}
	if !sawAdd || !sawPetition {
		t.Errorf("items = %+v, want the add and the reasoned petition", sent.Items)
	}
}

func TestContribSendMismatchedReceipt(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.sendResp = `{"results":[{"kind":"mapping_add","result":"staged"}],"job_id":7}`
	srv := contribImageServer(t, stub)

	form := url.Values{}
	form.Add("add", "blonde_hair")
	form.Add("petition", "hat")
	form.Set("petition_reason", "not in the image")
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	mustCode(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.Contains(body, "flash-err") || !strings.Contains(body, "answered 1 of 2 items") {
		t.Errorf("mismatched receipt should fail, got %q", body)
	}
	wantNotIn(t, body, "handed 1 to monloader")
}

func TestContribPanelPetitionOnly(t *testing.T) {
	stub := newStubContribMonloader(t)
	stub.preview = `{"provisional":false,"to_add":[{"tag":"1girl","ptr":"1girl","status":"known"}],"ptr_only":[{"tag":"hat","ptr":"hat","petitionable":true},{"tag":"glasses","ptr":"glasses","petitionable":false}]}`
	srv := contribImageServer(t, stub)

	body := mustGetBody(t, srv, "/images/1/ptr-contrib-panel")
	if !strings.Contains(body, `<span class="ptr-contrib-minus">--</span>`) ||
		!strings.Contains(body, `title="hat"`) ||
		!strings.Contains(body, "tag this image does not carry") {
		t.Errorf("panel should call out the PTR-only tag with a hover list, got %q", body)
	}
	if strings.Contains(body, "disabled") || !strings.Contains(body, "ptr-contrib-dialog") {
		t.Errorf("contribute button should stay enabled, got %q", body)
	}

	stub.preview = `{"provisional":false,"to_add":[{"tag":"1girl","ptr":"1girl","status":"known"}],"ptr_only":[{"tag":"glasses","ptr":"glasses","petitionable":false}]}`
	if _, body := getBody(t, srv, "/images/1/ptr-contrib-panel"); !strings.Contains(body, "disabled") {
		t.Errorf("panel with nothing actionable should disable the button, got %q", body)
	}
}

func TestContribSendUnavailableCollapses(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := contribImageServer(t, stub)

	form := url.Values{}
	form.Add("add", "blonde_hair")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer down.Close()
	srv.cfg.Monloader.APIURL = down.URL
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "unavailable") {
		t.Errorf("collapse = %d %q", w.Code, w.Body.String())
	}
}

func TestContribSendNamesTheRefusal(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := contribImageServer(t, stub)

	form := url.Values{}
	form.Add("add", "blonde_hair")
	banned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"ptr_banned","error":"the contribution account is banned"}`))
	}))
	defer banned.Close()
	srv.cfg.Monloader.APIURL = banned.URL
	w := postForm(t, srv, "/images/1/ptr-contrib", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "the contribution account is banned") {
		t.Errorf("refusal = %d %q", w.Code, w.Body.String())
	}
}

type stubPairMonloader struct {
	*stubContribMonloader
	direction   string
	directions  map[string]string
	graph       string
	graphStatus int
	asked       []string

	clusters map[string]string
	searched []string
	noSearch bool
}

func newStubPairMonloader(t *testing.T, direction string) *stubPairMonloader {
	base := newStubContribMonloader(t)
	s := &stubPairMonloader{stubContribMonloader: base, direction: direction, graph: `{"results":{}}`}
	mux := base.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/api/v1/ptr/contrib/pair-preview", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Kind, A, B string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		dir := s.direction
		if d, ok := s.directions[body.A+"=>"+body.B]; ok {
			dir = d
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"a_ptr":     strings.ReplaceAll(body.A, "_", " "),
			"b_ptr":     strings.ReplaceAll(body.B, "_", " "),
			"direction": dir,
		})
	})
	mux.HandleFunc("/api/v1/ptr/tags", func(w http.ResponseWriter, r *http.Request) {
		if s.graphStatus != 0 {
			w.WriteHeader(s.graphStatus)
			return
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.asked = append(s.asked, body.Tags...)
		_, _ = w.Write([]byte(s.graph))
	})
	mux.HandleFunc("/api/v1/ptr/tags/search", func(w http.ResponseWriter, r *http.Request) {
		if s.noSearch {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := r.URL.Query().Get("q")
		if r.URL.Query().Get("mode") == "contains" {
			q += "!"
		}
		s.searched = append(s.searched, q)
		body, ok := s.clusters[q]
		if !ok {
			body = `{"clusters":[]}`
		}
		_, _ = w.Write([]byte(body))
	})
	return s
}

func seedContribTag(t *testing.T, srv *Server) (tagID int64) {
	t.Helper()
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "blonde_hair", genID)
	mustCreateAlias(t, srv.tagSvc(), "blond_hair", genID, tag.ID)
	implied := mustTag(t, srv.tagSvc(), "hair", genID)
	mustAddImply(t, srv.tagSvc(), tag.ID, implied.ID)

	for _, name := range []string{"yellow_hair", "golden_hair", "long_hair", "dirty_blonde"} {
		mustTag(t, srv.tagSvc(), name, genID)
	}
	return tag.ID
}

func TestTagContribPanelCounts(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, `>2</span> of this tag's relations are new to the Public Tag Repository`) ||
		!strings.Contains(body, `class="ptr-count" title="blond_hair`) {
		t.Errorf("panel = %d %q", code, body)
	}

	stub.direction = "petition"
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if !strings.Contains(body, "nothing new to contribute") || !strings.Contains(body, "disabled") {
		t.Errorf("in-sync panel = %q", body)
	}
}

func TestTagContribRows_PreviewsRunInParallel(t *testing.T) {
	base := newStubContribMonloader(t)
	mux := base.srv.Config.Handler.(*http.ServeMux)
	var inFlight, peak, calls atomic.Int32
	mux.HandleFunc("/api/v1/ptr/contrib/pair-preview", func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		calls.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(`{"direction":"suggest"}`))
	})
	mux.HandleFunc("/api/v1/ptr/tags", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":{}}`))
	})
	srv := newTestServer(t)
	pairContribServer(t, srv, base)
	genID := srv.active().GeneralCategoryID
	hub := mustTag(t, srv.tagSvc(), "hub_tag", genID)
	for i := range 20 {
		mustCreateAlias(t, srv.tagSvc(), fmt.Sprintf("hub_alias_%d", i), genID, hub.ID)
	}

	if code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(hub.ID, 10)+"/ptr-contrib-panel"); code != 200 {
		t.Fatalf("panel = %d %q", code, body)
	}
	if n := calls.Load(); n != 20 {
		t.Errorf("pair previews = %d, want one per alias (20)", n)
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Errorf("peak previews in flight = %d, want 2 to 8", p)
	}
}

func TestTagContribPanelUnknown(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, "the Public Tag Repository does not know this tag.") {
		t.Errorf("panel = %d %q", code, body)
	}
	wantNotIn(t, body, "known to the Public Tag Repository as")
	if strings.Join(stub.asked, " ") != "blonde_hair blond_hair" {
		t.Errorf("graph asked %v, want the tag then its alias in one call", stub.asked)
	}
}

func TestTagContribPanelEmptyCluster(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"blonde_hair"},"blond_hair":{"known":false}}}`

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, "the Public Tag Repository holds no relations for this spelling.") {
		t.Errorf("panel = %d %q", code, body)
	}
	if strings.Contains(body, "does not know this tag") || strings.Contains(body, "Pull aliases and implications") {
		t.Errorf("empty cluster reads as unknown or pullable: %q", body)
	}

	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"blonde_hair","aliases":["yellow_hair"]},"blond_hair":{"known":false}}}`
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	wantNotIn(t, body, "holds no relations for this spelling")
}

func TestTagContribPanelKnownThroughAlias(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	stub.directions = map[string]string{"yellow_hair=>blond_hair": "petition"}
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	stub.graph = `{"results":{"blonde_hair":{"known":false},"blond_hair":{"known":true,"ideal":"blond_hair","aliases":["yellow_hair"],"implications":["long_hair"]}}}`

	golden, err := srv.tagSvc().FindTag("golden_hair", srv.active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateAlias(t, srv.tagSvc(), "yellow_hair", srv.active().GeneralCategoryID, golden.ID)

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, `known to the Public Tag Repository as <span class="ptr-known-as">blond_hair</span>, an alias here.`) {
		t.Errorf("panel = %d %q", code, body)
	}
	wantNotIn(t, body, "does not know this tag")
	if !strings.Contains(body, "Pull aliases and implications") || !strings.Contains(body, "known there as blond_hair") {
		t.Errorf("pull not offered through the alias: %q", body)
	}
	wantIn(t, body, `class="ptr-count" title="yellow_hair -&gt; blond_hair"`)

	longHair := mustTag(t, srv.tagSvc(), "long_hair", srv.active().GeneralCategoryID)
	mustAddImply(t, srv.tagSvc(), tagID, longHair.ID)
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if !strings.Contains(body, `>1</span> of this tag's relations is new`) || !strings.Contains(body, `title="blonde_hair =&gt; hair"`) {
		t.Errorf("cluster-held relations still read as new: %q", body)
	}
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	if !strings.Contains(body, "2 relations already handled") || strings.Count(body, `<span class="ptr-note">on the PTR</span>`) != 2 {
		t.Errorf("cluster-held relations not folded as on the PTR: %q", body)
	}
	wantIn(t, body, `name="pair" value="parent blonde_hair hair"`)
	if strings.Contains(body, `name="pair" value="sibling blond_hair blonde_hair"`) || strings.Contains(body, `name="pair" value="parent blonde_hair long_hair"`) {
		t.Errorf("cluster-held relation offered as a suggestion: %q", body)
	}
}

func TestTagContribPanelSkipsLocallyDeclaredIdeal(t *testing.T) {
	stub := newStubPairMonloader(t, "petition")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "1girl", genID)
	mustCreateAlias(t, srv.tagSvc(), "1_female", genID, tag.ID)
	stub.graph = `{"results":{"1girl":{"known":true,"ideal":"1_female","aliases":["1_female"]}}}`

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, "nothing new to contribute") {
		t.Errorf("panel = %d %q, want nothing to contribute", code, body)
	}
	wantNotIn(t, body, "not declared here")
}

func TestTagContribPanelIdealIsAnotherTagHere(t *testing.T) {
	stub := newStubPairMonloader(t, "petition")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "1girl", genID)
	mustTag(t, srv.tagSvc(), "1_female", genID)
	stub.graph = `{"results":{"1girl":{"known":true,"ideal":"1_female"}}}`

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body,
		`files this tag under <span class="ptr-known-as">1_female</span>, a separate tag here`) {
		t.Errorf("panel = %d %q", code, body)
	}
	wantNotIn(t, body, "not declared here")
}

func TestTagContribPanelSkipsImplicationAliasedHere(t *testing.T) {
	stub := newStubPairMonloader(t, "petition")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "blonde_hair", genID)
	hair := mustTag(t, srv.tagSvc(), "hair", genID)
	mustCreateAlias(t, srv.tagSvc(), "long_hair", genID, hair.ID)
	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"blonde_hair","implications":["long_hair"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantNotIn(t, body, "not declared here")
	wantIn(t, body, "nothing new to contribute")
}

func TestTagContribPanelPullGating(t *testing.T) {
	stub := newStubPairMonloader(t, "covered")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "1girl", genID)
	stub.graph = `{"results":{"1girl":{"known":true,"ideal":"1_female","aliases":["1girls"],"implied_by":["sole_female"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantIn(t, body, "Pull aliases and implications")
	wantNotIn(t, body, "nothing new to contribute")

	mustCreateAlias(t, srv.tagSvc(), "1girls", genID, tag.ID)
	mustCreateAlias(t, srv.tagSvc(), "1_female", genID, tag.ID)
	parent := mustTag(t, srv.tagSvc(), "sole_female", genID)
	mustAddImply(t, srv.tagSvc(), parent.ID, tag.ID)
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantNotIn(t, body, "Pull aliases and implications")
	wantIn(t, body, "nothing new to contribute")
}

func TestTagContribPanelPullWaitsForTheSync(t *testing.T) {
	stub := newStubPairMonloader(t, "covered")
	stub.statusBody = `{"enabled":true,"state":"syncing","contrib":{"account":true,"banned":false}}`
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	tag := mustTag(t, srv.tagSvc(), "1girl", srv.active().GeneralCategoryID)
	stub.graph = `{"results":{"1girl":{"known":true,"ideal":"1_female","aliases":["1girls"],"implied_by":["sole_female"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantIn(t, body, `disabled title="the Public Tag Repository is still syncing">Pull aliases and implications</button>`)
	wantNotIn(t, body, "nothing new to contribute")
	wantNotIn(t, body, "onclick=\"pullTagPTR")
}

func TestTagContribPanelPullSkipsAliasedRelation(t *testing.T) {
	stub := newStubPairMonloader(t, "covered")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	solo := mustTag(t, srv.tagSvc(), "solo", genID)
	futanari := mustTag(t, srv.tagSvc(), "futanari", genID)
	mustCreateAlias(t, srv.tagSvc(), "solo_futanari", genID, futanari.ID)
	stub.graph = `{"results":{"solo":{"known":true,"ideal":"solo","implied_by":["solo_futanari"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(solo.ID, 10)+"/ptr-contrib-panel")
	wantNotIn(t, body, "Pull aliases and implications")
	wantIn(t, body, "nothing new to contribute")
}

func TestTagContribDialogColumns(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"golden_hair","aliases":["yellow_hair"],"implications":["long_hair"],"implied_by":["dirty_blonde"]}}}`
	stub.directions = map[string]string{
		"yellow_hair=>blonde_hair":  "petition",
		"blonde_hair=>golden_hair":  "petition",
		"blonde_hair=>long_hair":    "petition",
		"dirty_blonde=>blonde_hair": "petition",
	}

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	if code != 200 {
		t.Fatalf("dialog = %d", code)
	}
	for _, want := range []string{
		`name="pair" value="sibling blond_hair blonde_hair"`,
		`name="pair" value="parent blonde_hair hair"`,
		`name="pair_petition" value="sibling yellow_hair blonde_hair"`,
		`name="pair_petition" value="parent blonde_hair long_hair"`,
		`>blond_hair</span> -&gt; <span`,
		`<span class="ptr-note">alias of this</span>`,
		`name="pair_petition" value="parent dirty_blonde blonde_hair"`,
		`<span class="ptr-note">implied by</span>`,
		"ptr-contrib-summary",
	} {
		wantIn(t, body, want)
	}

	wantNotIn(t, body, `name="pair_petition" value="sibling blonde_hair golden_hair"`)
	wantNotIn(t, body, " checked>")
	wantNotIn(t, body, "ptr-manual")
}

func TestTagContribDialogOffersNoPetitionForADeclaredImpliedBy(t *testing.T) {
	stub := newStubPairMonloader(t, "petition")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"blonde_hair","implied_by":["dirty_blonde"]}}}`
	dirty, err := srv.tagSvc().FindTag("dirty_blonde", srv.active().GeneralCategoryID)
	if err != nil {
		t.Fatal(err)
	}
	mustAddImply(t, srv.tagSvc(), dirty.ID, tagID)

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	wantNotIn(t, body, `name="pair_petition" value="parent dirty_blonde blonde_hair"`)
}

func TestTagContribDialogOffersNoPetitionForAnAliasThatIsATagHere(t *testing.T) {
	stub := newStubPairMonloader(t, "petition")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	stub.graph = `{"results":{"blonde_hair":{"known":true,"ideal":"blonde_hair","aliases":["yellow_hair"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	wantNotIn(t, body, `name="pair_petition" value="sibling yellow_hair blonde_hair"`)
}

func TestTagContribDialogFoldsCoveredImplication(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	hair := mustTag(t, srv.tagSvc(), "hair", genID)
	beard := mustTag(t, srv.tagSvc(), "beard", genID)
	mustAddImply(t, srv.tagSvc(), beard.ID, hair.ID)

	stub.directions = map[string]string{"beard=>hair": "covered"}

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(hair.ID, 10)+"/ptr-contrib-dialog")
	if code != 200 {
		t.Fatalf("dialog = %d", code)
	}
	wantNotIn(t, body, `name="pair" value="parent beard hair"`)
	wantIn(t, body, "on the PTR")
}

func TestTagContribDialogFoldsSameIdealAlias(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	stub.directions = map[string]string{"blond_hair=>blonde_hair": "covered"}

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	if code != 200 {
		t.Fatalf("dialog = %d", code)
	}
	wantNotIn(t, body, `name="pair" value="sibling blond_hair blonde_hair"`)
	wantIn(t, body, "on the PTR")
}

func TestTagContribDialogDropsAbsentEndpoint(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	stub.graph = `{"results":{"blonde_hair":{"known":true,"implications":["stranger_tag"]}}}`
	stub.directions = map[string]string{"blonde_hair=>stranger_tag": "petition"}

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-dialog")
	if code != 200 {
		t.Fatalf("dialog = %d", code)
	}
	wantNotIn(t, body, `name="pair_petition" value="parent blonde_hair stranger_tag"`)
	wantNotIn(t, body, "stranger_tag")
}

func TestTagContribSendFlip(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)

	form := url.Values{}
	form.Add("pair", "parent blonde_hair hair")
	form.Add("pair", "sibling blond_hair blonde_hair")

	form.Add("pair", "sibling bad|pipe blonde_hair")
	form.Set("suggest_reason", "canonical relation")
	form.Add("pair_petition", "parent blonde_hair long_hair")
	form.Set("petition_reason", "not implied")
	w := postForm(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib", form)
	mustCode(t, w, http.StatusOK)
	var sent struct {
		Origin string           `json:"origin"`
		Items  []map[string]any `json:"items"`
	}
	mustUnmarshal(t, []byte(stub.sendBody), &sent)
	if sent.Origin != "tag "+strconv.FormatInt(tagID, 10) {
		t.Errorf("origin = %q", sent.Origin)
	}
	if len(sent.Items) != 4 {
		t.Fatalf("items = %+v", sent.Items)
	}
	if it := sent.Items[0]; it["kind"] != "parent" || it["child"] != "blonde_hair" || it["parent"] != "hair" || it["reason"] != "canonical relation" {
		t.Errorf("flip wrong: %+v (want child=carrier, parent=implied)", it)
	}
	if it := sent.Items[1]; it["kind"] != "sibling" || it["bad"] != "blond_hair" || it["good"] != "blonde_hair" {
		t.Errorf("sibling wrong: %+v", it)
	}
	if it := sent.Items[2]; it["kind"] != "sibling" || it["bad"] != "bad|pipe" || it["good"] != "blonde_hair" {
		t.Errorf("piped-tag sibling wrong: %+v", it)
	}
	if it := sent.Items[3]; it["kind"] != "parent_petition" || it["reason"] != "not implied" {
		t.Errorf("petition wrong: %+v", it)
	}
}

func TestTagPtrLookupDialogAndPreview(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	base := "/tags/" + strconv.FormatInt(tagID, 10)
	stub.graph = `{"results":{"golden_hair":{"known":true,"ideal":"golden_hair","aliases":["blond_hair","yellow_hair","new_spelling"],"implications":["long_hair","hair","brand_new"],"implied_by":["dirty_blonde"]}}}`

	code, body := getBody(t, srv, base+"/ptr-lookup-dialog")
	if code != 200 || !strings.Contains(body, `id="ptr-lookup-dialog"`) || !strings.Contains(body, base+"/ptr-lookup-preview") {
		t.Fatalf("dialog = %d %q", code, body)
	}

	code, body = getBody(t, srv, base+"/ptr-lookup-preview?as=golden_hair")
	if code != 200 {
		t.Fatalf("preview = %d %q", code, body)
	}
	for _, want := range []string{
		`golden_hair</span> is known; ideal`,
		`here: a tag here (usage 0).`,
		`>golden_hair</span><span class="ptr-note">a tag here (usage 0)</span>`,
		`>blond_hair</span><span class="ptr-note">already an alias here</span>`,
		`>yellow_hair</span><span class="ptr-note">a tag here (usage 0)</span>`,
		`>new_spelling</span><span class="ptr-note">new alias</span>`,
		`>long_hair</span><span class="ptr-note">new implication</span>`,
		`>hair</span><span class="ptr-note">declared</span>`,
		`>brand_new</span><span class="ptr-note">new implication, new tag</span>`,
		`@@ implied by`,
		`>dirty_blonde</span><span class="ptr-note">new implication</span>`,
		`would add 1 alias and 3 implications; 2 already declared, 2 left as is.`,
		`onclick="pullTagPTRAs(&#34;golden_hair&#34;)"`,
	} {
		wantIn(t, body, want)
	}
	wantNotIn(t, body, `id="ptr-lookup-pull" class="peer-accent" onclick="pullTagPTRAs(&#34;golden_hair&#34;)" disabled`)

	_, body = getBody(t, srv, base+"/ptr-lookup-preview?as=nope")
	if !strings.Contains(body, "the Public Tag Repository does not know nope.") || strings.Contains(body, "ptr-lookup-pull") {
		t.Errorf("unknown preview = %q", body)
	}

	_, body = getBody(t, srv, base+"/ptr-lookup-preview?as=%22*%22")
	wantIn(t, body, "not a tag name")

	stub.graph = `{"results":{"blond_hair":{"known":true,"ideal":"blond_hair"}}}`
	_, body = getBody(t, srv, base+"/ptr-lookup-preview?as=blond_hair")
	if !strings.Contains(body, "here: alias of this tag.") || !strings.Contains(body, `pullTagPTRAs(&#34;blond_hair&#34;)" disabled`) {
		t.Errorf("declared-spelling preview = %q", body)
	}

	stub.statusBody = `{"enabled":true,"state":"syncing","contrib":{"account":true,"banned":false}}`
	srv.ml.Cache.Expire()
	srv.monloaderStatusHandler(httptest.NewRecorder(), httptest.NewRequest("GET", "/internal/monloader-status", nil))
	if code, _ := getBody(t, srv, base+"/ptr-lookup-dialog"); code != http.StatusConflict {
		t.Errorf("dialog under a syncing index = %d, want 409", code)
	}
}

func TestTagPtrLookupPreviewOffersTheMerge(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	base := "/tags/" + strconv.FormatInt(tagID, 10) + "/ptr-lookup-preview?as="
	stub.graph = `{"results":{"golden_hair":{"known":true,"ideal":"golden_hair"},"new_spelling":{"known":true,"ideal":"new_spelling"},` +
		`"blond_hair":{"known":true,"ideal":"blond_hair"},"blonde_hair":{"known":true,"ideal":"blonde_hair"}}}`

	for _, tc := range []struct {
		spelling string
		offered  bool
	}{
		{"golden_hair", true},
		{"new_spelling", true},
		{"blond_hair", false},
		{"blonde_hair", false},
	} {
		_, body := getBody(t, srv, base+tc.spelling)
		want := `onclick="pullTagPTRKeeping(&#34;` + tc.spelling + `&#34;)"`
		if !strings.Contains(body, want) {
			t.Fatalf("%s: preview lacks the merge control:\n%s", tc.spelling, body)
		}
		if got := strings.Contains(body, want+" disabled"); got == tc.offered {
			t.Errorf("%s: merge disabled = %v, want %v", tc.spelling, got, !tc.offered)
		}
	}

	if _, body := getBody(t, srv, base+"nope"); strings.Contains(body, "pullTagPTRKeeping") {
		t.Errorf("merge offered for a spelling the repository does not know: %q", body)
	}
}

func TestContribPanelUnknownSpelling(t *testing.T) {
	stub := newStubContribMonloader(t)
	srv := contribImageServer(t, stub)

	_, body := getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantNotIn(t, body, "does not know")
	_, body = getBody(t, srv, "/images/1/ptr-contrib-dialog")
	wantNotIn(t, body, "not a PTR tag")

	stub.preview = `{"provisional":false,"to_add":[{"tag":"blonde_hair","ptr":"blonde hair","status":"new"},{"tag":"varia_suit_(metroid)","ptr":"varia suit (metroid)","status":"new","unknown_tag":true},{"tag":"1girl","ptr":"1girl","status":"known"}],"ptr_only":[]}`
	_, body = getBody(t, srv, "/images/1/ptr-contrib-panel")
	wantIn(t, body, `>2</span> of this image's tags are new to the Public Tag Repository, <span class="ptr-count" title="varia_suit_(metroid)">1</span> of them not a PTR tag.`)
	_, body = getBody(t, srv, "/images/1/ptr-contrib-dialog")
	wantIn(t, body, `>varia_suit_(metroid)</span><span class="ptr-note ptr-note-warn">not a PTR tag</span>`)
	wantIn(t, body, `>blonde_hair</span><span class="ptr-note"></span>`)
}

func TestTagContribPanelPullTracksNormalizedAndInvalidNames(t *testing.T) {
	stub := newStubPairMonloader(t, "covered")
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	genID := srv.active().GeneralCategoryID
	tag := mustTag(t, srv.tagSvc(), "short_hair", genID)
	parent := mustTag(t, srv.tagSvc(), "very_short_hair", genID)
	mustAddImply(t, srv.tagSvc(), parent.ID, tag.ID)

	stub.graph = `{"results":{"short_hair":{"known":true,"ideal":"short_hair","implied_by":["short*hair","very_short_hair\u200b"]}}}`

	_, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantNotIn(t, body, "Pull aliases and implications")

	stub.graph = `{"results":{"short_hair":{"known":true,"ideal":"short_hair","implied_by":["buzz_cut"]}}}`
	_, body = getBody(t, srv, "/tags/"+strconv.FormatInt(tag.ID, 10)+"/ptr-contrib-panel")
	wantIn(t, body, "Pull aliases and implications")
}

func TestTagPTRLookupSearchSeedsFromTheTag(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	stub.clusters = map[string]string{
		"blonde_hair_(long)": `{"clusters":[{"ideal":"blonde_hair_(long)","matched":["blonde_hair_(long)"]}]}`,
		"blonde_hair":        `{"clusters":[{"ideal":"blonde_hair","matched":["blond_hair"],"aliases":3,"implications":1}]}`,
	}
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	tag := mustTag(t, srv.tagSvc(), "blonde_hair_(long)", srv.active().GeneralCategoryID)
	path := "/tags/" + strconv.FormatInt(tag.ID, 10) + "/ptr-lookup-search"

	code, body := getBody(t, srv, path)
	if code != 200 {
		t.Fatalf("seed = %d %q", code, body)
	}
	wantIn(t, body, "the Public Tag Repository holds blonde_hair_(long) with no relations; showing blonde_hair.")
	if !strings.Contains(body, `data-spelling="blonde_hair"`) || !strings.Contains(body, "via blond_hair") ||
		!strings.Contains(body, "3 aliases") {
		t.Errorf("seed rows = %q", body)
	}

	if strings.Join(stub.searched, " ") != "blonde_hair_(long) blonde_hair" {
		t.Errorf("searched %v, want the two longest stems", stub.searched)
	}

	delete(stub.clusters, "blonde_hair_(long)")
	_, body = getBody(t, srv, path)
	wantIn(t, body, "no PTR spelling starts with blonde_hair_(long); showing blonde_hair.")

	wantIn(t, body, "dialog-actions")
	_, typed := getBody(t, srv, path+"?q=blonde_hair")
	if strings.Contains(typed, "dialog-actions") || strings.Contains(typed, "ptr-lookup-note") {
		t.Errorf("typeahead shape carries the seed's chrome: %q", typed)
	}
	wantIn(t, typed, `data-spelling="blonde_hair"`)
}

func TestTagPTRLookupSearchSubstringAndRefusals(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	stub.clusters = map[string]string{
		"blonde_hair!": `{"clusters":[{"ideal":"dark_blonde_hair","matched":["dark_blonde_hair"],"aliases":1}]}`,
	}
	srv := newTestServer(t)
	pairContribServer(t, srv, stub.stubContribMonloader)
	tag := mustTag(t, srv.tagSvc(), "blonde_hair", srv.active().GeneralCategoryID)
	path := "/tags/" + strconv.FormatInt(tag.ID, 10) + "/ptr-lookup-search"

	_, body := getBody(t, srv, path+"?q=blonde_hair&anywhere=1")
	wantIn(t, body, `data-spelling="dark_blonde_hair"`)
	if got := stub.searched[len(stub.searched)-1]; got != "blonde_hair!" {
		t.Errorf("last search = %q, want the substring pass", got)
	}

	srv2 := newTestServer(t)
	stub2 := newStubPairMonloader(t, "suggest")
	stub2.noSearch = true
	pairContribServer(t, srv2, stub2.stubContribMonloader)
	tag2 := mustTag(t, srv2.tagSvc(), "blonde_hair", srv2.active().GeneralCategoryID)

	code, old := getBody(t, srv2, "/tags/"+strconv.FormatInt(tag2.ID, 10)+"/ptr-lookup-search")
	if code != 200 || strings.Contains(old, "suggest-item") || strings.Contains(old, "ptr-lookup-note") {
		t.Errorf("old monloader seed = %d %q", code, old)
	}
}

func TestTagContribPanelOrphanOwnNameYieldsToAlias(t *testing.T) {
	stub := newStubPairMonloader(t, "suggest")
	srv, tagID := contribTagServer(t, stub.stubContribMonloader)
	stub.graph = `{"results":{
		"blonde_hair":{"known":true,"ideal":"blonde_hair"},
		"blond_hair":{"known":true,"ideal":"blond_hair","aliases":["yellow_hair"],"implications":["long_hair"]}}}`

	code, body := getBody(t, srv, "/tags/"+strconv.FormatInt(tagID, 10)+"/ptr-contrib-panel")
	if code != 200 || !strings.Contains(body, `known to the Public Tag Repository as <span class="ptr-known-as">blond_hair</span>`) {
		t.Errorf("panel = %d %q", code, body)
	}
	wantNotIn(t, body, "holds no relations for this spelling")
	wantIn(t, body, "Pull aliases and implications")
}
