package upgrade

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

func TestEligible(t *testing.T) {
	tests := []struct {
		name string
		in   models.ImageSource
		want bool
	}{
		{"no url", models.ImageSource{MD5Match: "differ"}, false},
		{"hash differs", models.ImageSource{URL: "https://e/1", MD5Match: "differ"}, true},
		{"verified match withholds", models.ImageSource{URL: "https://e/1", MD5Match: "match"}, false},
		{"similarity with no claim", models.ImageSource{URL: "https://e/1", Similarity: 92}, true},
		{"similarity but verified", models.ImageSource{URL: "https://e/1", Similarity: 92, MD5Match: "match"}, false},
		{"exact with no claim", models.ImageSource{URL: "https://e/1"}, false},
		{"kept outranks a mismatch", models.ImageSource{URL: "https://e/1", MD5Match: "differ", UpgradeKept: true}, false},
		{"kept outranks a similarity match", models.ImageSource{URL: "https://e/1", Similarity: 92, UpgradeKept: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Eligible(tt.in); got != tt.want {
				t.Errorf("Eligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCandidateWhereSeeksTheUpgradableIndex(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	rows, err := database.Read.Query(`EXPLAIN QUERY PLAN SELECT s.image_id FROM image_sources s WHERE ` + CandidateWhere("s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_image_sources_upgradable") {
		t.Errorf("CandidateWhere(%q) plans as\n%s want idx_image_sources_upgradable", "s", plan)
	}
}
