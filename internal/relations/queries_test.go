package relations

import (
	"slices"
	"testing"
)

func TestLoadImageRelationsIncludesSelf(t *testing.T) {
	database, svc := setupTestDB(t)
	dupA := insertImage(t, database, "dupA", 100)
	dupB := insertImage(t, database, "dupB", 200)
	mustAddDuplicate(t, svc, dupA, dupB)
	altA := insertImage(t, database, "altA", 100)
	altB := insertImage(t, database, "altB", 100)
	mustAddAlternate(t, svc, altA, altB)

	rels, err := LoadImageRelations(database, altA)
	if err != nil {
		t.Fatalf("LoadImageRelations(altA): %v", err)
	}
	if !slices.Contains(rels.AltGroupMembers, altA) || !slices.Contains(rels.AltGroupMembers, altB) {
		t.Fatalf("AltGroupMembers = %v, want self %d and peer %d", rels.AltGroupMembers, altA, altB)
	}

	relsDup, err := LoadImageRelations(database, dupA)
	if err != nil {
		t.Fatalf("LoadImageRelations(dupA): %v", err)
	}
	if relsDup.DupGroup == nil || !slices.Contains(relsDup.DupGroup.Members, dupA) {
		t.Fatalf("DupGroup.Members = %v, want self %d", relsDup.DupGroup.Members, dupA)
	}
}

func TestCommonDerivativeAncestor(t *testing.T) {
	database, svc := setupTestDB(t)
	root := insertImage(t, database, "root", 100)
	branchA := insertImage(t, database, "branchA", 100)
	branchB := insertImage(t, database, "branchB", 100)
	sibA := insertImage(t, database, "sibA", 100)
	sibB := insertImage(t, database, "sibB", 100)
	cousin := insertImage(t, database, "cousin", 100)
	other := insertImage(t, database, "other", 100)
	loner := insertImage(t, database, "loner", 100)
	for _, e := range []struct{ source, derivative int64 }{
		{root, branchA},
		{root, branchB},
		{branchA, sibA},
		{branchA, sibB},
		{branchB, cousin},
		{other, loner},
	} {
		mustAddDerivativeEdge(t, svc, e.source, e.derivative)
	}

	for _, tc := range []struct {
		name string
		a, b int64
		want int64
	}{
		{"siblings", sibA, sibB, branchA},
		{"cousins", sibA, cousin, root},
		{"half-linked", sibA, other, 0},
		{"different trees", sibA, loner, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := CommonDerivativeAncestor(database, tc.a, tc.b)
			if err != nil {
				t.Fatalf("CommonDerivativeAncestor: %v", err)
			}
			if tc.want == 0 {
				if ok {
					t.Fatalf("got (%d, true), want none", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, true)", got, ok, tc.want)
			}
		})
	}
}
