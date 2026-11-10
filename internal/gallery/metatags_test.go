package gallery

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

func TestMetaTagsFor_Vocabulary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts metaFacts
		want  []string
	}{
		{"no dimensions", metaFacts{FileType: "png"}, nil},
		{"just under highres", metaFacts{Width: 1599, Height: 1199, FileType: "png"}, nil},
		{"highres on width", metaFacts{Width: 1600, Height: 900, FileType: "png"}, []string{"highres"}},
		{"highres on height", metaFacts{Width: 900, Height: 1200, FileType: "png"}, []string{"highres"}},
		{"absurdres carries highres", metaFacts{Width: 3200, Height: 2400, FileType: "png"},
			[]string{"absurdres", "highres"}},
		{"the whole band stacks", metaFacts{Width: 10000, Height: 10000, FileType: "png"},
			[]string{"absurdres", "highres", "incredibly_absurdres"}},
		{"lowres at the edge", metaFacts{Width: 500, Height: 500, FileType: "png"}, []string{"lowres"}},
		{"one pixel past lowres", metaFacts{Width: 501, Height: 500, FileType: "png"}, nil},
		{"wide needs both the width and the ratio", metaFacts{Width: 1024, Height: 256, FileType: "png"},
			[]string{"wide_image"}},
		{"ratio just short", metaFacts{Width: 1024, Height: 257, FileType: "png"}, nil},
		{"too narrow to be wide", metaFacts{Width: 1023, Height: 255, FileType: "png"}, nil},
		{"tall", metaFacts{Width: 256, Height: 1024, FileType: "png"}, []string{"tall_image"}},
		{"video is animated too", metaFacts{FileType: "mp4"}, []string{"animated", "video"}},
		{"video with audio", metaFacts{FileType: "webm", HasSound: true},
			[]string{"animated", "sound", "video"}},
		{"animated gif", metaFacts{Width: 100, Height: 100, FileType: "gif", AnimatedGIF: true},
			[]string{"animated", "animated_gif", "lowres"}},
		{"apng", metaFacts{FileType: "png", AnimatedPNG: true}, []string{"animated", "animated_png"}},
		{"animated webp", metaFacts{FileType: "webp", AnimatedWebP: true}, []string{"animated"}},
		{"avif sequence", metaFacts{FileType: "avif", AnimatedAVIF: true}, []string{"animated"}},
		{"jxl animation", metaFacts{FileType: "jxl", AnimatedJXL: true}, []string{"animated"}},
		{"greyscale and rotated", metaFacts{FileType: "jpeg", Greyscale: true, EXIFRotated: true},
			[]string{"exif_rotation", "greyscale"}},
		{"ai", metaFacts{FileType: "png", AIGenerated: true}, []string{"ai-generated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := metaTagsFor(tc.facts); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("metaTagsFor(%+v) = %v, want %v", tc.facts, got, tc.want)
			}
		})
	}
}

func TestAnimatedGIF_CountsFrames(t *testing.T) {
	dir := t.TempDir()
	one := writeGIF(t, dir, "one.gif", 1)
	many := writeGIF(t, dir, "many.gif", 3)
	if animatedGIF(one) {
		t.Error("a one-frame gif read as animated")
	}
	if !animatedGIF(many) {
		t.Error("a three-frame gif read as static")
	}
	if animatedGIF(filepath.Join(dir, "absent.gif")) {
		t.Error("a missing file read as animated")
	}
}

func TestAnimatedPNG_FindsTheControlChunk(t *testing.T) {
	dir := t.TempDir()
	plain := createTestPNGFile(t, dir, "plain.png")
	if animatedPNG(plain) {
		t.Error("a plain png read as animated")
	}
	apng := createTestPNGFile(t, dir, "moving.png")
	spliceAPNGChunk(t, apng)
	if !animatedPNG(apng) {
		t.Error("an acTL chunk went unseen")
	}
}

