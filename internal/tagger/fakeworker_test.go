package tagger

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/worker"
)

type fakeModel struct {
	name     string
	size     int
	labels   []worker.Label
	scheme   string
	mappings []worker.Mapping
}

func newFake(t *testing.T, models ...fakeModel) *worker.Fake {
	t.Helper()
	f := &worker.Fake{Token: "tok", Info: worker.Info{Name: "fake", Slots: 2}, Labels: map[string]worker.Labels{}}
	for _, m := range models {
		scheme := cmp.Or(m.scheme, "wd14_numeric")

		etag := fmt.Sprintf("%q", t.Name()+"/"+m.name)
		f.Models = append(f.Models, worker.Model{
			Name: m.name, Kind: worker.KindTagger, Digest: "d-" + m.name, ModelFile: "model.onnx", LabelFile: "tags.csv",
			Input: worker.Input{Size: cmp.Or(m.size, 8), Fill: [3]uint8{255, 255, 255}}, Labels: len(m.labels), LabelsETag: etag,
			Profile: json.RawMessage(fmt.Sprintf(`{"category_scheme":%q}`, scheme)), Ready: true,
		})
		f.Labels[m.name] = worker.Labels{Model: m.name, ETag: etag, CategoryScheme: scheme, Labels: m.labels, Mappings: m.mappings}
	}
	return f
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func usePool(t *testing.T, dir string, defs ...WorkerDef) *Pool {
	t.Helper()
	p := NewPool(dir)
	p.Set(defs)
	p.Refresh(context.Background())
	prev := currentPool()
	SetPool(p)
	t.Cleanup(func() { SetPool(prev) })
	return p
}

func useFakeWorker(t *testing.T, models ...fakeModel) *worker.Fake {
	t.Helper()
	f := newFake(t, models...)
	usePool(t, "", WorkerDef{Name: "fake", URL: serve(t, f), Token: "tok"})
	return f
}

func labelsOf(names ...string) []worker.Label {
	out := make([]worker.Label, len(names))
	for i, n := range names {
		out[i] = worker.Label{Name: n}
	}
	return out
}

var (
	wd14Profile   = Profile{CategoryScheme: "wd14_numeric"}
	joytagProfile = Profile{CategoryScheme: "single_general"}
)

func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
