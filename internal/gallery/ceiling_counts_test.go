package gallery

import (
	"slices"
	"testing"
)

func TestChildrenPage(t *testing.T) {
	root := FindFolder(buildFolderTree([]folderCount{{"a", 1}, {"b", 1}, {"c", 1}, {"d", 1}, {"e", 1}}), "")
	cases := []struct {
		name          string
		offset, limit int
		want          []string
	}{
		{"the first page", 0, 2, []string{"a", "b"}},
		{"a middle page", 2, 2, []string{"c", "d"}},
		{"the last page runs short", 4, 2, []string{"e"}},
		{"past the end, nothing", 9, 2, nil},
		{"more than there are", 0, 50, []string{"a", "b", "c", "d", "e"}},
	}
	for _, tc := range cases {
		var got []string
		for _, n := range root.ChildrenPage(tc.offset, tc.limit) {
			got = append(got, n.Path)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: ChildrenPage(%d, %d) = %v, want %v", tc.name, tc.offset, tc.limit, got, tc.want)
		}
	}
}

func TestSearchFolders(t *testing.T) {
	tree := buildFolderTree([]folderCount{
		{"art/sky", 1}, {"art/sea", 1}, {"photos/art", 1}, {"ARTWORK", 1}, {"misc", 1},
	})
	cases := []struct {
		name, q string
		limit   int
		want    []string
	}{
		{"those starting with it first, any case", "art", 10, []string{"ARTWORK", "art", "art/sea", "art/sky", "photos/art"}},
		{"by any part of the path", "sky", 10, []string{"art/sky"}},
		{"at most limit", "art", 2, []string{"ARTWORK", "art"}},
		{"nothing asked, nothing found", " ", 10, nil},
		{"no match", "zzz", 10, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, n := range SearchFolders(tree, tc.q, tc.limit) {
				got = append(got, n.Path)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("SearchFolders(%q, %d) = %v, want %v", tc.q, tc.limit, got, tc.want)
			}
		})
	}
	if got := SearchFolders(nil, "art", 10); got != nil {
		t.Errorf("an empty tree found %v", got)
	}
}
