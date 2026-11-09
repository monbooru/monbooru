-- Runs on every boot, before Bootstrap's migrations: each statement must
-- be idempotent, and none may reference a column a migration adds.

CREATE TABLE IF NOT EXISTS tag_categories (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    color      TEXT    NOT NULL DEFAULT '#888888',
    is_builtin INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO tag_categories (name, color, is_builtin) VALUES
    ('general',   '#3d90e3', 1),
    ('character', '#00aa00', 1),
    ('artist',    '#cc0000', 1),
    ('copyright', '#aa00aa', 1),
    ('meta',      '#ffaa00', 1),
    ('rating',    '#996666', 1),
    ('medium',    '#7d4fbf', 1),
    ('person',    '#b85c9e', 1),
    ('year',      '#4a8fa8', 1),
    ('species',   '#ed5d1f', 1);

-- A library may hold these as custom categories from before they were built in.
UPDATE tag_categories SET is_builtin = 1 WHERE name IN ('medium', 'person', 'year', 'species');

CREATE TABLE IF NOT EXISTS tags (
    id               INTEGER PRIMARY KEY,
    name             TEXT    NOT NULL,
    category_id      INTEGER NOT NULL REFERENCES tag_categories(id),
    usage_count      INTEGER NOT NULL DEFAULT 0,
    is_alias         INTEGER NOT NULL DEFAULT 0,
    canonical_tag_id INTEGER REFERENCES tags(id),
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- 'user', a booru site, 'ptr', a tagger or an import; set once at insert.
    origin           TEXT    NOT NULL DEFAULT '',
    -- NULL = never applied to an image.
    last_used_at     TEXT,
    -- 1 = PTR alias the latest refresh dropped; kept until the operator acts.
    stale            INTEGER NOT NULL DEFAULT 0,
    -- 1 = a name mapped to nothing: every add path drops it.
    rejected         INTEGER NOT NULL DEFAULT 0,
    rejected_drops   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(name, category_id)
);

-- Fixed: rating_rank and the tag service key on these four names.
INSERT OR IGNORE INTO tags (name, category_id) VALUES
    ('general',      (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('sensitive',    (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('questionable', (SELECT id FROM tag_categories WHERE name = 'rating')),
    ('explicit',     (SELECT id FROM tag_categories WHERE name = 'rating'));

CREATE TABLE IF NOT EXISTS images (
    id             INTEGER PRIMARY KEY,
    sha256         TEXT    NOT NULL UNIQUE,
    -- '' until computed. Never a dedup key, never copied from a source's claim.
    md5            TEXT    NOT NULL DEFAULT '',
    canonical_path TEXT    NOT NULL,
    folder_path    TEXT    NOT NULL DEFAULT '',
    file_type      TEXT    NOT NULL,
    width          INTEGER,
    height         INTEGER,
    file_size      INTEGER NOT NULL,
    is_missing     INTEGER NOT NULL DEFAULT 0,
    is_favorited   INTEGER NOT NULL DEFAULT 0,
    -- 1 = in the inbox awaiting triage, 0 = archived.
    is_inbox       INTEGER NOT NULL DEFAULT 1,
    auto_tagged_at TEXT,
    source_type    TEXT    NOT NULL DEFAULT 'none',
    origin         TEXT    NOT NULL DEFAULT 'ingest',
    source         TEXT    NOT NULL DEFAULT '',
    url            TEXT    NOT NULL DEFAULT '',
    note           TEXT    NOT NULL DEFAULT '',
    -- Operator-set URL of the artist's first post (not image_sources.original).
    original_source TEXT   NOT NULL DEFAULT '',
    -- NULL = unknown (not a video, or not probed), never read as zero.
    duration_seconds REAL,
    -- Mirror-canonical DCT pHash; NULL = not yet computed or nothing to hash.
    phash          INTEGER,
    ingested_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- UnixNano token shared by one web-UI upload's rows; NULL otherwise.
    upload_batch   INTEGER
);

CREATE TABLE IF NOT EXISTS image_paths (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL UNIQUE,
    is_canonical INTEGER NOT NULL DEFAULT 0,
    -- Unix seconds and full Unix nanoseconds; 0 = not recorded.
    mtime_unix   INTEGER NOT NULL DEFAULT 0,
    mtime_nsec   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS image_tags (
    image_id    INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id      INTEGER NOT NULL REFERENCES tags(id)   ON DELETE CASCADE,
    is_auto     INTEGER NOT NULL DEFAULT 0,
    is_implied  INTEGER NOT NULL DEFAULT 0,
    confidence  REAL,
    tagger_name TEXT,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- 1 = the attributed source's latest fetch no longer carried this tag.
    stale       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (image_id, tag_id)
);

-- images.series / series_order mirror one "home" membership per image.
CREATE TABLE IF NOT EXISTS image_collections (
    image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    name     TEXT    NOT NULL COLLATE NOCASE,
    position INTEGER,
    PRIMARY KEY (image_id, name)
);

-- Opt-ins: other collections' internal pairs stay hidden from find-relations.
CREATE TABLE IF NOT EXISTS collection_find_relations (
    name TEXT PRIMARY KEY COLLATE NOCASE
);

-- images.source / url mirror each image's first origin.
CREATE TABLE IF NOT EXISTS image_sources (
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    url        TEXT    NOT NULL DEFAULT '',
    md5        TEXT    NOT NULL DEFAULT '', -- the source's last claimed md5, as sent; never a dedup key
    commentary TEXT    NOT NULL DEFAULT '', -- operator-editable, but a re-pull overwrites it
    commentary_translated TEXT NOT NULL DEFAULT '',
    original   TEXT    NOT NULL DEFAULT '', -- the post's declared upstream source(s), newline-joined
    similarity REAL    NOT NULL DEFAULT 0, -- 0-100 best lookup match score; 0 = exact or manual
    md5_match  TEXT    NOT NULL DEFAULT '', -- claim vs file: '' unknown, 'match' or 'differ'
    parent_url TEXT    NOT NULL DEFAULT '', -- the declared parent post, in url's canonical form
    upgrade_kept INTEGER NOT NULL DEFAULT 0, -- 1 = operator kept the local file; a new claimed md5 clears it
    post_width  INTEGER NOT NULL DEFAULT 0, -- post_*: as the post declares, 0 / '' if unpublished; never measured
    post_height INTEGER NOT NULL DEFAULT 0,
    post_size   INTEGER NOT NULL DEFAULT 0,
    post_ext    TEXT    NOT NULL DEFAULT '',
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (image_id, site, post_id)
);

-- Danbooru-style notes: x/y/w/h in original-image pixels.
CREATE TABLE IF NOT EXISTS image_annotations (
    id         INTEGER PRIMARY KEY,
    image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    site       TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    post_id    TEXT    NOT NULL DEFAULT '',
    x          INTEGER NOT NULL,
    y          INTEGER NOT NULL,
    w          INTEGER NOT NULL,
    h          INTEGER NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    -- 1 = operator-drawn; source-keyed deletes and replaces must spare it.
    manual     INTEGER NOT NULL DEFAULT 0,
    fetched_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_image_annotations_image ON image_annotations(image_id);

CREATE TABLE IF NOT EXISTS tag_implications (
    parent_tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    implied_tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Same values as tags.origin, set when the edge is created.
    origin         TEXT    NOT NULL DEFAULT '',
    -- 1 = a PTR edge the latest refresh no longer carries.
    stale          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (parent_tag_id, implied_tag_id)
);

CREATE TABLE IF NOT EXISTS tag_notes (
    tag_id INTEGER PRIMARY KEY REFERENCES tags(id) ON DELETE CASCADE,
    body   TEXT    NOT NULL DEFAULT '',
    -- One link per line; a leading '-' marks a dead one.
    links  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sd_metadata (
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

CREATE TABLE IF NOT EXISTS comfyui_metadata (
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

-- Workflow search terms: node classes, titles and scalar inputs as name=value.
-- The FK targets comfyui_metadata so replacing that row clears its terms.
CREATE TABLE IF NOT EXISTS comfyui_terms (
    image_id INTEGER NOT NULL REFERENCES comfyui_metadata(image_id) ON DELETE CASCADE,
    term     TEXT    NOT NULL,
    PRIMARY KEY (image_id, term)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS manga_metadata (
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

-- No ON DELETE on original_image_id: deleting that image must first pick
-- a new original or dissolve the group.
CREATE TABLE IF NOT EXISTS dup_groups (
    id                INTEGER PRIMARY KEY,
    original_image_id INTEGER NOT NULL REFERENCES images(id),
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS dup_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES dup_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_groups (
    id         INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS alt_group_members (
    image_id   INTEGER PRIMARY KEY REFERENCES images(id)     ON DELETE CASCADE,
    group_id   INTEGER NOT NULL    REFERENCES alt_groups(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- A strict chain: one parent and one child per image; a branch is a derivative.
CREATE TABLE IF NOT EXISTS version_edges (
    child_image_id  INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
    parent_image_id INTEGER NOT NULL UNIQUE REFERENCES images(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Keyed on the pair: a composite derives from several sources.
CREATE TABLE IF NOT EXISTS derivative_edges (
    derivative_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    source_image_id     INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (derivative_image_id, source_image_id)
);

-- Canonicalised a < b.
CREATE TABLE IF NOT EXISTS not_related_pairs (
    a_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    b_image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (a_image_id, b_image_id)
);

CREATE TABLE IF NOT EXISTS relation_session (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    order_mode TEXT NOT NULL DEFAULT 'smallest_distance_first',
    detector   TEXT NOT NULL DEFAULT 'both',
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    paused_at  TEXT
);

-- find-pairs candidates, a < b; skipped_at sends a row to the back.
-- Bootstrap's triggers store collection_hidden (the pair shares a
-- non-opted-in collection) and max_rating_rank (the higher member rank).
CREATE TABLE IF NOT EXISTS potential_relation_pairs (
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

CREATE TABLE IF NOT EXISTS saved_searches (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    query      TEXT NOT NULL,
    sort       TEXT NOT NULL DEFAULT '',
    sort_order TEXT NOT NULL DEFAULT '',
    seed       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Canonicalised a < b. Two same-name tags the operator ruled are
-- different things, so the pair stops counting as a category conflict.
CREATE TABLE IF NOT EXISTS tag_distinct_pairs (
    a_tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    b_tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (a_tag_id, b_tag_id)
);

-- old_id (a pre-widening fold) -> new_id (the richer spelling that
-- superseded it); ambiguous = 1 when old_id has several candidates.
CREATE TABLE IF NOT EXISTS folded_tag_pairs (
    old_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    new_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    category_id INTEGER NOT NULL,
    ambiguous   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (old_id, new_id)
);

-- Created at first enqueue. queued_at set = in flight (job_id:
-- monloader's id); attempts = consecutive misses; next_due_at NULL =
-- nothing scheduled; ptr_cursor = monloader's index position at the miss.
CREATE TABLE IF NOT EXISTS image_lookups (
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

CREATE INDEX IF NOT EXISTS idx_tags_name         ON tags(name);
CREATE INDEX IF NOT EXISTS idx_tags_category     ON tags(category_id);
CREATE INDEX IF NOT EXISTS idx_tags_usage        ON tags(usage_count DESC);
CREATE INDEX IF NOT EXISTS idx_tags_active_usage ON tags(usage_count DESC, name) WHERE is_alias = 0;
CREATE INDEX IF NOT EXISTS idx_tags_alias_canonical ON tags(canonical_tag_id, name) WHERE is_alias = 1;
-- Not a duplicate of the alias index: every tag delete's self-FK check seeks it.
CREATE INDEX IF NOT EXISTS idx_tags_canonical ON tags(canonical_tag_id) WHERE canonical_tag_id IS NOT NULL;
-- image_id makes tag seeks covering, and tag_id = ? AND image_id >= ? a range.
CREATE INDEX IF NOT EXISTS idx_image_tags_tag_image ON image_tags(tag_id, image_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_image  ON image_tags(image_id);
CREATE INDEX IF NOT EXISTS idx_image_collections_name ON image_collections(name, image_id, position);
CREATE INDEX IF NOT EXISTS idx_image_sources_site ON image_sources(site, image_id);
CREATE INDEX IF NOT EXISTS idx_image_sources_url ON image_sources(url) WHERE url != '';
CREATE INDEX IF NOT EXISTS idx_tag_implications_implied ON tag_implications(implied_tag_id);
CREATE INDEX IF NOT EXISTS idx_image_tags_user_tag ON image_tags(tag_id) WHERE is_auto = 0;
CREATE INDEX IF NOT EXISTS idx_image_tags_auto_tagger ON image_tags(tagger_name)
    WHERE is_auto = 1 AND tagger_name IS NOT NULL AND tagger_name != '';
CREATE INDEX IF NOT EXISTS idx_images_sha256     ON images(sha256);
CREATE INDEX IF NOT EXISTS idx_images_ingested   ON images(ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_favorited  ON images(is_favorited);
CREATE INDEX IF NOT EXISTS idx_images_source_type ON images(source_type);
CREATE INDEX IF NOT EXISTS idx_images_missing    ON images(is_missing);
CREATE INDEX IF NOT EXISTS idx_images_folder     ON images(folder_path);
CREATE INDEX IF NOT EXISTS idx_images_folder_visible ON images(folder_path) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_filesize_visible ON images(file_size DESC, id DESC) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_ingested_visible ON images(ingested_at DESC, id DESC) WHERE is_missing = 0;
-- Visible-only, so mime:, type: and ai: don't fall back on idx_images_missing.
CREATE INDEX IF NOT EXISTS idx_images_file_type_visible   ON images(file_type)   WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_images_source_type_visible ON images(source_type) WHERE is_missing = 0;
CREATE INDEX IF NOT EXISTS idx_image_paths_image ON image_paths(image_id);
-- Aliases are a small slice of image_paths; alias walks seek here, not scan.
CREATE INDEX IF NOT EXISTS idx_image_paths_aliases ON image_paths(image_id) WHERE is_canonical = 0;
CREATE INDEX IF NOT EXISTS idx_sd_metadata_genhash      ON sd_metadata(generation_hash)      WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_genhash ON comfyui_metadata(generation_hash) WHERE generation_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sd_metadata_seed         ON sd_metadata(seed)                 WHERE seed IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comfyui_metadata_seed    ON comfyui_metadata(seed)            WHERE seed IS NOT NULL;
-- NOCASE so a lowercase comfyui: query seeks the stored CamelCase term.
CREATE INDEX IF NOT EXISTS idx_comfyui_terms_term ON comfyui_terms(term COLLATE NOCASE, image_id);
CREATE INDEX IF NOT EXISTS idx_dup_group_members_group ON dup_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_alt_group_members_group ON alt_group_members(group_id, image_id);
CREATE INDEX IF NOT EXISTS idx_derivative_edges_source ON derivative_edges(source_image_id);
CREATE INDEX IF NOT EXISTS idx_not_related_b           ON not_related_pairs(b_image_id, a_image_id);
CREATE INDEX IF NOT EXISTS idx_tag_distinct_pairs_b    ON tag_distinct_pairs(b_tag_id, a_tag_id);
CREATE INDEX IF NOT EXISTS idx_potential_pairs_distance ON potential_relation_pairs(skipped_at, distance, a_image_id);
-- b-side seek for the resweep triggers; the a side rides the primary key.
CREATE INDEX IF NOT EXISTS idx_potential_pairs_b        ON potential_relation_pairs(b_image_id);
CREATE INDEX IF NOT EXISTS idx_image_lookups_due        ON image_lookups(backend, next_due_at);
-- Keeps the reconcile sweep proportional to in-flight rows, not the table.
CREATE INDEX IF NOT EXISTS idx_image_lookups_inflight   ON image_lookups(queued_at) WHERE queued_at IS NOT NULL;
