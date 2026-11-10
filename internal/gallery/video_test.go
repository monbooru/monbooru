package gallery

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/binary"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monbooru/monbooru/internal/shared/desktop"

	"github.com/monbooru/monbooru/internal/models"
)

func fixtureFFmpeg(args ...string) *exec.Cmd {
	return exec.Command(cmp.Or(os.Getenv("MONBOORU_TEST_FIXTURE_FFMPEG"), "ffmpeg"), args...)
}

func rotateFixture(t *testing.T, src, dst string, degrees int) {
	t.Helper()
	out, err := fixtureFFmpeg("-y", "-loglevel", "quiet",
		"-display_rotation", strconv.Itoa(degrees), "-i", src, "-c", "copy", "--", dst).CombinedOutput()
	if err == nil {
		return
	}
	if out2, err2 := fixtureFFmpeg("-y", "-loglevel", "quiet", "-i", src, "-c", "copy",
		"-metadata:s:v:0", "rotate="+strconv.Itoa((360-degrees)%360), "--", dst).CombinedOutput(); err2 != nil {
		t.Fatalf("ffmpeg rotate: %v\n%s\n%s", err2, out, out2)
	}
}

func generateTinyMP4(t *testing.T, dst string) {
	t.Helper()
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	cmd := fixtureFFmpeg(
		"-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.1:r=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"--", dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
	}
}

func TestGenerate_VideoTrackShorterThanTenPercent(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}

	src := filepath.Join(t.TempDir(), "short-track.mp4")
	cmd := fixtureFFmpeg("-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10:duration=0.5",
		"-f", "lavfi", "-i", "sine=duration=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "--", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
	}
	thumbs := t.TempDir()
	if err := generate(src, thumbs, 1, "mp4"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if w, h := imageSize(t, ThumbnailPath(thumbs, 1), "jpeg"); w != 64 || h != 48 {
		t.Errorf("thumbnail %dx%d, want 64x48", w, h)
	}
	if fi, err := os.Stat(hoverPath(thumbs, 1)); err != nil || fi.Size() == 0 {
		t.Errorf("the hover preview is missing or empty: %v", err)
	}
}

func TestGenerate_PortraitVideoFitsTheLongSide(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	for _, tc := range []struct{ name, fileType, codec string }{
		{"tall.mp4", "mp4", "libx264"},
		{"tall.gif", "gif", "gif"},
	} {
		src := filepath.Join(dir, tc.name)
		cmd := fixtureFFmpeg("-y", "-loglevel", "quiet",
			"-f", "lavfi", "-i", "testsrc2=size=180x320:rate=10:duration=1",
			"-c:v", tc.codec, "--", src)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
		}
		thumbs := filepath.Join(dir, tc.fileType)
		if err := generate(src, thumbs, 1, tc.fileType); err != nil {
			t.Fatalf("%s: generate: %v", tc.name, err)
		}
		if w, h := imageSize(t, ThumbnailPath(thumbs, 1), "jpeg"); h != 300 || w >= 300 {
			t.Errorf("%s: thumbnail %dx%d, want 300 high and narrower", tc.name, w, h)
		}
		if w, h := imageSize(t, hoverPath(thumbs, 1), "webp"); h != 300 || w >= 300 {
			t.Errorf("%s: hover %dx%d, want 300 high and narrower", tc.name, w, h)
		}
	}
}

