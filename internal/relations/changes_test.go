package relations

import (
	"context"
	"testing"

	"github.com/monbooru/monbooru/internal/tags"
)

func TestService_SignalsAfterEveryCommit(t *testing.T) {
	database, _ := setupTestDB(t)
	calls := 0
	svc := New(database, tags.New(database, nil), func() { calls++ })
	img := make([]int64, 12)
	for i := range img {
		img[i] = insertImage(t, database, "chg-"+string(rune('a'+i)), int64(100+i))
	}
	dupGroup := func(id int64) int64 {
		t.Helper()
		g, _ := dupGroupOf(t, database, id)
		return g.Int64
	}
	altGroup := func(id int64) int64 {
		t.Helper()
		return altGroupOf(t, database, id).Int64
	}

	steps := []struct {
		name  string
		run   func() error
		fails bool
		quiet bool
	}{
		{name: "AddDuplicate", run: func() error { return svc.AddDuplicate(img[0], img[1]) }},
		{name: "AddDuplicate of itself", fails: true, run: func() error { return svc.AddDuplicate(img[0], img[0]) }},
		{name: "AddDuplicateOf", run: func() error { return svc.AddDuplicateOf(img[2], img[3]) }},
		{name: "MergeDupGroups", run: func() error {
			return svc.MergeDupGroups([]int64{dupGroup(img[0]), dupGroup(img[2])}, img[0])
		}},
		{name: "PromoteToOriginal", run: func() error { return svc.PromoteToOriginal(dupGroup(img[0]), img[1]) }},
		{name: "CopyTagsFromDuplicatesToOriginal", run: func() error {
			_, err := svc.CopyTagsFromDuplicatesToOriginal(dupGroup(img[0]))
			return err
		}},
		{name: "RemoveDupMember", run: func() error { return svc.RemoveDupMember(img[3]) }},
		{name: "DissolveDupGroup", run: func() error { return svc.DissolveDupGroup(dupGroup(img[0])) }},
		{name: "AddAlternate", run: func() error { return svc.AddAlternate(img[4], img[5]) }},
		{name: "AddAlternate again", run: func() error { return svc.AddAlternate(img[6], img[7]) }},
		{name: "MergeAltGroups", run: func() error { return svc.MergeAltGroups([]int64{altGroup(img[4]), altGroup(img[6])}) }},
		{name: "RemoveAltMember", run: func() error { return svc.RemoveAltMember(img[7]) }},
		{name: "DissolveAltGroup", run: func() error { return svc.DissolveAltGroup(altGroup(img[4])) }},
		{name: "AddVersionEdge", run: func() error { return svc.AddVersionEdge(img[8], img[9]) }},
		{name: "ReverseVersionEdge", run: func() error { return svc.ReverseVersionEdge(img[8], img[9]) }},
		{name: "RemoveVersionEdge", run: func() error { return svc.RemoveVersionEdge(img[8], img[9]) }},
		{name: "AddVersionEdge again", run: func() error { return svc.AddVersionEdge(img[8], img[9]) }},
		{name: "DissolveVersionChain", run: func() error { return svc.DissolveVersionChain(img[8]) }},
		{name: "AddDerivativeEdge", run: func() error { return svc.AddDerivativeEdge(img[10], img[11]) }},
		{name: "ReverseDerivativeEdge", run: func() error { return svc.ReverseDerivativeEdge(img[10], img[11]) }},
		{name: "RemoveDerivativeEdge", run: func() error { return svc.RemoveDerivativeEdge(img[10], img[11]) }},
		{name: "AddDerivativeEdge again", run: func() error { return svc.AddDerivativeEdge(img[10], img[11]) }},
		{name: "DissolveDerivativeTree", run: func() error { return svc.DissolveDerivativeTree(img[10]) }},
		{name: "Overwrite", run: func() error { return svc.Overwrite("alternate", img[10], img[11]) }},
		{name: "Overwrite with an unknown kind", fails: true, run: func() error { return svc.Overwrite("sibling", img[10], img[11]) }},
		{name: "AddNotRelated", run: func() error { return svc.AddNotRelated(img[0], img[5]) }},
		{name: "RemoveNotRelated", run: func() error { return svc.RemoveNotRelated(img[0], img[5]) }},
		{name: "QueueForReview", quiet: true, run: func() error { return svc.QueueForReview(img[1], img[6]) }},
		{name: "SkipPair", quiet: true, run: func() error { return svc.SkipPair(img[1], img[6], nowISO()) }},
		{name: "ResetSkipped", quiet: true, run: func() error { _, err := svc.ResetSkipped(context.Background()); return err }},
		{name: "DropQueuedPair", quiet: true, run: func() error { return svc.DropQueuedPair(img[1], img[6]) }},
		{name: "SetSessionOrder", quiet: true, run: func() error { svc.SetSessionOrder("random"); return nil }},
	}
	for _, step := range steps {
		before := calls
		err := step.run()
		switch {
		case step.fails:
			if err == nil {
				t.Errorf("%s: want a refusal", step.name)
			}
			if calls != before {
				t.Errorf("%s: a refused write signalled a change", step.name)
			}
		case err != nil:
			t.Errorf("%s: %v", step.name, err)
		case step.quiet:
			if calls != before {
				t.Errorf("%s: a write no cache reads signalled a change", step.name)
			}
		case calls == before:
			t.Errorf("%s: committed without signalling a change", step.name)
		}
	}
}
