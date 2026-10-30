// Package relations manages the operator-declared graph between images.
package relations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

var (
	ErrSelfRelation = errors.New("relations: pair refers to a single image")

	ErrRelationConflict = errors.New("relations: pair already has a different relation")

	ErrIndirectRelation = errors.New("relations: pair is already related through another image")

	ErrVersionExists = errors.New("relations: version edge already exists on one side")

	ErrDerivativeCycle = errors.New("relations: source already descends from the derivative")

	ErrChainTooDeep = errors.New("relations: chain would exceed the depth limit")

	ErrNotInGroup = errors.New("relations: image is not a member of the group")

	ErrUnknownKind = errors.New("relations: unknown kind")
)

// CrossConflictError names a pair, other than the one being related, that
// a join, merge or edge would give a second kind of relation.
type CrossConflictError struct{ A, B int64 }

func (e *CrossConflictError) Error() string {
	return fmt.Sprintf("relations: images %d and %d are already related another way", e.A, e.B)
}

type FriendlyError struct {
	Status  int
	Code    string
	Message string
}

func FriendlyErrorFor(err error) *FriendlyError {
	var cross *CrossConflictError
	switch {
	case errors.As(err, &cross):
		return &FriendlyError{Status: 409, Code: "conflict", Message: fmt.Sprintf(
			"That would also relate #%d and #%d, which are related another way; unlink them first.", cross.A, cross.B)}
	case errors.Is(err, ErrSelfRelation):
		return &FriendlyError{Status: 400, Code: "invalid_request", Message: "Cannot relate an image to itself."}
	case errors.Is(err, ErrRelationConflict):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "Pair already has a different relation; remove the existing one first."}
	case errors.Is(err, ErrIndirectRelation):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "These images are already related through another image."}
	case errors.Is(err, ErrVersionExists):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "One of the images already has a version edge; remove it first."}
	case errors.Is(err, ErrDerivativeCycle):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "The chosen source is already based on this image."}
	case errors.Is(err, ErrChainTooDeep):
		return &FriendlyError{Status: 409, Code: "conflict", Message: "The chain is already at its maximum depth."}
	case errors.Is(err, ErrNotInGroup):
		return &FriendlyError{Status: 400, Code: "invalid_request", Message: "Image isn't a member of that group."}
	}
	return nil
}

type Service struct {
	db       *db.DB
	tags     *tags.Service
	onChange func()
}

