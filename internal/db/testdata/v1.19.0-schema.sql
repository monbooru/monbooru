PRAGMA user_version = 13;
CREATE TABLE tag_categories (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    color      TEXT    NOT NULL DEFAULT '#888888',
    is_builtin INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tags (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    category_id      INTEGER NOT NULL REFERENCES tag_categories(id),
    usage_count      INTEGER NOT NULL DEFAULT 0,
    is_alias         INTEGER NOT NULL DEFAULT 0,
    canonical_tag_id INTEGER REFERENCES tags(id),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Creation provenance: 'user', a booru site, 'ptr', an auto-tagger
    -- name, an import label. Stamped once at insert, never overwritten.
    origin           TEXT    NOT NULL DEFAULT '',
    -- Most recent application to an image; NULL = never applied.
    last_used_at     TEXT,
    -- On PTR alias rows: the latest refresh no longer listed this
    -- spelling. The row stays until the operator acts.
    stale            INTEGER NOT NULL DEFAULT 0,
    UNIQUE(name, category_id)
);
CREATE TABLE images (
    id             INTEGER PRIMARY KEY,
    sha256         TEXT    NOT NULL UNIQUE,
    -- Digest of the same bytes sha256 addresses, kept because boorus key
    -- their posts on it. '' until computed; never a dedup key, and never
    -- written from a source's claimed md5 (image_sources.md5), which is
    -- allowed to disagree with the file.
    md5            TEXT    NOT NULL DEFAULT '',
    canonical_path TEXT    NOT NULL,
    folder_path    TEXT    NOT NULL DEFAULT '',
    file_type      TEXT    NOT NULL,
    width          INTEGER,
    height         INTEGER,
    file_size      INTEGER NOT NULL,
    is_missing     INTEGER NOT NULL DEFAULT 0,
    is_favorited   INTEGER NOT NULL DEFAULT 0,
    -- New ingests land in the inbox (1) for triage; archived rows sit at 0.
    -- Matching idx_images_inbox_visible is created in db.go Bootstrap so the
    -- migration's ALTER TABLE on existing libraries runs before the index
    -- references the new column.
    is_inbox       INTEGER NOT NULL DEFAULT 1,
    auto_tagged_at TEXT,
    source_type    TEXT    NOT NULL DEFAULT 'none',
    origin         TEXT    NOT NULL DEFAULT 'ingest',
    source         TEXT    NOT NULL DEFAULT '',
    url            TEXT    NOT NULL DEFAULT '',
    -- Operator's freeform note; never written by an import.
    note           TEXT    NOT NULL DEFAULT '',
    -- Operator's image-level original source (where the artist first posted
    -- it), one URL; distinct from the per-origin image_sources.original a
    -- booru pull fills. Never written by an import.
    original_source TEXT   NOT NULL DEFAULT '',
    -- Video duration in seconds (REAL so short clips and sub-second
    -- precision survive). NULL for non-video rows and for video rows
    -- that pre-date the column or whose ffprobe call failed; the
    -- search and detail surfaces treat NULL as "unknown" rather than
    -- "zero". Backfilled on re-extract metadata.
    duration_seconds REAL,
    -- 64-bit canonical perceptual hash (DCT-based pHash, mirror-
    -- canonicalised). NULL until backfilled or when the file has no
    -- visual surface. Added by ensureColumn on existing libraries;
    -- the matching idx_images_phash is created in db.Bootstrap.
    phash          INTEGER,
    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Upload-batch token (UnixNano) shared by every row from one web-UI
    -- upload POST; NULL for watcher / sync / API rows. Lets the inbox
    -- cluster view group a single drop as one batch regardless of the
    -- 15-minute time-gap rule. Added by ensureColumn on existing libraries.
    upload_batch   INTEGER
, page_count INTEGER, last_read_page INTEGER, basename_lower TEXT GENERATED ALWAYS AS (lower(basename(canonical_path))) VIRTUAL, series TEXT NOT NULL DEFAULT '', series_order INTEGER, scheduled_lookup INTEGER NOT NULL DEFAULT 1, scheduled_lookup_ptr INTEGER NOT NULL DEFAULT 1, tag_count INTEGER NOT NULL DEFAULT 0, rating_rank INTEGER NOT NULL DEFAULT -1);
CREATE TABLE image_paths (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,
    is_canonical INTEGER NOT NULL DEFAULT 0,
    -- File mtime at the time the row was last touched, in Unix seconds and
    -- again as Unix nanoseconds. Sync's unchanged-shortcut requires (size,
    -- mtime) parity so a same-size in-place edit is still re-hashed, and it
    -- reads mtime_nsec when the row has one: whole seconds cannot tell an
    -- edit that landed in the same second the file was last observed. 0
    -- marks rows that predate each column on upgraded libraries - a row
    -- with no nsec keeps the second-grained comparison rather than costing
    -- the library a full re-hash on upgrade.
    mtime_unix   INTEGER NOT NULL DEFAULT 0,
    mtime_nsec   INTEGER NOT NULL DEFAULT 0
