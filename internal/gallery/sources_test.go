package gallery

import (
	"errors"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

func scalarSource(t *testing.T, database *db.DB, id int64) (string, string) {
	t.Helper()
	var src, u string
	if err := database.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, id).Scan(&src, &u); err != nil {
		t.Fatal(err)
	}
	return src, u
}

func TestSourcesForImage_OrderAndPrimaryMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	mustAddSource(t, database, id, "danbooru", "111", "https://danbooru/posts/111")
	mustAddSource(t, database, id, "gelbooru", "222", "https://gelbooru/posts/222")
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[1].Site != "gelbooru" {
		t.Fatalf("sources = %+v, want danbooru then gelbooru", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "danbooru" || u != "https://danbooru/posts/111" {
		t.Errorf("mirror = (%q,%q), want the danbooru primary", s, u)
	}
}

func TestRemoveSourceMembership_RebindsAndClearsMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	_ = AddSourceMembership(database, id, "gelbooru", "222", "https://g/222")

	if err := RemoveSourceMembership(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "gelbooru" || u != "https://g/222" {
		t.Errorf("after removing primary, mirror = (%q,%q), want gelbooru", s, u)
	}
	if err := RemoveSourceMembership(database, id, "gelbooru", "222"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "" || u != "" {
		t.Errorf("after removing all, mirror = (%q,%q), want empty", s, u)
	}
}

func TestAddSourceMembership_UpsertsURL(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://old")
	mustAddSource(t, database, id, "danbooru", "111", "https://new")
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].URL != "https://new" {
		t.Fatalf("sources = %+v, want a single row with the updated url", srcs)
	}
}

func TestSetPrimarySource_CreateEditClear(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := SetPrimarySource(database, id, "pixiv", "https://pixiv/1"); err != nil {
		t.Fatal(err)
	}
	if s, u := scalarSource(t, database, id); s != "pixiv" || u != "https://pixiv/1" {
		t.Errorf("mirror = (%q,%q), want pixiv", s, u)
	}
	if err := SetPrimarySource(database, id, "twitter", "https://twitter/2"); err != nil {
		t.Fatal(err)
	}
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].Site != "twitter" {
		t.Fatalf("sources = %+v, want a single twitter origin", srcs)
	}
	if err := SetPrimarySource(database, id, "", ""); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := SourcesForImage(database, id); len(srcs) != 0 {
		t.Fatalf("sources = %+v, want none after clear", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "" || u != "" {
		t.Errorf("mirror = (%q,%q), want empty after clear", s, u)
	}
}

func TestSetSourceField_UpsertClearAndCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*db.DB, int64, string, string, string) error
		read   func(models.ImageSource) string
		first  string
		second string
		fresh  string
	}{
		{
			name:   "commentary",
			set:    SetSourceCommentary,
			read:   func(s models.ImageSource) string { return s.Commentary },
			first:  "hello\nworld",
			second: "new text",
			fresh:  "art notes",
		},
		{
			name:   "commentary_translated",
			set:    SetSourceCommentaryTranslated,
			read:   func(s models.ImageSource) string { return s.CommentaryTranslated },
			first:  "hello there",
			second: "hi there",
			fresh:  "art notes, translated",
		},
		{
			name:   "original",
			set:    SetSourceOriginal,
			read:   func(s models.ImageSource) string { return s.Original },
			first:  "https://pixiv/artworks/1",
			second: "https://twitter/2",
			fresh:  "http://tksn.web.infoseek.co.jp/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newCollectionsTestDB(t)
			id := insertImage(t, database, false)
			_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")

			if err := tc.set(database, id, "danbooru", "111", tc.first); err != nil {
				t.Fatal(err)
			}
			if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || tc.read(srcs[0]) != tc.first {
				t.Fatalf("sources = %+v, want %s set", srcs, tc.name)
			}

			if err := tc.set(database, id, "danbooru", "111", tc.second); err != nil {
				t.Fatal(err)
			}
			if srcs, _ := SourcesForImage(database, id); tc.read(srcs[0]) != tc.second {
				t.Fatalf("%s = %q, want overwritten", tc.name, tc.read(srcs[0]))
			}

			if err := tc.set(database, id, "danbooru", "111", ""); err != nil {
				t.Fatal(err)
			}
			if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 || tc.read(srcs[0]) != "" {
				t.Fatalf("sources = %+v, want the origin kept with an empty %s", srcs, tc.name)
			}

			if err := tc.set(database, id, "pixiv", "", ""); err != nil {
				t.Fatal(err)
			}
			if srcs, _ := SourcesForImage(database, id); len(srcs) != 1 {
				t.Fatalf("sources = %+v, want no phantom origin", srcs)
			}

			if err := tc.set(database, id, "pixiv", "", tc.fresh); err != nil {
				t.Fatal(err)
			}
			if srcs, _ := SourcesForImage(database, id); len(srcs) != 2 {
				t.Fatalf("sources = %+v, want a new pixiv origin", srcs)
			}
		})
	}
}

