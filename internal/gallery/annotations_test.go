package gallery

import (
	"errors"
	"testing"

	"github.com/monbooru/monbooru/internal/models"
)

func TestReplaceSourceAnnotations_CloneAndScope(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)

	boxes := func(site string, bodies ...string) []models.Annotation {
		var a []models.Annotation
		for i, b := range bodies {
			a = append(a, models.Annotation{Site: site, X: i, Y: i, W: 10, H: 10, Body: b})
		}
		return a
	}
	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", boxes("danbooru", "a", "b")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceSourceAnnotations(database, id, "gelbooru", "", boxes("gelbooru", "c")); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 3 {
		t.Fatalf("got %d annotations, want 3", len(anns))
	}

	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", boxes("danbooru", "a2")); err != nil {
		t.Fatal(err)
	}
	if anns, _ := AnnotationsForImage(database, id); len(anns) != 2 {
		t.Fatalf("got %d after re-pull, want 2", len(anns))
	}

	if err := ReplaceSourceAnnotations(database, id, "danbooru", "", nil); err != nil {
		t.Fatal(err)
	}
	anns, _ := AnnotationsForImage(database, id)
	if len(anns) != 1 || anns[0].Site != "gelbooru" {
		t.Fatalf("got %+v after clear, want only gelbooru", anns)
	}
}

func TestAnnotations_UpdateDeleteScopedToImage(t *testing.T) {
	database := newCollectionsTestDB(t)
	a := insertImage(t, database, false)
	b := insertImage(t, database, false)
	if err := AddManualAnnotation(database, a, 1, 1, 5, 5, "a's box"); err != nil {
		t.Fatal(err)
	}
	anns, _ := AnnotationsForImage(database, a)
	if len(anns) != 1 {
		t.Fatalf("seeded %d boxes", len(anns))
	}
	if err := UpdateAnnotation(database, b, anns[0].ID, 0, 0, 1, 1, "moved"); !errors.Is(err, ErrAnnotationNotFound) {
		t.Errorf("update through another image = %v, want ErrAnnotationNotFound", err)
	}
	if err := DeleteAnnotation(database, b, anns[0].ID); !errors.Is(err, ErrAnnotationNotFound) {
		t.Errorf("delete through another image = %v, want ErrAnnotationNotFound", err)
	}
	if after, _ := AnnotationsForImage(database, a); len(after) != 1 || after[0].Body != "a's box" {
		t.Errorf("a's boxes = %+v, want its box untouched", after)
	}
}

func TestAnnotations_CRUDAndManualSurvivesSourceOps(t *testing.T) {
	database := newCollectionsTestDB(t)
	id := insertImage(t, database, false)
	if err := AddSourceMembership(database, id, "danbooru", "", ""); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{Site: "danbooru", X: 1, Y: 1, W: 5, H: 5, Body: "src"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddManualAnnotation(database, id, 2, 3, 4, 5, "mine"); err != nil {
		t.Fatal(err)
	}

	anns, _ := AnnotationsForImage(database, id)
	var manual, source models.Annotation
	for _, a := range anns {
		if a.Manual {
			manual = a
		} else {
			source = a
		}
	}
	if manual.ID == 0 || manual.Body != "mine" {
		t.Fatalf("expected one manual box with an id, got %+v", anns)
	}

	if err := UpdateAnnotation(database, id, manual.ID, 10, 11, 12, 13, "edited"); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAnnotation(database, id, source.ID, 6, 7, 8, 9, "src edited"); err != nil {
		t.Fatalf("source box should be editable: %v", err)
	}
	if err := DeleteAnnotation(database, id, source.ID); err != nil {
		t.Fatalf("source box should be removable: %v", err)
	}

	if err := ReplaceSourceAnnotations(database, id, "danbooru", "",
		[]models.Annotation{{Site: "danbooru", X: 1, Y: 1, W: 5, H: 5, Body: "src2"}}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSourceMembership(database, id, "danbooru", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := AnnotationsForImage(database, id)
	if len(after) != 1 || !after[0].Manual || after[0].Body != "edited" {
		t.Fatalf("manual box must survive source removal, got %+v", after)
	}

	if err := DeleteAnnotation(database, id, after[0].ID); err != nil {
		t.Fatal(err)
	}
	if final, _ := AnnotationsForImage(database, id); len(final) != 0 {
		t.Fatalf("want 0 boxes after delete, got %d", len(final))
	}
}