// onChange runs after every write the service commits itself to a table
// a cache reads; a write into a caller's transaction leaves it to that
// caller.
func New(database *db.DB, tagSvc *tags.Service, onChange func()) *Service {
	return &Service{db: database, tags: tagSvc, onChange: onChange}
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// not_related_pairs and the queue store each pair once, as (lo, hi).
func canonicalPair(a, b int64) (int64, int64) {
	return min(a, b), max(a, b)
}

func (s *Service) inWriteTx(work func(*sql.Tx) error) error {
	if err := db.InWriteTx(s.db.Write, work); err != nil {
		return err
	}
	s.changed()
	return nil
}

func (s *Service) pairTx(a, b int64, work func(*sql.Tx) error) error {
	if a == b {
		return ErrSelfRelation
	}
	return s.inWriteTx(work)
}

// The caller binds a, b, b, a.
func eitherWay(colA, colB string) string {
	return `(` + colA + ` = ? AND ` + colB + ` = ?) OR (` + colA + ` = ? AND ` + colB + ` = ?)`
}

func (s *Service) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// exec commits one statement and signals the change.
func (s *Service) exec(query string, args ...any) (sql.Result, error) {
	res, err := s.db.Write.Exec(query, args...)
	if err == nil {
		s.changed()
	}
	return res, err
}

// Leaves the other kind's groups alone: a pair carries one relation type,
// and folding those too would give it two.
func (s *Service) addGroupRelation(a, b int64, label string, cfg groupMerge) error {
	return s.pairTx(a, b, func(tx *sql.Tx) error { return addGroupRelationTx(tx, a, b, label, cfg) })
}

func addGroupRelationTx(tx *sql.Tx, a, b int64, label string, cfg groupMerge) error {
	if err := pairConflictTx(tx, a, b, label); err != nil {
		return err
	}
	sideA, err := groupSideTx(tx, cfg.membersTbl, a)
	if err != nil {
		return err
	}
	if !slices.Contains(sideA, b) {
		sideB, err := groupSideTx(tx, cfg.membersTbl, b)
		if err != nil {
			return err
		}
		if err := crossConflictTx(tx, sideA, sideB, label); err != nil {
			return err
		}
	}
	if err := mergeIntoGroupTx(tx, a, b, cfg); err != nil {
		return err
	}
	return pruneQueueForGroupTx(tx, cfg.membersTbl, a)
}

// AddDuplicate makes a the original when it creates a group; an existing
// group keeps its own.
func (s *Service) AddDuplicate(a, b int64) error {
	return s.addGroupRelation(a, b, "duplicate", dupGroupMerge)
}

// AddDuplicateOf never leaves dup as its group's original, but keeps an
// original the group already had among the others.
func (s *Service) AddDuplicateOf(original, dup int64) error {
	return s.pairTx(original, dup, func(tx *sql.Tx) error {
		if err := addGroupRelationTx(tx, original, dup, "duplicate", dupGroupMerge); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE original_image_id = ?`, original, dup)
		return err
	})
}

func (s *Service) AddAlternate(a, b int64) error {
	return s.addGroupRelation(a, b, "alternate", altGroupMerge)
}

// MaxVersionChainDepth is enforced when an edge is added, so a walk
// capped at it covers the whole chain or tree.
const MaxVersionChainDepth = 16

// ChainPath returns the nodes past start, nearest first. It follows one
// row per step, so whereCol must be unique in table.
func ChainPath(q db.Querier, table, selectCol, whereCol string, start int64) ([]int64, error) {
	var path []int64
	cur := start
	for i := 0; i < MaxVersionChainDepth; i++ {
		next, err := db.QueryIDs(q, `SELECT `+selectCol+` FROM `+table+` WHERE `+whereCol+` = ?`, cur)
		if err != nil {
			return nil, err
		}
		if len(next) == 0 {
			return path, nil
		}
		path = append(path, next[0])
		cur = next[0]
	}
	return path, nil
}

func ChainRoot(q db.Querier, table, parentCol, childCol string, start int64) (int64, error) {
	path, err := ChainPath(q, table, parentCol, childCol, start)
	if err != nil {
		return 0, err
	}
	if len(path) == 0 {
		return start, nil
	}
	return path[len(path)-1], nil
}

func chainReachesTx(tx *sql.Tx, table, parentCol, childCol string, start, target int64) (bool, error) {
	above, _, err := chainSpan(tx, table, parentCol, childCol, start)
	if err != nil {
		return false, err
	}
	return slices.Contains(above[1:], target), nil
}

func chainRelatesTx(tx *sql.Tx, table, parentCol, childCol string, a, b int64) (bool, error) {
	if up, err := chainReachesTx(tx, table, parentCol, childCol, a, b); err != nil || up {
		return up, err
	}
	return chainReachesTx(tx, table, parentCol, childCol, b, a)
}

// The visited set matters: a derivative names several sources, and
// without it the frontier doubles at every shared level.
func chainSpan(q db.Querier, table, selectCol, whereCol string, start int64) ([]int64, int, error) {
	span := []int64{start}
	seen := map[int64]bool{start: true}
	frontier := []int64{start}
	levels := 0
	for level := 0; level < MaxVersionChainDepth && len(frontier) > 0; level++ {
		var next []int64
		for _, id := range frontier {
			ids, err := db.QueryIDs(q,
				`SELECT `+selectCol+` FROM `+table+` WHERE `+whereCol+` = ? ORDER BY `+selectCol, id)
			if err != nil {
				return nil, 0, err
			}
			for _, id := range ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				next = append(next, id)
			}
		}
		if len(next) == 0 {
			break
		}
		levels++
		span = append(span, next...)
		frontier = next
	}
	return span, levels, nil
}

// Uncapped: several sources per derivative mean no single root, and the
// visited set bounds the walk.
func DerivativeComponent(q db.Querier, imageID int64) ([]int64, error) {
	members := []int64{imageID}
	seen := map[int64]bool{imageID: true}
	for i := 0; i < len(members); i++ {
		for _, side := range [2][2]string{
			{"source_image_id", "derivative_image_id"},
			{"derivative_image_id", "source_image_id"},
		} {
			ids, err := db.QueryIDs(q,
				`SELECT `+side[0]+` FROM derivative_edges WHERE `+side[1]+` = ? ORDER BY `+side[0], members[i])
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if !seen[id] {
					seen[id] = true
					members = append(members, id)
				}
			}
		}
	}
	if len(members) < 2 {
		return nil, nil
	}
	return members, nil
}

type edgeSpec struct {
	table, parentCol, childCol string
	exists                     error
	occupied                   func(tx *sql.Tx, parent, child int64) (bool, error)
}

var versionEdge = edgeSpec{
	table: "version_edges", parentCol: "parent_image_id", childCol: "child_image_id",
	exists: ErrVersionExists,
	occupied: func(tx *sql.Tx, parent, child int64) (bool, error) {
		var n int
		err := tx.QueryRow(
			`SELECT COUNT(*) FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`,
			child, parent,
		).Scan(&n)
		return n > 0, err
	},
}

var derivativeEdge = edgeSpec{
	table: "derivative_edges", parentCol: "source_image_id", childCol: "derivative_image_id",
	exists: ErrDerivativeCycle,
}

// AddVersionEdge makes child the newer version of parent. A chain is
// strict: one parent and one child per image.
func (s *Service) AddVersionEdge(parent, child int64) error {
	return s.addEdge(versionEdge, "version", parent, child)
}

// AddDerivativeEdge records that derivative was made from source; unlike
// a version chain, both sides fan out.
func (s *Service) AddDerivativeEdge(source, derivative int64) error {
	return s.addEdge(derivativeEdge, "derivative", source, derivative)
}

func (s *Service) addEdge(spec edgeSpec, kind string, parent, child int64) error {
	return s.pairTx(parent, child, func(tx *sql.Tx) error { return addEdgeTx(tx, spec, kind, parent, child) })
}

func addEdgeTx(tx *sql.Tx, spec edgeSpec, kind string, parent, child int64) error {
	if err := pairConflictTx(tx, parent, child, kind); err != nil {
		return err
	}
	prune := func() error {
		return pruneQueueForChainTx(tx, spec.table, spec.parentCol, spec.childCol, parent, child)
	}
	// A re-add succeeds so a retried request cannot read as a
	// conflict. It still prunes: a queue row can predate the edge.
	var exact int
	if err := tx.QueryRow(
		`SELECT 1 FROM `+spec.table+` WHERE `+spec.childCol+` = ? AND `+spec.parentCol+` = ?`,
		child, parent,
	).Scan(&exact); err == nil {
		return prune()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if spec.occupied != nil {
		switch occupied, err := spec.occupied(tx, parent, child); {
		case err != nil:
			return err
		case occupied:
			return spec.exists
		}
	}
	// child already above parent would close a cycle.
	if reaches, err := chainReachesTx(tx, spec.table, spec.parentCol, spec.childCol, parent, child); err != nil {
		return err
	} else if reaches {
		return spec.exists
	}
	// The edge puts every ancestor of parent on one path with every
	// descendant of child, so their depths add. The levels saturate at
	// MaxVersionChainDepth, which is all the depth check needs.
	above, up, err := chainSpan(tx, spec.table, spec.parentCol, spec.childCol, parent)
	if err != nil {
		return err
	}
	below, down, err := chainSpan(tx, spec.table, spec.childCol, spec.parentCol, child)
	if err != nil {
		return err
	}
	if up+down+1 > MaxVersionChainDepth {
		return ErrChainTooDeep
	}
	if err := crossConflictTx(tx, above, below, kind); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO `+spec.table+` (`+spec.childCol+`, `+spec.parentCol+`, created_at) VALUES (?, ?, ?)`,
		child, parent, nowISO(),
	); err != nil {
		return err
	}
	return prune()
}

func (s *Service) AddNotRelated(a, b int64) error {
	return s.pairTx(a, b, func(tx *sql.Tx) error { return addNotRelatedTx(tx, a, b) })
}

func addNotRelatedTx(tx *sql.Tx, a, b int64) error {
	if err := pairConflictTx(tx, a, b, "not_related"); err != nil {
		return err
	}
	lo, hi := canonicalPair(a, b)
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO not_related_pairs (a_image_id, b_image_id, created_at) VALUES (?, ?, ?)`,
		lo, hi, nowISO(),
	); err != nil {
		return err
	}
	return pruneQueuePairTx(tx, a, b)
}

// RemoveDupMember dissolves a group it would leave with one member, and
// re-picks the original when the original leaves.
func (s *Service) RemoveDupMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeMemberTx(tx, dupGroupMerge, imageID) })
}

// %s takes the leaving set's placeholders. Shared so the unlink preview
// names the member the promotion will pick.
const nextDupOriginalQuery = `SELECT m.image_id FROM dup_group_members m
	JOIN images i ON i.id = m.image_id
	WHERE m.group_id = ? AND m.image_id NOT IN (%s)
	ORDER BY i.file_size DESC, m.image_id DESC
	LIMIT 1`

func removeMemberTx(tx *sql.Tx, cfg groupMerge, imageID int64) error {
	if err := groupsOnBatchDeleteTx(tx, cfg.membersTbl, cfg.groupTbl, []int64{imageID}, cfg.promoteOriginal); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM `+cfg.membersTbl+` WHERE image_id = ?`, imageID)
	return err
}

func (s *Service) DissolveDupGroup(groupID int64) error {
	_, err := s.exec(`DELETE FROM dup_groups WHERE id = ?`, groupID)
	return err
}

// NextOriginalIfRemoved names the member that would replace removeID as
// original, or 0 when the group would dissolve.
func (s *Service) NextOriginalIfRemoved(groupID, removeID int64) (int64, error) {
	var n int
	if err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ?`, groupID,
	).Scan(&n); err != nil {
		return 0, err
	}
	if n < 3 {
		return 0, nil
	}
	var nextID int64
	err := s.db.Read.QueryRow(fmt.Sprintf(nextDupOriginalQuery, "?"), groupID, removeID).Scan(&nextID)
	return nextID, err
}

func (s *Service) PromoteToOriginal(groupID, imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM dup_group_members WHERE group_id = ? AND image_id = ?`, groupID, imageID,
		).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotInGroup
		}
		_, err := tx.Exec(`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, imageID, groupID)
		return err
	})
}

// RemoveAltMember dissolves a group it would leave with one member.
func (s *Service) RemoveAltMember(imageID int64) error {
	return s.inWriteTx(func(tx *sql.Tx) error { return removeMemberTx(tx, altGroupMerge, imageID) })
}

func (s *Service) DissolveAltGroup(groupID int64) error {
	_, err := s.exec(`DELETE FROM alt_groups WHERE id = ?`, groupID)
	return err
}

// MergeAltGroups keeps the lowest group id.
func (s *Service) MergeAltGroups(groupIDs []int64) error {
	return s.mergeGroups("alt_group_members", "alternate", groupIDs, mergeAltGroupsTx)
}

// MergeDupGroups keeps the lowest group id and takes the original from
// keepOriginalFrom's group, or keeps the survivor's when it is 0.
func (s *Service) MergeDupGroups(groupIDs []int64, keepOriginalFrom int64) error {
	return s.mergeGroups("dup_group_members", "duplicate", groupIDs, func(tx *sql.Tx, ids []int64) error {
		return mergeDupGroupsTx(tx, ids, keepOriginalFrom)
	})
}

func (s *Service) mergeGroups(membersTbl, label string, groupIDs []int64, merge func(*sql.Tx, []int64) error) error {
	groupIDs = dedupAndSortInt64(groupIDs)
	if len(groupIDs) <= 1 {
		return nil
	}
	return s.inWriteTx(func(tx *sql.Tx) error {
		if err := groupsCrossConflictTx(tx, membersTbl, groupIDs, label); err != nil {
			return err
		}
		if err := merge(tx, groupIDs); err != nil {
			return err
		}
		return pruneQueueInGroupTx(tx, membersTbl, groupIDs[0])
	})
}

func dedupAndSortInt64(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// groupIDs must be deduplicated and ascending; the first survives.
func mergeAltGroupsTx(tx *sql.Tx, groupIDs []int64) error {
	return moveMembersTx(tx, "alt_group_members", "alt_groups", groupIDs[0], groupIDs[1:])
}

func moveMembersTx(tx *sql.Tx, membersTbl, groupTbl string, survivor int64, others []int64) error {
	for _, gid := range others {
		if _, err := tx.Exec(
			`UPDATE `+membersTbl+` SET group_id = ? WHERE group_id = ?`, survivor, gid,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM `+groupTbl+` WHERE id = ?`, gid); err != nil {
			return err
		}
	}
	return nil
}

