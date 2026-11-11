package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "embed"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

// Bootstrap runs every step on every boot, so each must be idempotent.
// Indexes and triggers on migrated columns, and tables with a backfill,
// go here: schema.sql runs before the ALTERs and the freshness probes.
func Bootstrap(db *DB) error {
	b := &bootstrapper{db: db}
	// Each one-shot is pinned to the marker that introduced it, or the one
	// it was gated under, so a library past it skips the step.
	marker := b.queryInt("read user_version", `PRAGMA user_version`)
	existing := b.queryInt("probe images table", `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'images'`) > 0
	if existing && marker < MinUpgradeUserVersion && b.err == nil {
		return fmt.Errorf("%w: its schema marker is %d and this release opens %d or later; open it once with monbooru %s, which upgrades it, then with this release",
			ErrLibraryTooOld, marker, MinUpgradeUserVersion, UpgradeVia)
	}
	if existing {
		b.exec("bootstrapping schema", schemaSQL)
	} else {
		// A fresh library left at marker 0 by a later failure would be refused as too old.
		b.execTx("bootstrapping schema", schemaSQL+fmt.Sprintf(";\nPRAGMA user_version = %d", MinUpgradeUserVersion))
	}
	b.exec("create idx_images_inbox_visible", `CREATE INDEX IF NOT EXISTS idx_images_inbox_visible ON images(is_inbox) WHERE is_missing = 0`)
	if marker < 15 {
		// images.source is the primary origin's mirror, read by id only:
		// the source: filter runs on image_sources.
		b.exec("drop idx_images_source", `DROP INDEX IF EXISTS idx_images_source`)
		b.exec("drop idx_images_source_visible", `DROP INDEX IF EXISTS idx_images_source_visible`)
		b.exec("drop idx_images_source_nocase_visible", `DROP INDEX IF EXISTS idx_images_source_nocase_visible`)
	}
	if marker < 16 {
		b.exec("null infinite sd cfg_scale", `UPDATE sd_metadata SET cfg_scale = NULL WHERE cfg_scale IN (9e999, -9e999)`)
		b.exec("null infinite comfyui cfg_scale", `UPDATE comfyui_metadata SET cfg_scale = NULL WHERE cfg_scale IN (9e999, -9e999)`)
		b.exec("null infinite community_rating", `UPDATE manga_metadata SET community_rating = NULL WHERE community_rating IN (9e999, -9e999)`)
	}
	b.ensureColumn("image_sources", "commentary_translated", `ALTER TABLE image_sources ADD COLUMN commentary_translated TEXT NOT NULL DEFAULT ''`)
	b.exec("create idx_image_sources_parent_url", `CREATE INDEX IF NOT EXISTS idx_image_sources_parent_url ON image_sources(parent_url) WHERE parent_url != ''`)
	// Must match upgrade.CandidateWhere, or the planner stops using it.
	b.exec("create idx_image_sources_upgradable", `CREATE INDEX IF NOT EXISTS idx_image_sources_upgradable ON image_sources(image_id)
		WHERE url <> '' AND upgrade_kept = 0 AND (md5_match = 'differ' OR (similarity > 0 AND md5_match = ''))`)
	b.ensureColumn("images", "page_count", `ALTER TABLE images ADD COLUMN page_count INTEGER`)
	b.ensureColumn("images", "last_read_page", `ALTER TABLE images ADD COLUMN last_read_page INTEGER`)
	b.exec("create idx_images_duration_visible", `CREATE INDEX IF NOT EXISTS idx_images_duration_visible ON images(duration_seconds) WHERE is_missing = 0 AND duration_seconds IS NOT NULL`)
	// Not UNIQUE: a crafted md5 collision would abort an ingest.
	b.exec("create idx_images_md5", `CREATE INDEX IF NOT EXISTS idx_images_md5 ON images(md5) WHERE md5 != ''`)
	// Triggers, not the write sites: many paths write either digest.
	// Neither side rules while a digest is empty, so a fetch's verdict
	// stands. Only the claim is lowercased: images.md5 already is.
	b.exec("create trg_image_sources_verdict_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_sources_verdict_ai
		AFTER INSERT ON image_sources
		WHEN NEW.md5 != '' AND (SELECT md5 FROM images WHERE id = NEW.image_id) != ''
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(NEW.md5) = (SELECT md5 FROM images WHERE id = NEW.image_id) THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id;
		END`)
	// A new claim lapses the keep ruling even when no verdict can be written.
	b.exec("create trg_image_sources_verdict_au", `CREATE TRIGGER IF NOT EXISTS trg_image_sources_verdict_au
		AFTER UPDATE OF md5 ON image_sources
		WHEN NEW.md5 != ''
		BEGIN
			UPDATE image_sources SET upgrade_kept = 0
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id
			   AND lower(NEW.md5) != lower(OLD.md5);
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(NEW.md5) = (SELECT md5 FROM images WHERE id = NEW.image_id) THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id
			   AND (SELECT md5 FROM images WHERE id = NEW.image_id) != '';
		END`)
	// Only a moved digest re-derives: the lazy fill rewrites the same
	// value on every hit, and must not undo a refused refetch's verdict.
	b.exec("create trg_images_verdict_au", `CREATE TRIGGER IF NOT EXISTS trg_images_verdict_au
		AFTER UPDATE OF md5 ON images
		WHEN NEW.md5 != '' AND NEW.md5 IS NOT OLD.md5
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(md5) = NEW.md5 THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.id AND md5 != '';
		END`)
	// One row per http(s) URL an image's sources carry, so sourceurl:
	// seeks a prefix instead of scanning the newline-joined originals.
	b.backfillIfFreshTable("image_source_urls",
		`CREATE TABLE IF NOT EXISTS image_source_urls (
			key      TEXT    NOT NULL COLLATE NOCASE,
			image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			PRIMARY KEY (key, image_id)
		) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO image_source_urls (image_id, key) `+sourceURLKeys("image_sources"),
		"backfill image_source_urls")
	b.exec("create idx_image_source_urls_image", `CREATE INDEX IF NOT EXISTS idx_image_source_urls_image ON image_source_urls(image_id)`)
	// Triggers, not the write sites: url and original are written by many
	// paths. A new row only adds its keys; a change or a removal rebuilds
	// the image's, as another of its sources may share a key. One copy of
	// sourceURLKeys each, as every write to image_sources compiles them. An
	// upsert that rewrites the same url fires the update, hence its WHEN.
	addRow := `INSERT OR IGNORE INTO image_source_urls (image_id, key) ` + sourceURLKeys("(SELECT NEW.image_id AS image_id, NEW.url AS url, NEW.original AS original)")
	rebuildImage := `DELETE FROM image_source_urls WHERE image_id = OLD.image_id;
		INSERT OR IGNORE INTO image_source_urls (image_id, key) ` + sourceURLKeys("(SELECT image_id, url, original FROM image_sources WHERE image_id = OLD.image_id)")
	for _, t := range []struct{ name, event, body string }{
		{"trg_image_sources_urls_ai", "AFTER INSERT ON image_sources", addRow},
		{"trg_image_sources_urls_au", "AFTER UPDATE OF url, original ON image_sources WHEN NEW.url IS NOT OLD.url OR NEW.original IS NOT OLD.original", rebuildImage},
		{"trg_image_sources_urls_ad", "AFTER DELETE ON image_sources", rebuildImage},
	} {
		b.exec("create "+t.name, `CREATE TRIGGER IF NOT EXISTS `+t.name+` `+t.event+`
		BEGIN
			`+t.body+`;
		END`)
	}
	// VIRTUAL: ALTER TABLE cannot add a STORED column, and the index stores it.
	b.ensureColumn("images", "basename_lower",
		`ALTER TABLE images ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(canonical_path))) VIRTUAL`)
	b.exec("create idx_images_basename_lower_visible", `CREATE INDEX IF NOT EXISTS idx_images_basename_lower_visible ON images(basename_lower) WHERE is_missing = 0 AND basename_lower != ''`)
	b.ensureColumn("image_paths", "basename_lower",
		`ALTER TABLE image_paths ADD COLUMN basename_lower TEXT GENERATED ALWAYS AS (lower(basename(path))) VIRTUAL`)
	b.ensureColumn("images", "series", `ALTER TABLE images ADD COLUMN series TEXT NOT NULL DEFAULT ''`)
	// NULL = no position; sorts after the numbered members.
	b.ensureColumn("images", "series_order", `ALTER TABLE images ADD COLUMN series_order INTEGER`)
	b.exec("create idx_images_series", `CREATE INDEX IF NOT EXISTS idx_images_series ON images(series) WHERE series != ''`)
	b.exec("create idx_images_series_nocase", `CREATE INDEX IF NOT EXISTS idx_images_series_nocase ON images(series COLLATE NOCASE)`)
	// folder: compares NOCASE, which the BINARY idx_images_folder_visible
	// can't serve; without this the planner walks idx_images_missing and
	// temp-sorts.
	b.exec("create idx_images_folder_nocase_visible", `CREATE INDEX IF NOT EXISTS idx_images_folder_nocase_visible ON images(folder_path COLLATE NOCASE) WHERE is_missing = 0`)
	b.widenDerivativeEdgesKey()
	// Bulk deletes before the relations hook left groups under two
	// members, and an import replays whatever the export carried.
	b.exec("dissolve degenerate dup groups",
		`DELETE FROM dup_groups WHERE (SELECT COUNT(*) FROM dup_group_members m WHERE m.group_id = dup_groups.id) < 2`)
	b.exec("dissolve degenerate alt groups",
		`DELETE FROM alt_groups WHERE (SELECT COUNT(*) FROM alt_group_members m WHERE m.group_id = alt_groups.id) < 2`)
	// 0 = opted out of that backend's scheduled lookup. The PTR flag is
	// seeded from the single flag it split off.
	b.ensureColumn("images", "scheduled_lookup", `ALTER TABLE images ADD COLUMN scheduled_lookup INTEGER NOT NULL DEFAULT 1`)
	b.backfillIfFreshColumn("images", "scheduled_lookup_ptr",
		`ALTER TABLE images ADD COLUMN scheduled_lookup_ptr INTEGER NOT NULL DEFAULT 1`,
		`UPDATE images SET scheduled_lookup_ptr = scheduled_lookup`,
		"backfill images.scheduled_lookup_ptr")
	b.exec("create idx_images_phash", `CREATE INDEX IF NOT EXISTS idx_images_phash ON images(phash) WHERE phash IS NOT NULL`)
	// Stored so tagcount: seeks an index instead of counting per visible row.
	b.backfillIfFreshColumn("images", "tag_count",
		`ALTER TABLE images ADD COLUMN tag_count INTEGER NOT NULL DEFAULT 0`,
		`UPDATE images SET tag_count = (SELECT COUNT(*) FROM image_tags WHERE image_id = images.id)`,
		"backfill images.tag_count")
	b.exec("create idx_images_tag_count_visible", `CREATE INDEX IF NOT EXISTS idx_images_tag_count_visible ON images(tag_count) WHERE is_missing = 0`)
	b.exec("create trg_image_tags_count_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ai
		AFTER INSERT ON image_tags
		BEGIN
			UPDATE images SET tag_count = tag_count + 1 WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_count_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_count_ad
		AFTER DELETE ON image_tags
		BEGIN
			UPDATE images SET tag_count = MAX(0, tag_count - 1) WHERE id = OLD.image_id;
		END`)
	// Trigger-maintained so the listing reads one row per label instead
	// of probing each membership's visibility. Rows at 0 stay; readers
	// filter visible_count > 0.
	b.backfillIfFreshTable("collection_counts",
		`CREATE TABLE IF NOT EXISTS collection_counts (
			name          TEXT PRIMARY KEY COLLATE NOCASE,
			visible_count INTEGER NOT NULL DEFAULT 0
		)`,
		collectionCountsFill,
		"backfill collection_counts")
	b.exec("create trg_image_collections_count_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_ai
		AFTER INSERT ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END`)
	b.exec("create trg_image_collections_count_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_ad
		AFTER DELETE ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = OLD.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
		END`)
	b.exec("create trg_image_collections_count_au", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_count_au
		AFTER UPDATE OF name ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END`)
	b.exec("create trg_images_collection_count_hide", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_hide
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing = 0 AND NEW.is_missing != 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END`)
	b.exec("create trg_images_collection_count_show", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_show
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing != 0 AND NEW.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = visible_count + 1
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END`)
	// BEFORE, while the memberships are readable: the cascade then finds
	// the image gone, so the membership trigger can't decrement twice.
	b.exec("create trg_images_collection_count_bd", `CREATE TRIGGER IF NOT EXISTS trg_images_collection_count_bd
		BEFORE DELETE ON images
		WHEN OLD.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = OLD.id);
		END`)
	b.exec("create trg_potential_pairs_hidden_ai", `CREATE TRIGGER IF NOT EXISTS trg_potential_pairs_hidden_ai
		AFTER INSERT ON potential_relation_pairs
		WHEN `+PairHiddenProbe("NEW.a_image_id", "NEW.b_image_id")+`
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = 1
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END`)
	b.exec("create trg_image_collections_pairs_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_ai
		AFTER INSERT ON image_collections
		BEGIN
			`+pairsResweepBody("a_image_id = NEW.image_id", "b_image_id = NEW.image_id")+`
		END`)
	b.exec("create trg_image_collections_pairs_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_ad
		AFTER DELETE ON image_collections
		BEGIN
			`+pairsResweepBody("a_image_id = OLD.image_id", "b_image_id = OLD.image_id")+`
		END`)
	b.exec("create trg_image_collections_pairs_au", `CREATE TRIGGER IF NOT EXISTS trg_image_collections_pairs_au
		AFTER UPDATE OF name ON image_collections
		BEGIN
			`+pairsResweepBody("a_image_id = NEW.image_id", "b_image_id = NEW.image_id")+`
		END`)
	b.exec("create trg_collection_find_relations_pairs_ai", `CREATE TRIGGER IF NOT EXISTS trg_collection_find_relations_pairs_ai
		AFTER INSERT ON collection_find_relations
		BEGIN
			`+pairsResweepBody("a_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name)", "b_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name)")+`
		END`)
	b.exec("create trg_collection_find_relations_pairs_ad", `CREATE TRIGGER IF NOT EXISTS trg_collection_find_relations_pairs_ad
		AFTER DELETE ON collection_find_relations
		BEGIN
			`+pairsResweepBody("a_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name)", "b_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name)")+`
		END`)
	// The position IS NULL key must match the member walks' ORDER BY, so
	// a LIMIT stops early instead of sorting the whole label.
	b.exec("create idx_image_collections_reading", `CREATE INDEX IF NOT EXISTS idx_image_collections_reading
		ON image_collections(name, position IS NULL, position, image_id)`)
	// Stored so the rating ceiling is one indexed range, not a NOT EXISTS
	// per excluded rating. -1 = unrated, not NULL, so rating_rank <= ?
	// passes unrated rows.
	b.backfillIfFreshColumn("images", "rating_rank",
		`ALTER TABLE images ADD COLUMN rating_rank INTEGER NOT NULL DEFAULT -1`,
		`UPDATE images SET rating_rank = `+ratingRankExpr("images.id")+``,
		"backfill images.rating_rank")
	b.backfillIfFreshColumn("tag_categories", "position",
		`ALTER TABLE tag_categories ADD COLUMN position INTEGER NOT NULL DEFAULT 0`,
		ResetCategoryPositions, "backfill tag_categories.position")
	b.ensureColumn("tags", "rejected", `ALTER TABLE tags ADD COLUMN rejected INTEGER NOT NULL DEFAULT 0`)
	b.ensureColumn("tags", "rejected_drops", `ALTER TABLE tags ADD COLUMN rejected_drops INTEGER NOT NULL DEFAULT 0`)
	// The ComfyTermsVersion its terms were derived under; 0 = never indexed.
	b.ensureColumn("comfyui_metadata", "terms_version",
		`ALTER TABLE comfyui_metadata ADD COLUMN terms_version INTEGER NOT NULL DEFAULT 0`)
	b.exec("create idx_comfyui_metadata_terms_version",
		`CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_terms_version ON comfyui_metadata(terms_version)`)
	b.exec("create idx_image_tags_stale_tag",
		`CREATE INDEX IF NOT EXISTS idx_image_tags_stale_tag ON image_tags(tag_id, image_id) WHERE stale = 1`)
	b.exec("create idx_image_tags_stale_image",
		`CREATE INDEX IF NOT EXISTS idx_image_tags_stale_image ON image_tags(image_id) WHERE stale = 1`)
	// Keeps the /tags sidebar's two whole-catalog scans inside an index.
	b.exec("create idx_tags_active_name",
		`CREATE INDEX IF NOT EXISTS idx_tags_active_name ON tags(name) WHERE is_alias = 0`)
	b.exec("create idx_tags_origin",
		`CREATE INDEX IF NOT EXISTS idx_tags_origin ON tags(origin)`)
	// Without it the created_at sort temp-sorts the catalog. last_used_at
	// gets none: every tag application rewrites it.
	b.exec("create idx_tags_active_created",
		`CREATE INDEX IF NOT EXISTS idx_tags_active_created ON tags(created_at DESC) WHERE is_alias = 0`)
	// One row per source that confirmed a tag, where image_tags keeps
	// only the first. Implied rows get none: their provenance is the
	// implication edge.
	b.backfillIfFreshTable("image_tag_sources",
		`CREATE TABLE IF NOT EXISTS image_tag_sources (
			image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			tag_id     INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
			source     TEXT    NOT NULL,
			created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			PRIMARY KEY (image_id, tag_id, source)
		)`,
		`INSERT OR IGNORE INTO image_tag_sources (image_id, tag_id, source, created_at)
		 SELECT it.image_id, it.tag_id, COALESCE(NULLIF(it.tagger_name, ''), 'user'), it.created_at
		 FROM image_tags it
		 JOIN images i ON i.id = it.image_id
		 JOIN tags t ON t.id = it.tag_id
		 WHERE it.is_implied = 0`,
		"backfill image_tag_sources from tagger_name")
	// The PK leads on image_id, so reads keyed by source would scan the table.
	b.exec("create idx_image_tag_sources_source",
		`CREATE INDEX IF NOT EXISTS idx_image_tag_sources_source ON image_tag_sources(source, tag_id)`)
	b.exec("create idx_image_tag_sources_tag", "CREATE INDEX IF NOT EXISTS idx_image_tag_sources_tag ON image_tag_sources(tag_id)")
	// A trigger, not per-call cleanup: image_tags rows die through too
	// many paths, FK cascades included.
	b.exec("create trg_image_tags_sources_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_sources_ad
		AFTER DELETE ON image_tags
		BEGIN
			DELETE FROM image_tag_sources WHERE image_id = OLD.image_id AND tag_id = OLD.tag_id;
		END`)
	// Lets fav:true walk favorites in sort order; with the bare
	// idx_images_favorited the planner may pick idx_images_missing and
	// temp-sort.
	b.exec("create idx_images_favorited_visible", `CREATE INDEX IF NOT EXISTS idx_images_favorited_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0 AND is_favorited = 1`)
	// rating_rank trails the sort key so a cursor walk under the rating
	// ceiling filters in the index, without a table lookup per row.
	b.exec("create idx_images_ingested_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_ingested_rating_visible ON images(ingested_at DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	b.exec("create idx_images_filesize_rating_visible", `CREATE INDEX IF NOT EXISTS idx_images_filesize_rating_visible ON images(file_size DESC, id DESC, rating_rank) WHERE is_missing = 0`)
	// The sort indexes above can't seek a bare rating_rank <= ? range.
	b.exec("create idx_images_rating_rank_visible", `CREATE INDEX IF NOT EXISTS idx_images_rating_rank_visible ON images(rating_rank) WHERE is_missing = 0`)
	b.exec("create trg_image_tags_rating_rank_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ai
		AFTER INSERT ON image_tags
		WHEN NEW.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = `+ratingRankExpr("NEW.image_id")+` WHERE id = NEW.image_id;
		END`)
	b.exec("create trg_image_tags_rating_rank_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_tags_rating_rank_ad
		AFTER DELETE ON image_tags
		WHEN OLD.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = `+ratingRankExpr("OLD.image_id")+` WHERE id = OLD.image_id;
		END`)
	b.exec("create trg_potential_pairs_rank_ai", `CREATE TRIGGER IF NOT EXISTS trg_potential_pairs_rank_ai
		AFTER INSERT ON potential_relation_pairs
		BEGIN
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				(SELECT rating_rank FROM images WHERE id = NEW.a_image_id),
				(SELECT rating_rank FROM images WHERE id = NEW.b_image_id))
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END`)
	b.exec("create trg_images_pairs_rank_au", `CREATE TRIGGER IF NOT EXISTS trg_images_pairs_rank_au
		AFTER UPDATE OF rating_rank ON images
		BEGIN
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				NEW.rating_rank,
				(SELECT rating_rank FROM images WHERE id = b_image_id))
			WHERE a_image_id = NEW.id;
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				NEW.rating_rank,
				(SELECT rating_rank FROM images WHERE id = a_image_id))
			WHERE b_image_id = NEW.id;
		END`)
	// Trigram FTS makes name: substrings a seek; a leading-% LIKE can't seek.
	b.exec("create image_basename_canonical_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_canonical_fts USING fts5(basename, tokenize='trigram', content='', contentless_delete=1)`)
	b.exec("create image_basename_alias_fts", `CREATE VIRTUAL TABLE IF NOT EXISTS image_basename_alias_fts USING fts5(basename, image_id UNINDEXED, tokenize='trigram', content='', contentless_delete=1)`)
	b.exec("create trg_image_basename_canonical_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ai
		AFTER INSERT ON images
		WHEN NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_canonical_fts (rowid, basename) VALUES (NEW.id, NEW.basename_lower);
		END`)
	b.exec("create trg_image_basename_canonical_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_au
		AFTER UPDATE OF canonical_path ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_canonical_fts (rowid, basename) SELECT NEW.id, NEW.basename_lower WHERE NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_canonical_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_canonical_fts_ad
		AFTER DELETE ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
		END`)
	b.exec("create trg_image_basename_alias_fts_ai", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ai
		AFTER INSERT ON image_paths
		WHEN NEW.is_canonical = 0 AND NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_alias_fts (rowid, basename, image_id) VALUES (NEW.id, NEW.basename_lower, NEW.image_id);
		END`)
	b.exec("create trg_image_basename_alias_fts_au", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_au
		AFTER UPDATE OF path, is_canonical ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
				SELECT NEW.id, NEW.basename_lower, NEW.image_id
				WHERE NEW.is_canonical = 0 AND NEW.basename_lower != '';
		END`)
	b.exec("create trg_image_basename_alias_fts_ad", `CREATE TRIGGER IF NOT EXISTS trg_image_basename_alias_fts_ad
		AFTER DELETE ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
		END`)
	// ANALYZE runs once per marker bump: over image_tags it can blow the
	// start-up budget on a cold cache, and PRAGMA optimize covers the
	// drift in between. 400 is SQLite's recommended analysis_limit; images
	// reads in full, as a sample makes its boolean indexes look selective.
	if b.queryInt("read user_version", `PRAGMA user_version`) < bootstrapSchemaVersion {
		b.exec("set analysis_limit", `PRAGMA analysis_limit = 400`)
		b.exec("analyze image_tags", `ANALYZE image_tags`)
		b.exec("reset analysis_limit", `PRAGMA analysis_limit = 0`)
		b.exec("analyze images", `ANALYZE images`)
		b.exec("set user_version", fmt.Sprintf(`PRAGMA user_version = %d`, bootstrapSchemaVersion))
	}
	b.exec("pragma optimize", `PRAGMA optimize`)
	return b.err
}

type bootstrapper struct {
	db  *DB
	err error
}

func (b *bootstrapper) queryInt(label, query string, args ...any) int {
	if b.err != nil {
		return 0
	}
	var n int
	if err := b.db.Write.QueryRow(query, args...).Scan(&n); err != nil {
		b.err = fmt.Errorf("%s: %w", label, err)
	}
	return n
}

func (b *bootstrapper) hasColumn(table, column string) bool {
	return b.queryInt(fmt.Sprintf("inspect %s.%s", table, column),
		`SELECT COUNT(*) FROM pragma_table_xinfo(?) WHERE name = ?`, table, column) > 0
}

const slowBootstrapStep = 100 * time.Millisecond

func logSlowStep(label string, started time.Time) {
	if took := time.Since(started); took >= slowBootstrapStep {
		logx.Debugf("bootstrap: %s took %s", label, took.Round(time.Millisecond))
	}
}

func (b *bootstrapper) exec(label, sql string) {
	if b.err != nil {
		return
	}
	defer logSlowStep(label, time.Now())
	if _, err := b.db.Write.Exec(sql); err != nil {
		b.err = fmt.Errorf("%s: %w", label, err)
	}
}

// A rebuild that dies between DROP and RENAME would strand its rows: the
// next boot's schema.sql creates an empty table under the old name.
func (b *bootstrapper) execTx(label, stmts string) {
	if b.err != nil {
		return
	}
	defer logSlowStep(label, time.Now())
	if err := InWriteTx(b.db.Write, func(tx *sql.Tx) error {
		_, err := tx.Exec(stmts)
		return err
	}); err != nil {
		b.err = fmt.Errorf("%s: %w", label, err)
	}
}

const collectionCountsFill = `INSERT INTO collection_counts (name, visible_count)
	SELECT c.name, SUM(EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0))
	FROM image_collections c GROUP BY c.name`

// RederiveTriggerColumns recomputes what the image_tags and
// image_collections insert triggers keep, for rows inserted before the
// images, tags and categories those triggers read.
func RederiveTriggerColumns(e Execer) error {
	for _, stmt := range []string{
		`UPDATE images SET tag_count = (SELECT COUNT(*) FROM image_tags WHERE image_id = images.id),
		                   rating_rank = ` + ratingRankExpr("images.id"),
		`DELETE FROM collection_counts`,
		collectionCountsFill,
	} {
		if _, err := e.Exec(stmt); err != nil {
			return fmt.Errorf("rederive trigger columns: %w", err)
		}
	}
	return nil
}

func ratingRankExpr(imageIDCol string) string {
	return `COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = ` + imageIDCol + ` AND tc.name = 'rating'
			), -1)`
}

// sourceURLKeys selects (image_id, key) for the url and each original line
// of rows, a table or subquery with image_id, url and original; search's
// sourceURLKey is its Go twin. The url is split as the first line. Newlines
// become U+E000 before json_quote, which leaves that character alone, so
// the split never meets an escape.
func sourceURLKeys(rows string) string {
	return `SELECT image_id, CASE
			WHEN u LIKE 'https://www.%' THEN substr(u, 13)
			WHEN u LIKE 'http://www.%' THEN substr(u, 12)
			WHEN u LIKE 'https://%' THEN substr(u, 9)
			ELSE substr(u, 8) END AS key
		FROM (SELECT s.image_id, trim(j.value, ' ' || char(9, 13)) AS u FROM ` + rows + ` s,
			json_each('[' || replace(json_quote(replace(s.url || char(10) || s.original, char(10), char(57344))), char(57344), '","') || ']') j)
		WHERE (u LIKE 'http://%' OR u LIKE 'https://%') AND key != ''`
}

func pairsResweepBody(predA, predB string) string {
	return `UPDATE potential_relation_pairs SET collection_hidden = ` + PairHiddenProbe("a_image_id", "b_image_id") + `
			WHERE ` + predA + `;
			UPDATE potential_relation_pairs SET collection_hidden = ` + PairHiddenProbe("a_image_id", "b_image_id") + `
			WHERE ` + predB + `;`
}

func PairHiddenProbe(aCol, bCol string) string {
	return `EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = ` + bCol + `
		WHERE ca.image_id = ` + aCol + `
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))`
}

func (b *bootstrapper) ensureColumn(table, column, alterSQL string) {
	if b.hasColumn(table, column) {
		return
	}
	b.exec("add column "+table+"."+column, alterSQL)
}

// Older libraries key derivative_edges on the derivative alone. SQLite
// can't widen a primary key in place, and the DROP takes
// idx_derivative_edges_source with it.
func (b *bootstrapper) widenDerivativeEdgesKey() {
	if b.queryInt("inspect derivative_edges key",
		`SELECT COUNT(*) FROM pragma_table_info('derivative_edges') WHERE name = 'source_image_id' AND pk > 0`) > 0 {
		return
	}
	b.execTx("widen derivative_edges key", `
		CREATE TABLE derivative_edges_wide (
		    derivative_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
		    source_image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
		    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		    PRIMARY KEY (derivative_image_id, source_image_id)
		);
		INSERT INTO derivative_edges_wide (derivative_image_id, source_image_id, created_at)
		    SELECT derivative_image_id, source_image_id, created_at FROM derivative_edges;
		DROP TABLE derivative_edges;
		ALTER TABLE derivative_edges_wide RENAME TO derivative_edges;
		CREATE INDEX IF NOT EXISTS idx_derivative_edges_source ON derivative_edges(source_image_id);`)
}

// The DDL and its backfill commit together: a kill between them would
// leave the column or table in place and the backfill never run.
func (b *bootstrapper) backfillIfFreshColumn(table, column, alterSQL, backfillSQL, backfillLabel string) {
	if !b.hasColumn(table, column) {
		b.execTx(backfillLabel, alterSQL+";\n"+backfillSQL)
	}
}

func (b *bootstrapper) backfillIfFreshTable(table, createSQL, backfillSQL, backfillLabel string) {
	if b.queryInt("inspect table "+table,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table) == 0 {
		b.execTx(backfillLabel, createSQL+";\n"+backfillSQL)
	}
}