func TestNormalizeImage(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()

	src := createTestJPEG(t, dir, "in.jpg", 320, 240)
	if err := NormalizeImage(src); err != nil {
		t.Fatalf("NormalizeImage: %v", err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("normalized file does not decode with the stdlib: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Errorf("normalized dimensions = %dx%d, want 320x240", cfg.Width, cfg.Height)
	}

	bad := filepath.Join(dir, "bad.jpg")
	mustWriteFile(t, bad, []byte("not an image"), 0o644)
	if err := NormalizeImage(bad); err == nil {
		t.Error("NormalizeImage on a non-image should return an error")
	}
}

func TestProbeVideoDimensions_RealMP4(t *testing.T) {
	tmp := t.TempDir()
	mp4 := filepath.Join(tmp, "clip.mp4")
	generateTinyMP4(t, mp4)

	w, h, ok := ProbeVideoDimensions(mp4)
	if !ok {
		t.Fatal("ProbeVideoDimensions returned ok=false for a real mp4")
	}
	if w != 32 || h != 18 {
		t.Errorf("ProbeVideoDimensions = %dx%d, want 32x18", w, h)
	}
}

func TestProbeVideoDimensions_RotatedMP4(t *testing.T) {
	tmp := t.TempDir()
	mp4 := filepath.Join(tmp, "clip.mp4")
	generateTinyMP4(t, mp4)
	rotated := filepath.Join(tmp, "rotated.mp4")
	rotateFixture(t, mp4, rotated, 90)

	w, h, ok := ProbeVideoDimensions(rotated)
	if !ok || w != 18 || h != 32 {
		t.Errorf("ProbeVideoDimensions = %dx%d (ok=%v), want the displayed 18x32", w, h, ok)
	}
}

func TestProbeVideoDimensions_UpsideDownMP4(t *testing.T) {
	tmp := t.TempDir()
	mp4 := filepath.Join(tmp, "clip.mp4")
	generateTinyMP4(t, mp4)
	flipped := filepath.Join(tmp, "flipped.mp4")
	rotateFixture(t, mp4, flipped, 180)
	if w, h, ok := ProbeVideoDimensions(flipped); !ok || w != 32 || h != 18 {
		t.Errorf("ProbeVideoDimensions = %dx%d (ok=%v), want 32x18", w, h, ok)
	}
}

func TestProbeVideoDimensions_MissingFile(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	if _, _, ok := ProbeVideoDimensions("/nonexistent/no-such-file.mp4"); ok {
		t.Error("ProbeVideoDimensions returned ok=true for a missing file")
	}
}

func TestProbeVideoDimensions_NonVideo(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	tmp := t.TempDir()
	junk := filepath.Join(tmp, "not-a-video.txt")
	mustWriteFile(t, junk, []byte("definitely not a video"), 0o644)
	if _, _, ok := ProbeVideoDimensions(junk); ok {
		t.Error("ProbeVideoDimensions returned ok=true for a text file")
	}
}

func generateStill(t *testing.T, dst string, w, h int) {
	t.Helper()
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	codec := map[string]string{".avif": "libaom-av1", ".jxl": "libjxl"}[filepath.Ext(dst)]
	cmd := fixtureFFmpeg("-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d", w, h),
		"-frames:v", "1", "-c:v", codec, "--", dst)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg cannot encode %s: %v", codec, err)
	}
}

func stillBytes(t *testing.T, ext string, w, h int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "still"+ext)
	generateStill(t, path, w, h)
	return mustReadFile(t, path)
}

func imageSize(t *testing.T, path, format string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, got, err := image.DecodeConfig(f)
	if err != nil || got != format {
		t.Fatalf("%s is not a %s (read as %q): %v", path, format, got, err)
	}
	return cfg.Width, cfg.Height
}

