package api

import (
	"cmp"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/peer"
	"github.com/monbooru/monbooru/internal/shared/contract"
)

func buildSpec(baseURL string) map[string]any {
	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Monbooru API",
			"description": "REST API for monbooru image library. Any operation can answer 500 internal_error on a server-side failure, and every authenticated one 503 api_disabled while no API token exists. On a desktop install whose first-run setup is not finished, every /api/v1 path answers 409 setup_required.",
			"version":     strconv.Itoa(peer.APIVersion),
		},
		"servers": []map[string]any{
			{"url": baseURL + "/api/v1", "description": "This server"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "token",
					"description":  "Named, scoped token from Settings -> Authentication. Scopes: read (GET), write (POST/PATCH), delete (DELETE); a token missing the scope gets 403 insufficient_scope.",
				},
			},
			"schemas": map[string]any{
				"Error": objectSchema(nil, map[string]any{
					"error": map[string]any{"type": "string"},
					"code":  map[string]any{"type": "string"},
				}),
				"Tag": objectSchema(nil, map[string]any{
					"name":        map[string]any{"type": "string"},
					"category":    map[string]any{"type": "string"},
					"is_auto":     map[string]any{"type": "boolean"},
					"confidence":  map[string]any{"type": "number", "nullable": true},
					"tagger_name": map[string]any{"type": "string", "nullable": true, "description": "Provenance identifier: auto-tagger subfolder name when is_auto, caller-supplied 'via' (e.g. app name) when manual, null for UI-driven user adds"},
				}),
				"TagRow": objectSchema(nil, map[string]any{
					"id":           map[string]any{"type": "integer"},
					"name":         map[string]any{"type": "string"},
					"category":     map[string]any{"type": "string"},
					"color":        map[string]any{"type": "string"},
					"usage_count":  map[string]any{"type": "integer"},
					"is_alias":     map[string]any{"type": "boolean"},
					"rejected":     map[string]any{"type": "boolean", "description": "True when the name is rejected: every add path drops it until it is un-rejected."},
					"origin":       map[string]any{"type": "string", "description": "Creation provenance label: 'user', a booru site, 'ptr', an auto-tagger name, an import label. Empty on rows predating the column."},
					"last_used_at": map[string]any{"type": "string", "description": "ISO 8601; most recent application to an image. Absent when never applied."},
				}),
				"TagDetail": objectSchema(nil, map[string]any{
					"id":           map[string]any{"type": "integer"},
					"name":         map[string]any{"type": "string"},
					"category":     map[string]any{"type": "string"},
					"color":        map[string]any{"type": "string"},
					"usage_count":  map[string]any{"type": "integer"},
					"is_alias":     map[string]any{"type": "boolean"},
					"rejected":     map[string]any{"type": "boolean", "description": "As on TagRow."},
					"origin":       map[string]any{"type": "string", "description": "Creation provenance label, as on TagRow."},
					"last_used_at": map[string]any{"type": "string", "description": "ISO 8601; absent when never applied."},
					"note":         map[string]any{"type": "string", "description": "The operator's note on the tag, in annotation markup; empty when none. An alias has none."},
					"links":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The tag's links as stored, one entry per line; a leading '-' marks a dead link."},
					"distinct_from": map[string]any{"type": "array", "description": "The same-name tags in other categories the operator marked as different things; they stop counting as a category conflict. Empty when none.", "items": objectSchema(nil, map[string]any{
						"id":       map[string]any{"type": "integer"},
						"category": map[string]any{"type": "string"},
					})},
				}),
				"Implication": objectSchema(nil, map[string]any{
					"parent_id":        map[string]any{"type": "integer"},
					"implied_id":       map[string]any{"type": "integer"},
					"implied_name":     map[string]any{"type": "string"},
					"implied_category": map[string]any{"type": "string"},
				}),
				"Category": objectSchema(nil, map[string]any{
					"id":         map[string]any{"type": "integer"},
					"name":       map[string]any{"type": "string"},
					"color":      map[string]any{"type": "string"},
					"is_builtin": map[string]any{"type": "boolean"},
					"position":   map[string]any{"type": "integer", "description": "Place in the order set on the Categories page, lowest first; list responses come in this order"},
				}),
				"Gallery": objectSchema(nil, map[string]any{
					"name":   map[string]any{"type": "string"},
					"images": map[string]any{"type": "integer", "description": "Visible (non-missing) image count"},
					"tags":   map[string]any{"type": "integer", "description": "Non-alias tag count"},
					"active": map[string]any{"type": "boolean", "description": "True for the gallery used when no ?gallery= selector is given"},
				}),
				"Tagger": objectSchema(nil, map[string]any{
					"name":      map[string]any{"type": "string", "description": "The tagger's folder name under paths.model_path; what tagger_name takes"},
					"enabled":   map[string]any{"type": "boolean", "description": "Turned on in Settings"},
					"available": map[string]any{"type": "boolean", "description": "Its model loads in this build"},
					"applies":   map[string]any{"type": "boolean", "description": "An auto-tag in the selected gallery runs it: enabled, available and in scope for the gallery"},
					"reason":    map[string]any{"type": "string", "description": "Why it is unavailable; absent when it is"},
				}),
				"CurrentJob": objectSchema(nil, map[string]any{
					"running":  map[string]any{"type": "boolean", "description": "The job slot is held: a job runs, or a scheduled run sits between its phases. A job started now would wait or be refused"},
					"finished": map[string]any{"type": "integer", "description": "Jobs finished since the server started. It only grows, so a poller that saw it below this value knows a job ended even after the status bar dropped it"},
					"queued":   map[string]any{"type": "integer", "description": "Images waiting in the auto-tag queue for the slot"},
					"job": objectSchema(nil, map[string]any{
						"type":        map[string]any{"type": "string", "description": "sync, autotag, tag, delete, move, relations, ..."},
						"processed":   map[string]any{"type": "integer"},
						"total":       map[string]any{"type": "integer"},
						"message":     map[string]any{"type": "string", "description": "The progress line"},
						"summary":     map[string]any{"type": "string", "description": "The outcome, once finished"},
						"error":       map[string]any{"type": "string", "description": "Why it failed, once finished"},
						"partial":     map[string]any{"type": "boolean", "description": "Finished with part of the work left undone"},
						"started_at":  map[string]any{"type": "string", "format": "date-time"},
						"finished_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"galleries":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The galleries it writes to; empty stands for all of them"},
					}),
				}),
				"Collection": objectSchema(nil, map[string]any{
					"name":  map[string]any{"type": "string", "description": "The label; images/search?q=collection:\"<name>\"&sort=order pages its members in reading order"},
					"count": map[string]any{"type": "integer", "description": "Members whose file is present"},
				}),
				"PaginatedCollections": paginatedSchema("#/components/schemas/Collection"),
				"ImplicationJob": objectSchema(nil, map[string]any{
					"created": map[string]any{"type": "boolean", "description": "Declare only: false when the edge already existed and is re-applied"},
					"job":     map[string]any{"type": "string", "description": "GET /jobs/current follows the run"},
				}),
				"PaginatedTags":   paginatedSchema("#/components/schemas/TagRow"),
				"PaginatedImages": paginatedSchema("#/components/schemas/Image"),
				"APIInfo": objectSchema(nil, map[string]any{
					"api":     map[string]any{"type": "string"},
					"version": map[string]any{"type": "string", "description": "monbooru version"},
					"docs":    map[string]any{"type": "string"},
					"openapi": map[string]any{"type": "string"},
				}),
				"Health": objectSchema(nil, map[string]any{
					"app":          map[string]any{"type": "string", "description": "Always monbooru, so a second launch can tell its own instance from another program on the port"},
					"status":       map[string]any{"type": "string"},
					"version":      map[string]any{"type": "string", "description": "monbooru version"},
					"api":          map[string]any{"type": "integer", "description": "The API version this monbooru speaks"},
					"min_peer_api": map[string]any{"type": "integer", "description": "The oldest monloader API version it works with"},
				}),
				"PairAccepted": objectSchema(nil, map[string]any{
					"request_id": map[string]any{"type": "string", "description": "The id to poll GET /pair/status with"},
					"status":     map[string]any{"type": "string", "enum": []string{"pending"}},
				}),
				"PairStatus": objectSchema(nil, map[string]any{
					"status": map[string]any{"type": "string", "enum": []string{"pending", "approved", "denied"}},
					"token":  map[string]any{"type": "string", "description": "The issued token, on the first poll after approval only"},
				}),
				"DeleteImageResponse": objectSchema(nil, map[string]any{
					"folder_deleted":     map[string]any{"type": "boolean", "description": "True when delete_empty_folder removed the emptied folder"},
					"folder":             map[string]any{"type": "string", "description": "The removed folder, relative to the gallery root"},
					"kept_changed_files": map[string]any{"type": "integer", "description": "The image's files kept on disk because they no longer hold its bytes"},
				}),
				"AutotagResponse": objectSchema(nil, map[string]any{
					"autotag": map[string]any{"type": "string", "description": "autotag job started, or autotag queued while another job holds the slot"},
					"images":  map[string]any{"type": "integer", "description": "How many images were queued"},
				}),
				"CreateImageResponse": map[string]any{
					"type":        "object",
					"description": "Bare Image on success; wrapped in {image, tag_warnings?, rejected_tags?, autotag?} when a tag was refused, a name is rejected in the gallery or autotag was requested.",
					"properties": map[string]any{
						"image":         map[string]any{"$ref": "#/components/schemas/Image"},
						"tag_warnings":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
						"rejected_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true, "description": "Pushed names the gallery rejects, dropped and counted; the rest landed."},
						"autotag":       map[string]any{"type": "string", "nullable": true, "description": "Human-readable status about the auto-tag job"},
					},
				},
				"DuplicateImageResponse": map[string]any{
					"type":        "object",
					"description": "Returned when the posted file's SHA-256 is already known. The existing image is returned with the pushed tags and provenance merged in. A multipart upload's redundant copy is discarded; a JSON path-reference at a new path is recorded as an alias.",
					"properties": map[string]any{
						"image":         map[string]any{"$ref": "#/components/schemas/Image"},
						"alias_added":   map[string]any{"type": "boolean", "description": "True when the posted path was recorded as an alias of the existing image (JSON path-reference); false when a redundant multipart upload was discarded"},
						"merge":         map[string]any{"$ref": "#/components/schemas/MergeSummary"},
						"tag_warnings":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
						"rejected_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true, "description": "Pushed names the gallery rejects, dropped and counted."},
					},
				},
				"MergeSummary": map[string]any{
					"type":        "object",
					"description": "What a duplicate push or enrich folded into the existing image. The merge reconciles the source's own tag slice to the pushed set; tags owned by the operator, the auto-tagger, or another source are never touched. Tags pushed without a source are only added, and tags_added counts them too.",
					"properties": map[string]any{
						"tags_added":    map[string]any{"type": "integer"},
						"tags_retired":  map[string]any{"type": "integer", "description": "Tags this source contributed earlier and no longer lists, kept but flagged stale"},
						"rating_filled": map[string]any{"type": "boolean", "description": "True when the push supplied a rating and the image had none; an existing rating is never displaced"},
						"source_added":  map[string]any{"type": "boolean"},
						"rejected_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true, "description": "Pushed names the gallery rejects, dropped and counted; absent when none."},
					},
				},
				"ReplaceFileResponse": objectSchema(nil, map[string]any{
					"replaced":     map[string]any{"type": "boolean", "description": "False when the uploaded bytes already matched the stored file; the metadata merge still applied"},
					"merge":        map[string]any{"$ref": "#/components/schemas/MergeSummary"},
					"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
				}),
				"EnrichResponse": objectSchema(nil, map[string]any{
					"merge":        map[string]any{"$ref": "#/components/schemas/MergeSummary"},
					"verified":     map[string]any{"type": "boolean", "description": "False when verify was requested but the source reported no md5, or when a similarity-matched origin's differing file was applied unverified"},
					"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
				}),
				"TagArray": map[string]any{
					"type":  "array",
					"items": map[string]any{"$ref": "#/components/schemas/Tag"},
				},
				"Image": objectSchema(nil, map[string]any{
					"id":                   map[string]any{"type": "integer"},
					"sha256":               map[string]any{"type": "string"},
					"md5":                  map[string]any{"type": "string", "description": "md5 of the stored file, \"\" until computed. The local digest, not the md5 an origin claims"},
					"canonical_path":       map[string]any{"type": "string"},
					"aliases":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"file_type":            map[string]any{"type": "string", "enum": fileTypeNames()},
					"width":                map[string]any{"type": "integer", "nullable": true},
					"height":               map[string]any{"type": "integer", "nullable": true},
					"file_size":            map[string]any{"type": "integer"},
					"is_favorited":         map[string]any{"type": "boolean"},
					"is_inbox":             map[string]any{"type": "boolean", "description": "true = needs triage (sits in the inbox); false = curated. New ingests default to true."},
					"is_missing":           map[string]any{"type": "boolean"},
					"scheduled_lookup":     map[string]any{"type": "boolean", "description": "Whether the nightly online-booru lookup considers this image. Single-image GET only. Settable via PATCH /images/{id}; setting it true also resets an exhausted retry ladder."},
					"scheduled_lookup_ptr": map[string]any{"type": "boolean", "description": "The same opt-in for the nightly PTR lookup, which is free per image and has its own ladder. Single-image GET only, settable the same way."},
					"lookup": map[string]any{
						"type":        "object",
						"description": "Recorded lookup history per backend ('ptr' / 'booru'), absent when nothing has ever been tried. Single-image GET only.",
						"additionalProperties": objectSchema(nil, map[string]any{
							"last_at":     map[string]any{"type": "string", "format": "date-time", "description": "When the last attempt concluded"},
							"last_result": map[string]any{"type": "string", "description": "'hit' or 'miss'; absent while nothing has concluded"},
							"attempts":    map[string]any{"type": "integer", "description": "Consecutive concluded misses"},
							"next_due_at": map[string]any{"type": "string", "format": "date-time", "description": "When the backend may try again; absent when no scheduled run will try it"},
						}),
					},
					"auto_tagged_at":   map[string]any{"type": "string", "format": "date-time", "nullable": true},
					"source_type":      map[string]any{"type": "string", "description": "Generation-tool source: 'a1111', 'comfyui', 'a1111,comfyui', or 'none'. Filtered by the search keyword 'ai:'."},
					"origin":           map[string]any{"type": "string", "description": "How the image got into the gallery: 'ingest' for watcher/sync, 'upload' for the web UI, or any caller-supplied string (app name, URL...) set via POST /images with 'via'"},
					"source":           map[string]any{"type": "string", "description": "Site label of the primary (first) entry of 'sources'. Empty string when the image has no origin. Filtered by the search keyword 'source:' which matches any of the image's sources. Settable on create and via PATCH /images/{id}."},
					"url":              map[string]any{"type": "string", "description": "URL of the primary (first) entry of 'sources'. Empty string when the image has no origin. Must start with http:// or https://. Settable on create and via PATCH /images/{id}."},
					"page_count":       map[string]any{"type": "integer", "nullable": true, "description": "Number of pages for cbz / manga rows; null on every other file type."},
					"collection":       map[string]any{"type": "string", "description": "Home collection label - mirrors the first entry of 'collections'. Empty string when unset. Filtered by the search keyword 'collection:' (exact match). Settable on create and via PATCH /images/{id}."},
					"collection_order": map[string]any{"type": "integer", "nullable": true, "description": "1-based position within the home collection. Null when unset. Settable on create and via PATCH /images/{id}."},
					"collections": map[string]any{
						"type":        "array",
						"description": "Every collection the image belongs to. The scalar collection / collection_order fields mirror the home membership. Managed through the web UI; the API sets the home via collection / collection_order, and a duplicate push files the image into its collection as a further membership.",
						"items": objectSchema(nil, map[string]any{
							"name":  map[string]any{"type": "string"},
							"order": map[string]any{"type": "integer", "nullable": true, "description": "1-based position within this collection. Null when unset."},
						}),
					},
					"sources": map[string]any{
						"type":        "array",
						"description": "Every origin the image came from. The scalar source / url fields mirror the primary (first) origin. PATCH edits the primary through source / url; a push or enrich adds or updates the origin it names.",
						"items": objectSchema(nil, map[string]any{
							"site":                  map[string]any{"type": "string", "description": "Site label, e.g. \"danbooru\"; empty for a url-only origin."},
							"post_id":               map[string]any{"type": "string", "description": "Upstream post id; omitted for a manually-added origin."},
							"url":                   map[string]any{"type": "string"},
							"commentary":            map[string]any{"type": "string", "description": "Artist commentary pulled from (or hand-entered for) this origin; omitted when empty."},
							"commentary_translated": map[string]any{"type": "string", "description": "Translation of that commentary where the source published one; omitted when empty."},
							"original":              map[string]any{"type": "string", "description": "Upstream artist source the post declared (usually a URL, newline-joined when several); omitted when empty."},
							"similarity":            map[string]any{"type": "number", "description": "The latest positive similarity-service score a lookup matched this origin with: a percentage as the service reports it (0-100), not range-checked. Omitted for an exact or manual origin. A matched origin's refetches skip the md5 verify."},
						}),
					},
					"note":            map[string]any{"type": "string", "description": "Operator's freeform note. Never written by a push or enrich."},
					"original_source": map[string]any{"type": "string", "description": "Legacy image-level original source URL. Read-only: only a gallery transfer still carries it."},
					"annotations": map[string]any{
						"type":        "array",
						"description": "Positional note boxes pulled per source, in original-image pixel coordinates. Omitted when empty.",
						"items": objectSchema(nil, map[string]any{
							"site":      map[string]any{"type": "string", "description": "The source site the note came from."},
							"post_id":   map[string]any{"type": "string", "description": "The post on that site; omitted when the source has none."},
							"x":         map[string]any{"type": "integer"},
							"y":         map[string]any{"type": "integer"},
							"w":         map[string]any{"type": "integer"},
							"h":         map[string]any{"type": "integer"},
							"body":      map[string]any{"type": "string", "description": "The stored body, in monbooru's annotation markup."},
							"body_text": map[string]any{"type": "string", "description": "The body flattened to plain text. Present only when the body carries markup."},
						}),
					},
					"ingested_at":   map[string]any{"type": "string", "format": "date-time"},
					"thumbnail_url": map[string]any{"type": "string"},
					"phash":         map[string]any{"type": "string", "nullable": true, "description": "16-char hex perceptual hash; null until the phash backfill or ingest has populated it."},
					"tags":          map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Tag"}},
					"tag_sources": map[string]any{
						"type":                 "object",
						"description":          "Per-tag source ledger keyed by the tag's category:name form (bare for general): every source that applied or re-confirmed the tag. Present on the single-image GET only.",
						"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				}),
				"ImageRelations": map[string]any{
					"type":        "object",
					"description": "Declared relations centred on one image. duplicate_group / alternate_group are null when the image is not part of a group; version_parent / version_child are null and derivative_sources / derivatives empty when no edge exists.",
					"properties": map[string]any{
						"duplicate_group": map[string]any{
							"type":     "object",
							"nullable": true,
							"properties": map[string]any{
								"id":                map[string]any{"type": "integer"},
								"original_image_id": map[string]any{"type": "integer"},
								"member_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
							},
						},
						"alternate_group": map[string]any{
							"type":     "object",
							"nullable": true,
							"properties": map[string]any{
								"id":         map[string]any{"type": "integer"},
								"member_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
							},
						},
						"version_parent":     map[string]any{"type": "integer", "nullable": true, "description": "id of the older revision; null when this image is the chain root."},
						"version_child":      map[string]any{"type": "integer", "nullable": true, "description": "id of the newer revision; null when this image is the chain leaf."},
						"derivative_sources": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "ids of the images this image is based on; empty when it is based on none."},
						"derivatives":        map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
					},
				},
			},
		},
		"security": []map[string]any{
			{"bearerAuth": []string{}},
		},
		"paths": map[string]any{
			"/": map[string]any{
				"get": op("API info", "apiInfo",
					nil,
					map[string]any{
						"200": resp("API metadata", "#/components/schemas/APIInfo"),
						"503": resp("API disabled (no token configured)", "#/components/schemas/Error"),
					}),
			},
			"/images": map[string]any{
				"post": opBody("Add an image", "createImage",
					[]map[string]any{galleryParam()},
					map[string]any{
						"required": true,
						"content": map[string]any{
							"multipart/form-data": map[string]any{
								"schema": objectSchema([]string{"file"}, map[string]any{
									"file":                        map[string]any{"type": "string", "format": "binary", "description": "Image, video, or cbz/zip archive"},
									"tags":                        map[string]any{"type": "string", "description": "JSON-encoded array of tag names"},
									"client_tags":                 map[string]any{"type": "string", "description": "Optional JSON array of the sender's own tags, attributed to via rather than the source, so a refetch of that source does not read them as dropped."},
									"folder":                      map[string]any{"type": "string", "description": "Destination subfolder under the gallery root; missing directories are created. Blank uses gallery.default_upload_folder (the gallery root when that is unset)."},
									"autotag":                     map[string]any{"type": "string", "description": "Set to \"true\" to auto-tag the new image, queued while another job runs"},
									"tagger_name":                 map[string]any{"type": "string", "description": "Optional auto-tagger name; when set with autotag, restricts the job to that tagger"},
									"via":                         map[string]any{"type": "string", "description": "Optional caller-supplied identifier (app name, URL...). Stored as images.origin and attached to each initial tag via image_tags.tagger_name, unless a non-empty source is sent, which takes the attribution instead so each source owns a prunable slice. Blank defaults images.origin to 'upload' for multipart mode."},
									"source":                      map[string]any{"type": "string", "description": "Optional site label for the image's origin (site name, scraper...). Recorded as a source; on a duplicate-SHA push its tags and provenance merge into the existing image instead of being discarded."},
									"url":                         map[string]any{"type": "string", "description": "Optional canonical web URL the image came from. Must start with http:// or https://. Recorded on the same origin as 'source'."},
									"md5":                         map[string]any{"type": "string", "description": "Optional md5 the source claims for the file (<=64 chars). Recorded on the origin row as an audit trail; never a dedup key."},
									"post_id":                     map[string]any{"type": "string", "description": "Optional post id at that source, recorded on the origin row."},
									"post_width":                  map[string]any{"type": "string", "description": "What the post says its own file's width is; recorded on the origin row beside the md5, never measured from the bytes."},
									"post_height":                 map[string]any{"type": "string", "description": "The same for height."},
									"post_size":                   map[string]any{"type": "string", "description": "The same for the file size in bytes."},
									"post_ext":                    map[string]any{"type": "string", "description": "The same for the extension the post declares."},
									"withheld_categories":         map[string]any{"type": "string", "description": "Optional JSON array of tag categories the sender left out of tags by choice: the source reconcile does not read their tags as gone from the post."},
									"withheld_tags":               map[string]any{"type": "string", "description": "Optional JSON array of tags the post still lists that the sender left out of tags by choice: the source reconcile does not read them as gone from the post."},
									"parent_url":                  map[string]any{"type": "string", "description": "Optional canonical URL of the post this post declares as its parent. Must start with http:// or https://. Recorded on the same origin as 'source'; when the parent's post lands in the gallery too, the pair is linked as a derivative relation."},
									"commentary":                  map[string]any{"type": "string", "description": "Optional artist commentary for the pushed source (<=10000 chars)."},
									"commentary_translated":       map[string]any{"type": "string", "description": "Optional translation of that commentary (<=10000 chars)."},
									"commentary_dtext":            map[string]any{"type": "string", "description": "The source's own DText for that commentary, converted to monbooru's markup on the way in and preferred over the plain rendering beside it. Same idea as a note's body_html."},
									"commentary_translated_dtext": map[string]any{"type": "string", "description": "The same for the translation."},
									"original":                    map[string]any{"type": "string", "description": "Optional upstream artist source the post declared (<=2048 chars, newline-joined when several). Recorded on the same origin as 'source'."},
									"notes":                       map[string]any{"type": "string", "description": "Optional JSON-encoded array of positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, cut to its first 16000 characters and converted to monbooru's markup on the way in; a body keeps its first 4000 characters) for the pushed source."},
									"collection":                  map[string]any{"type": "string", "description": "Optional collection label (images.series). Written on the new row; on a duplicate-SHA push added as a membership that never displaces the existing home."},
									"collection_order":            map[string]any{"type": "string", "description": "Optional 1-based position within the collection. Requires a non-empty collection in the same request."},
								}),
							},
							"application/json": map[string]any{
								"schema": objectSchema([]string{"path"}, map[string]any{
									"path":                        map[string]any{"type": "string", "description": "Path to a file already inside the gallery. Absolute paths are used as given; a relative path resolves under gallery/<folder> when folder is set, and otherwise against the server's working directory, so send an absolute path or a folder. The file must sit inside the gallery root, in a folder the gallery indexes, or the request is refused with 400."},
									"tags":                        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
									"client_tags":                 map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The sender's own tags, attributed to via rather than the source, so a refetch of that source does not read them as dropped."},
									"folder":                      map[string]any{"type": "string", "description": "Destination subfolder for relative paths"},
									"autotag":                     map[string]any{"type": "boolean", "description": "Auto-tag the new image, queued while another job runs"},
									"tagger_name":                 map[string]any{"type": "string", "description": "Optional auto-tagger name"},
									"via":                         map[string]any{"type": "string", "description": "Optional caller-supplied identifier. Stored as images.origin and attached to each initial tag, unless a non-empty source is sent, which takes the attribution instead. Blank defaults images.origin to 'ingest' for JSON path-reference mode."},
									"source":                      map[string]any{"type": "string", "description": "Optional site label for the image's origin (site name, scraper...). Recorded as a source; on a duplicate-SHA push its tags and provenance merge into the existing image instead of being discarded."},
									"url":                         map[string]any{"type": "string", "description": "Optional canonical web URL the image came from. Must start with http:// or https://. Recorded on the same origin as 'source'."},
									"md5":                         map[string]any{"type": "string", "description": "Optional md5 the source claims for the file (<=64 chars). Recorded on the origin row as an audit trail; never a dedup key."},
									"post_id":                     map[string]any{"type": "string", "description": "Optional post id at that source, recorded on the origin row."},
									"post_width":                  map[string]any{"type": "integer", "description": "What the post says its own file's width is; recorded on the origin row beside the md5, never measured from the bytes."},
									"post_height":                 map[string]any{"type": "integer", "description": "The same for height."},
									"post_size":                   map[string]any{"type": "integer", "description": "The same for the file size in bytes."},
									"post_ext":                    map[string]any{"type": "string", "description": "The same for the extension the post declares."},
									"withheld_categories":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tag categories the sender left out of tags by choice: the source reconcile does not read their tags as gone from the post."},
									"withheld_tags":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags the post still lists that the sender left out of tags by choice: the source reconcile does not read them as gone from the post."},
									"parent_url":                  map[string]any{"type": "string", "description": "Optional canonical URL of the post this post declares as its parent. Must start with http:// or https://. Recorded on the same origin as 'source'; when the parent's post lands in the gallery too, the pair is linked as a derivative relation."},
									"commentary":                  map[string]any{"type": "string", "description": "Optional artist commentary for the pushed source (<=10000 chars)."},
									"commentary_translated":       map[string]any{"type": "string", "description": "Optional translation of that commentary (<=10000 chars)."},
									"commentary_dtext":            map[string]any{"type": "string", "description": "The source's own DText for that commentary, converted to monbooru's markup on the way in and preferred over the plain rendering beside it. Same idea as a note's body_html."},
									"commentary_translated_dtext": map[string]any{"type": "string", "description": "The same for the translation."},
									"original":                    map[string]any{"type": "string", "description": "Optional upstream artist source the post declared (<=2048 chars, newline-joined when several). Recorded on the same origin as 'source'."},
									"notes":                       map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Optional positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, cut to its first 16000 characters and converted to monbooru's markup on the way in; a body keeps its first 4000 characters) for the pushed source."},
									"collection":                  map[string]any{"type": "string", "description": "Optional collection label (images.series). Written on the new row; on a duplicate-SHA push added as a membership that never displaces the existing home."},
									"collection_order":            map[string]any{"type": "integer", "description": "Optional 1-based position within the collection. Requires a non-empty collection in the same request."},
								}),
							},
						},
					},
					map[string]any{
						"200": resp("Duplicate SHA-256: existing image returned with the pushed tags and provenance merged; alias_added is true only when a JSON path-reference recorded a new alias", "#/components/schemas/DuplicateImageResponse"),
						"201": resp("Image created", "#/components/schemas/CreateImageResponse"),
						"400": resp("Invalid request or unsupported file type", "#/components/schemas/Error"),
						"413": resp("File exceeds max size", "#/components/schemas/Error"),
						"415": resp("The file does not decode as an image", "#/components/schemas/Error"),
						"500": resp("Ingest failure", "#/components/schemas/Error"),
					}),
			},
			"/images/search": map[string]any{
				"get": op("Search images", "searchImages",
					[]map[string]any{
						galleryParam(),
						queryParam("q", "Search query (tag list, filters, wildcards)"),
						queryParam("sort", "Sort field: newest, filesize, order, random, similarity (ranks by tag overlap with the image a similar: term names; newest without one)"),
						queryParam("order", "Sort order: asc, desc"),
						queryParam("seed", "Random-sort seed for stable pagination when sort=random"),
						queryParam("page", "Page number (1-based)"),
						queryParam("limit", "Results per page (max 200)"),
						queryParam("page_size", "Synonym for limit"),
					},
					map[string]any{
						"200": resp("Paginated image list", "#/components/schemas/PaginatedImages"),
					}),
			},
			"/images/{id}": map[string]any{
				"get": op("Get image metadata", "getImage",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Image metadata", "#/components/schemas/Image"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
				"patch": opBody("Edit image fields", "patchImage",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					jsonBodySchema(true, map[string]any{
						"type":        "object",
						"description": "Any subset of the editable fields. An absent or null field is left unchanged; an empty string clears a text field. Clearing collection also clears collection_order, and collection_order sent with an empty collection is refused; to clear collection_order on its own, clear the collection.",
						"properties": map[string]any{
							"source":               map[string]any{"type": "string", "description": "Operator-edited provenance label (<=200 chars). Empty clears."},
							"url":                  map[string]any{"type": "string", "description": "Canonical web URL (<=2048 chars, http:// or https://). Empty clears."},
							"collection":           map[string]any{"type": "string", "description": "Collection label (images.series). Empty clears."},
							"collection_order":     map[string]any{"type": "integer", "description": "1-based position within the collection. Requires a non-empty collection (incoming or already stored)."},
							"is_favorited":         map[string]any{"type": "boolean"},
							"is_inbox":             map[string]any{"type": "boolean", "description": "true = sits in the inbox (needs triage); false = curated."},
							"scheduled_lookup":     map[string]any{"type": "boolean", "description": "Whether the nightly online-booru lookup considers this image. Setting it true also resets an exhausted retry ladder, exactly like the detail page's [look again]."},
							"scheduled_lookup_ptr": map[string]any{"type": "boolean", "description": "The same opt-in for the nightly PTR lookup. Setting it true resets that backend's ladder."},
						},
					}),
					map[string]any{
						"200": resp("Updated image", "#/components/schemas/Image"),
						"400": resp("Invalid request (bad url, order without a collection, or no editable fields supplied)", "#/components/schemas/Error"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
						"409": resp("Relabel collides with another origin the image already carries", "#/components/schemas/Error"),
					}),
				"delete": op("Delete image from library", "deleteImage",
					[]map[string]any{
						pathParam("id", "Image ID"),
						galleryParam(),
						queryParam("delete_empty_folder", "Remove containing folder if empty after deletion"),
					},
					map[string]any{
						"200": resp("Deleted, with what else the delete did", "#/components/schemas/DeleteImageResponse"),
						"204": map[string]any{"description": "Deleted"},
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Delete failed server-side", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/enrich": map[string]any{
				"post": map[string]any{
					"summary":     "Apply fetched metadata to an existing image",
					"operationId": "enrichImage",
					"description": "Metadata-only counterpart of a duplicate push, used by monloader's source refetch. Shares the duplicate-push merge semantics: the origin upserts, the source's own tag slice reconciles to the pushed set, and empty url / md5 / commentary / commentary_translated / original / notes leave the stored values be.",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, objectSchema(nil, map[string]any{
						"tags":                        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"source":                      map[string]any{"type": "string", "description": "Site label of the origin being refetched"},
						"post_id":                     map[string]any{"type": "string", "description": "Optional post id at that source, recorded on the origin row."},
						"url":                         map[string]any{"type": "string"},
						"source_md5":                  map[string]any{"type": "string", "description": "md5 the source claims (<=64 chars). Compared against the stored file when verify is set; recorded on the origin row."},
						"parent_url":                  map[string]any{"type": "string", "description": "Canonical URL of the post this post declares as its parent. Recorded on the origin row; when both sides are in the gallery the pair is linked as a derivative relation. Empty keeps the stored value."},
						"post_width":                  map[string]any{"type": "integer", "description": "What the post says the file it serves is. Recorded on the origin row and rendered beside the origin; drives upgrade:bigger. 0 or absent keeps the stored value."},
						"post_height":                 map[string]any{"type": "integer"},
						"post_size":                   map[string]any{"type": "integer", "description": "Byte size the post declares. Used for the gain test when the post publishes no dimensions."},
						"post_ext":                    map[string]any{"type": "string", "description": "File extension the post serves, no dot."},
						"withheld_categories":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tag categories the sender left out of tags by choice: the source reconcile does not read their tags as gone from the post."},
						"withheld_tags":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags the post still lists that the sender left out of tags by choice: the source reconcile does not read them as gone from the post."},
						"verify":                      map[string]any{"type": "boolean", "description": "When true and source_md5 is non-empty, the stored file is hashed and compared first; a mismatch applies nothing and returns 409 - unless the origin is similarity-matched, whose file differs by design."},
						"similarity":                  map[string]any{"type": "number", "description": "Similarity-service score when the post was found by image similarity rather than an exact hash: a percentage as the service reports it (0-100), not range-checked. Recorded on the origin row over the stored score; a matched origin's refetches skip the md5 verify. 0, negative or absent keeps the stored score."},
						"commentary":                  map[string]any{"type": "string"},
						"commentary_translated":       map[string]any{"type": "string", "description": "Translation of the commentary. A non-empty value overwrites the stored one."},
						"commentary_dtext":            map[string]any{"type": "string", "description": "The source's own DText for that commentary, converted to monbooru's markup on the way in and preferred over the plain rendering beside it. Same idea as a note's body_html."},
						"commentary_translated_dtext": map[string]any{"type": "string", "description": "The same for the translation."},
						"original":                    map[string]any{"type": "string", "description": "Upstream artist source the post declared (<=2048 chars, newline-joined when several). A non-empty value overwrites the stored one."},
						"notes":                       map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, cut to its first 16000 characters and converted to monbooru's markup on the way in; a body keeps its first 4000 characters); a non-empty array replaces the set this origin contributed."},
					})),
					"responses": map[string]any{
						"200": resp("Metadata applied", "#/components/schemas/EnrichResponse"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"409": resp("Hash mismatch: the source returned a different file; nothing was changed. Not raised for a similarity-matched origin", "#/components/schemas/Error"),
						"500": resp("A merge write failed (recorded as an error fetch status)", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/fetch-status": map[string]any{
				"post": map[string]any{
					"summary":     "Report a source-fetch outcome",
					"operationId": "reportFetchStatus",
					"description": "Lets monloader report a fetch that failed before it could enrich (unsupported URL, timeout, blocked) so the detail page's pending pill resolves instead of polling to its cap.",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, objectSchema([]string{"state"}, map[string]any{
						"state":   map[string]any{"type": "string", "description": "Outcome state, e.g. \"error\""},
						"message": map[string]any{"type": "string", "description": "Operator-facing detail shown on the image's fetch pill"},
						"kind":    map[string]any{"type": "string", "description": "The reporting job's kind (lookup, metadata or replace): a report of any kind but lookup concludes no scheduled lookup"},
						"backend": map[string]any{"type": "string", "description": "The lookup job's backend (ptr, booru or all): a terminal state concludes only that backend's in-flight scheduled lookup, all both. Without kind and backend every in-flight lookup on the image concludes"},
					})),
					"responses": map[string]any{
						"200": map[string]any{"description": "Recorded"},
						"400": resp("Missing state", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/autotag": map[string]any{
				"post": opBody("Auto-tag an image", "autotagImage",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					jsonBodySchema(false, objectSchema(nil, map[string]any{
						"tagger_name": map[string]any{"type": "string", "description": "Run only this tagger (a name GET /taggers lists as applying). Empty runs every tagger that applies."},
					})),
					autotagResponses("Not found")),
			},
			"/images/autotag": map[string]any{
				"post": opBody("Auto-tag several images", "autotagImages",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"ids"}, map[string]any{
						"ids":         map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "1 to 5000 image ids; nothing is queued when one names no image"},
						"tagger_name": map[string]any{"type": "string", "description": "Run only this tagger. Empty runs every tagger that applies."},
					})),
					autotagResponses("An id names no image")),
			},
			"/images/{id}/tags": map[string]any{
				"get": op("List image tags", "listImageTags",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Image tag list", "#/components/schemas/TagArray"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
				"post": opBody("Add tags to image", "addImageTags",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"tags"}, map[string]any{
						"tags": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Tag names to add",
						},
						"via": map[string]any{
							"type":        "string",
							"description": "Optional caller-supplied identifier attached to each added tag (app name, URL...); stored in image_tags.tagger_name so the detail page can surface which third party contributed them",
						},
					})),
					map[string]any{
						"400": resp("Invalid request (invalid JSON, no tags, or a malformed via)", "#/components/schemas/Error"),
						"404": resp("Image not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
						"200": map[string]any{
							"description": "Bare TagArray on success; wrapped in {tags, tag_warnings?, rejected_tags?} when any tag failed validation or a name is rejected in the gallery.",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"oneOf": []map[string]any{
											{"$ref": "#/components/schemas/TagArray"},
											objectSchema(nil, map[string]any{
												"tags":          map[string]any{"$ref": "#/components/schemas/TagArray"},
												"tag_warnings":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
												"rejected_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
											}),
										},
									},
								},
							},
						},
					}),
				"delete": opBody("Remove tags from image", "removeImageTags",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"tags"}, map[string]any{
						"tags": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Tag names to remove",
						},
					})),
					map[string]any{
						"200": resp("Updated tag list", "#/components/schemas/TagArray"),
						"400": resp("Empty `tags`", "#/components/schemas/Error"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
						"409": resp("conflict for a name matching more than one tag on the image; tag_implied when another tag on the image implies the one being removed", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/file": map[string]any{
				"get": op("Download original image/video bytes", "getImageFile",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Original file bytes", "content": binaryContent("application/octet-stream")},
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
				"post": map[string]any{
					"summary":     "Replace the image's file in place",
					"description": "The file-carrying sibling of enrich: the uploaded bytes replace the existing row's file while the row and everything attached to it survive. Content-derived state (sha256, dimensions, size, type, embedded metadata, thumbnail, phash) is re-derived, annotation boxes scale to the new dimensions, and the accompanying metadata fields land through the same merge as a push. The driving origin is marked exact (similarity reset, md5 refreshed), except after a JPEG rescue (the upload re-encoded because it does not decode): that origin keeps the md5 the source served, a similarity mark of 100 and upgrade_kept, so a verified refresh applies unverified instead of answering 409 hash_mismatch. An upload whose sha256 matches the stored file applies the metadata only and answers replaced=false. Still-image and GIF rows only; a video or archive row, or a video or archive upload, is 409 wrong_type.",
					"operationId": "replaceImageFile",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"multipart/form-data": map[string]any{
								"schema": objectSchema([]string{"file"}, map[string]any{
									"file":                        map[string]any{"type": "string", "format": "binary"},
									"tags":                        map[string]any{"type": "string", "description": "JSON array of tag names, merged like an enrich"},
									"source":                      map[string]any{"type": "string"},
									"post_id":                     map[string]any{"type": "string"},
									"url":                         map[string]any{"type": "string"},
									"md5":                         map[string]any{"type": "string", "description": "md5 the source claims; recorded on the origin row"},
									"post_width":                  map[string]any{"type": "string", "description": "What the post says its own file's width is; recorded on the origin row beside the md5, never measured from the bytes."},
									"post_height":                 map[string]any{"type": "string", "description": "The same for height."},
									"post_size":                   map[string]any{"type": "string", "description": "The same for the file size in bytes."},
									"post_ext":                    map[string]any{"type": "string", "description": "The same for the extension the post declares."},
									"withheld_categories":         map[string]any{"type": "string", "description": "Optional JSON array of tag categories the sender left out of tags by choice: the source reconcile does not read their tags as gone from the post."},
									"withheld_tags":               map[string]any{"type": "string", "description": "Optional JSON array of tags the post still lists that the sender left out of tags by choice: the source reconcile does not read them as gone from the post."},
									"parent_url":                  map[string]any{"type": "string"},
									"commentary":                  map[string]any{"type": "string"},
									"commentary_translated":       map[string]any{"type": "string"},
									"commentary_dtext":            map[string]any{"type": "string", "description": "The source's own DText for that commentary, converted to monbooru's markup on the way in and preferred over the plain rendering beside it. Same idea as a note's body_html."},
									"commentary_translated_dtext": map[string]any{"type": "string", "description": "The same for the translation."},
									"original":                    map[string]any{"type": "string"},
									"notes":                       map[string]any{"type": "string", "description": "JSON array of positional note boxes"},
								}),
							},
						},
					},
					"responses": map[string]any{
						"200": resp("Replace outcome", "#/components/schemas/ReplaceFileResponse"),
						"400": resp("Invalid request (not multipart, no file field, a field over its limit, or an unsupported file type)", "#/components/schemas/Error"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"413": resp("File exceeds max size", "#/components/schemas/Error"),
						"415": resp("The file does not decode as an image", "#/components/schemas/Error"),
						"500": resp("The replacement failed server-side", "#/components/schemas/Error"),
						"409": resp("wrong_type for an archive/video row or a non-image upload; already_exists when the uploaded bytes are another image (the pair is recorded as potential duplicates); conflict when the image's file is not this gallery's to rewrite (outside its root, or in a folder it leaves out)", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/still": map[string]any{
				"get": map[string]any{
					"summary":     "Download the image in a format any decoder reads",
					"description": "An AVIF or JPEG XL is rendered by ffmpeg to a lossless WebP at its own size, a JPEG XL's alpha kept, which fails without ffmpeg or past WebP's 16383 px a side. Any other image answers its original bytes, as getImageFile does.",
					"operationId": "getImageStill",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"responses": map[string]any{
						"200": map[string]any{"description": "Image bytes: the original, or a lossless WebP for an AVIF or JPEG XL", "content": binaryContent("application/octet-stream")},
						"404": resp("Not found, or a video or archive row", "#/components/schemas/Error"),
						"500": resp("Database read failed, or the still could not be rendered", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/thumbnail": map[string]any{
				"get": op("Download the static thumbnail", "getImageThumbnail",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "JPEG thumbnail", "content": binaryContent("image/jpeg")},
						"404": resp("Image or thumbnail not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/page/{n}": map[string]any{
				"get": op("Download a manga page (cbz rows)", "getMangaPage",
					[]map[string]any{pathParam("id", "Image ID"), pathParam("n", "1-based page number"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Page bytes (lazily extracted from the archive)", "content": binaryContent("application/octet-stream")},
						"400": resp("Invalid page number", "#/components/schemas/Error"),
						"404": resp("Not a manga row, or page out of range", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/page/{n}/thumb": map[string]any{
				"get": op("Download a manga page thumbnail (cbz rows)", "getMangaPageThumb",
					[]map[string]any{pathParam("id", "Image ID"), pathParam("n", "1-based page number"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Page thumbnail (JPEG)", "content": binaryContent("image/jpeg")},
						"400": resp("Invalid page number", "#/components/schemas/Error"),
						"404": resp("Not a manga row, or page out of range", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/relations": map[string]any{
				"get": op("Get declared relations for an image", "getImageRelations",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Declared relations", "#/components/schemas/ImageRelations"),
						"404": resp("Image not found", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
			},
			"/relations": map[string]any{
				"post": opBody("Declare a relation", "addRelation",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"type", "a", "b"}, map[string]any{
						"type":      map[string]any{"type": "string", "description": "One of: duplicate, alternate, version, derivative, not_related"},
						"a":         map[string]any{"type": "integer", "description": "Left image id. For directed types (version, derivative), `a` is the parent / source (older revision, derived-from)."},
						"b":         map[string]any{"type": "integer", "description": "Right image id. For directed types, the child / derivative."},
						"direction": map[string]any{"type": "string", "description": "Optional `ba` swaps left/right; default `ab` treats `a` as the parent/source side."},
					})),
					map[string]any{
						"201": map[string]any{"description": "Relation created"},
						"400": resp("Invalid request (self-relation, unknown type, missing ids)", "#/components/schemas/Error"),
						"404": resp("a or b names no image", "#/components/schemas/Error"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
						"409": resp("Pair carries a conflicting relation already; remove it first.", "#/components/schemas/Error"),
					}),
				"delete": opBody("Remove a declared relation", "removeRelation",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"type"}, map[string]any{
						"type":     map[string]any{"type": "string", "description": "One of: duplicate, alternate, version, derivative, not_related, dissolve_dup, dissolve_alt, dissolve_version, dissolve_derivative, promote_original"},
						"image_id": map[string]any{"type": "integer", "description": "Required for duplicate / alternate (the member to unlink); the new original for promote_original."},
						"a":        map[string]any{"type": "integer", "description": "Required for version / derivative / not_related; either order is accepted."},
						"b":        map[string]any{"type": "integer", "description": "Pair partner for a."},
						"group_id": map[string]any{"type": "integer", "description": "Required for dissolve_dup / dissolve_alt / promote_original."},
						"root_id":  map[string]any{"type": "integer", "description": "Required for dissolve_version / dissolve_derivative; any chain or tree member id (the walker locates the root)."},
					})),
					map[string]any{
						"204": map[string]any{"description": "Removed (idempotent)"},
						"400": resp("Invalid request", "#/components/schemas/Error"),
					}),
			},
			"/tags": map[string]any{
				"get": op("List tags", "listTags",
					[]map[string]any{
						galleryParam(),
						queryParam("q", "Name filter; prefix match, or * as a wildcard anywhere"),
						queryParam("category", "Filter by category name. An unknown name matches nothing."),
						queryParam("sort", "Sort field: usage (default), name, created, last_used; alpha is accepted for name"),
						queryParam("order", "Sort order: asc, desc. Empty keeps the sort's default: descending for usage, created and last_used, ascending for name."),
						queryParam("page", "Page number"),
						queryParam("limit", "Results per page (default 100, max 500)"),
						queryParam("page_size", "Synonym for limit"),
						queryParam("show_zero", "'0' hides zero-usage non-alias tags; the default shows them so the API total matches the /tags page total."),
						queryParam("origin", "Filter by stored creation origin ('user', a booru site, 'ptr', an auto-tagger name, ...). The legacy 'alias' value narrows to alias rows. Empty (default) returns every tag."),
						queryParam("type", "Structural filter: 'tag' (plain rows), 'alias' (alias rows), 'rejected' (rejected names). Empty (default) returns all; any other value matches nothing."),
						queryParam("used_by", "Only tags this source applied to at least one image (an auto-tagger name, a booru site, 'user', a via label), whoever created the tag."),
					},
					map[string]any{
						"200": resp("Paginated tag list", "#/components/schemas/PaginatedTags"),
					}),
				"post": opBody("Create a tag (get-or-create)", "createTag",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"name"}, map[string]any{
						"name":     map[string]any{"type": "string", "description": "Tag name. A category:name prefix naming an existing category files the tag there, overriding category; any other prefix stays part of the name."},
						"category": map[string]any{"type": "string", "description": "Category name; defaults to general."},
					})),
					map[string]any{
						"201": resp("The tag (created, or the existing row when the name already exists in the category)", "#/components/schemas/TagRow"),
						"400": resp("Missing name, a name left empty by its category prefix, unknown category, or invalid tag name", "#/components/schemas/Error"),
						"409": resp("rejected: the name is rejected in this gallery", "#/components/schemas/Error"),
					}),
			},
			"/tags/{id}": map[string]any{
				"get": op("Get a tag with its note and links", "getTag",
					[]map[string]any{pathParam("id", "Tag ID"), galleryParam()},
					map[string]any{
						"200": resp("The tag", "#/components/schemas/TagDetail"),
						"404": resp("Tag not found", "#/components/schemas/Error"),
					}),
				"patch": opBody("Rename a tag and/or move it to another category", "patchTag",
					[]map[string]any{pathParam("id", "Tag ID"), galleryParam()},
					jsonBodySchema(true, objectSchema(nil, map[string]any{
						"name":     map[string]any{"type": "string", "description": "New name. A category:name prefix naming an existing category also moves the tag there, overriding category."},
						"category": map[string]any{"type": "string", "description": "Move to this category by name."},
					})),
					map[string]any{
						"200": resp("Updated tag", "#/components/schemas/TagRow"),
						"400": resp("No fields supplied, a rating-tag edit, or a name left empty by its category prefix", "#/components/schemas/Error"),
						"404": resp("Tag not found", "#/components/schemas/Error"),
						"409": resp("A tag with the new name already exists in the target category", "#/components/schemas/Error"),
					}),
				"delete": op("Delete a tag", "deleteTag",
					[]map[string]any{pathParam("id", "Tag ID"), galleryParam()},
					map[string]any{
						"204": map[string]any{"description": "Deleted (the four canonical rating rows are usage-stripped; the catalog row stays)"},
						"404": resp("Tag not found", "#/components/schemas/Error"),
					}),
			},
			"/tags/aliases": map[string]any{
				"post": opBody("Create an alias", "createAlias",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"name", "canonical_id"}, map[string]any{
						"name":         map[string]any{"type": "string", "description": "Alias name. A category:name prefix naming an existing category files the alias there, overriding category."},
						"category":     map[string]any{"type": "string", "description": "Category for the alias; defaults to general."},
						"canonical_id": map[string]any{"type": "integer", "description": "The tag this alias resolves to."},
					})),
					map[string]any{
						"201": resp("The alias row", "#/components/schemas/TagRow"),
						"400": resp("Missing fields or an invalid alias target", "#/components/schemas/Error"),
						"404": resp("Canonical tag not found", "#/components/schemas/Error"),
						"409": resp("The name already names a tag with image_tags rows, merge it instead; or an alias of another tag, repoint it instead; or rejected for a rejected tag", "#/components/schemas/Error"),
					}),
			},
			"/tags/resolve": map[string]any{
				"post": opBody("What the gallery does with each name on arrival", "resolveTags",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"names"}, map[string]any{
						"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "1 to 500 names as a push would spell them, a category:name prefix read as on create. Nothing is created."},
					})),
					map[string]any{
						"200": map[string]any{"description": "One entry per name, in order", "content": map[string]any{"application/json": map[string]any{"schema": objectSchema(nil, map[string]any{
							"results": map[string]any{"type": "array", "items": objectSchema(nil, map[string]any{
								"name":        map[string]any{"type": "string", "description": "As given"},
								"status":      map[string]any{"type": "string", "description": "exists (the tag is held), alias (resolves_to names what it lands as), rejected (dropped on arrival), new (created on arrival), refused (an invalid name; reason says why)"},
								"resolves_to": map[string]any{"type": "string"},
								"implies":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "What adding it also adds, general names bare and the rest category:name"},
								"reason":      map[string]any{"type": "string"},
							})},
						})}}},
						"400": resp("No names, or over 500", "#/components/schemas/Error"),
					}),
			},
			"/tags/merge": map[string]any{
				"post": opBody("Merge one tag into another", "mergeTags",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"alias_id", "canonical_id"}, map[string]any{
						"alias_id":     map[string]any{"type": "integer", "description": "Tag to retire; becomes an alias of canonical_id and its image_tags move onto it."},
						"canonical_id": map[string]any{"type": "integer", "description": "Surviving tag."},
					})),
					map[string]any{
						"200": resp("The canonical tag", "#/components/schemas/TagRow"),
						"400": resp("Self-merge, missing ids, or a merge into an alias", "#/components/schemas/Error"),
						"404": resp("alias_id or canonical_id names no tag", "#/components/schemas/Error"),
						"409": resp("The merge would close an implication cycle, or rejected for a rejected tag", "#/components/schemas/Error"),
					}),
			},
			"/tags/{id}/implications": map[string]any{
				"get": op("List a tag's implications", "listImplications",
					[]map[string]any{pathParam("id", "Parent tag ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Direct implications", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Implication"}}}}},
						"404": resp("Parent tag not found", "#/components/schemas/Error"),
					}),
				"post": map[string]any{
					"summary":     "Declare an implication",
					"operationId": "addImplication",
					"description": "Declares parent -> implied. The edge is effective immediately for future tag adds. Images already carrying the parent gain the implied tag only with propagate=true, which runs the background job the web page runs; re-declaring an edge with it re-applies the edge.",
					"parameters":  []map[string]any{pathParam("id", "Parent tag ID"), galleryParam(), propagateParam()},
					"requestBody": jsonBodySchema(true, objectSchema([]string{"implied_id"}, map[string]any{
						"implied_id": map[string]any{"type": "integer", "description": "Existing tag id the parent implies. Both sides must be canonical (non-alias)."},
					})),
					"responses": map[string]any{
						"200": map[string]any{"description": "Edge already declared (no-op)"},
						"201": map[string]any{"description": "Edge created"},
						"202": resp("propagate=true: the edge is declared and the fan-out job started", "#/components/schemas/ImplicationJob"),
						"400": resp("Missing implied_id, self-implication, or an alias on either side", "#/components/schemas/Error"),
						"404": resp("Parent or implied tag not found", "#/components/schemas/Error"),
						"409": resp("Edge would close a cycle, rejected for a rejected tag, or propagate=true while another job runs (nothing is written)", "#/components/schemas/Error"),
					},
				},
			},
			"/tags/{id}/implications/{impliedID}": map[string]any{
				"delete": map[string]any{
					"summary":     "Remove an implication",
					"operationId": "removeImplication",
					"description": "Drops the edge. The tags it implied stay on the images unless propagate=true, which runs the web page's sweep of the rows no other parent still implies.",
					"parameters":  []map[string]any{pathParam("id", "Parent tag ID"), pathParam("impliedID", "Implied tag ID"), galleryParam(), propagateParam()},
					"responses": map[string]any{
						"202": resp("propagate=true: the edge is removed and the sweep job started", "#/components/schemas/ImplicationJob"),
						"204": map[string]any{"description": "Removed"},
						"404": resp("Edge not found", "#/components/schemas/Error"),
						"409": resp("propagate=true while another job runs (nothing is removed)", "#/components/schemas/Error"),
					},
				},
			},
			"/categories": map[string]any{
				"get": op("List tag categories", "listCategories",
					[]map[string]any{galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Categories", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Category"}}}}},
					}),
				"post": opBody("Create a category", "createCategory",
					[]map[string]any{galleryParam()},
					jsonBodySchema(true, objectSchema([]string{"name"}, map[string]any{
						"name":  map[string]any{"type": "string", "description": "Lowercase letters, digits, underscore, or hyphen, not starting with a hyphen. Search-filter keywords are reserved."},
						"color": map[string]any{"type": "string", "description": "#rgb or #rrggbb; defaults to #888888 when blank."},
					})),
					map[string]any{
						"201": resp("Created category", "#/components/schemas/Category"),
						"400": resp("Invalid or reserved name, or invalid colour", "#/components/schemas/Error"),
						"409": resp("A category with this name already exists", "#/components/schemas/Error"),
					}),
			},
			"/categories/{id}": map[string]any{
				"patch": opBody("Rename and/or recolor a category", "patchCategory",
					[]map[string]any{pathParam("id", "Category ID"), galleryParam()},
					jsonBodySchema(true, objectSchema(nil, map[string]any{
						"name":  map[string]any{"type": "string", "description": "New name. Built-in categories refuse a rename."},
						"color": map[string]any{"type": "string", "description": "#rgb or #rrggbb. Allowed on built-in categories."},
					})),
					map[string]any{
						"200": resp("Updated category", "#/components/schemas/Category"),
						"400": resp("No fields, invalid colour, reserved/invalid name, or a built-in rename", "#/components/schemas/Error"),
						"404": resp("Category not found", "#/components/schemas/Error"),
						"409": resp("A category with the new name already exists", "#/components/schemas/Error"),
					}),
				"delete": opBody("Delete a category", "deleteCategory",
					[]map[string]any{pathParam("id", "Category ID"), galleryParam()},
					jsonBodySchema(false, objectSchema(nil, map[string]any{
						"action":    map[string]any{"type": "string", "description": "'move' (default; reparent the category's tags) or 'delete_all' (drop the tags too)."},
						"target_id": map[string]any{"type": "integer", "description": "Move target category id; defaults to general when omitted."},
					})),
					map[string]any{
						"204": map[string]any{"description": "Deleted"},
						"400": resp("Built-in category, an unknown action, or an unusable move target", "#/components/schemas/Error"),
						"404": resp("Category not found", "#/components/schemas/Error"),
						"409": resp("A moved tag collides with a same-named tag in the target category", "#/components/schemas/Error"),
					}),
			},
			"/health": map[string]any{
				"servers": []map[string]any{{"url": baseURL, "description": "This server, outside /api/v1"}},
				"get": withoutAuth(op("Liveness and API version", "health",
					nil,
					map[string]any{
						"200": resp("Running", "#/components/schemas/Health"),
					})),
			},
			"/pair/request": map[string]any{
				"post": withoutAuth(opBody("Offer a pairing", "pairRequest",
					nil,
					jsonBodySchema(true, objectSchema([]string{"app"}, map[string]any{
						"app":              map[string]any{"type": "string", "description": "The client's name, shown to the operator: [A-Za-z0-9_-]{1,32}; monbooru, api and default are reserved. A plugin's name; monloader pairs as monloader"},
						"url":              map[string]any{"type": "string", "description": "Where monbooru reaches the client"},
						"requested_scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Scopes asked for; none asks for read and write"},
						"peer_token":       map[string]any{"type": "string", "description": "The client's own token for monbooru's calls back"},
						"version":          map[string]any{"type": "string", "description": "The client's version, shown on its pairing card"},
						"buttons":          map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "A plugin's buttons"},
						"contract":         map[string]any{"type": "integer", "description": "The plugin contract the client speaks"},
					})),
					map[string]any{
						"200": resp("Filed for the operator to approve in Settings", "#/components/schemas/PairAccepted"),
						"400": resp("No JSON body or app, or an invalid offer (invalid_request)", "#/components/schemas/Error"),
						"403": resp("CORS: origin not allowed (forbidden)", "#/components/schemas/Error"),
						"409": resp("A plugin contract newer than this monbooru's (contract_unsupported)", "#/components/schemas/Error"),
						"429": resp("Too many pending pairing requests (too_many_requests)", "#/components/schemas/Error"),
					})),
			},
			"/pair/status": map[string]any{
				"get": withoutAuth(op("Poll a pairing request", "pairStatus",
					[]map[string]any{queryParam("id", "The request_id POST /pair/request answered")},
					map[string]any{
						"200": resp("The request's state, and the token once on approval", "#/components/schemas/PairStatus"),
						"403": resp("CORS: origin not allowed (forbidden)", "#/components/schemas/Error"),
						"404": resp("Unknown or expired request (not_found)", "#/components/schemas/Error"),
						"500": resp("The token could not be saved (mint_failed)", "#/components/schemas/Error"),
					})),
			},
			"/pair/remove": map[string]any{
				"post": op("Remove the pairing from the client's side", "pairRemove",
					nil,
					map[string]any{
						"200": map[string]any{"description": "Removed: {\"status\": \"removed\"}", "content": map[string]any{"application/json": map[string]any{"schema": objectSchema(nil, map[string]any{"status": map[string]any{"type": "string", "enum": []string{"removed"}}})}}},
						"401": resp("The bearer is not a paired token (unauthorized)", "#/components/schemas/Error"),
						"403": resp("CORS: origin not allowed (forbidden)", "#/components/schemas/Error"),
						"500": resp("The pairing could not be removed (remove_failed)", "#/components/schemas/Error"),
					}),
			},
			"/collections": map[string]any{
				"get": op("List collections", "listCollections",
					[]map[string]any{
						galleryParam(),
						queryParam("q", "Substring of the label, case-insensitive"),
						queryParam("sort", "size (default; most members first) or name (case-insensitive)"),
						queryParam("page", "Page number"),
						queryParam("limit", "Results per page (default 100, max 500)"),
						queryParam("page_size", "Synonym for limit"),
					},
					map[string]any{
						"200": resp("Paginated collection list; a label whose every member is missing is left out", "#/components/schemas/PaginatedCollections"),
						"500": resp("Database read failed", "#/components/schemas/Error"),
					}),
			},
			"/galleries": map[string]any{
				"get": op("List configured galleries", "listGalleries",
					nil,
					map[string]any{
						"200": map[string]any{"description": "Galleries with counts and the active flag", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Gallery"}}}}},
						"500": resp("A gallery's counts could not be read", "#/components/schemas/Error"),
					}),
			},
			"/jobs/current": map[string]any{
				"get": op("Read the background job state", "currentJob",
					nil,
					map[string]any{
						"200": resp("The job slot, the current or last job while the status bar still shows it, and the auto-tag queue", "#/components/schemas/CurrentJob"),
					}),
			},
			"/taggers": map[string]any{
				"get": op("List the auto-taggers", "listTaggers",
					[]map[string]any{galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Every tagger found under paths.model_path or configured, in the Settings order", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Tagger"}}}}},
					}),
			},
		},
	}
	declareRequestErrors(spec["paths"].(map[string]any))
	return spec
}

