package gallery

import (
	"slices"
	"testing"
)

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
