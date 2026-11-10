package galleryio

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/gallery"
)

var exportedTables = []struct {
	table   string
	row     any
	exclude []string
}{
	{"tag_categories", tagCategoryRow{}, nil},
	{"tags", tagRow{}, nil},
	{"tag_implications", tagImplicationRow{}, nil},
	{"tag_notes", tagNoteRow{}, nil},

	{"images", imageRow{}, []string{"series", "series_order", "tag_count", "rating_rank"}},
	{"image_collections", imageCollectionRow{}, nil},
	{"collection_find_relations", findRelationsRow{}, nil},
	{"image_sources", imageSourceRow{}, nil},

	{"image_annotations", imageAnnotationRow{}, []string{"id"}},
	{"image_paths", imagePathRow{}, nil},
	{"image_tags", imageTagRow{}, nil},
	{"image_tag_sources", imageTagSourceRow{}, nil},
	{"sd_metadata", sdMetadataRow{}, nil},

	{"comfyui_metadata", comfyMetadataRow{}, []string{"terms_version"}},
	{"manga_metadata", mangaMetadataRow{}, nil},
	{"dup_groups", dupGroupRow{}, nil},
	{"dup_group_members", dupGroupMemberRow{}, nil},
	{"alt_groups", altGroupRow{}, nil},
	{"alt_group_members", altGroupMemberRow{}, nil},
	{"version_edges", versionEdgeRow{}, nil},
	{"derivative_edges", derivativeEdgeRow{}, nil},
	{"not_related_pairs", notRelatedPairRow{}, nil},
	{"saved_searches", savedSearchRow{}, nil},
	{"image_lookups", imageLookupRow{}, nil},
}

func TestExportCarriesEverySchemaColumn(t *testing.T) {
	g := newGallery(t, t.TempDir(), "cover")

	for _, tc := range exportedTables {
		carried := jsonKeys(tc.row)
		for _, col := range tableColumns(t, g, tc.table) {
			if carried[col] || slices.Contains(tc.exclude, col) {
				continue
			}
			t.Errorf("%s.%s is neither carried by %T nor excluded", tc.table, col, tc.row)
		}
	}
}

func jsonKeys(row any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(row)
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

func tableColumns(t *testing.T, g gallery.Handle, table string) []string {
	t.Helper()
	out, err := db.QueryStrings(g.DB.Read, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no columns - is it still in schema.sql?", table)
	}
	return out
}