func declareRequestErrors(paths map[string]any) {
	for _, item := range paths {
		for _, raw := range item.(map[string]any) {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			body, _ := op["requestBody"].(map[string]any)
			if content, _ := body["content"].(map[string]any); content["application/json"] != nil && !isWithoutAuth(op) {
				declareJSONBodyCap(op["responses"].(map[string]any))
			}
			params, _ := op["parameters"].([]map[string]any)
			var reasons []string
			scoped := false
			for _, p := range params {
				switch {
				case p["name"] == "gallery":
					scoped = true
					reasons = append(reasons, "an unknown gallery (invalid_gallery)")
				case p["in"] == "path":
					reasons = append(reasons, "a non-numeric "+p["name"].(string))
				}
			}
			if len(reasons) == 0 {
				continue
			}
			responses := op["responses"].(map[string]any)
			if bad, ok := responses["400"].(map[string]any); ok {
				bad["description"] = bad["description"].(string) + "; also " + strings.Join(reasons, " or ")
			} else {
				responses["400"] = resp("Invalid request: "+strings.Join(reasons, " or "), "#/components/schemas/Error")
			}
			if _, ok := responses["503"]; scoped && !ok {
				responses["503"] = resp("No active gallery", "#/components/schemas/Error")
			}
		}
	}
}

