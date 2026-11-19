package tagger

import (
	"context"
	"testing"

	"github.com/monbooru/monbooru/internal/config"
)

func TestDiscoverTaggers_OneListOfWhatWorkersHoldAndTheConfigNames(t *testing.T) {
	useFakeWorker(t, fakeModel{name: "wd", labels: labelsOf("a")}, fakeModel{name: "joytag", labels: labelsOf("b")})
	cfg := &config.Config{Tagger: config.TaggerConfig{Taggers: []config.TaggerInstance{
		{Name: "wd", Enabled: false, ConfidenceThreshold: 0.5},
		{Name: "gone", Enabled: true},
	}}}
	got := map[string]TaggerStatus{}
	var order []string
	for _, s := range DiscoverTaggers(cfg) {
		got[s.Name] = s
		order = append(order, s.Name)
	}
	if len(order) != 3 || order[0] != "wd" || order[2] != "gone" {
		t.Fatalf("order = %v", order)
	}
	if wd := got["wd"]; !wd.Available || wd.Enabled || wd.ConfidenceThreshold != 0.5 || wd.ModelFile != "model.onnx" {
		t.Errorf("wd = %+v, want held, the config's settings", wd)
	}
	if jt := got["joytag"]; !jt.Available || !jt.Enabled {
		t.Errorf("joytag = %+v, want held and seeded enabled", jt)
	}
	if g := got["gone"]; g.Available || !g.Missing || g.Reason != "no worker holds it" {
		t.Errorf("gone = %+v, want missing", g)
	}
	if !IsAvailable(&config.Config{Tagger: config.TaggerConfig{Taggers: []config.TaggerInstance{{Name: "joytag", Enabled: true}}}}) {
		t.Error("IsAvailable = false with an enabled tagger a worker holds")
	}
}

func TestDiscoverTaggers_AModelItsWorkerCannotRun(t *testing.T) {
	f := useFakeWorker(t, fakeModel{name: "wd", labels: labelsOf("a")}, fakeModel{name: "broken", labels: labelsOf("a")})
	f.Mu.Lock()
	f.Models[1].Ready, f.Models[1].Reason = false, "no inference profile: bad layout; add a tagger.json"
	f.Mu.Unlock()
	currentPool().Refresh(context.Background())
	for _, s := range DiscoverTaggers(&config.Config{}) {
		if s.Name == "broken" && (s.Available || !s.Malformed) {
			t.Errorf("broken = %+v, want malformed", s)
		}
	}

	f.Mu.Lock()
	f.Token = "changed"
	f.Mu.Unlock()
	currentPool().Refresh(context.Background())
	for _, s := range DiscoverTaggers(&config.Config{}) {
		if s.Name == "wd" && (s.Available || s.Missing || s.Reason != "the workers that hold it are off") {
			t.Errorf("wd with its worker down = %+v", s)
		}
	}
}

func TestTheReasonWithNoWorkerIsNoWorker(t *testing.T) {
	prev := currentPool()
	SetPool(NewPool(""))
	t.Cleanup(func() { SetPool(prev) })
	cfg := &config.Config{Tagger: config.TaggerConfig{Taggers: []config.TaggerInstance{{Name: "wd", Enabled: true}}}}
	if IsAvailable(cfg) || UnavailableReason(cfg) != "no worker" {
		t.Errorf("available %v, reason %q, want no worker", IsAvailable(cfg), UnavailableReason(cfg))
	}
}