func TestSeedImageSourcesFromScalar(t *testing.T) {
	database := newCollectionsTestDB(t)

	res, err := database.Write.Exec(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size, source, url) VALUES (?,?,?,?,?,?)`,
		randSHA(t), "/y.png", "image", 1, "danbooru", "https://d/9")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	mustExec(t, database.Write, `INSERT OR IGNORE INTO image_sources (image_id, site, post_id, url) SELECT id, source, '', url FROM images WHERE (source != '' OR url != '') AND NOT EXISTS (SELECT 1 FROM image_sources)`)
	srcs, _ := SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].Site != "danbooru" || srcs[0].URL != "https://d/9" {
		t.Fatalf("seeded sources = %+v, want one danbooru origin", srcs)
	}
}

func TestRenameSourceMembership_KeepsCommentaryAndRekeysAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	if err := SetSourceCommentary(database, id, "danbooru", "111", "artist words"); err != nil {
		t.Fatal(err)
	}
	if err := SetSourceCommentaryTranslated(database, id, "danbooru", "111", "artist words, translated"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "111",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "box"}}); err != nil {
		t.Fatal(err)
	}

	if err := SetSourceOriginal(database, id, "danbooru", "111", "https://pixiv/artworks/1"); err != nil {
		t.Fatal(err)
	}

	if err := RenameSourceMembership(database, id, "danbooru", "111", "gelbooru", "111", "https://g/111"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Site != "gelbooru" || srcs[0].Commentary != "artist words" || srcs[0].Original != "https://pixiv/artworks/1" {
		t.Fatalf("after rename: %+v, want gelbooru row keeping its commentary and original", srcs)
	}
	if srcs[0].CommentaryTranslated != "artist words, translated" {
		t.Fatalf("translation after rename = %q, want it carried with the commentary", srcs[0].CommentaryTranslated)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("annotations after rename = %+v, want re-keyed to gelbooru", anns)
	}
	if s, u := scalarSource(t, database, id); s != "gelbooru" || u != "https://g/111" {
		t.Errorf("mirror = (%q,%q), want the renamed primary", s, u)
	}
}

func TestRenameSourceMembership_MergesOntoExistingIdentity(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	if err := SetSourceCommentary(database, id, "danbooru", "", "kept words"); err != nil {
		t.Fatal(err)
	}
	_ = AddSourceMembership(database, id, "gelbooru", "", "https://g/1")
	if err := SetSourceOriginal(database, id, "gelbooru", "", "https://pixiv/artworks/1"); err != nil {
		t.Fatal(err)
	}
	if err := SetSourceCommentaryTranslated(database, id, "gelbooru", "", "kept words, translated"); err != nil {
		t.Fatal(err)
	}

	if err := RenameSourceMembership(database, id, "gelbooru", "", "danbooru", "", "https://g/1"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].Site != "danbooru" || srcs[0].Commentary != "kept words" || srcs[0].URL != "https://g/1" {
		t.Fatalf("after merge: %+v, want one danbooru row keeping its commentary with the submitted url", srcs)
	}
	if srcs[0].Original != "https://pixiv/artworks/1" {
		t.Fatalf("original = %q, want the merged row to fill its empty original from the dropped one", srcs[0].Original)
	}
	if srcs[0].CommentaryTranslated != "kept words, translated" {
		t.Fatalf("translation = %q, want the merged row to fill its empty translation from the dropped one", srcs[0].CommentaryTranslated)
	}
}

func TestRemoveSourceMembership_CascadesAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	_ = AddSourceMembership(database, id, "gelbooru", "222", "https://g/222")
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "111",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "goes away"}}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "gelbooru", "222",
		[]models.Annotation{{X: 5, Y: 6, W: 7, H: 8, Body: "stays"}}); err != nil {
		t.Fatal(err)
	}

	if err := RemoveSourceMembership(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("annotations after remove = %+v, want only gelbooru's", anns)
	}
}

func TestSetPrimarySource_ClearCascadesAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "box"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetPrimarySource(database, id, "", ""); err != nil {
		t.Fatal(err)
	}
	anns, err := AnnotationsForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 0 {
		t.Fatalf("annotations after clearing the only origin = %+v, want none", anns)
	}
}

func TestAddSourceMembership_EmptyURLKeepsStoredOne(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/100")

	mustAddSource(t, database, id, "danbooru", "", "")
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].URL != "https://d/100" {
		t.Fatalf("after url-less re-add: %+v, want the stored url kept", srcs)
	}
	if _, u := scalarSource(t, database, id); u != "https://d/100" {
		t.Errorf("mirror url = %q, want kept", u)
	}

	mustAddSource(t, database, id, "danbooru", "", "https://d/200")
	if srcs, _ = SourcesForImage(database, id); srcs[0].URL != "https://d/200" {
		t.Errorf("url after non-empty re-add = %q, want https://d/200", srcs[0].URL)
	}
}

func TestSetPrimarySource_RelabelCarriesThePulledAnnotations(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")
	box := []models.Annotation{{X: 1, Y: 2, W: 3, H: 4, Body: "box"}}
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "111", box); err != nil {
		t.Fatal(err)
	}

	if err := SetPrimarySource(database, id, "gelbooru", "https://g/111"); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "gelbooru", "111", box); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("annotations after relabel and re-pull = %+v, want one keyed to gelbooru", anns)
	}
	if err := RemoveSourceMembership(database, id, "gelbooru", "111"); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 0 {
		t.Errorf("annotations after removing the only origin = %+v, want none", anns)
	}
}

func TestSetPrimarySource_RefusesCollidingIdentity(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "danbooru", "", "https://d/1")
	_ = AddSourceMembership(database, id, "gelbooru", "", "https://g/1")

	err := SetPrimarySource(database, id, "gelbooru", "https://g/2")
	if !errors.Is(err, ErrSourceIdentityExists) {
		t.Fatalf("err = %v, want ErrSourceIdentityExists", err)
	}
	srcs, qerr := SourcesForImage(database, id)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[0].URL != "https://d/1" {
		t.Fatalf("rows after refused relabel = %+v, want unchanged", srcs)
	}
}

func TestMakeSourcePrimary_ReordersAndRebindsMirror(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	_ = AddSourceMembership(database, id, "ptr", "", "")
	_ = AddSourceMembership(database, id, "danbooru", "111", "https://d/111")

	if err := MakeSourcePrimary(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 || srcs[0].Site != "danbooru" || srcs[1].Site != "ptr" {
		t.Fatalf("sources = %+v, want danbooru promoted first", srcs)
	}
	if s, u := scalarSource(t, database, id); s != "danbooru" || u != "https://d/111" {
		t.Errorf("mirror = (%q,%q), want the promoted danbooru row", s, u)
	}

	if err := MakeSourcePrimary(database, id, "danbooru", "111"); err != nil {
		t.Fatal(err)
	}
	if s, _ := scalarSource(t, database, id); s != "danbooru" {
		t.Errorf("mirror after re-promoting = %q, want danbooru", s)
	}

	if err := MakeSourcePrimary(database, id, "nowhere", ""); err == nil {
		t.Error("promoting a missing source should error")
	}
}

func TestSetSourceMD5Match_FallsBackToIdlessRow(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	mustAddSource(t, database, id, "danbooru", "", "https://danbooru/posts/9")

	if err := SetSourceMD5Match(database, id, "danbooru", "9", "differ"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources = %+v (%v)", srcs, err)
	}
	if srcs[0].MD5Match != "differ" {
		t.Errorf("md5_match = %q, want differ on the id-less row", srcs[0].MD5Match)
	}

	mustAddSource(t, database, id, "danbooru", "9", "https://danbooru/posts/9")
	if err := SetSourceMD5Match(database, id, "danbooru", "9", "match"); err != nil {
		t.Fatal(err)
	}
	srcs, _ = SourcesForImage(database, id)
	if len(srcs) != 1 || srcs[0].MD5Match != "match" {
		t.Errorf("sources = %+v, want the adopted row marked match", srcs)
	}
}

func TestMarkSourceExact_ResetsSimilarityAndLedger(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	mustAddSource(t, database, id, "danbooru", "42", "https://danbooru/posts/42")
	if err := setSourceFieldIfSet(database, id, "danbooru", "42", "md5", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := SetSourceSimilarity(database, id, "danbooru", "42", 91); err != nil {
		t.Fatal(err)
	}
	if err := SetSourceMD5Match(database, id, "danbooru", "42", "differ"); err != nil {
		t.Fatal(err)
	}
	if err := MarkSourceExact(database, id, "danbooru", "42", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	srcs, err := SourcesForImage(database, id)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("sources = %+v (%v)", srcs, err)
	}
	s := srcs[0]
	if s.Similarity != 0 || s.MD5 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || s.MD5Match != "match" {
		t.Errorf("origin = %+v, want similarity 0, refreshed md5, match", s)
	}
}

func TestSourceWritersMaintainTheUpgradeVerdict(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	mustExec(t, database.Write, `UPDATE images SET md5 = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id = ?`, id)
	mustAddSource(t, database, id, "danbooru", "1", "https://d/1")
	verdict := func() models.ImageSource {
		srcs, err := SourcesForImage(database, id)
		if err != nil || len(srcs) != 1 {
			t.Fatalf("sources = %+v (err %v)", srcs, err)
		}
		return srcs[0]
	}
	if got := verdict(); got.MD5Match != "" {
		t.Errorf("origin with no claim = %q, want no verdict", got.MD5Match)
	}
	if err := setSourceFieldIfSet(database, id, "danbooru", "1", "md5", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"); err != nil {
		t.Fatal(err)
	}
	if got := verdict(); got.MD5Match != "differ" {
		t.Errorf("claim against a different file = %q, want differ", got.MD5Match)
	}
	if err := MarkSourceExact(database, id, "danbooru", "1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if got := verdict(); got.MD5Match != "match" || got.Similarity != 0 {
		t.Errorf("after a replace = %+v, want a match at similarity 0", got)
	}
}