func TestGenerate_FFmpegStills(t *testing.T) {
	for _, tc := range []struct {
		name, fileType string
		w, h           int
		wantW, wantH   int
	}{
		{"big.avif", models.FileTypeAVIF, 640, 360, 300, 169},
		{"big.jxl", models.FileTypeJXL, 640, 360, 300, 169},
		{"small.avif", models.FileTypeAVIF, 64, 48, 64, 48},
		{"small.jxl", models.FileTypeJXL, 64, 48, 64, 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, tc.name)
			generateStill(t, src, tc.w, tc.h)
			thumbs := filepath.Join(dir, "thumbs")
			if err := generate(src, thumbs, 1, tc.fileType); err != nil {
				t.Fatalf("generate: %v", err)
			}
			if w, h := imageSize(t, ThumbnailPath(thumbs, 1), "jpeg"); w != tc.wantW || h != tc.wantH {
				t.Errorf("thumbnail = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestEnsureViewRendition_FFmpegStills(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maxDim       int
		wantW, wantH int
	}{
		{"pic.avif", 0, 640, 360},
		{"pic.jxl", 0, 640, 360},
		{"pic.avif", 320, 320, 180},
		{"pic.jxl", 320, 320, 180},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.name, tc.maxDim), func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, tc.name)
			generateStill(t, src, 640, 360)
			got, err := EnsureViewRendition(src, filepath.Join(dir, "thumbs"), 1, models.ExtFileType(tc.name), tc.maxDim)
			if err != nil {
				t.Fatalf("EnsureViewRendition: %v", err)
			}
			if w, h := imageSize(t, got, "jpeg"); w != tc.wantW || h != tc.wantH {
				t.Errorf("rendition = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestRenderStill_TruncatedJPEGXLReturnsAtOnce(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "cut.jxl")
	generateStill(t, src, 640, 360)
	whole, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, src, whole[:len(whole)/2], 0o644)
	start := time.Now()

	_ = renderStill(src, filepath.Join(dir, "out.jpg"), thumbMaxDim)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("rendering took %v", took)
	}
}

func TestRenderStillFrame(t *testing.T) {
	src := filepath.Join(t.TempDir(), "wide.jxl")
	generateStill(t, src, 4100, 64)
	for _, tc := range []struct{ maxDim, wantW int }{{ViewMaxDim, ViewMaxDim}, {0, 4100}} {
		dir := t.TempDir()
		got, err := RenderStillFrame(src, dir, tc.maxDim)
		if err != nil {
			t.Fatalf("RenderStillFrame(%d): %v", tc.maxDim, err)
		}
		if filepath.Dir(got) != dir {
			t.Errorf("frame at %s, want it in %s", got, dir)
		}
		if w, _ := imageSize(t, got, "webp"); w != tc.wantW {
			t.Errorf("maxDim %d: frame = %d wide, want a WebP %d wide", tc.maxDim, w, tc.wantW)
		}
	}
}

func TestRenderStillFrame_LeavesNothingOnFailure(t *testing.T) {
	src := filepath.Join(t.TempDir(), "broken.avif")
	mustWriteFile(t, src, []byte("not an image"), 0o644)
	dir := t.TempDir()
	if _, err := RenderStillFrame(src, dir, ViewMaxDim); err == nil {
		t.Fatal("no error for bytes nothing decodes")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestIngest_FFmpegStillsPopulateWidthHeight(t *testing.T) {
	for _, name := range []string{"pic.avif", "pic.jxl"} {
		t.Run(name, func(t *testing.T) {
			database, env, galleryDir := setupSyncTest(t)
			src := filepath.Join(galleryDir, name)
			generateStill(t, src, 640, 360)

			img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, src, models.OriginIngest)
			if want := models.ExtFileType(name); img.FileType != want {
				t.Errorf("file_type = %q, want %q", img.FileType, want)
			}
			if img.Width == nil || img.Height == nil || *img.Width != 640 || *img.Height != 360 {
				t.Errorf("width/height = %v/%v, want 640/360", img.Width, img.Height)
			}
			if _, err := os.Stat(ThumbnailPath(env.thumbnailsPath, img.ID)); err != nil {
				t.Errorf("no thumbnail: %v", err)
			}
		})
	}
}

func TestIngest_FFmpegStillIndexedWithoutADecode(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	src := filepath.Join(galleryDir, "broken.avif")
	mustWriteFile(t, src, append([]byte{0x00, 0x00, 0x00, 0x1C, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0}, make([]byte, 64)...), 0o644)

	img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, src, models.OriginIngest)
	if img.FileType != models.FileTypeAVIF || img.Width != nil {
		t.Errorf("row = %q with width %v, want avif with none", img.FileType, img.Width)
	}
	if _, err := os.Stat(ThumbnailPath(env.thumbnailsPath, img.ID)); err == nil {
		t.Error("a thumbnail was written for bytes nothing can decode")
	}
}

func TestIngest_CBZWithAnAVIFCover(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	cbz := writeTestZip(t, galleryDir, "vol.cbz", map[string][]byte{
		"01.avif": stillBytes(t, ".avif", 640, 360),
		"02.png":  solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
	})

	img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, cbz, models.OriginIngest)
	if img.Width == nil || img.Height == nil || *img.Width != 640 || *img.Height != 360 {
		t.Errorf("width/height = %v/%v, want the cover's 640/360", img.Width, img.Height)
	}
	if w, h := imageSize(t, ThumbnailPath(env.thumbnailsPath, img.ID), "jpeg"); w != 300 || h != 169 {
		t.Errorf("thumbnail = %dx%d, want 300x169", w, h)
	}
	for n := 1; n <= 2; n++ {
		waitForPageThumb(t, mangaPageThumbPath(mangaImageDir(env.thumbnailsPath, img.ID), n), 10*time.Second)
	}
}