// groupIDs must be deduplicated and ascending; the first survives.
func mergeDupGroupsTx(tx *sql.Tx, groupIDs []int64, keepOriginalFrom int64) error {
	survivor := groupIDs[0]
	others := groupIDs[1:]
	if keepOriginalFrom != 0 && keepOriginalFrom != survivor {
		if slices.Contains(others, keepOriginalFrom) {
			original, err := GroupOriginal(tx, keepOriginalFrom)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE dup_groups SET original_image_id = ? WHERE id = ?`, original, survivor,
			); err != nil {
				return err
			}
		}
	}
	return moveMembersTx(tx, "dup_group_members", "dup_groups", survivor, others)
}

// RemoveVersionEdge matches either orientation, so a post with the sides
// swapped still drops the edge.
func (s *Service) RemoveVersionEdge(a, b int64) error {
	_, err := s.exec(`DELETE FROM version_edges WHERE `+eitherWay("parent_image_id", "child_image_id"), a, b, b, a)
	return err
}

// ReverseVersionEdge refuses a mid-chain reversal with ErrVersionExists
// rather than a raw constraint error.
func (s *Service) ReverseVersionEdge(parent, child int64) error {
	return s.reverseEdge(versionEdge, "version", parent, child)
}

// RemoveDerivativeEdge matches either orientation, so a post with the
// sides swapped still drops the edge.
func (s *Service) RemoveDerivativeEdge(a, b int64) error {
	_, err := s.exec(`DELETE FROM derivative_edges WHERE `+eitherWay("source_image_id", "derivative_image_id"), a, b, b, a)
	return err
}

func (s *Service) DissolveVersionChain(anyMember int64) error {
	return s.dissolveEdges(anyMember, collectVersionChainMembersTx, "version_edges", "parent_image_id", "child_image_id")
}

func (s *Service) DissolveDerivativeTree(anyMember int64) error {
	component := func(tx *sql.Tx, id int64) ([]int64, error) { return DerivativeComponent(tx, id) }
	return s.dissolveEdges(anyMember, component, "derivative_edges", "source_image_id", "derivative_image_id")
}

func (s *Service) dissolveEdges(
	anyMember int64,
	collect func(*sql.Tx, int64) ([]int64, error),
	table, parentCol, childCol string,
) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		members, err := collect(tx, anyMember)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		return deleteEdgesByEndpointsTx(tx, table, parentCol, childCol, members)
	})
}

func collectVersionChainMembersTx(tx *sql.Tx, anyMember int64) ([]int64, error) {
	var has int
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM version_edges WHERE parent_image_id = ? OR child_image_id = ?)`,
		anyMember, anyMember,
	).Scan(&has); err != nil {
		return nil, err
	}
	if has == 0 {
		return nil, nil
	}
	root, err := ChainRoot(tx, "version_edges", "parent_image_id", "child_image_id", anyMember)
	if err != nil {
		return nil, err
	}
	// parent_image_id is UNIQUE, so ChainPath can walk down as well.
	below, err := ChainPath(tx, "version_edges", "child_image_id", "parent_image_id", root)
	if err != nil {
		return nil, err
	}
	return append([]int64{root}, below...), nil
}

func deleteEdgesByEndpointsTx(tx *sql.Tx, table, colA, colB string, ids []int64) error {
	return db.Chunked(ids, 500, func(chunk []int64) error {
		placeholders, idArgs := db.InPlaceholders(chunk)
		args := append(append([]any{}, idArgs...), idArgs...)
		q := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s) OR %s IN (%s)`, table, colA, placeholders, colB, placeholders)
		_, err := tx.Exec(q, args...)
		return err
	})
}

// ReverseDerivativeEdge refuses what adding the reversed edge would, such as
// ErrDerivativeCycle when source still reaches derivative through another path.
func (s *Service) ReverseDerivativeEdge(source, derivative int64) error {
	return s.reverseEdge(derivativeEdge, "derivative", source, derivative)
}

// reverseEdge runs the reversed edge through every check a fresh add
// takes; a missing edge is a no-op.
func (s *Service) reverseEdge(spec edgeSpec, kind string, parent, child int64) error {
	return s.pairTx(parent, child, func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM `+spec.table+` WHERE `+spec.parentCol+` = ? AND `+spec.childCol+` = ?`, parent, child,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return addEdgeTx(tx, spec, kind, child, parent)
	})
}