func TestEnabledTaggersForGallery_FiltersByGalleries(t *testing.T) {
	useFakeWorker(t, fakeModel{name: "ok-everywhere", labels: labelsOf("a")}, fakeModel{name: "ok-default", labels: labelsOf("a")}, fakeModel{name: "ok-stock", labels: labelsOf("a")})
	cfg := &config.Config{
		Tagger: config.TaggerConfig{
			Taggers: []config.TaggerInstance{
				{Name: "ok-everywhere", Enabled: true, ConfidenceThreshold: 0.4},
				{Name: "ok-default", Enabled: true, ConfidenceThreshold: 0.4, Galleries: []string{"default"}},
				{Name: "ok-stock", Enabled: true, ConfidenceThreshold: 0.4, Galleries: []string{"stock"}},
			},
		},
	}

	gotDefault := EnabledTaggersForGallery(cfg, "default")
	if len(gotDefault) != 2 {
		t.Errorf("default: got %d taggers, want 2 (ok-everywhere + ok-default)", len(gotDefault))
	}
	gotStock := EnabledTaggersForGallery(cfg, "stock")
	if len(gotStock) != 2 {
		t.Errorf("stock: got %d taggers, want 2 (ok-everywhere + ok-stock)", len(gotStock))
	}
	gotPhantom := EnabledTaggersForGallery(cfg, "missing")

	if len(gotPhantom) != 1 {
		t.Errorf("phantom gallery: got %d taggers, want only ok-everywhere", len(gotPhantom))
	}

	for i := range cfg.Tagger.Taggers {
		cfg.Tagger.Taggers[i].Galleries = []string{"default"}
	}
	want := `no enabled tagger applies to gallery "stock"`
	if _, err := SelectForGallery(cfg, "stock", ""); err == nil || err.Error() != want {
		t.Errorf("SelectForGallery(stock) error = %v, want %q", err, want)
	}
	if _, _, reason := GalleryTaggers(cfg, "stock"); reason != want {
		t.Errorf("GalleryTaggers(stock) reason = %q, want %q", reason, want)
	}
	if got, err := SelectForGallery(cfg, "default", ""); err != nil || len(got) != 3 {
		t.Errorf("SelectForGallery(default) = %d taggers, %v; want all three", len(got), err)
	}
}

func TestAppliesToGallery(t *testing.T) {
	t1 := config.TaggerInstance{}
	if !t1.AppliesToGallery("anything") {
		t.Errorf("nil Galleries should apply everywhere")
	}
	t2 := config.TaggerInstance{Galleries: []string{"a", "b"}}
	if !t2.AppliesToGallery("a") {
		t.Errorf("matching name should apply")
	}
	if t2.AppliesToGallery("c") {
		t.Errorf("non-matching name should not apply")
	}

	t3 := config.TaggerInstance{Galleries: []string{}}
	if t3.AppliesToGallery("anything") {
		t.Errorf("explicit-empty Galleries should apply to nothing")
	}
}

func TestGalleryTaggersPresentSeparatesAbsentFromDisabled(t *testing.T) {
	useFakeWorker(t)
	if _, present, _ := GalleryTaggers(&config.Config{}, "default"); present {
		t.Errorf("present = true with no tagger held or configured")
	}
	useFakeWorker(t, fakeModel{name: "wd", labels: labelsOf("a")})
	cfg := &config.Config{Tagger: config.TaggerConfig{Taggers: []config.TaggerInstance{{Name: "wd", Enabled: false}}}}
	if _, got, _ := GalleryTaggers(cfg, "default"); !got {
		t.Error("present = false with a tagger held but disabled")
	}
	if len(EnabledTaggers(cfg)) != 0 {
		t.Errorf("EnabledTaggers = %+v, want none while the tagger is disabled", EnabledTaggers(cfg))
	}
}

func TestValidateTaggerName(t *testing.T) {
	for _, name := range []string{"wd-swinv2", "joytag", "A_1", "x"} {
		if err := ValidateTaggerName(name); err != nil {
			t.Errorf("ValidateTaggerName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"", "..", ".", "a/b", "../etc", "a\\b", "a b", "a.b", "foo$bar",
		"wd\nswinv2", "wd-swinv2/../..", "/abs", "café",
	} {
		if err := ValidateTaggerName(name); err == nil {
			t.Errorf("ValidateTaggerName(%q) = nil, want a refusal", name)
		}
	}
}