, basename_lower TEXT GENERATED ALWAYS AS (lower(basename(path))) VIRTUAL);
CREATE TABLE image_tags (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
    is_auto     INTEGER NOT NULL DEFAULT 0,
    is_implied  INTEGER NOT NULL DEFAULT 0,
    confidence  REAL,
    tagger_name TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- The attributed source's latest fetch no longer carried this tag.
    stale       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (image_id, tag_id)
);
CREATE TABLE image_collections (
    image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    name     TEXT    NOT NULL COLLATE NOCASE,
    position INTEGER,
    PRIMARY KEY (image_id, name)
);
CREATE TABLE collection_find_relations (
    name TEXT PRIMARY KEY COLLATE NOCASE
);
CREATE TABLE image_sources (
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    md5        TEXT    NOT NULL DEFAULT '', -- md5 the source last claimed on a push/enrich; audit trail, never a dedup key
    commentary TEXT    NOT NULL DEFAULT '', -- artist commentary from this source; operator-editable, overwritten by a re-pull
    original   TEXT    NOT NULL DEFAULT '', -- upstream artist source the booru post declared (usually a URL, newline-joined when several); operator-editable, overwritten by a re-pull
    similarity REAL    NOT NULL DEFAULT 0,  -- best similarity-service score (0-100) a lookup matched this origin with; 0 = exact or manual. A matched origin's file differs by design, so refetches skip the md5 verify
    md5_match  TEXT    NOT NULL DEFAULT '', -- claimed-md5 vs local-file verdict: '' unknown, 'match', 'differ'. Maintained by the trg_*_verdict triggers off the two stored digests; gates the [upgrade] action
    parent_url TEXT    NOT NULL DEFAULT '', -- canonical URL of the post this booru post declared as its parent; drives derivative-edge linking once both sides are in the gallery
    upgrade_kept INTEGER NOT NULL DEFAULT 0, -- operator kept the local file; hides the upgrade offer until the post claims a different md5
    post_width  INTEGER NOT NULL DEFAULT 0,  -- what the post says the file it serves is; 0 / '' where the source published nothing. Never measured here, unlike images.width
    post_height INTEGER NOT NULL DEFAULT 0,
    post_size   INTEGER NOT NULL DEFAULT 0,
    post_ext    TEXT    NOT NULL DEFAULT '',
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (image_id, site, post_id)
);
CREATE TABLE image_annotations (
    id         INTEGER PRIMARY KEY,
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    x          INTEGER NOT NULL,
    y          INTEGER NOT NULL,
    w          INTEGER NOT NULL,
    h          INTEGER NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    -- 1 for an operator-drawn box (site/post_id empty), 0 for a source-pulled
    -- one. Source-keyed deletes/replaces gate on manual = 0 so an operator box
    -- survives a source edit, removal or re-pull.
    manual     INTEGER NOT NULL DEFAULT 0,
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX idx_image_annotations_image ON image_annotations(image_id);
CREATE TABLE tag_implications (
    parent_tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    implied_tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Same vocabulary as tags.origin; stamped when the edge is created.
    origin         TEXT    NOT NULL DEFAULT '',
    -- On PTR edges: the latest refresh no longer carried the edge.
    stale          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (parent_tag_id, implied_tag_id)
);
CREATE TABLE sd_metadata (
    image_id        INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt          TEXT,
    negative_prompt TEXT,
    model           TEXT,
    seed            INTEGER,
    sampler         TEXT,
    steps           INTEGER,
    cfg_scale       REAL,
    raw_params      TEXT,
    generation_hash TEXT
);
CREATE TABLE comfyui_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    prompt           TEXT,
    model_checkpoint TEXT,
    seed             INTEGER,
    sampler          TEXT,
    steps            INTEGER,
    cfg_scale        REAL,
    raw_workflow     TEXT,
    generation_hash  TEXT
);
CREATE TABLE manga_metadata (
    image_id         INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    title            TEXT,
    series           TEXT,
    number           TEXT,
    volume           TEXT,
    count            INTEGER,
    summary          TEXT,
    notes            TEXT,
    year             INTEGER,
    month            INTEGER,
    day              INTEGER,
    writer           TEXT,
    penciller        TEXT,
    inker            TEXT,
    colorist         TEXT,
    letterer         TEXT,
    cover_artist     TEXT,
    editor           TEXT,
    publisher        TEXT,
    imprint          TEXT,
    genre            TEXT,
    web              TEXT,
    language_iso     TEXT,
    format           TEXT,
    manga            TEXT,
    age_rating       TEXT,
    community_rating REAL,
    xml_page_count   INTEGER,
    raw_xml          TEXT
);
CREATE TABLE dup_groups (
    id                INTEGER PRIMARY KEY,
    original_image_id INTEGER NOT NULL REFERENCES images(id),
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE dup_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES dup_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE alt_groups (
    id         INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE alt_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES alt_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE version_edges (
    child_image_id  INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    parent_image_id INTEGER NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE derivative_edges (
    derivative_image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    source_image_id     INTEGER NOT NULL    REFERENCES images(id) ON DELETE CASCADE,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE not_related_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (a_image_id, b_image_id)
);
CREATE TABLE relation_session (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    order_mode TEXT NOT NULL DEFAULT 'smallest_distance_first',
    detector   TEXT NOT NULL DEFAULT 'both',
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    paused_at  TEXT
);
CREATE TABLE potential_relation_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    distance   INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    skipped_at TEXT,
    source     TEXT NOT NULL DEFAULT 'phash',
    score      REAL,
    collection_hidden INTEGER NOT NULL DEFAULT 0,
    max_rating_rank   INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY (a_image_id, b_image_id)
);
CREATE TABLE saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    query      TEXT NOT NULL,
    sort       TEXT NOT NULL DEFAULT '',
    sort_order TEXT NOT NULL DEFAULT '',
    seed       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE folded_tag_pairs (
    old_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    new_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    category_id INTEGER NOT NULL,
    ambiguous   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (old_id, new_id)
);
CREATE TABLE image_lookups (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    backend     TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    queued_at   TEXT,
    job_id      INTEGER,
    last_at     TEXT,
    last_result TEXT    NOT NULL DEFAULT '',
    next_due_at TEXT,
    ptr_cursor  INTEGER,
    PRIMARY KEY (image_id, backend)
);
CREATE INDEX idx_tags_name         ON tags(name);
CREATE INDEX idx_tags_category     ON tags(category_id);
CREATE INDEX idx_tags_usage        ON tags(usage_count DESC);
CREATE INDEX idx_tags_active_usage ON tags(usage_count DESC, name) WHERE is_alias = 0;
CREATE INDEX idx_tags_alias_canonical ON tags(canonical_tag_id, name) WHERE is_alias = 1;
CREATE INDEX idx_image_tags_tag_image ON image_tags(tag_id, image_id);
CREATE INDEX idx_image_tags_image  ON image_tags(image_id);
CREATE INDEX idx_image_collections_name ON image_collections(name, image_id, position);
CREATE INDEX idx_image_sources_site ON image_sources(site, image_id);
CREATE INDEX idx_image_sources_url ON image_sources(url) WHERE url != '';
CREATE INDEX idx_tag_implications_implied ON tag_implications(implied_tag_id);
CREATE INDEX idx_image_tags_user_tag ON image_tags(tag_id) WHERE is_auto = 0;
CREATE INDEX idx_image_tags_auto_tagger ON image_tags(tagger_name)
    WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != '';
CREATE INDEX idx_images_sha256     ON images(sha256);
CREATE INDEX idx_images_ingested   ON images(ingested_at DESC);
CREATE INDEX idx_images_favorited  ON images(is_favorited);
CREATE INDEX idx_images_source_type ON images(source_type);
CREATE INDEX idx_images_missing    ON images(is_missing);
CREATE INDEX idx_images_folder     ON images(folder_path);
CREATE INDEX idx_images_folder_visible ON images(folder_path) WHERE is_missing = 0;
CREATE INDEX idx_images_filesize_visible ON images(file_size DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX idx_images_ingested_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX idx_images_file_type_visible   ON images(file_type)   WHERE is_missing = 0;
CREATE INDEX idx_images_source_type_visible ON images(source_type) WHERE is_missing = 0;
CREATE INDEX idx_image_paths_image ON image_paths(image_id);
CREATE INDEX idx_image_paths_aliases ON image_paths(image_id) WHERE is_canonical = 0;
CREATE INDEX idx_sd_metadata_genhash      ON sd_metadata(generation_hash)      WHERE generation_hash IS NOT NULL;
CREATE INDEX idx_comfyui_metadata_genhash ON comfyui_metadata(generation_hash) WHERE generation_hash IS NOT NULL;
CREATE INDEX idx_sd_metadata_seed         ON sd_metadata(seed)                 WHERE seed IS NOT NULL;
CREATE INDEX idx_comfyui_metadata_seed    ON comfyui_metadata(seed)            WHERE seed IS NOT NULL;
CREATE INDEX idx_dup_group_members_group ON dup_group_members(group_id, image_id);
CREATE INDEX idx_alt_group_members_group ON alt_group_members(group_id, image_id);
CREATE INDEX idx_derivative_edges_source ON derivative_edges(source_image_id);
CREATE INDEX idx_not_related_b           ON not_related_pairs(b_image_id, a_image_id);
CREATE INDEX idx_potential_pairs_distance ON potential_relation_pairs(skipped_at, distance, a_image_id);
CREATE INDEX idx_potential_pairs_b        ON potential_relation_pairs(b_image_id);
CREATE INDEX idx_image_lookups_due        ON image_lookups(backend, next_due_at);
CREATE INDEX idx_image_lookups_inflight   ON image_lookups(queued_at) WHERE queued_at IS NOT NULL;
CREATE INDEX idx_images_inbox_visible ON images(is_inbox) WHERE is_missing = 0;
CREATE INDEX idx_images_source ON images(source);
CREATE INDEX idx_images_source_visible ON images(source) WHERE is_missing = 0;
CREATE INDEX idx_images_source_nocase_visible ON images(source COLLATE NOCASE) WHERE is_missing = 0;
CREATE INDEX idx_image_sources_parent_url ON image_sources(parent_url) WHERE parent_url != '';
CREATE INDEX idx_image_sources_upgradable ON image_sources(image_id)
		WHERE url <> '' AND upgrade_kept = 0 AND (md5_match = 'differ' OR (similarity > 0 AND md5_match = ''));
CREATE INDEX idx_images_duration_visible ON images(duration_seconds) WHERE is_missing = 0 AND duration_seconds IS NOT NULL;
CREATE INDEX idx_images_md5 ON images(md5) WHERE md5 != '';
CREATE TRIGGER trg_image_sources_verdict_ai
		AFTER INSERT ON image_sources
		WHEN NEW.md5 != '' AND (SELECT md5 FROM images WHERE id = NEW.image_id) != ''
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(NEW.md5) = (SELECT md5 FROM images WHERE id = NEW.image_id) THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.image_id AND site = NEW.site AND post_id = NEW.post_id;
		END;
CREATE TRIGGER trg_image_sources_verdict_au
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
		END;
CREATE TRIGGER trg_images_verdict_au
		AFTER UPDATE OF md5 ON images
		WHEN NEW.md5 != '' AND NEW.md5 IS NOT OLD.md5
		BEGIN
			UPDATE image_sources SET md5_match = CASE
				WHEN lower(md5) = NEW.md5 THEN 'match' ELSE 'differ' END
			 WHERE image_id = NEW.id AND md5 != '';
		END;
CREATE INDEX idx_images_basename_lower_visible ON images(basename_lower) WHERE is_missing = 0 AND basename_lower != '';
CREATE INDEX idx_images_series ON images(series) WHERE series != '';
CREATE INDEX idx_images_series_nocase ON images(series COLLATE NOCASE);
CREATE INDEX idx_images_folder_nocase_visible ON images(folder_path COLLATE NOCASE) WHERE is_missing = 0;
CREATE INDEX idx_images_phash ON images(phash) WHERE phash IS NOT NULL;
CREATE INDEX idx_images_tag_count_visible ON images(tag_count) WHERE is_missing = 0;
CREATE TRIGGER trg_image_tags_count_ai
		AFTER INSERT ON image_tags
		BEGIN
			UPDATE images SET tag_count = tag_count + 1 WHERE id = NEW.image_id;
		END;
CREATE TRIGGER trg_image_tags_count_ad
		AFTER DELETE ON image_tags
		BEGIN
			UPDATE images SET tag_count = MAX(0, tag_count - 1) WHERE id = OLD.image_id;
		END;
CREATE TABLE collection_counts (
			name          TEXT PRIMARY KEY COLLATE NOCASE,
			visible_count INTEGER NOT NULL DEFAULT 0
		);
CREATE TRIGGER trg_image_collections_count_ai
		AFTER INSERT ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END;
CREATE TRIGGER trg_image_collections_count_ad
		AFTER DELETE ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = OLD.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
		END;
CREATE TRIGGER trg_image_collections_count_au
		AFTER UPDATE OF name ON image_collections
		WHEN EXISTS (SELECT 1 FROM images i WHERE i.id = NEW.image_id AND i.is_missing = 0)
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1) WHERE name = OLD.name;
			INSERT INTO collection_counts (name, visible_count) VALUES (NEW.name, 1)
			ON CONFLICT(name) DO UPDATE SET visible_count = visible_count + 1;
		END;
CREATE TRIGGER trg_images_collection_count_hide
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing = 0 AND NEW.is_missing != 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END;
CREATE TRIGGER trg_images_collection_count_show
		AFTER UPDATE OF is_missing ON images
		WHEN OLD.is_missing != 0 AND NEW.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = visible_count + 1
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = NEW.id);
		END;
CREATE TRIGGER trg_images_collection_count_bd
		BEFORE DELETE ON images
		WHEN OLD.is_missing = 0
		BEGIN
			UPDATE collection_counts SET visible_count = MAX(0, visible_count - 1)
			WHERE name IN (SELECT name FROM image_collections WHERE image_id = OLD.id);
		END;
CREATE TRIGGER trg_potential_pairs_hidden_ai
		AFTER INSERT ON potential_relation_pairs
		WHEN EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = NEW.b_image_id
		WHERE ca.image_id = NEW.a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = 1
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END;
CREATE TRIGGER trg_image_collections_pairs_ai
		AFTER INSERT ON image_collections
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE a_image_id = NEW.image_id;
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE b_image_id = NEW.image_id;
		END;
CREATE TRIGGER trg_image_collections_pairs_ad
		AFTER DELETE ON image_collections
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE a_image_id = OLD.image_id;
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE b_image_id = OLD.image_id;
		END;
CREATE TRIGGER trg_image_collections_pairs_au
		AFTER UPDATE OF name ON image_collections
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE a_image_id = NEW.image_id;
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE b_image_id = NEW.image_id;
		END;
CREATE TRIGGER trg_collection_find_relations_pairs_ai
		AFTER INSERT ON collection_find_relations
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE a_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name);
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE b_image_id IN (SELECT image_id FROM image_collections WHERE name = NEW.name);
		END;