func TestAnimatedWebP_CountsFrames(t *testing.T) {
	dir := t.TempDir()
	vp8x := riffChunk("VP8X", make([]byte, 10))
	anim := riffChunk("ANIM", make([]byte, 6))
	frame := riffChunk("ANMF", make([]byte, 24))
	for _, tc := range []struct {
		name string
		file []byte
		want bool
	}{
		{"still.webp", webpFile(riffChunk("VP8 ", make([]byte, 31))), false},
		{"one-frame.webp", webpFile(vp8x, anim, frame), false},
		{"two-frames.webp", webpFile(vp8x, anim, frame, frame), true},
		{"padded.webp", webpFile(vp8x, riffChunk("ICCP", make([]byte, 7)), anim, frame, frame), true},
		{"avi.webp", slices.Concat([]byte("RIFF\x00\x00\x00\x00AVI "), frame, frame), false},
	} {
		path := filepath.Join(dir, tc.name)
		mustWriteFile(t, path, tc.file, 0o644)
		if got := animatedWebP(path); got != tc.want {
			t.Errorf("animatedWebP(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if animatedWebP(filepath.Join(dir, "absent.webp")) {
		t.Error("a missing file read as animated")
	}
}

func TestAnimatedAVIF_ReadsTheSequenceBrand(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		head []byte
		want bool
	}{
		{"still.avif", ftypBox("avif", "avif", "mif1", "miaf", "MA1B"), false},
		{"sequence.avif", ftypBox("avis", "avis", "avif", "msf1", "iso8", "mif1", "miaf", "MA1B"), true},
		{"compatible.avif", ftypBox("mif1", "miaf", "avis"), true},
		{"not-ftyp.avif", isoBox("skip", []byte("avisavisavis")), false},
	} {
		path := filepath.Join(dir, tc.name)
		mustWriteFile(t, path, tc.head, 0o644)
		if got := animatedAVIF(path); got != tc.want {
			t.Errorf("animatedAVIF(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if animatedAVIF(filepath.Join(dir, "absent.avif")) {
		t.Error("a missing file read as animated")
	}
}

var (
	jxlStill          = []byte{0xff, 0x0a, 0xcb, 0x06, 0x00, 0x2d, 0x88, 0x02}
	jxlAnimation      = []byte{0xff, 0x0a, 0xcb, 0x04, 0x4d, 0x00, 0x20, 0x27}
	jxlLargeAnimation = []byte{0xff, 0x0a, 0x88, 0x06, 0x96, 0x0a, 0x9a, 0x00}
	jxlRotatedStill   = []byte{0xff, 0x0a, 0xc8, 0x32, 0x0b, 0xc4, 0x04}
)

func TestAnimatedJXL_ReadsTheImageHeader(t *testing.T) {
	dir := t.TempDir()
	container := func(boxes ...[]byte) []byte {
		return slices.Concat(append([][]byte{jxlContainer, ftypBox("jxl ", "jxl ")}, boxes...)...)
	}
	part := func(index uint32, data []byte) []byte {
		return isoBox("jxlp", slices.Concat(binary.BigEndian.AppendUint32(nil, index), data))
	}
	for _, tc := range []struct {
		name string
		file []byte
		want bool
	}{
		{"still.jxl", jxlStill, false},
		{"animation.jxl", jxlAnimation, true},
		{"large.jxl", jxlLargeAnimation, true},
		{"rotated.jxl", jxlRotatedStill, false},
		{"jxlc.jxl", container(isoBox("Exif", make([]byte, 4096)), isoBox("jxlc", jxlAnimation)), true},
		{"unsized.jxl", container(slices.Concat([]byte{0, 0, 0, 0}, []byte("jxlc"), jxlAnimation)), true},
		{"jxlp.jxl", container(part(0, jxlAnimation), part(1<<31|1, make([]byte, 64))), true},

		{"jbrd.jxl", container(part(0, jxlRotatedStill), isoBox("jbrd", make([]byte, 200)), part(1<<31|1, make([]byte, 64))), false},
		{"empty.jxl", container(isoBox("Exif", make([]byte, 64))), false},
	} {
		path := filepath.Join(dir, tc.name)
		mustWriteFile(t, path, tc.file, 0o644)
		if got := animatedJXL(path); got != tc.want {
			t.Errorf("animatedJXL(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if animatedJXL(filepath.Join(dir, "absent.jxl")) {
		t.Error("a missing file read as animated")
	}
}

func TestJXLHasAnimation_ReachesTheFlagPastEveryOptionalSize(t *testing.T) {

	size := [][2]uint32{{0, 1}, {3, 2}, {1<<30 - 1, 30}, {0, 3}, {3, 2}, {1<<30 - 1, 30}}
	header := packBits(slices.Concat(
		[][2]uint32{{0x0AFF, 16}}, size,
		[][2]uint32{{0, 1}, {1, 1}, {0, 3}, {1, 1}}, size,
		[][2]uint32{{1, 1}, {0, 1}, {3, 2}, {4095, 12}, {0, 3}, {3, 2}, {4095, 12}},
		[][2]uint32{{1, 1}},
	))
	if !jxlHasAnimation(bytes.NewReader(header)) {
		t.Errorf("a %d-byte header with an intrinsic size and a preview read as still", len(header))
	}
	if jxlHasAnimation(bytes.NewReader(header[:len(header)-1])) {
		t.Error("a header cut before the flag read as animated")
	}
}

func TestApplyMetaTags_AnimatedWebPAVIFAndJXL(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	frame := riffChunk("ANMF", make([]byte, 24))
	for _, tc := range []struct {
		name string
		file []byte
		want bool
	}{
		{"still.webp", webpFile(riffChunk("VP8 ", make([]byte, 30))), false},
		{"moving.webp", webpFile(riffChunk("VP8X", make([]byte, 10)), riffChunk("ANIM", make([]byte, 6)), frame, frame), true},
		{"still.avif", ftypBox("avif", "avif", "mif1", "miaf"), false},
		{"moving.avif", ftypBox("avis", "avis", "avif", "mif1", "miaf"), true},
		{"still.jxl", jxlStill, false},
		{"moving.jxl", jxlAnimation, true},
	} {
		path := filepath.Join(galleryDir, tc.name)
		mustWriteFile(t, path, tc.file, 0o644)
		rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
		if got := slices.Contains(metaTagNames(t, database, rec.ID), "animated"); got != tc.want {
			t.Errorf("%s: animated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGreyscaleThumb_RejectsATint(t *testing.T) {
	dir := t.TempDir()
	if !greyscaleThumb(writeJPEG(t, dir, "grey.jpg", color.RGBA{90, 90, 90, 255})) {
		t.Error("a flat grey thumbnail read as coloured")
	}
	if greyscaleThumb(writeJPEG(t, dir, "sepia.jpg", color.RGBA{120, 96, 64, 255})) {
		t.Error("a sepia thumbnail read as greyscale")
	}
	if greyscaleThumb(filepath.Join(dir, "absent.jpg")) {
		t.Error("a missing thumbnail produced a verdict")
	}
}

func TestGreyscaleThumb_AllocatesNothingPerPixel(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 256, 200))
	for y := range 200 {
		for x := range 256 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	path := filepath.Join(t.TempDir(), "thumb.jpg")
	f := mustCreate(t, path)
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if allocs := testing.AllocsPerRun(5, func() { greyscaleThumb(path) }); allocs > 50 {
		t.Errorf("greyscaleThumb allocated %.0f times on a 256x200 thumbnail, want the decode's few", allocs)
	}
}

func TestGreyscaleThumb_FastPathAgreesWithTheGenericLoop(t *testing.T) {
	type generic struct{ image.Image }
	dir := t.TempDir()
	var imgs []image.Image
	for _, c := range []color.RGBA{{200, 40, 40, 255}, {90, 90, 90, 255}, {120, 96, 64, 255}, {100, 104, 108, 255}} {
		f, err := os.Open(writeJPEG(t, dir, "c.jpg", c))
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		imgs = append(imgs, img)
	}
	sub := image.NewYCbCr(image.Rect(3, 5, 67, 45), image.YCbCrSubsampleRatio420)
	for i := range sub.Y {
		sub.Y[i] = uint8(i * 7)
	}
	for i := range sub.Cb {
		sub.Cb[i], sub.Cr[i] = uint8(124+i%9), uint8(132-i%7)
	}
	gray := image.NewGray(image.Rect(0, 0, 9, 9))
	imgs = append(imgs, sub, gray)
	for i, img := range imgs {
		fg, ft := greyPixels(img)
		gg, gt := greyPixels(generic{img})
		if fg != gg || ft != gt {
			t.Errorf("image %d (%T): fast path %d/%d grey, generic loop %d/%d", i, img, fg, ft, gg, gt)
		}
	}
	for y := 0; y < 256; y += 5 {
		for cb := 0; cb < 256; cb += 7 {
			for cr := 0; cr < 256; cr += 11 {
				r, g, b := color.YCbCrToRGB(uint8(y), uint8(cb), uint8(cr))
				R, G, B, _ := color.YCbCr{Y: uint8(y), Cb: uint8(cb), Cr: uint8(cr)}.RGBA()
				if uint32(r) != R>>8 || uint32(g) != G>>8 || uint32(b) != B>>8 {
					t.Fatalf("YCbCr(%d,%d,%d): YCbCrToRGB %d,%d,%d, RGBA>>8 %d,%d,%d", y, cb, cr, r, g, b, R>>8, G>>8, B>>8)
				}
			}
		}
	}
}

func TestApplyMetaTags_AttributesTheDerivation(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "big.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	if got := metaTagNames(t, database, rec.ID); !slices.Contains(got, "highres") {
		t.Fatalf("tags = %v, want highres among them", got)
	}
	var category, tagger string
	if err := database.Read.QueryRow(
		`SELECT tc.name, COALESCE(it.tagger_name, '') FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.name = 'highres'`, rec.ID,
	).Scan(&category, &tagger); err != nil {
		t.Fatal(err)
	}
	if category != "meta" || tagger != models.TagSourceMonbooru {
		t.Errorf("highres filed as %s/%s, want meta/%s", category, tagger, models.TagSourceMonbooru)
	}
	if n := countRows(t, database, "image_tag_sources WHERE image_id = ? AND source = ?",
		rec.ID, models.TagSourceMonbooru); n == 0 {
		t.Error("the derivation left no ledger row")
	}
	var auto, stale int
	if err := database.Read.QueryRow(
		`SELECT it.is_auto, it.stale FROM image_tags it JOIN tags t ON t.id = it.tag_id
		 WHERE it.image_id = ? AND t.name = 'highres'`, rec.ID,
	).Scan(&auto, &stale); err != nil {
		t.Fatal(err)
	}
	if auto != 0 || stale != 0 {
		t.Errorf("is_auto=%d stale=%d, want 0/0: the row is derived, not inferred, and never goes stale", auto, stale)
	}
}

func TestApplyMetaTags_Idempotent(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "same.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	before := metaTagNames(t, database, rec.ID)
	svc, metaCat := metaFixture(t, database)
	moved, err := applyMetaTags(database, svc, metaCat, env.thumbnailsPath, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Error("a second pass over an unchanged file reported a change")
	}
	if got := metaTagNames(t, database, rec.ID); !reflect.DeepEqual(got, before) {
		t.Errorf("tags = %v, want %v unchanged", got, before)
	}
}

func TestApplyMetaTags_StableAfterItsTagIsAliased(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "aliased.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	svc, metaCat := metaFixture(t, database)
	highres, err := svc.GetOrCreateTag("highres", metaCat)
	if err != nil {
		t.Fatal(err)
	}
	spelling, err := svc.GetOrCreateTag("high_resolution", metaCat)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MergeTags(highres.ID, spelling.ID); err != nil {
		t.Fatal(err)
	}
	want := metaTagNames(t, database, rec.ID)
	if !slices.Contains(want, "high_resolution") {
		t.Fatalf("tags after the merge = %v, want high_resolution among them", want)
	}

	for pass := 1; pass <= 3; pass++ {
		moved, err := applyMetaTags(database, svc, metaCat, env.thumbnailsPath, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if moved {
			t.Errorf("pass %d reported a change", pass)
		}
		if got := metaTagNames(t, database, rec.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("pass %d: tags = %v, want %v", pass, got, want)
		}
	}
}

func TestApplyMetaTags_WithdrawsWhatTheFileLost(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "shrink.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	if got := metaTagNames(t, database, rec.ID); !slices.Contains(got, "highres") {
		t.Fatalf("tags = %v, want highres before the shrink", got)
	}

	mustExec(t, database.Write, `UPDATE images SET width = 400, height = 400 WHERE id = ?`, rec.ID)
	svc, metaCat := metaFixture(t, database)
	if _, err := applyMetaTags(database, svc, metaCat, env.thumbnailsPath, rec.ID); err != nil {
		t.Fatal(err)
	}
	got := metaTagNames(t, database, rec.ID)
	if slices.Contains(got, "highres") {
		t.Errorf("tags = %v, want highres gone", got)
	}
	if !slices.Contains(got, "lowres") {
		t.Errorf("tags = %v, want lowres for the new size", got)
	}
}

func TestApplyMetaTags_LeavesWhatASourceAlsoVouchesFor(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "shared.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")

	svc, metaCat := metaFixture(t, database)
	highres, err := svc.GetOrCreateTagFrom("highres", metaCat, "danbooru")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncSourceTags(rec.ID, []int64{highres.ID}, "danbooru", false); err != nil {
		t.Fatal(err)
	}

	mustExec(t, database.Write, `UPDATE images SET width = 400, height = 400 WHERE id = ?`, rec.ID)
	if _, err := applyMetaTags(database, svc, metaCat, env.thumbnailsPath, rec.ID); err != nil {
		t.Fatal(err)
	}
	if got := metaTagNames(t, database, rec.ID); !slices.Contains(got, "highres") {
		t.Errorf("tags = %v, want highres kept for danbooru", got)
	}
	if n := countRows(t, database, "image_tag_sources WHERE image_id = ? AND source = ? AND tag_id = ?",
		rec.ID, models.TagSourceMonbooru, highres.ID); n != 0 {
		t.Error("the derivation kept a claim it no longer makes")
	}
}

func TestApplyMetaTags_GateStopsTheDerivation(t *testing.T) {
	MetaTagsEnabled.Store(false)
	t.Cleanup(func() { MetaTagsEnabled.Store(true) })

	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "off.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	if got := metaTagNames(t, database, rec.ID); len(got) != 0 {
		t.Errorf("tags = %v, want none while the switch is off", got)
	}

	if _, updated, err := BackfillMetaTags(context.Background(), testHandle(database, "", env.thumbnailsPath), nil); err != nil || updated != 1 {
		t.Fatalf("BackfillMetaTags: updated=%d err=%v, want 1 and no error", updated, err)
	}
	if got := metaTagNames(t, database, rec.ID); !slices.Contains(got, "highres") {
		t.Errorf("tags = %v, want the backfill to have derived them", got)
	}
}

func TestRemoveMetaTags_ClearsTheClaim(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "strip.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	if len(metaTagNames(t, database, rec.ID)) == 0 {
		t.Fatal("nothing derived to remove")
	}

	processed, removed, err := RemoveMetaTags(context.Background(), testHandle(database, "", ""), nil)
	if err != nil || processed != 1 || removed == 0 {
		t.Fatalf("RemoveMetaTags: processed=%d removed=%d err=%v", processed, removed, err)
	}
	if got := metaTagNames(t, database, rec.ID); len(got) != 0 {
		t.Errorf("tags = %v, want none left", got)
	}
	if processed, _, err := RemoveMetaTags(context.Background(), testHandle(database, "", ""), nil); err != nil || processed != 0 {
		t.Errorf("second pass: processed=%d err=%v, want an empty walk", processed, err)
	}
}

func TestRemoveMetaTags_KeepsATagTheOperatorRetyped(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	path := createTestPNGFileSize(t, galleryDir, "retyped.png", 1600, 1200)
	rec, _ := mustIngest(t, database, galleryDir, env.thumbnailsPath, path, "")
	svc, metaCat := metaFixture(t, database)
	highres, err := svc.GetOrCreateTag("highres", metaCat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddTagsToOneImage(rec.ID, []int64{highres.ID}, ""); err != nil {
		t.Fatal(err)
	}

	if _, _, err := RemoveMetaTags(context.Background(), testHandle(database, "", ""), nil); err != nil {
		t.Fatal(err)
	}
	if got := metaTagNames(t, database, rec.ID); !reflect.DeepEqual(got, []string{"highres"}) {
		t.Errorf("tags = %v, want only the retyped highres left", got)
	}
}

func metaFixture(t *testing.T, database *db.DB) (*tags.Service, int64) {
	t.Helper()
	id, err := metaCategoryID(database)
	if err != nil {
		t.Fatal(err)
	}
	return tags.New(database, nil), id
}

func metaTagNames(t *testing.T, database *db.DB, imageID int64) []string {
	t.Helper()
	out, err := db.QueryStrings(database.Read,
		`SELECT t.name FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND tc.name = 'meta' ORDER BY t.name`, imageID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeGIF(t *testing.T, dir, name string, frames int) string {
	t.Helper()
	g := &gif.GIF{}
	for range frames {
		frame := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White})
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 10)
	}
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	if err := gif.EncodeAll(f, g); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJPEG(t *testing.T, dir, name string, c color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := range 32 {
		for x := range 32 {
			img.Set(x, y, c)
		}
	}
	path := filepath.Join(dir, name)
	f := mustCreate(t, path)
	defer func() { _ = f.Close() }()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return path
}

func riffChunk(typ string, payload []byte) []byte {
	chunk := slices.Concat([]byte(typ), binary.LittleEndian.AppendUint32(nil, uint32(len(payload))), payload)
	if len(payload)%2 == 1 {
		chunk = append(chunk, 0)
	}
	return chunk
}

func webpFile(chunks ...[]byte) []byte {
	body := slices.Concat(append([][]byte{[]byte("WEBP")}, chunks...)...)
	return slices.Concat([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body)
}

func isoBox(typ string, payload []byte) []byte {
	return slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(payload))), []byte(typ), payload)
}

func ftypBox(major string, compatible ...string) []byte {
	body := []byte(major + "\x00\x00\x00\x00")
	for _, brand := range compatible {
		body = append(body, brand...)
	}
	return isoBox("ftyp", body)
}

func packBits(fields [][2]uint32) []byte {
	var out []byte
	pos := 0
	for _, f := range fields {
		for i := range f[1] {
			if pos%8 == 0 {
				out = append(out, 0)
			}
			out[len(out)-1] |= byte(f[0]>>i&1) << (pos % 8)
			pos++
		}
	}
	return out
}

func spliceAPNGChunk(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(raw, []byte("IDAT"))
	if at < 4 {
		t.Fatal("no IDAT chunk in the fixture")
	}
	body := []byte{0, 0, 0, 2, 0, 0, 0, 0}
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	chunk = append(chunk, "acTL"...)
	chunk = append(chunk, body...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("acTL"), body...)))

	out := slices.Concat(raw[:at-4], chunk, raw[at-4:])
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}