// RemoveNotRelated drops both orientations: a restored document can carry
// the row either way round.
func (s *Service) RemoveNotRelated(a, b int64) error {
	_, err := s.exec(`DELETE FROM not_related_pairs WHERE `+eitherWay("a_image_id", "b_image_id"), a, b, b, a)
	return err
}

// QueueForReview files the pair as SourceReview so the card claims no
// phash match; a row already queued keeps its distance.
func (s *Service) QueueForReview(a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := s.db.Write.Exec(
		`INSERT OR IGNORE INTO potential_relation_pairs (a_image_id, b_image_id, distance, created_at, source)
		 VALUES (?, ?, 0, ?, ?)`,
		lo, hi, nowISO(), SourceReview,
	)
	return err
}

func (s *Service) ResetSkipped(ctx context.Context) (int64, error) {
	res, err := s.db.Write.ExecContext(ctx,
		`UPDATE potential_relation_pairs SET skipped_at = NULL WHERE skipped_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

var overwriteRelationTx = map[string]func(tx *sql.Tx, a, b int64) error{
	"duplicate": func(tx *sql.Tx, a, b int64) error {
		return addGroupRelationTx(tx, a, b, "duplicate", dupGroupMerge)
	},
	"alternate": func(tx *sql.Tx, a, b int64) error {
		return addGroupRelationTx(tx, a, b, "alternate", altGroupMerge)
	},
	"version": func(tx *sql.Tx, a, b int64) error {
		// Only a's child edge and b's parent edge block the insert; the
		// rest of both chains stays.
		if _, err := tx.Exec(
			`DELETE FROM version_edges WHERE child_image_id = ? OR parent_image_id = ?`, b, a,
		); err != nil {
			return err
		}
		return addEdgeTx(tx, versionEdge, "version", a, b)
	},
	"derivative": func(tx *sql.Tx, a, b int64) error {
		return addEdgeTx(tx, derivativeEdge, "derivative", a, b)
	},
	"not_related": addNotRelatedTx,
}

var relationAdders = map[string]func(s *Service, a, b int64) error{
	"duplicate":   (*Service).AddDuplicate,
	"alternate":   (*Service).AddAlternate,
	"version":     (*Service).AddVersionEdge,
	"derivative":  (*Service).AddDerivativeEdge,
	"not_related": (*Service).AddNotRelated,
}

func (s *Service) Add(kind string, a, b int64) error {
	add, ok := relationAdders[kind]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownKind, kind)
	}
	return add(s, a, b)
}

// Overwrite replaces whatever relates a and b with kind in one
// transaction, so a refused add leaves the old relation in place.
func (s *Service) Overwrite(kind string, a, b int64) error {
	add, ok := overwriteRelationTx[kind]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownKind, kind)
	}
	return s.pairTx(a, b, func(tx *sql.Tx) error {
		if err := clearBetweenTx(tx, a, b); err != nil {
			return err
		}
		return add(tx, a, b)
	})
}

// b leaves a group the two share; the rest of the group stays.
func clearBetweenTx(tx *sql.Tx, a, b int64) error {
	for _, cfg := range []groupMerge{dupGroupMerge, altGroupMerge} {
		if share, err := pairShareGroupTx(tx, cfg.membersTbl, a, b); err != nil {
			return err
		} else if share {
			if err := removeMemberTx(tx, cfg, b); err != nil {
				return err
			}
		}
	}
	for _, e := range []edgeSpec{versionEdge, derivativeEdge} {
		if _, err := tx.Exec(
			`DELETE FROM `+e.table+` WHERE `+eitherWay(e.childCol, e.parentCol), a, b, b, a,
		); err != nil {
			return err
		}
	}
	lo, hi := canonicalPair(a, b)
	_, err := tx.Exec(`DELETE FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi)
	return err
}