func declareJSONBodyCap(responses map[string]any) {
	if big, ok := responses["413"].(map[string]any); ok {
		big["description"] = big["description"].(string) + "; also a JSON body over 16 MiB"
		return
	}
	responses["413"] = resp("JSON body exceeds 16 MiB", "#/components/schemas/Error")
}

func withoutAuth(op map[string]any) map[string]any {
	op["security"] = []map[string]any{}
	return op
}

func isWithoutAuth(op map[string]any) bool {
	sec, ok := op["security"].([]map[string]any)
	return ok && len(sec) == 0
}

func opBody(summary, id string, params []map[string]any, body, responses map[string]any) map[string]any {
	m := op(summary, id, params, responses)
	m["requestBody"] = body
	return m
}

func op(summary, id string, params []map[string]any, responses map[string]any) map[string]any {
	m := map[string]any{"summary": summary, "operationId": id, "responses": responses}
	if len(params) > 0 {
		m["parameters"] = params
	}
	return m
}

func propagateParam() map[string]any {
	return queryParam("propagate", "true runs the fan-out across the images already carrying the parent as a background job")
}

func autotagResponses(notFound string) map[string]any {
	return map[string]any{
		"202": resp("Queued; GET /jobs/current follows the run", "#/components/schemas/AutotagResponse"),
		"400": resp("Invalid ids, or no enabled tagger (or not the named one) applies to the gallery", "#/components/schemas/Error"),
		"404": resp(notFound, "#/components/schemas/Error"),
		"503": resp("No tagger can run in this build or configuration (tagger_unavailable)", "#/components/schemas/Error"),
	}
}

