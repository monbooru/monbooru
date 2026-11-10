package gallery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFacts() NameFacts {
	return NameFacts{
		Name:       "IMG_4417",
		Ext:        "jpg",
		Type:       "jpeg",
		Gallery:    "default",
		ID:         10428,
		SHA256:     "3f9a2c1b8c04de771a5b90ef3f9a2c1b8c04de771a5b90ef3f9a2c1b8c04de77",
		MD5:        "d41d8cd98f00b204e9800998ecf8427e",
		IngestedAt: time.Date(2026, 8, 14, 15, 43, 2, 0, time.UTC),
		Width:      2048,
		Height:     1536,
		Size:       1843201,
		Origin:     "upload",
	}
}

func mustRender(t *testing.T, tmpl *NameTemplate, f NameFacts) string {
	t.Helper()
	got, err := tmpl.Render(f)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return got
}

func TestParseNameTemplate_Blank(t *testing.T) {
	for _, s := range []string{"", "   "} {
		tmpl := mustParseNameTemplate(t, s, ScopeRename)
		if tmpl != nil {
			t.Errorf("parse %q = %v, want nil", s, tmpl)
		}
		if tmpl.HasTokens() {
			t.Errorf("parse %q: nil template reports tokens", s)
		}
	}
}

func TestRenderTokens(t *testing.T) {
	tests := []struct {
		tmpl  string
		scope Scope
		want  string
	}{
		{"{name}", ScopeRename, "IMG_4417"},
		{"{name}.{ext}", ScopeRename, "IMG_4417.jpg"},
		{"{type}", ScopeRename, "jpeg"},
		{"{id}", ScopeRename, "10428"},
		{"{hash}", ScopeRename, "3f9a2c1b8c04de771a5b90ef3f9a2c1b8c04de771a5b90ef3f9a2c1b8c04de77"},
		{"{hash:8}", ScopeRename, "3f9a2c1b"},
		{"{md5}", ScopeRename, "d41d8cd98f00b204e9800998ecf8427e"},
		{"{gallery}-{id}", ScopeRename, "default-10428"},
		{"{date}", ScopeRename, "2026-08-14"},
		{"{time}", ScopeRename, "154302"},
		{"{w}x{h}", ScopeRename, "2048x1536"},
		{"{size}", ScopeRename, "1843201"},
		{"{origin}", ScopeRename, "upload"},
		{"holiday", ScopeRename, "holiday"},
		{"set{{1}}", ScopeRename, "set{1}"},
		{"{year}/{month}/{day}", ScopeMove, "2026/08/14"},
		{"archive/{year}/{type}", ScopeMove, "archive/2026/jpeg"},
	}
	for _, tc := range tests {
		tmpl := mustParseNameTemplate(t, tc.tmpl, tc.scope)
		if got := mustRender(t, tmpl, testFacts()); got != tc.want {
			t.Errorf("render %q = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestRenderSequence(t *testing.T) {
	f := testFacts()
	f.N, f.NWidth = 7, 2

	tmpl := mustParseNameTemplate(t, "{n}-{id}", ScopeRenameBatch)
	if got := mustRender(t, tmpl, f); got != "07-10428" {
		t.Errorf("render = %q, want 07-10428", got)
	}

	wide := mustParseNameTemplate(t, "{n:4}", ScopeRenameBatch)
	if got := mustRender(t, wide, f); got != "0007" {
		t.Errorf("render = %q, want 0007", got)
	}
}

func TestRenderMissingValuesCloseUp(t *testing.T) {
	f := testFacts()
	f.Width, f.Height = 0, 0

	tests := []struct {
		tmpl  string
		scope Scope
		want  string
	}{

		{"{source}-{id}", ScopeUploadName, "10428"},
		{"{id}_{post_id}", ScopeUploadName, "10428"},
		{"{name}-{source}-{id}", ScopeUploadName, "IMG_4417-10428"},

		{"{name} - {id}", ScopeUploadName, "IMG_4417 - 10428"},
		{"{name} - {source} - {id}", ScopeUploadName, "IMG_4417 - 10428"},
		{"{source}/{year}", ScopeUploadFolder, "2026"},
		{"a/{source}/b", ScopeUploadFolder, "a/b"},

		{"{source}", ScopeUploadFolder, ""},
	}
	for _, tc := range tests {
		tmpl := mustParseNameTemplate(t, tc.tmpl, tc.scope)
		if got := mustRender(t, tmpl, f); got != tc.want {
			t.Errorf("render %q = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

func TestRenderKeepsTheSeparatorsInsideAName(t *testing.T) {
	f := testFacts()
	for _, tc := range []struct{ name, want string }{
		{"my--file", "my--file.jpg"},
		{"a__b", "a__b.jpg"},
		{"hello  world", "hello  world.jpg"},
	} {
		f.Name = tc.name
		tmpl := mustParseNameTemplate(t, "{name}.{ext}", ScopeUploadName)
		if got := mustRender(t, tmpl, f); got != tc.want {
			t.Errorf("render of %q = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderRefusesANameItCannotBuild(t *testing.T) {
	f := testFacts()
	f.Width, f.Height = 0, 0

	for _, sc := range []Scope{ScopeRename, ScopeRenameBatch, ScopeMove, ScopeMoveBatch} {
		tmpl := mustParseNameTemplate(t, "{w}", sc)
		if got, renderErr := tmpl.Render(f); renderErr == nil {
			t.Errorf("scope %d rendered %q, want a refusal", sc, got)
		}
	}

	upload := mustParseNameTemplate(t, "{w}", ScopeUploadName)
	if got := mustRender(t, upload, f); got != "10428" {
		t.Errorf("upload name = %q, want the id", got)
	}
}

func TestRenderSanitisesValues(t *testing.T) {
	f := testFacts()
	f.Name = `a/b:c*d?e"f<g>h|i\j`

	tmpl := mustParseNameTemplate(t, "{name}", ScopeRename)
	got := mustRender(t, tmpl, f)
	if want := "a_b_c_d_e_f_g_h_i_j"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

func TestRenderSanitisesLiterals(t *testing.T) {
	f := testFacts()

	tmpl := mustParseNameTemplate(t, `a:b*c?d"e<f>g|h{id}`, ScopeRename)
	if got, want := mustRender(t, tmpl, f), "a_b_c_d_e_f_g_h10428"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}

	folder := mustParseNameTemplate(t, "by-date./{year}", ScopeMove)
	if got, want := mustRender(t, folder, f), "by-date/2026"; got != want {
		t.Errorf("folder render = %q, want %q", got, want)
	}
}

func TestRenderTruncatesOnRuneBoundary(t *testing.T) {
	f := testFacts()
	f.Name = strings.Repeat("é", 200)

	tmpl := mustParseNameTemplate(t, "{name}", ScopeRename)
	got := mustRender(t, tmpl, f)
	if len(got) > maxNameBytes {
		t.Errorf("render is %d bytes, want at most %d", len(got), maxNameBytes)
	}
	if !strings.HasSuffix(got, "é") {
		t.Errorf("render = %q, ends mid-rune", got)
	}
}

func TestParseNameTemplate_Refusals(t *testing.T) {
	tests := []struct {
		tmpl  string
		scope Scope
		want  string
	}{
		{"{artist}", ScopeRename, "tags are not known at ingest time"},
		{"{tag:artist}", ScopeRename, "tags are not known at ingest time"},
		{"{collection}", ScopeRename, "not a fact about the file"},
		{"{rating}", ScopeRename, "a tag by another name"},
		{"{inbox}", ScopeRename, "triage state"},
		{"{nope}", ScopeRename, "unknown token {nope}"},
		{"sub/dir", ScopeRename, "set that in the folder field"},
		{"sub/dir", ScopeUploadName, "set that in the folder field"},
		{"{n}", ScopeRename, "{n} needs a batch"},
		{"{n}", ScopeUploadName, "{n} needs a batch"},
		{"{folder}", ScopeRename, "{folder} is a destination"},
		{"{folder}", ScopeUploadName, "{folder} is a destination"},
		{"{source}", ScopeRename, "only available for received files"},
		{"{post_id}", ScopeMoveBatch, "only available for received files"},
		{"{hash:0}", ScopeRename, "between 4 and 64"},
		{"{hash:65}", ScopeRename, "between 4 and 64"},
		{"{hash:x}", ScopeRename, "between 4 and 64"},
		{"{n:0}", ScopeRenameBatch, "between 1 and 12"},
		{"{name:2}", ScopeRename, "{name} takes no width"},
		{"{name", ScopeRename, "unclosed {"},
	}
	for _, tc := range tests {
		_, err := ParseNameTemplate(tc.tmpl, tc.scope)
		if err == nil {
			t.Errorf("parse %q: no error, want %q", tc.tmpl, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parse %q = %q, want it to mention %q", tc.tmpl, err, tc.want)
		}
	}
}

func TestParseBatchRenameTemplate_NumbersAPlainBase(t *testing.T) {
	f := testFacts()
	f.N, f.NWidth = 2, 2

	plain, err := ParseBatchRenameTemplate("holiday")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRender(t, plain, f); got != "holiday02" {
		t.Errorf("plain base render = %q, want holiday02", got)
	}

	own, err := ParseBatchRenameTemplate("shot{n}-{id}")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRender(t, own, f); got != "shot02-10428" {
		t.Errorf("tokened base render = %q, want shot02-10428", got)
	}
	if blank, err := ParseBatchRenameTemplate("  "); err != nil || blank != nil {
		t.Errorf("blank base = %v, %v; want nil, nil", blank, err)
	}
}

func TestParseNameTemplate_HasTokens(t *testing.T) {
	plain := mustParseNameTemplate(t, "holiday", ScopeRenameBatch)
	if plain.HasTokens() {
		t.Error("plain string reports tokens")
	}
	tokened := mustParseNameTemplate(t, "holiday-{id}", ScopeRenameBatch)
	if !tokened.HasTokens() {
		t.Error("templated string reports no tokens")
	}
}

func TestTruncateFilename(t *testing.T) {
	if got := TruncateFilename("short", 10); got != "short" {
		t.Errorf("under budget = %q, want short", got)
	}

	if got := TruncateFilename("ééé", 3); got != "é" {
		t.Errorf("rune boundary = %q, want é", got)
	}
	if got := TruncateFilename("ab .", 3); got != "ab" {
		t.Errorf("trailing dot = %q, want ab", got)
	}
}

func TestSanitizeReceivedName_DecodesTheFormEncodedQuote(t *testing.T) {
	for in, want := range map[string]string{
		"new %22one%22.png": "new _one_.png",
		"two%0Alines.png":   "two_lines.png",
		"100%.png":          "100%.png",
	} {
		if got := SanitizeReceivedName(in); got != want {
			t.Errorf("SanitizeReceivedName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadNameFactsAndRenameByTemplate(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFileSize(t, galleryDir, "original.PNG", 12, 34)

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")

	facts, _, err := LoadNameFacts(t.Context(), database, "default", img.ID, 0)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if facts.Name != "original" || facts.Ext != "png" {
		t.Errorf("name/ext = %q/%q, want original/png", facts.Name, facts.Ext)
	}

	if facts.Base != "original.PNG" {
		t.Errorf("base = %q, want original.PNG", facts.Base)
	}
	if facts.Type != "png" || facts.Gallery != "default" {
		t.Errorf("type/gallery = %q/%q, want png/default", facts.Type, facts.Gallery)
	}
	if facts.Width != 12 || facts.Height != 34 {
		t.Errorf("dimensions = %dx%d, want 12x34", facts.Width, facts.Height)
	}
	if facts.Origin != "upload" || facts.SHA256 == "" || facts.IngestedAt.IsZero() {
		t.Errorf("facts = %+v, want origin, sha and ingested_at filled", facts)
	}
	if facts.MD5 == "" || facts.MD5 == facts.SHA256 {
		t.Errorf("md5 = %q, want the file's own digest", facts.MD5)
	}

	tmpl := mustParseNameTemplate(t, "{gallery}-{id}-{hash:8}", ScopeUploadName)
	newPath, err := Naming{Name: tmpl, Gallery: "default"}.Apply(t.Context(), database, NewBoundary(galleryDir, nil, nil), img.ID, "", "")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	want := filepath.Join(galleryDir, fmt.Sprintf("default-%d-%s.PNG", img.ID, facts.SHA256[:8]))
	if newPath != want {
		t.Errorf("new path = %q, want %q", newPath, want)
	}
	if _, statErr := os.Stat(newPath); statErr != nil {
		t.Errorf("file not at the new path: %v", statErr)
	}
}

func TestLoadNameFacts_HashesTheMD5OnlyWhenTheTemplateNamesIt(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "nodigest.png")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")

	mustExec(t, database.Write, `UPDATE images SET md5 = '' WHERE id = ?`, img.ID)

	plain := mustParseNameTemplate(t, "{id}", ScopeRename)
	facts, _, err := LoadNameFacts(t.Context(), database, "default", img.ID, 0, plain)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if facts.MD5 != "" {
		t.Errorf("md5 = %q, want no read for a template that does not name it", facts.MD5)
	}

	tmpl := mustParseNameTemplate(t, "{md5}", ScopeRename)
	facts, _, err = LoadNameFacts(t.Context(), database, "default", img.ID, 0, tmpl)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if len(facts.MD5) != 32 {
		t.Fatalf("md5 = %q, want the file's digest", facts.MD5)
	}
	if got := mustRender(t, tmpl, facts); got != facts.MD5 {
		t.Errorf("render = %q, want %q", got, facts.MD5)
	}
	var stored string
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, img.ID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != facts.MD5 {
		t.Errorf("stored md5 = %q, want %q", stored, facts.MD5)
	}
}

func TestNamingApply_EmptyIsNoOp(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "keepme.png")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")
	newPath, err := Naming{Gallery: "default"}.Apply(t.Context(), database, NewBoundary(galleryDir, nil, nil), img.ID, "", "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if newPath != "" {
		t.Errorf("new path = %q, want empty", newPath)
	}
	if _, statErr := os.Stat(srcPath); statErr != nil {
		t.Errorf("file moved despite an empty naming: %v", statErr)
	}
}

func TestNamingApply_FolderThenName(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "incoming.png")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")
	folder := mustParseNameTemplate(t, "by/{source}/{year}", ScopeUploadFolder)
	name := mustParseNameTemplate(t, "{source}-{post_id}", ScopeUploadName)
	n := Naming{Folder: folder, Name: name, Gallery: "default"}
	newPath, err := n.Apply(t.Context(), database, NewBoundary(galleryDir, nil, nil), img.ID, "danbooru", "4821993")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	year := time.Now().UTC().Format("2006")
	want := filepath.Join(galleryDir, "by", "danbooru", year, "danbooru-4821993.png")
	if newPath != want {
		t.Errorf("new path = %q, want %q", newPath, want)
	}
	if _, statErr := os.Stat(want); statErr != nil {
		t.Errorf("file not at the applied path: %v", statErr)
	}

	var folderPath string
	if err := database.Read.QueryRow(
		`SELECT folder_path FROM images WHERE id = ?`, img.ID,
	).Scan(&folderPath); err != nil {
		t.Fatal(err)
	}
	if want := "by/danbooru/" + year; folderPath != want {
		t.Errorf("folder_path = %q, want %q", folderPath, want)
	}
}

func TestReceivedNaming(t *testing.T) {

	dir, n := ReceivedNaming("default", "", "inbox", "{id}")
	if dir != "inbox" || n.Folder != nil || n.Name == nil {
		t.Errorf("literal folder: dir=%q folder=%v name=%v", dir, n.Folder, n.Name)
	}

	dir, n = ReceivedNaming("default", "", "inbox/{year}", "")
	if dir != "" || n.Folder == nil || n.Name != nil {
		t.Errorf("token folder: dir=%q folder=%v name=%v", dir, n.Folder, n.Name)
	}

	dir, n = ReceivedNaming("default", "pushed", "inbox/{year}", "{id}")
	if dir != "pushed" || n.Folder != nil || n.Name == nil {
		t.Errorf("request folder: dir=%q folder=%v name=%v", dir, n.Folder, n.Name)
	}
	if n.Gallery != "default" {
		t.Errorf("gallery = %q, want default", n.Gallery)
	}
}

func TestIngestNaming(t *testing.T) {

	n := IngestNaming("default", "inbox", "{id}")
	if n.Folder == nil || n.Name == nil || n.Gallery != "default" {
		t.Errorf("literal folder: %+v", n)
	}

	n = IngestNaming("default", "", "{id}")
	if n.Folder != nil || n.Name == nil {
		t.Errorf("blank folder: folder=%v name=%v", n.Folder, n.Name)
	}
	if !IngestNaming("default", "", "").Empty() {
		t.Error("blank settings should be an empty naming")
	}
}

func TestNamingExisting_LeavesAnEmptyHalfAlone(t *testing.T) {
	f := testFacts()
	f.Folder = "sorted"

	folder, name := IngestNaming("default", "inbox/{year}", "{name}-{id}").Existing(f)
	if folder == nil || *folder != "inbox/2026" || name == nil || *name != "IMG_4417-10428" {
		t.Errorf("both halves: folder=%v name=%v", folder, name)
	}
	folder, name = IngestNaming("default", "{source}", "{post_id}").Existing(f)
	if folder != nil || name != nil {
		t.Errorf("empty renders: folder=%v name=%v, want both left alone", folder, name)
	}
	folder, name = IngestNaming("default", "", "{id}").Existing(f)
	if folder != nil || name == nil || *name != "10428" {
		t.Errorf("blank folder: folder=%v name=%v", folder, name)
	}
}

func TestLoadNameFacts_LeavesAnOversizeMD5ToTheBackfill(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	srcPath := createTestPNGFile(t, galleryDir, "big.png")

	img, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, srcPath, "upload")
	mustExec(t, database.Write, `UPDATE images SET md5 = '' WHERE id = ?`, img.ID)

	tmpl := mustParseNameTemplate(t, "{md5}", ScopeRename)
	facts, _, err := LoadNameFacts(t.Context(), database, "default", img.ID, 1, tmpl)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if facts.MD5 != "" {
		t.Errorf("md5 = %q, want the read skipped under the cap", facts.MD5)
	}
	var stored string
	if err := database.Read.QueryRow(`SELECT md5 FROM images WHERE id = ?`, img.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Errorf("stored md5 = %q, want the row left alone", stored)
	}
}

func TestNameTemplate_PerImage(t *testing.T) {
	for _, tc := range []struct {
		tmpl string
		want bool
	}{
		{"{name}", true},
		{"{id}", true},
		{"{hash:8}", true},
		{"{md5}", true},
		{"{folder}/{n}", true},

		{"{folder}", false},
		{"archive/{year}", false},
		{"{folder}/{type}", false},
		{"{gallery}-{date}", false},
		{"plain", false},
	} {
		tmpl := mustParseNameTemplate(t, tc.tmpl, ScopeMoveBatch)
		if got := tmpl.PerImage(); got != tc.want {
			t.Errorf("%q PerImage = %v, want %v", tc.tmpl, got, tc.want)
		}
	}
	if (*NameTemplate)(nil).PerImage() {
		t.Error("a blank template names nothing per image")
	}
}