// Ratings are left out: they are highest-wins, so a copy would raise the
// original's. Implied rows come back through the fan-out of their tags.
const tagsToCopyFrom = `
	FROM image_tags it
	JOIN dup_group_members m ON m.image_id = it.image_id
	JOIN dup_groups g ON g.id = m.group_id
	LEFT JOIN tags t ON t.id = it.tag_id
	WHERE m.image_id != g.original_image_id AND it.is_implied = 0
	  AND (? = 0 OR t.category_id != ?)
	  AND (NOT EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source = '` + models.TagSourceMonbooru + `')
	       OR EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source != '` + models.TagSourceMonbooru + `'))
	  AND NOT EXISTS (SELECT 1 FROM image_tags o WHERE o.image_id = g.original_image_id AND o.tag_id = it.tag_id)`

// TagsToCopy is what CopyTagsFromDuplicatesToOriginal would add.
func (s *Service) TagsToCopy(groupID int64) ([]int64, error) {
	rc := s.tags.RatingCategoryID()
	return db.QueryIDs(s.db.Read, `SELECT DISTINCT it.tag_id`+tagsToCopyFrom+` AND g.id = ?`, rc, rc, groupID)
}

func (s *Service) GroupsWithTagsToCopy(groupIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(groupIDs) == 0 {
		return out, nil
	}
	rc := s.tags.RatingCategoryID()
	placeholders, args := db.InPlaceholders(groupIDs)
	err := db.QueryIDsFunc(s.db.Read, func(id int64) bool { out[id] = true; return true },
		`SELECT DISTINCT g.id`+tagsToCopyFrom+` AND g.id IN (`+placeholders+`)`, append([]any{rc, rc}, args...)...)
	return out, err
}