func resp(desc, ref string) map[string]any {
	return map[string]any{"description": desc, "content": jsonContent(ref)}
}

func jsonBodySchema(required bool, schema map[string]any) map[string]any {
	return map[string]any{
		"required": required,
		"content":  map[string]any{"application/json": map[string]any{"schema": schema}},
	}
}

func objectSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if required != nil {
		s["required"] = required
	}
	return s
}

func paginatedSchema(itemRef string) map[string]any {
	return objectSchema(nil, map[string]any{
		"page":    map[string]any{"type": "integer"},
		"limit":   map[string]any{"type": "integer"},
		"total":   map[string]any{"type": "integer"},
		"results": map[string]any{"type": "array", "items": map[string]any{"$ref": itemRef}},
	})
}

func jsonContent(ref string) map[string]any {
	return map[string]any{
		"application/json": map[string]any{
			"schema": map[string]any{"$ref": ref},
		},
	}
}

func binaryContent(mime string) map[string]any {
	return map[string]any{
		mime: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
	}
}

func pathParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "path", "required": true,
		"description": desc,
		"schema":      map[string]any{"type": "string"},
	}
}

func queryParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "query", "required": false,
		"description": desc,
		"schema":      map[string]any{"type": "string"},
	}
}

func galleryParam() map[string]any {
	return queryParam("gallery", "Target gallery name; omit for the active gallery (also accepted as X-Monbooru-Gallery header)")
}

