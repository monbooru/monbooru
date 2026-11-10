package library

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
)

func pngBytes(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := 0; x < 32; x++ {
		for y := 0; y < 32; y++ {
			img.Set(x, y, color.RGBA{shade, uint8(x * 8), uint8(y * 8), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func land(t *testing.T, cx *Gallery, name string, body []byte, spec LandSpec) (LandResult, error) {
	t.Helper()
	return cx.Land(context.Background(), cx.GalleryPath, name, FromReader(bytes.NewReader(body)), spec)
}

func TestLand_IngestsAndFeedsABuiltTree(t *testing.T) {
	cx := openTestGallery(t)
	if err := cx.bkTree.EnsureBuilt(cx.DB); err != nil {
		t.Fatal(err)
	}
	res, err := land(t, cx, "a.png", pngBytes(t, 10), LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Image == nil || res.Duplicate || res.Path != filepath.Join(cx.GalleryPath, "a.png") || !exists(res.Path) {
		t.Fatalf("landed %+v", res)
	}
	if res.Image.Phash == nil {
		t.Fatal("test premise broken: no phash for the landed image")
	}
	if ids, _ := cx.bkTree.SearchWithinDistance(*res.Image.Phash, 0); !slices.Contains(ids, res.Image.ID) {
		t.Errorf("the built tree lacks image %d: %v", res.Image.ID, ids)
	}
}

func TestLand_DropsTheCopyOfBytesAlreadyHeld(t *testing.T) {
	cx := openTestGallery(t)
	body := pngBytes(t, 20)
	first, err := land(t, cx, "a.png", body, LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := land(t, cx, "a.png", body, LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Duplicate || !again.Dropped || again.Image.ID != first.Image.ID {
		t.Fatalf("second landing %+v, want a dropped duplicate of %d", again, first.Image.ID)
	}
	if exists(again.Path) || !exists(first.Path) {
		t.Errorf("copy kept %t, original kept %t; want only the original", exists(again.Path), exists(first.Path))
	}
}

func TestLand_KeepsAFileTheIngestFailedOnOnlyWhenAsked(t *testing.T) {
	for _, keep := range []bool{false, true} {
		cx := openTestGallery(t)
		mustExec(t, cx.DB.Write, `CREATE TRIGGER refuse BEFORE INSERT ON images BEGIN SELECT RAISE(ABORT, 'busy'); END`)
		_, err := cx.Land(context.Background(), cx.GalleryPath, "a.png", FromReader(bytes.NewReader(pngBytes(t, 30))),
			LandSpec{Origin: "upload", KeepOnFailure: keep})
		if LandStageOf(err) != LandIngest {
			t.Fatalf("keep=%t: err %v, want an ingest failure", keep, err)
		}
		if got := exists(filepath.Join(cx.GalleryPath, "a.png")); got != keep {
			t.Errorf("keep=%t: file kept %t", keep, got)
		}
	}
}

func TestLand_RemovesAFileOfNoSupportedType(t *testing.T) {
	cx := openTestGallery(t)
	_, err := land(t, cx, "notes.txt", []byte("not an image"), LandSpec{Origin: "upload", KeepOnFailure: true})
	if LandStageOf(err) != LandType {
		t.Fatalf("err %v, want a type failure", err)
	}
	if exists(filepath.Join(cx.GalleryPath, "notes.txt")) {
		t.Error("an unsupported file stayed")
	}
}

func TestLand_RefusesAFolderTheGalleryLeavesOut(t *testing.T) {
	cx := openTestGallery(t)
	inner := filepath.Join(cx.GalleryPath, ".H")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	cx.Bounds.Store(gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))
	_, err := cx.Land(context.Background(), inner, "a.png", FromReader(bytes.NewReader(pngBytes(t, 40))), LandSpec{Origin: "upload"})
	if LandStageOf(err) != LandPlace {
		t.Fatalf("err %v, want the place refused", err)
	}
	if exists(filepath.Join(inner, "a.png")) {
		t.Error("a file was written into another gallery's folder")
	}
}

func TestLand_RefusesALeftOutFolderBeforeCreatingIt(t *testing.T) {
	cx := openTestGallery(t)
	inner := filepath.Join(cx.GalleryPath, ".H")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	cx.Bounds.Store(gallery.NewBoundary(cx.GalleryPath, []gallery.Fence{{Path: inner, Owner: "H", Kind: gallery.FenceGallery}}, nil))
	sub := filepath.Join(inner, "sub")
	_, err := cx.Land(context.Background(), sub, "a.png", FromReader(bytes.NewReader(pngBytes(t, 40))), LandSpec{Origin: "merge"})
	if LandStageOf(err) != LandPlace {
		t.Fatalf("err %v, want the place refused", err)
	}
	if exists(sub) {
		t.Error("a folder was created inside another gallery's folder")
	}
}

func TestLand_ALandingInsideAnothersWriteKeepsBoth(t *testing.T) {
	cx := openTestGallery(t)
	outerBody, innerBody := pngBytes(t, 30), pngBytes(t, 31)
	var inner LandResult
	outer, err := cx.Land(context.Background(), cx.GalleryPath, "a.png", func(dst string) error {
		var err error
		inner, err = land(t, cx, "a.png", innerBody, LandSpec{Origin: "upload"})
		if err != nil {
			return err
		}
		return os.WriteFile(dst, outerBody, 0o644)
	}, LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if inner.Image == nil || outer.Image == nil || inner.Image.ID == outer.Image.ID || inner.Path == outer.Path {
		t.Fatalf("inner %+v, outer %+v: want two rows on two paths", inner, outer)
	}
	for _, r := range []LandResult{inner, outer} {
		sha, err := gallery.HashFile(r.Path)
		if err != nil {
			t.Fatal(err)
		}
		var rowSHA string
		if err := cx.DB.Read.QueryRow(`SELECT sha256 FROM images WHERE id = ?`, r.Image.ID).Scan(&rowSHA); err != nil {
			t.Fatal(err)
		}
		if sha != rowSHA {
			t.Errorf("%s holds other bytes than image %d's", r.Path, r.Image.ID)
		}
	}
}

func TestLand_RefusesAFileOverTheCap(t *testing.T) {
	cx := openTestGallery(t)
	body := pngBytes(t, 50)
	_, err := land(t, cx, "big.png", body, LandSpec{Origin: "upload", MaxBytes: int64(len(body) - 1)})
	if LandStageOf(err) != LandSize {
		t.Fatalf("err %v, want the size refused", err)
	}
	if exists(filepath.Join(cx.GalleryPath, "big.png")) {
		t.Error("the oversized file stayed")
	}
}

func TestLandInPlace_LeavesTheOperatorsFileAlone(t *testing.T) {
	cx := openTestGallery(t)
	body := pngBytes(t, 60)
	held, err := land(t, cx, "held.png", body, LandSpec{Origin: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(cx.GalleryPath, "second.png")
	mustWriteFile(t, second, body, 0o644)
	refused := errors.New("refused")
	if _, err := cx.LandInPlace(context.Background(), second, LandSpec{Origin: "ingest", Check: func(string, string) error { return refused }}); !errors.Is(err, refused) {
		t.Fatalf("err %v, want the check's refusal", err)
	}
	if !exists(second) {
		t.Fatal("a refused in-place file was removed")
	}
	res, err := cx.LandInPlace(context.Background(), second, LandSpec{Origin: "ingest"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate || res.Dropped || res.Image.ID != held.Image.ID || !exists(second) {
		t.Errorf("in-place duplicate %+v, want it kept as an alias of %d", res, held.Image.ID)
	}
}

func TestLand_AGalleryNamedByARelativePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monbooru.toml")
	mustWriteFile(t, path, []byte("[[galleries]]\nname = \"rel\"\ngallery_path = \"gallery\"\n\n[paths]\ndata_path = \"data\"\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "gallery", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cfg, err := config.LoadWithDefaults(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cx, err := Open(cfg.Galleries[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cx.Close)

	sub, err := cx.Boundary().ResolveSubdir("sub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cx.Land(context.Background(), sub, "a.png", FromFile(writeTemp(t, pngBytes(t, 10))), LandSpec{Origin: "upload"}); err != nil {
		t.Errorf("landing into a subfolder: %v", err)
	}
	inPlace := filepath.Join(dir, "gallery", "sub", "b.png")
	mustWriteFile(t, inPlace, pngBytes(t, 200), 0o644)
	if _, err := cx.LandInPlace(context.Background(), inPlace, LandSpec{Origin: "api"}); err != nil {
		t.Errorf("landing a file in place by its absolute path: %v", err)
	}
}

func writeTemp(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	mustWriteFile(t, p, body, 0o644)
	return p
}
