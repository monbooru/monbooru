package compatibility

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func buildZip(t *testing.T, entries map[string]string) *zip.Reader {
	t.Helper()

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, k := range keys {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatalf("zip.Create %q: %v", k, err)
		}
		if _, err := w.Write([]byte(entries[k])); err != nil {
			t.Fatalf("zip.Write %q: %v", k, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func TestPickValidSHA256(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", ""},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", ""},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550", ""},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := PickValidSHA256(c.in); got != c.want {
			t.Errorf("PickValidSHA256(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    []string
	}{
		{"StripsSharedPrefix", map[string]string{
			"hydrus_export/a.png":         "x",
			"hydrus_export/a.png.txt":     "tag\n",
			"hydrus_export/sub/b.png":     "y",
			"hydrus_export/sub/b.png.txt": "tag2\n",
		}, []string{"a.png", "a.png.txt", "sub/b.png", "sub/b.png.txt"}},
		{"FlatNoPrefix", map[string]string{
			"backup.json": "{}",
			"media/a.png": "x",
		}, []string{"backup.json", "media/a.png"}},
		{"MixedPrefixesNotStripped", map[string]string{
			"a/x.png":     "1",
			"b/y.png":     "2",
			"backup.json": "{}",
		}, []string{"a/x.png", "b/y.png", "backup.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEntries(buildZip(t, tc.entries).File)
			rels := make([]string, len(got))
			for i, e := range got {
				rels[i] = e.Rel
			}
			sort.Strings(rels)
			if !reflect.DeepEqual(rels, tc.want) {
				t.Errorf("NormalizeEntries rels = %v, want %v", rels, tc.want)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{"Blombooru", map[string]string{
			"backup.json": `{"version":1,"type":"full","media":[]}`,
			"media/a.png": "x",
		}, "blombooru"},
		{"Hydrus", map[string]string{
			"a.png":     "x",
			"a.png.txt": "tag\n",
		}, "hydrus"},
		{"Unknown", map[string]string{
			"random.txt": "nothing",
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(buildZip(t, tc.entries).File); got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadBlombooruTagsCSV_MapsEveryCategoryID(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"tags.csv": "g,0\na,1\nunused,2\nc,3\nch,4\nm,5\nhigh,9\nspaced, 4 \nblank,\n",
	})
	got, err := readBlombooruTagsCSV(zr.File[0])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"g": "general", "a": "artist", "unused": "general", "c": "copyright", "ch": "character",
		"m": "meta", "high": "general", "spaced": "character", "blank": "general",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTranslate_BlombooruHappyPath(t *testing.T) {
	backup := `{
		"version":1, "type":"full",
		"media":[
			{"filename":"cat.png","hash":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			 "tags":["cat","sky","foo"], "archive_path":"media/cat.png"}
		]
	}`
	csvText := "cat,0\nsky,0\nfoo,1\n"
	zr := buildZip(t, map[string]string{
		"backup.json":   backup,
		"tags.csv":      csvText,
		"media/cat.png": "PNGBYTES",
	})
	res, err := Translate(zr.File, "blombooru")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.Path != "cat.png" {
		t.Errorf("Path = %q, want cat.png", got.Path)
	}
	if got.SHA256 != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("SHA256 = %q, want full hash", got.SHA256)
	}
	wantTags := []string{"cat", "sky", "artist:foo"}
	sort.Strings(got.Tags)
	sort.Strings(wantTags)
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", got.Tags, wantTags)
	}
	if _, ok := res.Files["cat.png"]; !ok {
		t.Error("Files missing cat.png entry")
	}
}

func TestTranslate_BlombooruRefusesADocumentOverTheImportCap(t *testing.T) {
	for _, doc := range []string{"backup.json", "tags.csv"} {
		t.Run(doc, func(t *testing.T) {
			zr := buildZip(t, map[string]string{
				"backup.json":   `{"version":1,"media":[{"filename":"cat.png","archive_path":"media/cat.png"}]}`,
				"tags.csv":      "cat,0\n",
				"media/cat.png": "PNGBYTES",
			})
			for _, f := range zr.File {
				if f.Name == doc {
					f.UncompressedSize64 = 16<<30 + 1
				}
			}
			if _, err := Translate(zr.File, "blombooru"); err == nil || !strings.Contains(err.Error(), doc+`" exceeds`) {
				t.Fatalf("err = %v, want %s refused over the cap", err, doc)
			}
		})
	}
}

func TestTranslate_HydrusRefusesASidecarOverTheImportCap(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"cat.png":     "PNGBYTES",
		"cat.png.txt": "cat\n",
	})
	for _, f := range zr.File {
		if f.Name == "cat.png.txt" {
			f.UncompressedSize64 = 16<<30 + 1
		}
	}
	if _, err := Translate(zr.File, "hydrus"); err == nil || !strings.Contains(err.Error(), `cat.png.txt" exceeds`) {
		t.Fatalf("err = %v, want the sidecar refused over the cap", err)
	}
}

func TestCappedReader_RefusesTheByteAfterTheCap(t *testing.T) {
	for _, tc := range []struct {
		left    int64
		refused bool
	}{{5, false}, {4, true}} {
		got, err := io.ReadAll(&cappedReader{r: strings.NewReader("12345"), left: tc.left, name: "doc"})
		if refused := errors.Is(err, errDocumentTooLarge); refused != tc.refused {
			t.Errorf("cap %d: read %q, err %v, want refused %t", tc.left, got, err, tc.refused)
		}
	}
}

func TestBlombooruTagTokens_QualifiesAColonInAGeneralName(t *testing.T) {
	got := blombooruTagTokens(
		[]string{"re:zero", "meta:highres", "cat", "fate:zero"},
		map[string]string{"meta:highres": "general", "cat": "general", "fate:zero": "copyright"})

	want := []string{"general:re:zero", "general:meta:highres", "cat", "copyright:fate:zero"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
}

func TestTranslate_BlombooruRejectsBadHash(t *testing.T) {
	backup := `{
		"version":1, "type":"full",
		"media":[
			{"filename":"x.png","hash":"NOT-A-HASH","tags":[],"archive_path":"media/x.png"}
		]
	}`
	zr := buildZip(t, map[string]string{
		"backup.json": backup,
		"media/x.png": "x",
	})
	res, err := Translate(zr.File, "blombooru")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := res.Manifest.Images[0].SHA256; got != "" {
		t.Errorf("SHA256 = %q, want empty", got)
	}
}

func TestTranslate_HydrusHappyPath(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	zr := buildZip(t, map[string]string{
		sha + ".png":     "PNGBYTES",
		sha + ".png.txt": "cat\n# comment line\n\nartist:foo\n",
	})
	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.SHA256 != sha {
		t.Errorf("SHA256 = %q, want hydrus-style basename hash", got.SHA256)
	}
	wantTags := []string{"cat", "artist:foo"}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v (blank lines and # comments stripped)", got.Tags, wantTags)
	}
}

func TestTranslate_HydrusMixedCaseSidecar(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"a.png":     "PNGBYTES",
		"a.png.Txt": "cat\n",
	})
	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	if got := res.Manifest.Images[0].Tags; !reflect.DeepEqual(got, []string{"cat"}) {
		t.Errorf("Tags = %v, want [cat]", got)
	}
}

func TestTranslate_HydrusNoSidecar(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"plain.png": "x",
	})

	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty (basename is not 64 hex chars)", got.SHA256)
	}
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want empty (no sidecar)", got.Tags)
	}
}

func TestTranslate_UnknownFormat(t *testing.T) {
	zr := buildZip(t, map[string]string{"x": "y"})
	if _, err := Translate(zr.File, "wat"); err == nil {
		t.Error("Translate(unknown) returned nil error")
	}
}

func TestTranslate_HydrusKeepsArchives(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"a.cbz":       "PK",
		"a.cbz.txt":   "cat\n",
		"b.ZIP":       "PK",
		"notes.txt":   "not a sidecar",
		"backup.json": "{}",
		"folder/":     "",
		"noext":       "x",
	})
	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	got := map[string][]string{}
	for _, img := range res.Manifest.Images {
		got[img.Path] = img.Tags
	}
	want := map[string][]string{"a.cbz": {"cat"}, "b.ZIP": nil}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest = %v, want %v", got, want)
	}
}