CREATE TRIGGER trg_collection_find_relations_pairs_ad
		AFTER DELETE ON collection_find_relations
		BEGIN
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE a_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name);
			UPDATE potential_relation_pairs SET collection_hidden = EXISTS (
		SELECT 1 FROM image_collections ca
		JOIN image_collections cb ON cb.name = ca.name AND cb.image_id = b_image_id
		WHERE ca.image_id = a_image_id
		  AND NOT EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = ca.name))
			WHERE b_image_id IN (SELECT image_id FROM image_collections WHERE name = OLD.name);
		END;
CREATE INDEX idx_image_collections_reading
		ON image_collections(name, position IS NULL, position, image_id);
CREATE INDEX idx_image_tags_stale_tag ON image_tags(tag_id, image_id) WHERE stale = 1;
CREATE INDEX idx_image_tags_stale_image ON image_tags(image_id) WHERE stale = 1;
CREATE INDEX idx_tags_active_name ON tags(name) WHERE is_alias = 0;
CREATE INDEX idx_tags_origin ON tags(origin);
CREATE INDEX idx_tags_active_created ON tags(created_at DESC) WHERE is_alias = 0;
CREATE TABLE image_tag_sources (
			image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			tag_id     INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
			source     TEXT    NOT NULL,
			created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			PRIMARY KEY (image_id, tag_id, source)
		);