func (s *Service) CopyTagsFromDuplicatesToOriginal(groupID int64) (int, error) {
	var added int
	err := s.inWriteTx(func(tx *sql.Tx) error {
		original, err := GroupOriginal(tx, groupID)
		if err != nil {
			return err
		}
		rc := s.tags.RatingCategoryID()
		ids, err := db.QueryIDs(tx, `SELECT DISTINCT it.tag_id`+tagsToCopyFrom+` AND g.id = ?`, rc, rc, groupID)
		if err != nil || len(ids) == 0 {
			return err
		}
		// As the operator's own add: a user row and ledger claim, with the fan-out.
		added, _, _, err = s.tags.BatchAddTagsTx(tx, []int64{original}, ids)
		return err
	})
	return added, err
}

// OnImagesDeleteTx must run in the delete's transaction before the rows go,
// as dup_groups.original_image_id has no CASCADE. It decides each group once,
// against the chunk's survivors, or it could promote a member being deleted.
func (s *Service) OnImagesDeleteTx(tx *sql.Tx, imageIDs []int64) error {
	if len(imageIDs) == 0 {
		return nil
	}
	for _, cfg := range []groupMerge{dupGroupMerge, altGroupMerge} {
		if err := groupsOnBatchDeleteTx(tx, cfg.membersTbl, cfg.groupTbl, imageIDs, cfg.promoteOriginal); err != nil {
			return err
		}
	}
	return nil
}

func groupsOnBatchDeleteTx(tx *sql.Tx, memberTbl, groupTbl string, imageIDs []int64, promoteOriginal bool) error {
	placeholders, args := db.InPlaceholders(imageIDs)
	groupIDs, err := db.QueryIDs(tx,
		`SELECT DISTINCT group_id FROM `+memberTbl+` WHERE image_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return err
	}
	for _, gid := range groupIDs {
		gidArgs := append([]any{gid}, args...)
		var survivors int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM `+memberTbl+` WHERE group_id = ? AND image_id NOT IN (`+placeholders+`)`,
			gidArgs...,
		).Scan(&survivors); err != nil {
			return err
		}
		if survivors < 2 {
			if _, err := tx.Exec(`DELETE FROM `+groupTbl+` WHERE id = ?`, gid); err != nil {
				return err
			}
			continue
		}
		if !promoteOriginal {
			continue
		}
		var original int64
		if err := tx.QueryRow(`SELECT original_image_id FROM `+groupTbl+` WHERE id = ?`, gid).Scan(&original); err != nil {
			return err
		}
		if !slices.Contains(imageIDs, original) {
			continue
		}
		var newOriginal int64
		if err := tx.QueryRow(fmt.Sprintf(nextDupOriginalQuery, placeholders), gidArgs...).Scan(&newOriginal); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE `+groupTbl+` SET original_image_id = ? WHERE id = ?`, newOriginal, gid); err != nil {
			return err
		}
	}
	return nil
}

func lookupGroupIDTx(tx *sql.Tx, table string, imageID int64) (sql.NullInt64, error) {
	return optionalID(tx, fmt.Sprintf(`SELECT group_id FROM %s WHERE image_id = ?`, table), imageID)
}

func pairHasOtherRelationTx(tx *sql.Tx, a, b int64, ignore string) (bool, error) {
	for _, p := range pairProbes {
		if p.kind == ignore {
			continue
		}
		ok, err := p.probe(tx, a, b)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// The cheap group joins go first; the order does not change the answer.
var pairProbes = []struct {
	kind  string
	probe func(*sql.Tx, int64, int64) (bool, error)
}{
	{"duplicate", func(tx *sql.Tx, a, b int64) (bool, error) {
		return pairShareGroupTx(tx, "dup_group_members", a, b)
	}},
	{"alternate", func(tx *sql.Tx, a, b int64) (bool, error) {
		return pairShareGroupTx(tx, "alt_group_members", a, b)
	}},
	{"version", func(tx *sql.Tx, a, b int64) (bool, error) {
		return pairEdgeExistsTx(tx, "version_edges", "child_image_id", "parent_image_id", a, b)
	}},
	{"derivative", func(tx *sql.Tx, a, b int64) (bool, error) {
		return pairEdgeExistsTx(tx, "derivative_edges", "derivative_image_id", "source_image_id", a, b)
	}},
	{"not_related", func(tx *sql.Tx, a, b int64) (bool, error) {
		lo, hi := canonicalPair(a, b)
		var n int
		err := tx.QueryRow(
			`SELECT COUNT(*) FROM not_related_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi,
		).Scan(&n)
		return n > 0, err
	}},
}