func (h *Handler) openAPIJSON(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg()
	if !CORSOK(w, r, cfg) {
		return
	}
	contract.WriteJSON(w, http.StatusOK, buildSpec(cfg.Server.BaseURL))
}

// No external assets, so the page works offline.
func (h *Handler) openAPIDocs(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg()
	if !CORSOK(w, r, cfg) {
		return
	}
	view := extractDocsView(buildSpec(cfg.Server.BaseURL))
	view.APIEnabled = len(cfg.Auth.Tokens) > 0
	view.CORSOrigins = cfg.Server.CORSOrigins
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := docsTemplate.Execute(w, view); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

type docsView struct {
	Title       string
	Version     string
	BaseURL     string
	APIEnabled  bool
	CORSOrigins []string
	Endpoints   []endpointView
	Schemas     []schemaView
}

type endpointView struct {
	Method      string
	MethodLower string
	Path        string
	Summary     string
	Anchor      string
	Params      []paramView
	Request     *requestView
	Responses   []responseView
}

type paramView struct {
	Name, In, Description string
	Required              bool
}

type requestView struct {
	MediaTypes []mediaTypeView
}

type mediaTypeView struct {
	ContentType string
	Required    []string
	Properties  []propertyView
	Ref         string
	RefAnchor   string
}

type propertyView struct {
	Name, Type, Description string
	Nullable                bool
}

type responseView struct {
	Status, Description, Ref, RefAnchor string
}

type schemaView struct {
	Name       string
	Anchor     string
	Properties []propertyView
}

var methodOrder = []string{"get", "post", "put", "patch", "delete"}

func extractDocsView(spec map[string]any) docsView {
	view := docsView{}
	if info, ok := spec["info"].(map[string]any); ok {
		view.Title, _ = info["title"].(string)
		view.Version, _ = info["version"].(string)
	}
	if servers, ok := spec["servers"].([]map[string]any); ok && len(servers) > 0 {
		view.BaseURL, _ = servers[0]["url"].(string)
	}

	paths, _ := spec["paths"].(map[string]any)
	sortedPaths := slices.Sorted(maps.Keys(paths))
	for _, p := range sortedPaths {
		methods, _ := paths[p].(map[string]any)
		shown := p
		if servers, ok := methods["servers"].([]map[string]any); ok && len(servers) > 0 {
			base, _ := servers[0]["url"].(string)
			shown = base + p
		}
		for _, m := range methodOrder {
			op, ok := methods[m].(map[string]any)
			if !ok {
				continue
			}
			e := endpointView{
				Method:      strings.ToUpper(m),
				MethodLower: m,
				Path:        shown,
				Anchor:      m + "-" + anchorize(p),
			}
			e.Summary, _ = op["summary"].(string)
			if params, ok := op["parameters"].([]map[string]any); ok {
				for _, pp := range params {
					name, _ := pp["name"].(string)
					in, _ := pp["in"].(string)
					desc, _ := pp["description"].(string)
					req, _ := pp["required"].(bool)
					e.Params = append(e.Params, paramView{Name: name, In: in, Description: desc, Required: req})
				}
			}
			if body, ok := op["requestBody"].(map[string]any); ok {
				e.Request = extractRequest(body)
			}
			if resps, ok := op["responses"].(map[string]any); ok {
				e.Responses = extractResponses(resps)
			}
			view.Endpoints = append(view.Endpoints, e)
		}
	}

	if comps, ok := spec["components"].(map[string]any); ok {
		if schemas, ok := comps["schemas"].(map[string]any); ok {
			names := slices.Sorted(maps.Keys(schemas))
			for _, n := range names {
				s, _ := schemas[n].(map[string]any)
				sv := schemaView{Name: n, Anchor: anchorize(n)}
				if props, ok := s["properties"].(map[string]any); ok {
					sv.Properties = extractProps(props)
				}
				view.Schemas = append(view.Schemas, sv)
			}
		}
	}

	return view
}

func extractRequest(body map[string]any) *requestView {
	rv := &requestView{}
	content, ok := body["content"].(map[string]any)
	if !ok {
		return rv
	}
	cts := slices.Sorted(maps.Keys(content))
	for _, ct := range cts {
		mt, _ := content[ct].(map[string]any)
		schema, _ := mt["schema"].(map[string]any)
		mtv := mediaTypeView{ContentType: ct}
		if ref, ok := schema["$ref"].(string); ok {
			mtv.Ref = strings.TrimPrefix(ref, "#/components/schemas/")
			mtv.RefAnchor = anchorize(mtv.Ref)
		} else {
			if req, ok := schema["required"].([]string); ok {
				mtv.Required = req
			}
			if props, ok := schema["properties"].(map[string]any); ok {
				mtv.Properties = extractProps(props)
			}
		}
		rv.MediaTypes = append(rv.MediaTypes, mtv)
	}
	return rv
}

func extractResponses(resps map[string]any) []responseView {
	codes := slices.Sorted(maps.Keys(resps))
	out := make([]responseView, 0, len(codes))
	for _, c := range codes {
		r, _ := resps[c].(map[string]any)
		rv := responseView{Status: c}
		rv.Description, _ = r["description"].(string)
		if content, ok := r["content"].(map[string]any); ok {
			if app, ok := content["application/json"].(map[string]any); ok {
				if schema, ok := app["schema"].(map[string]any); ok {
					if ref, ok := schema["$ref"].(string); ok {
						rv.Ref = strings.TrimPrefix(ref, "#/components/schemas/")
						rv.RefAnchor = anchorize(rv.Ref)
					}
				}
			}
		}
		out = append(out, rv)
	}
	return out
}

func extractProps(props map[string]any) []propertyView {
	names := slices.Sorted(maps.Keys(props))
	out := make([]propertyView, 0, len(names))
	for _, n := range names {
		p, _ := props[n].(map[string]any)
		t, _ := p["type"].(string)
		d, _ := p["description"].(string)
		nullable, _ := p["nullable"].(bool)
		out = append(out, propertyView{Name: n, Type: t, Description: d, Nullable: nullable})
	}
	return out
}

var anchorReplacer = strings.NewReplacer("/", "-", "{", "", "}", "")

func anchorize(s string) string {
	return cmp.Or(strings.Trim(anchorReplacer.Replace(strings.ToLower(s)), "-"), "root")
}

var docsTemplate = template.Must(template.New("api-docs").Parse(`<!DOCTYPE html>
<html lang="en" data-app="monbooru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Docs</title>
<style>
 body { background:#0d0d0d; color:#c8c8c8; font-family:"JetBrains Mono","Fira Mono","Courier New",monospace; font-size:14px; line-height:1.5; padding:24px; max-width:1000px; margin:0 auto; }
 h1 { font-size:20px; font-weight:bold; margin-bottom:4px; }
 h2 { font-size:16px; color:#c8c8c8; border-bottom:1px solid #2a2a2a; padding-bottom:4px; margin:24px 0 8px; }
 h3 { font-size:13px; color:#666; margin:12px 0 4px; font-weight:normal; text-transform:uppercase; letter-spacing:0.5px; }
 a { color:#9d2235; text-decoration:none; }
 a:hover { text-decoration:underline; }
 code { font-family:inherit; }
 table { border-collapse:collapse; width:100%; margin:6px 0 10px; font-size:13px; }
 th, td { border:1px solid #2a2a2a; padding:4px 8px; text-align:left; vertical-align:top; }
 th { color:#666; font-weight:normal; background:#111; }
 .muted { color:#666; font-size:12px; }
 .method { display:inline-block; padding:1px 6px; border:1px solid; font-weight:bold; margin-right:8px; font-size:12px; min-width:52px; text-align:center; }
 .method-get    { color:#22aa44; border-color:#22aa44; }
 .method-post   { color:#3d90e3; border-color:#3d90e3; }
 .method-put    { color:#ffaa00; border-color:#ffaa00; }
 .method-patch  { color:#ffaa00; border-color:#ffaa00; }
 .method-delete { color:#cc3333; border-color:#cc3333; }
 .path { color:#c8c8c8; }
 ul.toc { list-style:none; padding:0; margin:8px 0 20px; }
 ul.toc li { padding:2px 0; }
 .tag { color:#ffaa00; }
 .endpoint { padding-bottom:6px; }
 .hdr { position:sticky; top:0; background:#0d0d0d; padding:4px 0; }
</style>
</head>
{{define "proptable"}}<table>
 <thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead>
 <tbody>
 {{range .}}
  <tr><td><code>{{.Name}}</code></td><td>{{.Type}}{{if .Nullable}} · nullable{{end}}</td><td>{{.Description}}</td></tr>
 {{end}}
 </tbody>
</table>{{end}}
<body>
 <p class="muted"><a href="/">‹ Back</a></p>
 <h1>{{.Title}}</h1>
 <p class="muted">Version {{.Version}} · base URL <code>{{.BaseURL}}</code></p>
 {{if .APIEnabled}}
 <p style="color:#22aa44;border:1px solid #22aa44;padding:4px 8px;">API is active - authenticate with your bearer token from Settings -&gt; Authentication.</p>
 {{else}}
 <p style="color:#ffaa00;border:1px solid #ffaa00;padding:4px 8px;">API is disabled - generate a token in Settings -&gt; Authentication to enable it. All endpoints currently return <code>503 api_disabled</code>.</p>
 {{end}}
 <p>Every endpoint except <code>/docs</code> and <code>/openapi.json</code> requires <code>Authorization: Bearer &lt;token&gt;</code>. Create a named token in Settings -&gt; Authentication; while none exists every authenticated endpoint returns <code>503 api_disabled</code>. Tokens are scoped (read/write/delete); a request whose token lacks the scope gets <code>403 insufficient_scope</code>.</p>
 <p>Endpoints take an optional <code>?gallery=&lt;name&gt;</code> (or <code>X-Monbooru-Gallery</code> header) to target a specific gallery; omit both for the active one.</p>
 <p>From a browser, the address you reached this page on can call the API. {{if .CORSOrigins}}So can: {{range $i, $o := .CORSOrigins}}{{if $i}}, {{end}}<code>{{$o}}</code>{{end}}.{{else}}Add others to <code>server.cors_origins</code> in <code>monbooru.toml</code>.{{end}}</p>
 <p class="muted">Raw spec: <a href="/api/v1/openapi.json">openapi.json</a></p>

 <h2>Endpoints</h2>
 <ul class="toc">
 {{range .Endpoints}}
  <li><a href="#{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></a>{{if .Summary}} <span class="muted">- {{.Summary}}</span>{{end}}</li>
 {{end}}
 </ul>

 {{range .Endpoints}}
 <div class="endpoint">
  <h2 id="{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></h2>
  {{if .Summary}}<p>{{.Summary}}</p>{{end}}

  {{if .Params}}
  <h3>Parameters</h3>
  <table>
   <thead><tr><th>Name</th><th>In</th><th>Required</th><th>Description</th></tr></thead>
   <tbody>
   {{range .Params}}
    <tr><td><code>{{.Name}}</code></td><td>{{.In}}</td><td>{{if .Required}}yes{{else}}no{{end}}</td><td>{{.Description}}</td></tr>
   {{end}}
   </tbody>
  </table>
  {{end}}

  {{if .Request}}
  <h3>Request body</h3>
  {{range .Request.MediaTypes}}
   <p class="muted">Content-Type: <code>{{.ContentType}}</code>{{if .Ref}} - schema <a href="#schema-{{.RefAnchor}}"><code>{{.Ref}}</code></a>{{end}}</p>
   {{if .Required}}<p class="muted">Required: {{range .Required}}<code>{{.}}</code> {{end}}</p>{{end}}
   {{if .Properties}}
   {{template "proptable" .Properties}}
   {{end}}
  {{end}}
  {{end}}

  {{if .Responses}}
  <h3>Responses</h3>
  <table>
   <thead><tr><th>Status</th><th>Description</th><th>Schema</th></tr></thead>
   <tbody>
   {{range .Responses}}
    <tr><td><code>{{.Status}}</code></td><td>{{.Description}}</td><td>{{if .Ref}}<a href="#schema-{{.RefAnchor}}"><code>{{.Ref}}</code></a>{{end}}</td></tr>
   {{end}}
   </tbody>
  </table>
  {{end}}
 </div>
 {{end}}

 <h2>Schemas</h2>
 {{range .Schemas}}
  <h3 id="schema-{{.Anchor}}" style="color:#c8c8c8; font-size:14px; text-transform:none; letter-spacing:0; margin-top:14px">{{.Name}}</h3>
  {{if .Properties}}
  {{template "proptable" .Properties}}
  {{else}}
  <p class="muted">(no fields)</p>
  {{end}}
 {{end}}
</body>
</html>`))

func fileTypeNames() []string {
	names := make([]string, len(models.FileTypes))
	for i, ft := range models.FileTypes {
		names[i] = ft.Name
	}
	return names
}