CREATE INDEX idx_image_tag_sources_source ON image_tag_sources(source, tag_id);
CREATE TRIGGER trg_image_tags_sources_ad
		AFTER DELETE ON image_tags
		BEGIN
			DELETE FROM image_tag_sources WHERE image_id = OLD.image_id AND tag_id = OLD.tag_id;
		END;
CREATE INDEX idx_images_favorited_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0 AND is_favorited = 1;
CREATE INDEX idx_images_ingested_rating_visible ON images(ingested_at DESC, id DESC, rating_rank) WHERE is_missing = 0;
CREATE INDEX idx_images_filesize_rating_visible ON images(file_size DESC, id DESC, rating_rank) WHERE is_missing = 0;
CREATE INDEX idx_images_rating_rank_visible ON images(rating_rank) WHERE is_missing = 0;
CREATE TRIGGER trg_image_tags_rating_rank_ai
		AFTER INSERT ON image_tags
		WHEN NEW.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = NEW.image_id AND tc.name = 'rating'
			), -1) WHERE id = NEW.image_id;
		END;
CREATE TRIGGER trg_image_tags_rating_rank_ad
		AFTER DELETE ON image_tags
		WHEN OLD.tag_id IN (SELECT t.id FROM tags t JOIN tag_categories tc ON tc.id = t.category_id WHERE tc.name = 'rating')
		BEGIN
			UPDATE images SET rating_rank = COALESCE((
				SELECT MAX(CASE t.name
					WHEN 'general' THEN 0
					WHEN 'sensitive' THEN 1
					WHEN 'questionable' THEN 2
					WHEN 'explicit' THEN 3
					ELSE -1 END)
				FROM image_tags it
				JOIN tags t ON t.id = it.tag_id
				JOIN tag_categories tc ON tc.id = t.category_id
				WHERE it.image_id = OLD.image_id AND tc.name = 'rating'
			), -1) WHERE id = OLD.image_id;
		END;