// table and the column names are constants, never input.
func pairEdgeExistsTx(tx *sql.Tx, table, colA, colB string, a, b int64) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+eitherWay(colA, colB), a, b, b, a).Scan(&n)
	return n > 0, err
}

func pairChainRelatedTx(tx *sql.Tx, a, b int64, ignore string) (bool, error) {
	if ignore != "version" {
		ok, err := chainRelatesTx(tx, "version_edges", "parent_image_id", "child_image_id", a, b)
		if err != nil || ok {
			return ok, err
		}
	}
	if ignore == "derivative" {
		return false, nil
	}
	return chainRelatesTx(tx, "derivative_edges", "source_image_id", "derivative_image_id", a, b)
}

// A link through a third image has no edge between the two to overwrite,
// hence ErrIndirectRelation.
func pairConflictTx(tx *sql.Tx, a, b int64, label string) error {
	if conflict, err := pairHasOtherRelationTx(tx, a, b, label); err != nil {
		return err
	} else if conflict {
		return ErrRelationConflict
	}
	if chained, err := pairChainRelatedTx(tx, a, b, label); err != nil {
		return err
	} else if chained {
		return ErrIndirectRelation
	}
	return nil
}

// Relating every image of left to every image of right as label must not
// give a pair another kind of relation too.
func crossConflictTx(tx *sql.Tx, left, right []int64, label string) error {
	err := db.Chunked(left, 500, func(l []int64) error {
		return db.Chunked(right, 500, func(r []int64) error {
			return crossProbesTx(tx, l, r, label)
		})
	})
	if err != nil {
		return err
	}
	// A chain relates its members through other images too, direct edges
	// included.
	for _, c := range []struct{ kind, table, parentCol, childCol string }{
		{"version", "version_edges", "parent_image_id", "child_image_id"},
		{"derivative", "derivative_edges", "source_image_id", "derivative_image_id"},
	} {
		if c.kind == label {
			continue
		}
		for _, x := range left {
			above, _, err := chainSpan(tx, c.table, c.parentCol, c.childCol, x)
			if err != nil {
				return err
			}
			below, _, err := chainSpan(tx, c.table, c.childCol, c.parentCol, x)
			if err != nil {
				return err
			}
			for _, y := range slices.Concat(above[1:], below[1:]) {
				if slices.Contains(right, y) {
					return &CrossConflictError{A: x, B: y}
				}
			}
		}
	}
	return nil
}

