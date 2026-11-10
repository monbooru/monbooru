package metadata

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func zipWith(t *testing.T, entries map[string]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func TestParseComicInfo_AllFields(t *testing.T) {
	xml := `<?xml version="1.0"?>
<ComicInfo>
  <Title>The Test</Title>
  <Series>Naruto</Series>
  <Number>12</Number>
  <Volume>3</Volume>
  <Count>720</Count>
  <Year>2003</Year>
  <Month>9</Month>
  <Day>21</Day>
  <Writer>Kishimoto, Masashi</Writer>
  <Penciller>Kishimoto, Masashi</Penciller>
  <Publisher>Shueisha</Publisher>
  <Genre>Shounen</Genre>
  <Web>https://example.com</Web>
  <LanguageISO>ja</LanguageISO>
  <Manga>YesAndRightToLeft</Manga>
  <AgeRating>Teen</AgeRating>
  <CommunityRating>4.5</CommunityRating>
  <PageCount>188</PageCount>
  <Summary>A ninja learns the ways.</Summary>
</ComicInfo>`
	zr := zipWith(t, map[string]string{"ComicInfo.xml": xml})
	m := mustParseComicInfo(t, zr)
	if m == nil {
		t.Fatal("parsed metadata is nil")
	}
	if m.Title != "The Test" {
		t.Errorf("Title = %q", m.Title)
	}
	if m.Series != "Naruto" {
		t.Errorf("Series = %q", m.Series)
	}
	if m.Number != "12" {
		t.Errorf("Number = %q", m.Number)
	}
	if m.Year == nil || *m.Year != 2003 {
		t.Errorf("Year = %v", m.Year)
	}
	if m.XMLPageCount == nil || *m.XMLPageCount != 188 {
		t.Errorf("XMLPageCount = %v", m.XMLPageCount)
	}
	if m.CommunityRating == nil || *m.CommunityRating != 4.5 {
		t.Errorf("CommunityRating = %v", m.CommunityRating)
	}
	if m.Manga != "YesAndRightToLeft" {
		t.Errorf("Manga = %q", m.Manga)
	}
	if !strings.Contains(m.RawXML, "<ComicInfo>") {
		t.Errorf("RawXML missing root: %q", m.RawXML[:min(80, len(m.RawXML))])
	}
}

func TestParseComicInfo_MissingFile(t *testing.T) {
	zr := zipWith(t, map[string]string{"01.png": "not really a png"})
	m := mustParseComicInfo(t, zr)
	if m != nil {
		t.Errorf("got %v, want nil for archive without ComicInfo.xml", m)
	}
}

func TestParseComicInfo_NestedComicInfoIgnored(t *testing.T) {
	zr := zipWith(t, map[string]string{
		"chapter1/ComicInfo.xml": `<ComicInfo><Title>Wrong</Title></ComicInfo>`,
		"01.png":                 "page bytes",
	})
	m := mustParseComicInfo(t, zr)
	if m != nil {
		t.Errorf("got %v, want nil for nested ComicInfo.xml", m)
	}
}

func TestParseComicInfo_MalformedXML(t *testing.T) {
	zr := zipWith(t, map[string]string{
		"ComicInfo.xml": `<ComicInfo>` +
			`<Title>broken`,
	})
	m := mustParseComicInfo(t, zr)
	if m == nil {
		t.Fatal("parsed metadata is nil; expected truncated row to survive")
	}
	if m.RawXML == "" {
		t.Errorf("RawXML empty even on malformed input")
	}
}

func TestParseComicInfo_ABadNumberKeepsTheFieldsAfterIt(t *testing.T) {
	zr := zipWith(t, map[string]string{
		"ComicInfo.xml": `<ComicInfo><Year>2021-03</Year><Month>3</Month><Writer>Someone</Writer>` +
			`<CommunityRating>4,5</CommunityRating><PageCount>12</PageCount></ComicInfo>`,
	})
	m := mustParseComicInfo(t, zr)
	if m.Year != nil {
		t.Errorf("Year = %d, want none for a value that is not a number", *m.Year)
	}
	if m.Month == nil || *m.Month != 3 || m.Writer != "Someone" {
		t.Errorf("Month = %v, Writer = %q; want 3 and Someone past the bad Year", m.Month, m.Writer)
	}
	if m.CommunityRating == nil || *m.CommunityRating != 4.5 {
		t.Errorf("CommunityRating = %v, want 4.5 from a decimal comma", m.CommunityRating)
	}
	if m.XMLPageCount == nil || *m.XMLPageCount != 12 {
		t.Errorf("XMLPageCount = %v, want 12", m.XMLPageCount)
	}
}

func TestParseComicInfo_DropsANonFiniteCommunityRating(t *testing.T) {
	zr := zipWith(t, map[string]string{
		"ComicInfo.xml": `<ComicInfo><CommunityRating>Infinity</CommunityRating><PageCount>12</PageCount></ComicInfo>`,
	})
	m := mustParseComicInfo(t, zr)
	if m.CommunityRating != nil {
		t.Errorf("CommunityRating = %v, want none for Infinity", *m.CommunityRating)
	}
}

func TestParseComicInfo_TruncatedRawXML(t *testing.T) {
	body := strings.Repeat("a", ComicInfoMaxRawXML+5000)
	xml := `<ComicInfo><Title>` + body + `</Title></ComicInfo>`
	zr := zipWith(t, map[string]string{"ComicInfo.xml": xml})
	m := mustParseComicInfo(t, zr)
	if len(m.RawXML) > ComicInfoMaxRawXML {
		t.Errorf("RawXML len = %d, want <= %d", len(m.RawXML), ComicInfoMaxRawXML)
	}
}

func TestParseComicInfo_LowercaseFilenameAcceptedAtRoot(t *testing.T) {
	zr := zipWith(t, map[string]string{
		"comicinfo.xml": `<ComicInfo><Title>case</Title></ComicInfo>`,
	})
	m := mustParseComicInfo(t, zr)
	if m == nil || m.Title != "case" {
		t.Errorf("got %v, want Title=case", m)
	}
}

func TestParseComicInfo_NilReader(t *testing.T) {
	m := mustParseComicInfo(t, nil)
	if m != nil {
		t.Errorf("got %v, want nil", m)
	}
}