CREATE TRIGGER trg_potential_pairs_rank_ai
		AFTER INSERT ON potential_relation_pairs
		BEGIN
			UPDATE potential_relation_pairs SET max_rating_rank = max(
				(SELECT rating_rank FROM images WHERE id = NEW.a_image_id),
				(SELECT rating_rank FROM images WHERE id = NEW.b_image_id))
			WHERE a_image_id = NEW.a_image_id AND b_image_id = NEW.b_image_id;
		END;
CREATE TRIGGER trg_images_pairs_rank_au
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
		END;
CREATE VIRTUAL TABLE image_basename_canonical_fts USING fts5(basename, tokenize='trigram', content='', contentless_delete=1);
CREATE VIRTUAL TABLE image_basename_alias_fts USING fts5(basename, image_id UNINDEXED, tokenize='trigram', content='', contentless_delete=1);
CREATE TRIGGER trg_image_basename_canonical_fts_ai
		AFTER INSERT ON images
		WHEN NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_canonical_fts (rowid, basename) VALUES (NEW.id, NEW.basename_lower);
		END;
CREATE TRIGGER trg_image_basename_canonical_fts_au
		AFTER UPDATE OF canonical_path ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_canonical_fts (rowid, basename) SELECT NEW.id, NEW.basename_lower WHERE NEW.basename_lower != '';
		END;
CREATE TRIGGER trg_image_basename_canonical_fts_ad
		AFTER DELETE ON images
		BEGIN
			DELETE FROM image_basename_canonical_fts WHERE rowid = OLD.id;
		END;
CREATE TRIGGER trg_image_basename_alias_fts_ai
		AFTER INSERT ON image_paths
		WHEN NEW.is_canonical = 0 AND NEW.basename_lower != ''
		BEGIN
			INSERT OR REPLACE INTO image_basename_alias_fts (rowid, basename, image_id) VALUES (NEW.id, NEW.basename_lower, NEW.image_id);
		END;
CREATE TRIGGER trg_image_basename_alias_fts_au
		AFTER UPDATE OF path, is_canonical ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
			INSERT INTO image_basename_alias_fts (rowid, basename, image_id)
				SELECT NEW.id, NEW.basename_lower, NEW.image_id
				WHERE NEW.is_canonical = 0 AND NEW.basename_lower != '';
		END;
CREATE TRIGGER trg_image_basename_alias_fts_ad
		AFTER DELETE ON image_paths
		BEGIN
			DELETE FROM image_basename_alias_fts WHERE rowid = OLD.id;
		END;