func crossProbesTx(tx *sql.Tx, left, right []int64, label string) error {
	lIn, lArgs := db.InPlaceholders(left)
	rIn, rArgs := db.InPlaceholders(right)
	sharedGroup := func(table string) string {
		return `SELECT m1.image_id, m2.image_id FROM ` + table + ` m1
		        JOIN ` + table + ` m2 ON m1.group_id = m2.group_id
		        WHERE m1.image_id IN (` + lIn + `) AND m2.image_id IN (` + rIn + `)`
	}
	probes := []struct {
		kind, query string
		args        []any
	}{
		{"duplicate", sharedGroup("dup_group_members"), slices.Concat(lArgs, rArgs)},
		{"alternate", sharedGroup("alt_group_members"), slices.Concat(lArgs, rArgs)},
		{"not_related", `SELECT a_image_id, b_image_id FROM not_related_pairs
		                 WHERE (a_image_id IN (` + lIn + `) AND b_image_id IN (` + rIn + `))
		                    OR (a_image_id IN (` + rIn + `) AND b_image_id IN (` + lIn + `))`,
			slices.Concat(lArgs, rArgs, rArgs, lArgs)},
	}
	for _, p := range probes {
		if p.kind == label {
			continue
		}
		var x, y int64
		switch err := tx.QueryRow(p.query+` LIMIT 1`, p.args...).Scan(&x, &y); {
		case err == nil:
			return &CrossConflictError{A: x, B: y}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	return nil
}

func groupsCrossConflictTx(tx *sql.Tx, table string, groupIDs []int64, label string) error {
	sides := make([][]int64, len(groupIDs))
	for i, gid := range groupIDs {
		members, err := db.QueryIDs(tx, `SELECT image_id FROM `+table+` WHERE group_id = ?`, gid)
		if err != nil {
			return err
		}
		sides[i] = members
	}
	for i := range sides {
		for j := i + 1; j < len(sides); j++ {
			if err := crossConflictTx(tx, sides[i], sides[j], label); err != nil {
				return err
			}
		}
	}
	return nil
}

func groupSideTx(tx *sql.Tx, table string, imageID int64) ([]int64, error) {
	gid, err := lookupGroupIDTx(tx, table, imageID)
	if err != nil || !gid.Valid {
		return []int64{imageID}, err
	}
	return db.QueryIDs(tx, `SELECT image_id FROM `+table+` WHERE group_id = ?`, gid.Int64)
}

func pairSettledTx(tx *sql.Tx, a, b int64) (bool, error) {
	if got, err := pairHasOtherRelationTx(tx, a, b, ""); err != nil || got {
		return got, err
	}
	return pairChainRelatedTx(tx, a, b, "")
}

// A merge can settle queued pairs beyond a and b, so every pair inside
// anchor's group goes.
func pruneQueueForGroupTx(tx *sql.Tx, table string, anchor int64) error {
	gid, err := lookupGroupIDTx(tx, table, anchor)
	if err != nil || !gid.Valid {
		return err
	}
	return pruneQueueInGroupTx(tx, table, gid.Int64)
}

func pruneQueueInGroupTx(tx *sql.Tx, table string, gid int64) error {
	q := fmt.Sprintf(`
		DELETE FROM potential_relation_pairs
		WHERE a_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)
		  AND b_image_id IN (SELECT image_id FROM %s WHERE group_id = ?)`, table, table)
	_, err := tx.Exec(q, gid, gid)
	return err
}

// The edge relates every ancestor of parent to every descendant of child,
// so all those pairs go.
func pruneQueueForChainTx(tx *sql.Tx, table, parentCol, childCol string, parent, child int64) error {
	above, _, err := chainSpan(tx, table, parentCol, childCol, parent)
	if err != nil {
		return err
	}
	below, _, err := chainSpan(tx, table, childCol, parentCol, child)
	if err != nil {
		return err
	}
	return db.Chunked(above, 500, func(a []int64) error {
		return db.Chunked(below, 500, func(b []int64) error {
			aIn, aArgs := db.InPlaceholders(a)
			bIn, bArgs := db.InPlaceholders(b)
			q := fmt.Sprintf(`
				DELETE FROM potential_relation_pairs
				WHERE (a_image_id IN (%s) AND b_image_id IN (%s))
				   OR (a_image_id IN (%s) AND b_image_id IN (%s))`, aIn, bIn, bIn, aIn)
			_, err := tx.Exec(q, slices.Concat(aArgs, bArgs, bArgs, aArgs)...)
			return err
		})
	})
}

func pruneQueuePairTx(tx *sql.Tx, a, b int64) error {
	lo, hi := canonicalPair(a, b)
	_, err := tx.Exec(
		`DELETE FROM potential_relation_pairs WHERE a_image_id = ? AND b_image_id = ?`, lo, hi,
	)
	return err
}

func pairShareGroupTx(tx *sql.Tx, table string, a, b int64) (bool, error) {
	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM %s m1
		JOIN %s m2 ON m1.group_id = m2.group_id
		WHERE m1.image_id = ? AND m2.image_id = ?`, table, table)
	var n int
	err := tx.QueryRow(q, a, b).Scan(&n)
	return n > 0, err
}

type groupMerge struct {
	membersTbl, groupTbl string
	promoteOriginal      bool
	insertGroup          func(tx *sql.Tx, original int64) (int64, error)
	mergeGroups          func(tx *sql.Tx, ids []int64) error
}

var dupGroupMerge = groupMerge{
	membersTbl: "dup_group_members", groupTbl: "dup_groups", promoteOriginal: true,
	insertGroup: func(tx *sql.Tx, original int64) (int64, error) {
		var gid int64
		err := tx.QueryRow(
			`INSERT INTO dup_groups (original_image_id, created_at) VALUES (?, ?) RETURNING id`,
			original, nowISO(),
		).Scan(&gid)
		return gid, err
	},
	mergeGroups: func(tx *sql.Tx, ids []int64) error { return mergeDupGroupsTx(tx, ids, 0) },
}

var altGroupMerge = groupMerge{
	membersTbl: "alt_group_members", groupTbl: "alt_groups",
	insertGroup: func(tx *sql.Tx, _ int64) (int64, error) {
		var gid int64
		err := tx.QueryRow(`INSERT INTO alt_groups (created_at) VALUES (?) RETURNING id`, nowISO()).Scan(&gid)
		return gid, err
	},
	mergeGroups: mergeAltGroupsTx,
}

func mergeIntoGroupTx(tx *sql.Tx, a, b int64, cfg groupMerge) error {
	groupA, err := lookupGroupIDTx(tx, cfg.membersTbl, a)
	if err != nil {
		return err
	}
	groupB, err := lookupGroupIDTx(tx, cfg.membersTbl, b)
	if err != nil {
		return err
	}
	addMember := func(gid, imageID int64) error {
		_, err := tx.Exec(
			`INSERT INTO `+cfg.membersTbl+` (image_id, group_id, created_at) VALUES (?, ?, ?)`,
			imageID, gid, nowISO(),
		)
		return err
	}
	switch {
	case !groupA.Valid && !groupB.Valid:
		gid, err := cfg.insertGroup(tx, a)
		if err != nil {
			return err
		}
		now := nowISO()
		if _, err := tx.Exec(
			`INSERT INTO `+cfg.membersTbl+` (image_id, group_id, created_at) VALUES (?, ?, ?), (?, ?, ?)`,
			a, gid, now, b, gid, now,
		); err != nil {
			return err
		}
	case groupA.Valid && !groupB.Valid:
		return addMember(groupA.Int64, b)
	case !groupA.Valid && groupB.Valid:
		return addMember(groupB.Int64, a)
	case groupA.Int64 == groupB.Int64:
		// Already in the same group.
	default:
		return cfg.mergeGroups(tx, []int64{min(groupA.Int64, groupB.Int64), max(groupA.Int64, groupB.Int64)})
	}
	return nil
}
