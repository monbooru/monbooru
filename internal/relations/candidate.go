package relations

import (
	"database/sql"
	"errors"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

// DuplicateCandidateDetails inspects what a duplicate relationship between
// originalCandidate and dupCandidate would resolve to before saving:
// which image will be the group's effective original, and whether any
// non-original member across both sides holds unique tags or sources to copy.
func (s *Service) DuplicateCandidateDetails(originalCandidate, dupCandidate int64) (effectiveOriginal int64, hasUniqueTags bool, hasUniqueSources bool, err error) {
	var origGroupID, origGroupOriginal sql.NullInt64
	err = s.db.Read.QueryRow(`
		SELECT m.group_id, g.original_image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id = ?`, originalCandidate,
	).Scan(&origGroupID, &origGroupOriginal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, err
	}

	var dupGroupID, dupGroupOriginal sql.NullInt64
	err = s.db.Read.QueryRow(`
		SELECT m.group_id, g.original_image_id
		FROM dup_group_members m
		JOIN dup_groups g ON g.id = m.group_id
		WHERE m.image_id = ?`, dupCandidate,
	).Scan(&dupGroupID, &dupGroupOriginal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, err
	}

	// Resolve the effective original image matching AddDuplicateOf logic:
	effectiveOriginal = originalCandidate
	switch {
	case origGroupID.Valid && !dupGroupID.Valid:
		effectiveOriginal = origGroupOriginal.Int64
	case !origGroupID.Valid && !dupGroupID.Valid:
		effectiveOriginal = originalCandidate
	case !origGroupID.Valid && dupGroupID.Valid:
		if dupGroupOriginal.Int64 == dupCandidate {
			effectiveOriginal = originalCandidate
		} else {
			effectiveOriginal = dupGroupOriginal.Int64
		}
	case origGroupID.Valid && dupGroupID.Valid:
		if origGroupID.Int64 == dupGroupID.Int64 {
			effectiveOriginal = origGroupOriginal.Int64
		} else {
			survivor := min(origGroupID.Int64, dupGroupID.Int64)
			survivorOriginal := origGroupOriginal.Int64
			if survivor == dupGroupID.Int64 {
				survivorOriginal = dupGroupOriginal.Int64
			}
			if survivorOriginal == dupCandidate {
				effectiveOriginal = originalCandidate
			} else {
				effectiveOriginal = survivorOriginal
			}
		}
	}

	var allMembers []int64
	if origGroupID.Valid && dupGroupID.Valid && origGroupID.Int64 == dupGroupID.Int64 {
		allMembers, err = db.QueryIDs(s.db.Read, `SELECT image_id FROM dup_group_members WHERE group_id = ?`, origGroupID.Int64)
	} else {
		allMembers, err = db.QueryIDs(s.db.Read, `
			SELECT image_id FROM dup_group_members WHERE group_id IN (?, ?)
			UNION SELECT ?
			UNION SELECT ?`,
			origGroupID.Int64, dupGroupID.Int64, originalCandidate, dupCandidate,
		)
	}
	if err != nil {
		return 0, false, false, err
	}

	var dupMemberIDs []int64
	for _, id := range allMembers {
		if id != effectiveOriginal {
			dupMemberIDs = append(dupMemberIDs, id)
		}
	}
	if len(dupMemberIDs) == 0 {
		return effectiveOriginal, false, false, nil
	}

	rc := s.tags.RatingCategoryID()
	placeholders, args := db.InPlaceholders(dupMemberIDs)

	var hasTagsInt int
	tagQuery := `SELECT 1 FROM image_tags it
		LEFT JOIN tags t ON t.id = it.tag_id
		WHERE it.image_id IN (` + placeholders + `)
		  AND it.is_implied = 0
		  AND (? = 0 OR t.category_id != ?)
		  AND (NOT EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source = '` + models.TagSourceMonbooru + `')
		       OR EXISTS (SELECT 1 FROM image_tag_sources s WHERE s.image_id = it.image_id AND s.tag_id = it.tag_id AND s.source != '` + models.TagSourceMonbooru + `'))
		  AND NOT EXISTS (SELECT 1 FROM image_tags o WHERE o.image_id = ? AND o.tag_id = it.tag_id)
		LIMIT 1`
	tagArgs := append(slices.Clone(args), rc, rc, effectiveOriginal)
	err = s.db.Read.QueryRow(tagQuery, tagArgs...).Scan(&hasTagsInt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, err
	}
	hasTags := err == nil && hasTagsInt == 1

	var hasSourcesInt int
	sourceQuery := `SELECT 1 FROM image_sources s
		WHERE s.image_id IN (` + placeholders + `)
		  AND lower(s.site) != '` + contract.SourcePTR + `'
		  AND NOT EXISTS (SELECT 1 FROM image_sources o WHERE o.image_id = ? AND o.site = s.site AND o.post_id = s.post_id)
		  AND (s.url = '' OR NOT EXISTS (SELECT 1 FROM image_sources o WHERE o.image_id = ? AND o.url = s.url))
		LIMIT 1`
	sourceArgs := append(slices.Clone(args), effectiveOriginal, effectiveOriginal)
	err = s.db.Read.QueryRow(sourceQuery, sourceArgs...).Scan(&hasSourcesInt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, err
	}
	hasSources := err == nil && hasSourcesInt == 1

	return effectiveOriginal, hasTags, hasSources, nil
}
