package monloader

import (
	"reflect"
	"testing"

	"github.com/monbooru/monbooru/internal/shared/contract"
)

func TestReadContribReceiptTalliesAndLabelsEachItem(t *testing.T) {
	items := []contract.ContribItem{
		{Kind: "mapping_add", Tag: "a"},
		{Kind: "mapping_petition", Tag: "b"},
		{Kind: "sibling", Bad: "x", Good: "y"},
		{Kind: "parent", Child: "c", Parent: "d"},
	}
	results := []contract.ContribResult{{Result: "staged"}, {Result: "staged"}, {Result: "refused", Note: "dup"}, {Result: "staged"}}
	got, ok := ReadContribReceipt(items, results)
	want := ContribReceipt{
		Sent: 3, Refused: 1,
		Adds:        []ContribReceiptRow{{Label: "a"}, {Label: "c => d"}},
		Petitions:   []ContribReceiptRow{{Label: "b"}},
		RefusedRows: []ContribReceiptRow{{Label: "x -> y", Note: "refused - dup"}},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("receipt = %+v %v, want %+v", got, ok, want)
	}
	if _, ok := ReadContribReceipt(items, results[:3]); ok {
		t.Error("a short answer was read: its verdicts would pair with the wrong items")
	}
}

func TestPairItemsDecodesTheRowValues(t *testing.T) {
	got := PairItems([]string{"sibling a b", "parent c d", "malformed"}, "suggest", "why")
	want := []contract.ContribItem{
		{Kind: "sibling", Bad: "a", Good: "b", Reason: "why"},
		{Kind: "parent", Child: "c", Parent: "d", Reason: "why"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("suggest items = %v, want %v", got, want)
	}
	got = PairItems([]string{"parent c d"}, "petition", "gone")
	if want := []contract.ContribItem{{Kind: "parent_petition", Child: "c", Parent: "d", Reason: "gone"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("petition items = %v, want %v", got, want)
	}
	if got := PairItems([]string{"sibling a b"}, "other", ""); got != nil {
		t.Errorf("unknown direction = %v, want nothing", got)
	}
}