func TestIngest_CBZWithAnUndecodableCover(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	cbz := writeTestZip(t, galleryDir, "vol.cbz", map[string][]byte{
		"01.jxl": {0xFF, 0x0A, 0x00, 0x00},
		"02.png": solidPNG(t, 16, 16, [3]uint8{0, 0, 0}),
	})

	img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, cbz, models.OriginIngest)
	if img.FileType != models.FileTypeCBZ || img.Width != nil {
		t.Errorf("row = %q with width %v, want cbz with none", img.FileType, img.Width)
	}
	if _, err := os.Stat(ThumbnailPath(env.thumbnailsPath, img.ID)); err == nil {
		t.Error("a thumbnail was written for a cover nothing can decode")
	}
}

func TestIngest_VideoPopulatesWidthHeight(t *testing.T) {
	database, env, galleryDir := setupSyncTest(t)
	mp4 := filepath.Join(galleryDir, "clip.mp4")
	generateTinyMP4(t, mp4)

	img, dup := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, mp4, models.OriginIngest)
	if dup {
		t.Fatalf("expected non-duplicate")
	}
	if img.Width == nil || img.Height == nil {
		t.Fatalf("expected populated width/height, got width=%v height=%v", img.Width, img.Height)
	}
	if *img.Width != 32 || *img.Height != 18 {
		t.Errorf("ingest returned width/height = %d/%d, want 32/18", *img.Width, *img.Height)
	}

	var dbW, dbH sql.NullInt64
	if err := database.Read.QueryRow(`SELECT width, height FROM images WHERE id = ?`, img.ID).Scan(&dbW, &dbH); err != nil {
		t.Fatal(err)
	}
	if !dbW.Valid || dbW.Int64 != 32 {
		t.Errorf("DB width = %v, want 32", dbW)
	}
	if !dbH.Valid || dbH.Int64 != 18 {
		t.Errorf("DB height = %v, want 18", dbH)
	}
}

const (
	taggedVideoPrompt   = `{"1":{"inputs":{"text":"a lighthouse"},"class_type":"CLIPTextEncode"},"2":{"inputs":{"positive":["1",0],"seed":4242,"steps":25,"cfg":7.5,"sampler_name":"euler","scheduler":"normal"},"class_type":"KSampler"},"3":{"inputs":{"ckpt_name":"video.safetensors"},"class_type":"CheckpointLoaderSimple"}}`
	taggedVideoWorkflow = `{"nodes":[{"id":1,"type":"CustomNode","inputs":[]}],"extra":{"preview":NaN}}`
)

func TestIngest_VideoReadsComfyUITags(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	database, env, galleryDir := setupSyncTest(t)

	for _, tc := range []struct {
		name  string
		codec []string
	}{
		{"clip.webm", []string{"-c:v", "libvpx-vp9"}},
		{"clip.mp4", []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "use_metadata_tags"}},
	} {
		src := filepath.Join(galleryDir, tc.name)
		args := append([]string{"-y", "-loglevel", "quiet",
			"-f", "lavfi", "-i", "color=c=black:s=32x18:d=0.1:r=10"}, tc.codec...)
		cmd := fixtureFFmpeg(append(args,
			"-metadata", "prompt="+taggedVideoPrompt, "-metadata", "workflow="+taggedVideoWorkflow,
			"--", src)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
		}

		img, _ := mustIngest(t, database, env.galleryPath, env.thumbnailsPath, src, models.OriginIngest)
		if img.SourceType != models.SourceTypeComfyUI {
			t.Errorf("%s: source type = %q, want comfyui", tc.name, img.SourceType)
		}
		var prompt, model, raw string
		var seed int64
		if err := database.Read.QueryRow(
			`SELECT prompt, model_checkpoint, seed, raw_workflow FROM comfyui_metadata WHERE image_id = ?`, img.ID,
		).Scan(&prompt, &model, &seed, &raw); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if prompt != "a lighthouse" || model != "video.safetensors" || seed != 4242 || raw != taggedVideoPrompt {
			t.Errorf("%s: prompt=%q model=%q seed=%d raw=%q, want the prompt tag's recipe and graph", tc.name, prompt, model, seed, raw)
		}
	}
}

