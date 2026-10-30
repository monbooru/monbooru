package gallery

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/desktop"
)

var (
	toolsOnce   sync.Once
	ffmpegPath  string
	ffprobePath string
)

// A copy beside the executable wins over PATH: a downloaded bundle has
// nothing else to find.
func resolveTools() {
	toolsOnce.Do(func() {
		ffmpegPath = resolveTool("ffmpeg")
		ffprobePath = resolveTool("ffprobe")
	})
}

func resolveTool(name string) string {
	if p := desktop.BundledTool(name); p != "" {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// The size cap bounds bytes, not decode time: a small pathological file
// could otherwise wedge an ingest until killed by hand.
const ffmpegTimeout = 60 * time.Second

func runFFmpeg(combinedOutput bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	desktop.HideConsole(cmd)
	if combinedOutput {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

func FFmpegAvailable() bool {
	resolveTools()
	return ffmpegPath != ""
}

// Shipped with ffmpeg, but nothing guarantees both are present.
func ffprobeAvailable() bool {
	resolveTools()
	return ffprobePath != ""
}

// args must end with "--" and the temp path, so a name starting with "-"
// stays an output.
func runFFmpegToFile(dstPath, tmpPattern, label string, args func(tmp string) []string) error {
	if !FFmpegAvailable() {
		return fmt.Errorf("ffmpeg not available")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	_ = tmp.Close()
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if out, err := runFFmpeg(true, ffmpegPath, args(tmpName)...); err != nil {
		return fmt.Errorf("ffmpeg %s: %w\n%s", label, err, string(out))
	}
	if wroteNothing(tmpName) {
		return fmt.Errorf("ffmpeg %s wrote nothing", label)
	}
	return os.Rename(tmpName, dstPath)
}

// A seek past a video track's last frame exits 0 having written nothing.
func wroteNothing(path string) bool {
	fi, err := os.Stat(path)
	return err != nil || fi.Size() == 0
}

// NormalizeImage rewrites srcPath in place, so it is only for a file the
// caller owns. It rescues JPEGs whose subsampling Go's image/jpeg
// refuses, as some CDN resizers emit.
func NormalizeImage(srcPath string) error {
	// -update 1: one still image, not a numbered sequence.
	return runFFmpegToFile(srcPath, ".normalize.*.jpg", "normalize", func(tmp string) []string {
		return []string{
			"-y",
			"-i", srcPath,
			"-update", "1",
			"-frames:v", "1",
			"-q:v", "2",
			"--",
			tmp,
		}
	})
}

func fitWithin(maxDim int) string {
	return fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDim, maxDim)
}

// ffmpeg picks the encoder from the extension, so the temp file takes
// dstPath's.
func renderStill(srcPath, dstPath string, maxDim int) error {
	return runFFmpegToFile(dstPath, ".still.*"+filepath.Ext(dstPath), "still", func(tmp string) []string {
		// The first decode error ends the run: on a truncated JPEG XL, ffmpeg's
		// libjxl decoder repeats its error forever instead of stopping.
		args := []string{"-y", "-xerror", "-i", srcPath, "-update", "1", "-frames:v", "1"}
		if maxDim > 0 {
			args = append(args, "-vf", fitWithin(maxDim))
		}
		if filepath.Ext(dstPath) == ".webp" {
			return append(args, "-c:v", "libwebp", "-lossless", "1", "--", tmp)
		}
		return append(args, "-q:v", "2", "--", tmp)
	})
}

// RenderStillFrame renders an AVIF or JPEG XL to a lossless WebP in dir
// for a Go decode, which keeps a JPEG XL's alpha (the shipped ffmpeg has
// no PNG encoder); the caller removes it.
func RenderStillFrame(srcPath, dir string, maxDim int) (string, error) {
	tmp, err := os.CreateTemp(dir, ".still-frame.*.webp")
	if err != nil {
		return "", fmt.Errorf("creating temp frame file: %w", err)
	}
	_ = tmp.Close()
	if err := renderStill(srcPath, tmp.Name(), maxDim); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func tenPercentOffset(srcPath string) string {
	return strconv.FormatFloat(probeVideo(srcPath).duration*0.10, 'f', 3, 64)
}

// The video track can end before 10% of the container's length, so a
// render that fails there is tried again from the start.
func fromTenPercent(offset string, render func(offset string) error) error {
	err := render(offset)
	if err != nil && offset != "0.000" {
		err = render("0")
	}
	return err
}

func generateVideoThumb(srcPath, dstPath, tenPercent string) error {
	return fromTenPercent(tenPercent, func(offset string) error {
		return runFFmpegToFile(dstPath, ".vthumb.*.jpg", "thumbnail", func(tmp string) []string {
			return []string{
				"-y",
				"-ss", offset,
				"-i", srcPath,
				"-update", "1",
				"-frames:v", "1",
				"-vf", fitWithin(thumbMaxDim),
				"-q:v", "2",
				"--",
				tmp,
			}
		})
	})
}

func generateVideoHover(srcPath, dstPath, tenPercent string) error {
	return fromTenPercent(tenPercent, func(offset string) error {
		return runFFmpegToFile(dstPath, ".vhover.*.webp", "hover", func(tmp string) []string {
			return []string{
				"-y",
				"-ss", offset,
				"-t", "4",
				"-i", srcPath,
				"-vf", fitWithin(thumbMaxDim),
				"-an",
				"-loop", "0", // infinite loop
				"--",
				tmp,
			}
		})
	})
}

func generateGIFHover(srcPath, dstPath string) error {
	return runFFmpegToFile(dstPath, ".ghover.*.webp", "gif hover", func(tmp string) []string {
		return []string{
			"-y",
			"-t", "4",
			"-i", srcPath,
			"-vf", fitWithin(thumbMaxDim),
			"-loop", "0",
			"--",
			tmp,
		}
	})
}

// ExtractVideoFrames takes offsets as fractions of the duration and skips
// a frame that fails, so a short result is a partial success.
func ExtractVideoFrames(srcPath, tmpDir string, positions []float64) ([]string, error) {
	if !FFmpegAvailable() {
		return nil, fmt.Errorf("ffmpeg not available")
	}
	duration := probeVideo(srcPath).duration
	var out []string
	for i, pos := range positions {
		offset := duration * min(max(pos, 0), 1)
		tmp, err := os.CreateTemp(tmpDir, fmt.Sprintf(".frame-%d.*.jpg", i))
		if err != nil {
			return out, fmt.Errorf("creating temp frame file: %w", err)
		}
		_ = tmp.Close()
		args := []string{
			"-y",
			"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
			"-i", srcPath,
			"-update", "1",
			"-frames:v", "1",
			"-q:v", "2",
			"--",
			tmp.Name(),
		}
		if _, err := runFFmpeg(true, ffmpegPath, args...); err != nil || wroteNothing(tmp.Name()) {
			_ = os.Remove(tmp.Name())
			continue
		}
		out = append(out, tmp.Name())
	}
	return out, nil
}

func ProbeDurationSeconds(srcPath string) (float64, bool) {
	p := probeVideo(srcPath)
	return p.duration, p.duration > 0
}

func ProbeVideoDimensions(srcPath string) (int, int, bool) {
	p := probeVideo(srcPath)
	return p.width, p.height, p.width > 0
}

type videoProbe struct {
	width, height int
	duration      float64
	comfy         *models.ComfyUIMetadata
}

// The size is the display size: the thumbnail and the player apply a
// phone video's rotation, so a quarter turn swaps the coded width and height.
func probeVideo(srcPath string) videoProbe {
	var p videoProbe
	if !ffprobeAvailable() {
		return p
	}
	out, err := runFFmpeg(false, ffprobePath,
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "format=duration:format_tags=prompt,workflow:stream=width,height:stream_side_data=rotation:stream_tags=rotate",
		"-print_format", "json",
		"--",
		srcPath,
	)
	if err != nil {
		return p
	}
	var probe struct {
		Streams []struct {
			Width    int `json:"width"`
			Height   int `json:"height"`
			SideData []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
			Tags struct {
				Rotate string `json:"rotate"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			Tags     struct {
				Prompt   string `json:"prompt"`
				Workflow string `json:"workflow"`
			} `json:"tags"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return p
	}
	p.comfy = metadata.ParseComfyTags(probe.Format.Tags.Prompt, probe.Format.Tags.Workflow)
	if d, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil && d > 0 {
		p.duration = d
	}
	if len(probe.Streams) == 0 {
		return p
	}
	st := probe.Streams[0]
	if st.Width <= 0 || st.Height <= 0 {
		return p
	}
	rotation, _ := strconv.ParseFloat(st.Tags.Rotate, 64)
	for _, sd := range st.SideData {
		if sd.Rotation != 0 {
			rotation = sd.Rotation
		}
	}
	p.width, p.height = st.Width, st.Height
	if int(math.Abs(rotation))%180 == 90 {
		p.width, p.height = st.Height, st.Width
	}
	return p
}

// probeVideoHasAudio is false also when ffprobe is missing or fails, so
// false must not be read as silent.
func probeVideoHasAudio(srcPath string) bool {
	if !ffprobeAvailable() {
		return false
	}
	out, err := runFFmpeg(false, ffprobePath,
		"-v", "quiet",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_type",
		"-print_format", "csv=p=0",
		"--",
		srcPath,
	)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "audio"
}
