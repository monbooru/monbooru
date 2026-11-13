package relations

import (
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
)

func addSourceRow(t *testing.T, database *db.DB, imageID int64, site, postID, url string) {
	t.Helper()
	mustExec(t, database.Write, `INSERT INTO image_sources (image_id, site, post_id, url) VALUES (?, ?, ?, ?)`,
		imageID, site, postID, url)
}

// dupTrio groups orig with b and c and checks orig stays the original.
func dupTrio(t *testing.T) (database *db.DB, svc *Service, orig, b, c, gid int64) {
	t.Helper()
	database, svc = setupTestDB(t)
	orig = insertImage(t, database, "src-orig", 10000)
	b = insertImage(t, database, "src-b", 100)
	c = insertImage(t, database, "src-c", 100)
	mustAddDuplicate(t, svc, orig, b)
	mustAddDuplicate(t, svc, orig, c)
	g, o := dupGroupOf(t, database, orig)
	if o.Int64 != orig {
		t.Fatalf("original = %d, want %d", o.Int64, orig)
	}
	return database, svc, orig, b, c, g.Int64
}

func TestCopySourcesFromDuplicatesToOriginal(t *testing.T) {
	database, svc, orig, b, c, gid := dupTrio(t)
	mustExec(t, database.Write, `UPDATE images SET md5 = ? WHERE id = ?`, strings.Repeat("a", 32), orig)
	addSourceRow(t, database, orig, "site_a", "1", "https://a.example/1")

	addSourceRow(t, database, b, "site_b", "7", "https://b.example/7")
	mustExec(t, database.Write, `UPDATE image_sources SET md5 = ?, commentary = 'note from user_a', post_width = 800
		WHERE image_id = ? AND site = 'site_b'`, strings.Repeat("b", 32), b)
	mustExec(t, database.Write, `INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual)
		VALUES (?, 'site_b', '7', 1, 2, 3, 4, 'pulled', 0), (?, 'site_b', '7', 5, 6, 7, 8, 'drawn', 1)`, b, b)
	addSourceRow(t, database, b, "site_a", "1", "https://a.example/other")
	addSourceRow(t, database, b, "mirror", "", "https://a.example/1")
	addSourceRow(t, database, c, "site_b", "7", "https://b.example/7")
	addSourceRow(t, database, c, "site_c", "3", "https://c.example/3")

	preview, err := svc.SourcesToCopy(gid)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range preview {
		got = append(got, s.Site+"/"+s.PostID)
	}
	if strings.Join(got, " ") != "site_b/7 site_c/3" {
		t.Errorf("preview = %v, want site_b/7 and site_c/3 (the original's post and url skipped, the shared post once)", got)
	}
	if with, _ := svc.GroupsWithSourcesToCopy([]int64{gid}); !with[gid] {
		t.Error("the group is not offered a copy")
	}

	added, err := svc.CopySourcesFromDuplicatesToOriginal(gid)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}

	var commentary, md5Match string
	var width, kept int
	if err := database.Read.QueryRow(`SELECT commentary, post_width, upgrade_kept, md5_match FROM image_sources
		WHERE image_id = ? AND site = 'site_b' AND post_id = '7'`, orig).Scan(&commentary, &width, &kept, &md5Match); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"commentary", commentary, "note from user_a"},
		{"post width", width, 800},
		{"kept, so no upgrade is offered", kept, 1},
		{"md5 verdict against the original's file", md5Match, "differ"},
		{"the original's own source untouched", countRows(t, database, "image_sources WHERE image_id = ? AND site = 'site_a' AND url = 'https://a.example/1'", orig), 1},
		{"no row for the original's url under another label", countRows(t, database, "image_sources WHERE image_id = ? AND site = 'mirror'", orig), 0},
		{"pulled notes copied, drawn ones not", countRows(t, database, "image_annotations WHERE image_id = ? AND site = 'site_b'", orig), 1},
		{"the duplicates keep their rows", countRows(t, database, "image_sources WHERE image_id IN (?, ?)", b, c), 5},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}

	if again, err := svc.CopySourcesFromDuplicatesToOriginal(gid); err != nil || again != 0 {
		t.Errorf("second run added %d, %v; want 0", again, err)
	}
	if with, _ := svc.GroupsWithSourcesToCopy([]int64{gid}); with[gid] {
		t.Error("the group is still offered a copy after it ran")
	}
}

func TestCopySourcesGivesASourcelessOriginalItsPrimary(t *testing.T) {
	database, svc, orig, b, _, gid := dupTrio(t)
	addSourceRow(t, database, b, "site_b", "7", "https://b.example/7")

	if _, err := svc.CopySourcesFromDuplicatesToOriginal(gid); err != nil {
		t.Fatal(err)
	}
	var source, url string
	if err := database.Read.QueryRow(`SELECT source, url FROM images WHERE id = ?`, orig).Scan(&source, &url); err != nil {
		t.Fatal(err)
	}
	if source != "site_b" || url != "https://b.example/7" {
		t.Errorf("images.source, url = %q, %q; want the copied origin mirrored", source, url)
	}
}