func TestExtractGenerationMeta_UntaggedVideoIsNone(t *testing.T) {
	mp4 := filepath.Join(t.TempDir(), "clip.mp4")
	generateTinyMP4(t, mp4)
	if sd, comfy, sourceType := ExtractGenerationMeta(mp4, models.FileTypeMP4); sd != nil || comfy != nil || sourceType != models.SourceTypeNone {
		t.Errorf("got %+v, %+v, %q, want no metadata", sd, comfy, sourceType)
	}
}

func TestResolveToolPrefersTheExecutablesOwnDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode-bit check is POSIX-only")
	}
	dir := desktop.ExeDir()
	if dir == "" {
		t.Skip("cannot locate the test executable")
	}
	beside := filepath.Join(dir, "ffprobe")
	if _, err := os.Stat(beside); err == nil {
		t.Skip("something is already there")
	}
	if err := os.WriteFile(beside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	if got := resolveTool("ffprobe"); got != beside {
		t.Errorf("resolveTool = %q, want the bundled %q", got, beside)
	}
}

func TestResolveToolIgnoresANonExecutableNeighbour(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the mode-bit check is POSIX-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the exec bit")
	}
	dir := desktop.ExeDir()
	if dir == "" {
		t.Skip("cannot locate the test executable")
	}
	beside := filepath.Join(dir, "ffprobe")
	if _, err := os.Stat(beside); err == nil {
		t.Skip("something is already there")
	}
	if err := os.WriteFile(beside, []byte("not executable"), 0o644); err != nil {
		t.Skipf("cannot write beside the executable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(beside) })

	onPath, err := exec.LookPath("ffprobe")
	got := resolveTool("ffprobe")
	if got == beside {
		t.Fatal("a non-executable neighbour was taken")
	}
	if err == nil && got != onPath {
		t.Errorf("resolveTool = %q, want the PATH copy %q", got, onPath)
	}
}

func TestProbeVideoResolvesTheToolsItself(t *testing.T) {
	if resolveTool("ffprobe") == "" {
		t.Skip("ffprobe not available")
	}
	toolsOnce = sync.Once{}
	ffmpegPath, ffprobePath = "", ""
	t.Cleanup(func() {
		toolsOnce = sync.Once{}
		ffmpegPath, ffprobePath = "", ""
		resolveTools()
	})

	_ = probeVideo(filepath.Join(t.TempDir(), "absent.mp4"))
	if ffprobePath == "" {
		t.Error("probeVideo ran with no ffprobe resolved")
	}
}

func TestIngest_VideoSpawnsThreeProbes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the counting shim is a shell script")
	}
	database, env, galleryDir := setupSyncTest(t)
	src := filepath.Join(galleryDir, "clip.mp4")
	generateTinyMP4(t, src)
	if !ffprobeAvailable() {
		t.Skip("ffprobe not available")
	}
	dir := t.TempDir()
	count, shim := filepath.Join(dir, "count"), filepath.Join(dir, "ffprobe")
	mustWriteFile(t, shim, []byte("#!/bin/sh\necho >> '"+count+"'\nexec '"+ffprobePath+"' \"$@\"\n"), 0o755)
	real := ffprobePath
	ffprobePath = shim
	t.Cleanup(func() { ffprobePath = real })
	MetaTagsEnabled.Store(true)

	mustIngest(t, database, galleryDir, env.thumbnailsPath, src, "")
	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Errorf("one video ingest ran ffprobe %d times, want 3", n)
	}
}

func TestGenerate_GIFHoverIsCappedAtFourSeconds(t *testing.T) {
	if !FFmpegAvailable() {
		t.Skip("ffmpeg not available")
	}
	src := filepath.Join(t.TempDir(), "long.gif")
	cmd := fixtureFFmpeg("-y", "-loglevel", "quiet",
		"-f", "lavfi", "-i", "testsrc2=size=32x24:rate=10:duration=6", "--", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot build the fixture: %v\n%s", err, out)
	}
	thumbs := t.TempDir()
	if err := generate(src, thumbs, 1, "gif"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if ms := webpAnimationMillis(t, hoverPath(thumbs, 1)); ms == 0 || ms > 4100 {
		t.Errorf("the hover runs %d ms, want at most 4 s of the 6 s GIF", ms)
	}
}

func webpAnimationMillis(t *testing.T, path string) int {
	t.Helper()
	b := mustReadFile(t, path)
	total := 0
	for off := 12; off+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[off+4:]))
		if string(b[off:off+4]) == "ANMF" && off+8+15 <= len(b) {
			d := b[off+8+12:]
			total += int(d[0]) | int(d[1])<<8 | int(d[2])<<16
		}
		off += 8 + size + size%2
	}
	return total
}

