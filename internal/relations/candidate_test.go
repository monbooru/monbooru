package relations

import (
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func addTagRow(t *testing.T, database *db.DB, imageID int64, name string, implied bool) {
	t.Helper()
	tagID := insertTagRow(t, database, name)
	if implied {
		attachImpliedTagRow(t, database, imageID, tagID)
	} else {
		attachTagRow(t, database, imageID, tagID)
	}
}

func TestDuplicateCandidateDetails_SimplePair(t *testing.T) {
	database, svc := setupTestDB(t)
	orig := insertImage(t, database, "cand-o", 1000)
	dup := insertImage(t, database, "cand-d", 1000)

	addTagRow(t, database, dup, "cand-tag", false)
	addSourceRow(t, database, dup, "site_x", "10", "https://x.example/10")

	effectiveOrig, hasTags, hasSources, err := svc.DuplicateCandidateDetails(orig, dup)
	if err != nil {
		t.Fatal(err)
	}
	if effectiveOrig != orig {
		t.Errorf("effectiveOriginal = %d, want %d", effectiveOrig, orig)
	}
	if !hasTags {
		t.Errorf("hasTags = false, want true")
	}
	if !hasSources {
		t.Errorf("hasSources = false, want true")
	}
}

func TestDuplicateCandidateDetails_LeftIsAlreadyDuplicate(t *testing.T) {
	database, svc := setupTestDB(t)
	orig := insertImage(t, database, "cand-root", 1000)
	left := insertImage(t, database, "cand-left", 1000)
	otherMember := insertImage(t, database, "cand-other", 1000)
	right := insertImage(t, database, "cand-right", 1000)

	mustAddDuplicate(t, svc, orig, left)
	mustAddDuplicate(t, svc, orig, otherMember)

	// otherMember in the left's group has a unique tag
	addTagRow(t, database, otherMember, "left-group-tag", false)

	effectiveOrig, hasTags, _, err := svc.DuplicateCandidateDetails(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if effectiveOrig != orig {
		t.Errorf("effectiveOriginal = %d, want %d (the root original)", effectiveOrig, orig)
	}
	if !hasTags {
		t.Errorf("hasTags = false, want true because left image's group has tags to copy")
	}
}

func TestDuplicateCandidateDetails_NoUnique(t *testing.T) {
	database, svc := setupTestDB(t)
	orig := insertImage(t, database, "cand-same-o", 1000)
	dup := insertImage(t, database, "cand-same-d", 1000)

	addTagRow(t, database, orig, "shared-tag", false)
	addTagRow(t, database, dup, "shared-tag", false)
	addSourceRow(t, database, orig, "site_same", "1", "https://same.example/1")
	addSourceRow(t, database, dup, "site_same", "1", "https://same.example/1")

	_, hasTags, hasSources, err := svc.DuplicateCandidateDetails(orig, dup)
	if err != nil {
		t.Fatal(err)
	}
	if hasTags {
		t.Errorf("hasTags = true, want false")
	}
	if hasSources {
		t.Errorf("hasSources = true, want false")
	}
}

func TestDuplicateCandidateDetails_BothInGroups(t *testing.T) {
	database, svc := setupTestDB(t)
	rootA := insertImage(t, database, "cand-roota", 1000)
	left := insertImage(t, database, "cand-left2", 1000)
	rootB := insertImage(t, database, "cand-rootb", 1000)
	right := insertImage(t, database, "cand-right2", 1000)

	mustAddDuplicate(t, svc, rootA, left)
	mustAddDuplicate(t, svc, rootB, right)

	effectiveOrig, _, _, err := svc.DuplicateCandidateDetails(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if effectiveOrig != rootA && effectiveOrig != rootB {
		t.Errorf("effectiveOriginal = %d, want either %d or %d", effectiveOrig, rootA, rootB)
	}
}

