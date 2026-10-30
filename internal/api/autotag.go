package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/library"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/contract"
	"github.com/monbooru/monbooru/internal/shared/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

const maxAutotagIDs = 5000

func (h *Handler) autotagImage(w http.ResponseWriter, r *http.Request) {
	g, id, ok := h.galleryAndExistingID(w, r)
	if !ok {
		return
	}
	var body struct {
		TaggerName string `json:"tagger_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		contract.RefuseJSONBody(w, err, maxJSONBody)
		return
	}
	h.startAutotag(w, g, strings.TrimSpace(body.TaggerName), []int64{id})
}

func (h *Handler) autotagImages(w http.ResponseWriter, r *http.Request) {
	g, ok := h.resolveGallery(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs        []int64 `json:"ids"`
		TaggerName string  `json:"tagger_name"`
	}
	if !contract.DecodeJSON(w, r, &body, maxJSONBody) {
		return
	}
	ids := slices.Compact(slices.Sorted(slices.Values(body.IDs)))
	if len(ids) == 0 || len(ids) > maxAutotagIDs {
		contract.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("`ids` must name 1 to %d images", maxAutotagIDs))
		return
	}
	placeholders, args := db.InPlaceholders(ids)
	found, err := db.QueryIDs(g.DB.Read, `SELECT id FROM images WHERE id IN (`+placeholders+`)`, args...)
	if serverError(w, err) {
		return
	}
	if len(found) != len(ids) {
		for _, id := range ids {
			if !slices.Contains(found, id) {
				contract.WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("image %d not found", id))
				return
			}
		}
	}
	h.startAutotag(w, g, strings.TrimSpace(body.TaggerName), ids)
}

func (h *Handler) startAutotag(w http.ResponseWriter, g *library.Gallery, taggerName string, ids []int64) {
	cfg := h.cfg()
	if !tagger.IsAvailable(cfg) {
		contract.WriteError(w, http.StatusServiceUnavailable, "tagger_unavailable", "auto-tagger not available: "+tagger.UnavailableReason(cfg))
		return
	}
	if _, err := tagger.SelectForGallery(cfg, g.Name, taggerName); badRequest(w, err) {
		return
	}
	contract.WriteJSON(w, http.StatusAccepted, map[string]any{
		"autotag": h.queueAutotag(g.Name, taggerName, ids...),
		"images":  len(ids),
	})
}

type queuedAutotag struct {
	gallery, tagger string
	id              int64
}

type queuedBatch struct {
	gallery, tagger string
	ids             []int64
}

func (h *Handler) queueAutotag(gallery, taggerName string, ids ...int64) string {
	h.autotagMu.Lock()
	defer h.autotagMu.Unlock()
	for _, id := range ids {
		h.autotagQueue = append(h.autotagQueue, queuedAutotag{gallery, taggerName, id})
	}
	if h.autotagDraining {
		return "autotag queued"
	}
	h.autotagDraining = true
	started := h.jobs.Start(models.JobTypeAutotag) == nil
	go h.drainAutotag(started)
	if started {
		return "autotag job started"
	}
	return "autotag queued"
}

func (h *Handler) drainAutotag(started bool) {
	for {
		if !started && !h.awaitJobSlot() {
			h.dropAutotagQueue()
			return
		}
		started = false
		if h.runAutotagBatch(h.nextAutotagBatch()) {
			h.dropAutotagQueue()
			return
		}
		h.autotagMu.Lock()
		if len(h.autotagQueue) == 0 {
			h.autotagDraining = false
			h.autotagMu.Unlock()
			return
		}
		h.autotagMu.Unlock()
	}
}

func (h *Handler) awaitJobSlot() bool {
	for h.jobs.Start(models.JobTypeAutotag) != nil {
		select {
		case <-h.done:
			return false
		case <-time.After(time.Second):
		}
	}
	select {
	case <-h.done:
		// The shutdown's cancel may have run before this Start took the slot.
		h.jobs.Finish(nil, true, "auto-tagging cancelled", "")
		return false
	default:
		return true
	}
}

func (h *Handler) nextAutotagBatch() queuedBatch {
	h.autotagMu.Lock()
	defer h.autotagMu.Unlock()
	head := h.autotagQueue[0]
	b := queuedBatch{gallery: head.gallery, tagger: head.tagger}
	rest := h.autotagQueue[:0]
	for _, q := range h.autotagQueue {
		switch {
		case q.gallery != b.gallery || q.tagger != b.tagger:
			rest = append(rest, q)
		case !slices.Contains(b.ids, q.id):
			b.ids = append(b.ids, q.id)
		}
	}
	h.autotagQueue = rest
	return b
}

func (h *Handler) runAutotagBatch(b queuedBatch) (cancelled bool) {
	cfg := h.cfg()
	fail := func(reason string) {
		h.jobs.Fail(fmt.Sprintf("auto-tagging %d queued %s: %s", len(b.ids), models.Plural(len(b.ids), "image", "images"), reason))
	}
	g, ok := h.resolver(b.gallery)
	if !ok {
		fail(fmt.Sprintf("gallery %q is gone", b.gallery))
		return false
	}
	if !tagger.IsAvailable(cfg) {
		fail(tagger.UnavailableReason(cfg))
		return false
	}
	selected, err := tagger.SelectForGallery(cfg, b.gallery, b.tagger)
	if err != nil {
		fail(err.Error())
		return false
	}
	h.jobs.Update(0, len(b.ids), "starting (loading model may take a few seconds)…")
	peak := func(string) {}
	if h.memoryMark != nil {
		peak = h.memoryMark()
	}
	ctx := h.jobs.Context()
	skipped, err := tagger.RunWithTaggers(ctx, g.Handle, cfg, b.ids, selected, h.jobs, cfg.Tagger.ExecutionProvider)
	if len(b.ids) == 1 {
		if finished, _ := tagger.SettleImage(ctx, h.jobs, b.ids[0], skipped, err); finished {
			peak(fmt.Sprintf("image #%d", b.ids[0]))
		}
	} else if finished, _ := tagger.Settle(ctx, h.jobs, "", "", len(b.ids), skipped, err); finished {
		peak(fmt.Sprintf("batch %d %s", len(b.ids), models.Plural(len(b.ids), "image", "images")))
	}
	return ctx.Err() != nil
}

func (h *Handler) dropAutotagQueue() {
	h.autotagMu.Lock()
	defer h.autotagMu.Unlock()
	if n := len(h.autotagQueue); n > 0 {
		logx.Infof("autotag: dropped %d queued %s", n, models.Plural(n, "upload", "uploads"))
	}
	h.autotagQueue = nil
	h.autotagDraining = false
}