func TestEnsureViewRendition_ConcurrentRequestsRenderOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the counting shim is a shell script")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.avif")
	generateStill(t, src, 64, 48)
	count, shim := filepath.Join(dir, "count"), filepath.Join(dir, "ffmpeg")

	mustWriteFile(t, shim, []byte("#!/bin/sh\necho >> '"+count+"'\nsleep 0.2\nexec '"+ffmpegPath+"' \"$@\"\n"), 0o755)
	real := ffmpegPath
	ffmpegPath = shim
	t.Cleanup(func() { ffmpegPath = real })

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := EnsureViewRendition(src, filepath.Join(dir, "thumbs"), 1, "avif", 0); err != nil {
				t.Errorf("EnsureViewRendition: %v", err)
			}
		})
	}
	wg.Wait()
	if n := strings.Count(string(mustReadFile(t, count)), "\n"); n != 1 {
		t.Errorf("four concurrent requests ran ffmpeg %d times, want 1", n)
	}
}

func mp4Box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	return append(append(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), typ...), body...)
}

func be32(v ...uint32) []byte {
	var b []byte
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, x)
	}
	return b
}

func writeTaggedMP4(t *testing.T, path string, tags [][2]string) {
	t.Helper()
	var keys, ilst []byte
	for i, kv := range tags {
		keys = append(keys, mp4Box("mdta", []byte(kv[0]))...)
		ilst = append(ilst, mp4Box(string(be32(uint32(i+1))), mp4Box("data", be32(1, 0), []byte(kv[1])))...)
	}
	matrix := be32(0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000)
	entry := mp4Box("jpeg", make([]byte, 6), []byte{0, 1}, make([]byte, 16), []byte{0, 32, 0, 18},
		be32(0x00480000, 0x00480000, 0), []byte{0, 1}, make([]byte, 32), []byte{0, 24, 0xff, 0xff})
	stbl := mp4Box("stbl", mp4Box("stsd", be32(0, 1), entry), mp4Box("stts", be32(0, 0)), mp4Box("stsc", be32(0, 0)),
		mp4Box("stsz", be32(0, 0, 0)), mp4Box("stco", be32(0, 0)))
	minf := mp4Box("minf", mp4Box("vmhd", be32(1), make([]byte, 8)), mp4Box("dinf", mp4Box("dref", be32(0, 1), mp4Box("url ", be32(1)))), stbl)
	mdia := mp4Box("mdia", mp4Box("mdhd", be32(0, 0, 0, 1000, 2000), []byte{0x55, 0xc4, 0, 0}),
		mp4Box("hdlr", be32(0, 0), []byte("vide"), make([]byte, 12), []byte{0}), minf)
	moov := mp4Box("moov",
		mp4Box("mvhd", be32(0, 0, 0, 1000, 2000, 0x00010000), []byte{1, 0}, make([]byte, 10), matrix, make([]byte, 24), be32(2)),
		mp4Box("trak", mp4Box("tkhd", be32(3, 0, 0, 1, 0, 2000, 0, 0), make([]byte, 8), matrix, be32(32<<16, 18<<16)), mdia),
		mp4Box("meta", be32(0), mp4Box("hdlr", be32(0, 0), []byte("mdta"), make([]byte, 12), []byte{0}),
			mp4Box("keys", be32(0, uint32(len(tags))), keys), mp4Box("ilst", ilst)))
	mustWriteFile(t, path, append(mp4Box("ftyp", []byte("isom"), be32(0x200), []byte("isommp41")), moov...), 0o644)
}

func TestTenPercentOffsetReadsTheDurationPastABigWorkflowTag(t *testing.T) {
	if !ffprobeAvailable() {
		t.Skip("ffprobe not available")
	}
	src := filepath.Join(t.TempDir(), "clip.mp4")
	writeTaggedMP4(t, src, [][2]string{
		{"prompt", taggedVideoPrompt},
		{"workflow", `{"nodes":[],"extra":{"pad":"` + strings.Repeat("x", 1<<20) + `"}}`},
	})
	if got := tenPercentOffset(src); got != "0.200" {
		t.Errorf("tenPercentOffset = %q, want 0.200", got)
	}
}
