'use strict';

// A string leaf is a URL, an array holds selectors whose first match is
// clicked, a function runs, an object is a sub-chord.
var chordMap = {
  g: {
    g: '/',
    i: '/?q=inbox:true',
    c: {
      a: '/categories',
      o: '/collections',
    },
    t: '/tags',
    r: '/relations',
    s: '/settings',
  },
  t: {
    r: ['#btn-detail-rename'],
    n: ['#btn-detail-note'],
    c: ['#tag-batch-bar:not([hidden]) #btn-batch-category', '#btn-detail-category'],
    a: ['#tag-batch-bar:not([hidden]) #btn-batch-alias', '#btn-detail-alias'],
    m: ['#tag-batch-bar:not([hidden]) #btn-batch-merge'],
    i: ['#tag-batch-bar:not([hidden]) #btn-batch-imply'],
    f: ['#tag-batch-bar:not([hidden]) #btn-batch-merge-folded'],
    d: ['#tag-batch-bar:not([hidden]) #btn-batch-distinct', '#tag-batch-bar:not([hidden]) #btn-batch-undistinct'],
    p: ['#tag-batch-bar:not([hidden]) #btn-batch-ptr:not([disabled])'],
  },
  // Empty on purpose: its leaves are the digits, one per plugin button on
  // the bar that is up.
  x: {},
  e: {
    s: ['.btn-add-source'],
    c: ['.btn-add-collection'],
    n: ['.btn-edit-note'],
    a: ['.btn-add-annotation'],
    r: ['[data-relations-add]'],
    t: ['#remove-tags-btn'],
    g: ['#transfer-image-btn'],
  },
};

function chordDigitTargets(node) {
  if (node === chordMap.e) return document.querySelectorAll('.btn-edit-collection');
  if (node === chordMap.x) return document.querySelectorAll('.plugin-card .plugin-btn, #plugin-batch-bar.visible .plugin-btn');
  return null;
}

// body is overflow:hidden and #content scrolls instead, so the window
// usually does not.
function pageScroller(el) {
  var node = el && el.parentElement;
  while (node) {
    var ov = getComputedStyle(node).overflowY;
    if ((ov === 'auto' || ov === 'scroll') && node.scrollHeight > node.clientHeight) {
      return node;
    }
    node = node.parentElement;
  }
  return document.scrollingElement || document.documentElement;
}

function scrollPageTo(el, top) {
  var s = pageScroller(el);
  if (s === document.documentElement || s === document.body || s === document.scrollingElement) {
    window.scrollTo({ top: top });
  } else {
    s.scrollTo({ top: top });
  }
}

function scrollToTop(el) { scrollPageTo(el, 0); }
function scrollToBottom(el) {
  var s = pageScroller(el);
  scrollPageTo(el, s.scrollHeight);
}

function setFocused(items, idx, block) {
  items.forEach(function(el) { el.classList.remove('focused'); });
  items[idx].classList.add('focused');
  items[idx].scrollIntoView({ block: block || 'nearest' });
}

function moveGridCursor(dx, dy, cells) {
  var cards = cells || Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  var idx = cards.findIndex(function(c) { return c.classList.contains('focused'); });
  if (idx < 0) {
    idx = 0;
  } else if (dy !== 0) {
    // By position: the inbox's cluster headers take full rows and leave
    // short ones, so the index step drifts across columns.
    var from = cards[idx];
    var rowTop = null;
    cards.forEach(function(c) {
      var d = (c.offsetTop - from.offsetTop) * dy;
      if (d > 0 && (rowTop === null || d < (rowTop - from.offsetTop) * dy)) rowTop = c.offsetTop;
    });
    if (rowTop === null) {
      if (dy < 0) scrollToTop(from); else scrollToBottom(from);
      return true;
    }
    var best = -1;
    cards.forEach(function(c, i) {
      if (c.offsetTop !== rowTop) return;
      if (best < 0 || Math.abs(c.offsetLeft - from.offsetLeft) < Math.abs(cards[best].offsetLeft - from.offsetLeft)) best = i;
    });
    idx = best;
  } else {
    idx = Math.max(0, Math.min(cards.length - 1, idx + dx));
  }
  setFocused(cards, idx);
  return true;
}

function moveTagRowCursor(step) {
  var rows = Array.from(document.querySelectorAll('.tag-row'));
  if (rows.length === 0) return false;
  var focused = document.querySelector('.tag-row.focused');
  var at = focused ? rows.indexOf(focused) + step : 0;
  setFocused(rows, Math.max(0, Math.min(rows.length - 1, at)));
  return true;
}

function jumpGridCursor(target, cells) {
  var cards = cells || Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  setFocused(cards, target === 'first' ? 0 : cards.length - 1);
  return true;
}

function stepThumbSize(delta) {
  var btns = Array.from(document.querySelectorAll('#thumb-size-ramp button'));
  if (btns.length === 0) return false;
  var at = btns.findIndex(function(b) { return b.getAttribute('aria-pressed') === 'true'; });
  var next = Math.max(0, Math.min(btns.length - 1, (at < 0 ? 0 : at) + delta));
  if (next !== at) btns[next].click();
  return true;
}

function clickPagination(needle) {
  var links = document.querySelectorAll('.pagination a');
  for (var i = 0; i < links.length; i++) {
    if (links[i].textContent.indexOf(needle) >= 0) { links[i].click(); return true; }
  }
  return false;
}

function handlePaginationKey(e) {
  if (e.key === '[') return clickPagination('Prev');
  if (e.key === ']') return clickPagination('Next');
  if (e.key === 'G') return clickPagination('Last');
  if (e.key === 'p') {
    var jp = document.querySelector('.page-jump');
    if (jp) { jp.click(); return true; }
  }
  return false;
}

// A data-ref page was opened from another image's similar list, so back
// unwinds that chain one step at a time.
function detailRefBack() {
  var detailPage = document.getElementById('detail-page');
  if (detailPage && detailPage.dataset.ref && history.length > 1) { history.back(); return true; }
  return false;
}

function detailBack() {
  if (detailRefBack()) return;
  var backLink = document.querySelector('.back-link');
  if (backLink) backLink.click();
  else window.location.href = '/';
}

function navDetailArrow(direction, noun) {
  var word = direction === 'prev' ? 'Previous' : 'Next';
  var a = document.querySelector('.nav-arrow[title^="' + word + ' ' + noun + '"]');
  if (a && a.href) { window.location.href = a.href; return true; }
  return false;
}

// A pinch-zoomed page is left alone, since there the finger pans.
function onSwipe(el, prev, next) {
  var x0 = null, y0 = 0, t0 = 0;
  el.addEventListener('pointerdown', function (e) {
    if (e.pointerType !== 'touch' || !e.isPrimary || e.target.closest('video[controls]')) return;
    x0 = e.clientX; y0 = e.clientY; t0 = e.timeStamp;
  });
  el.addEventListener('pointercancel', function () { x0 = null; });
  el.addEventListener('pointerup', function (e) {
    if (x0 === null || !e.isPrimary) return;
    var dx = e.clientX - x0, dy = e.clientY - y0;
    x0 = null;
    if (e.timeStamp - t0 > 600 || Math.abs(dx) < 50 || Math.abs(dx) < 2 * Math.abs(dy)) return;
    if (window.visualViewport && window.visualViewport.scale > 1.01) return;
    if (dx > 0) prev(); else next();
  });
}

document.addEventListener('DOMContentLoaded', function () {
  var media = document.querySelector('.detail-media');
  if (media) onSwipe(media, function () { navDetailArrow('prev', 'image'); }, function () { navDetailArrow('next', 'image'); });
  var stage = document.querySelector('.reader-stage');
  if (stage) {
    var follow = function (sel) {
      return function () { var a = stage.querySelector(sel); if (a) window.location.href = a.href; };
    };
    onSwipe(stage, follow('a.reader-side-prev'), follow('a.reader-side-next'));
  }
});

// The tag page and the tags listing both host the editor as
// #implications-editor, carrying its tag's id.
function handleImplicationAdded(event) {
  var hdr = event.detail.xhr.getResponseHeader('HX-Trigger') || '';
  if (hdr.indexOf('implication-added') === -1) return;
  var input = document.getElementById('implication-add-input');
  if (input) input.value = '';
  clearSlots('implication-add-suggest');
  var editor = document.getElementById('implications-editor');
  htmx.ajax('GET', '/tags/' + editor.dataset.tagId + '/implications', {target: '#implications-editor', swap: 'innerHTML'});
}

function pruneRelationRow(btn) {
  var li = btn.closest('li');
  if (!li) return;
  var ul = li.parentElement;
  li.remove();
  if (ul && ul.classList.contains('tag-list') && !ul.querySelector('li')) {
    var sub = ul.previousElementSibling;
    if (sub && sub.classList.contains('relation-origin-sub')) sub.remove();
    ul.remove();
  }
}

function pruneRelationGroup(btn) {
  var sub = btn.closest('.relation-origin-sub');
  if (!sub) return;
  var ul = sub.nextElementSibling;
  if (ul && ul.classList.contains('tag-list')) ul.remove();
  sub.remove();
}

// The footer lists the ceilings low to high, so +1 is more permissive.
function cycleRatingCeiling(delta) {
  var active = document.querySelector('.footer-rating-active');
  if (!active) return false;
  var siblings = active.parentNode.parentNode.querySelectorAll('.footer-rating-active, .footer-rating-link');
  var ordered = Array.prototype.slice.call(siblings);
  var idx = ordered.indexOf(active);
  if (idx < 0) return false;
  var next = idx + delta;
  if (next < 0 || next >= ordered.length) return false;
  var target = ordered[next];
  if (target.tagName === 'BUTTON') {
    var f = target.closest('form');
    if (f) { f.requestSubmit ? f.requestSubmit() : f.submit(); return true; }
  }
  return false;
}

// Collection order forces asc: a descending pick carried over would
// render the collection backwards.
function applySortChange(sortEl) {
  if (sortEl.value === 'order') {
    var orderEl = document.querySelector('#search-form select[name="order"]');
    if (orderEl) orderEl.value = 'asc';
  }
  // A seed belongs to the random sort; left on the form it rides into the
  // pushed URL and into Save search.
  if (sortEl.value !== 'random') {
    var seedEl = document.querySelector('#search-form input[name="seed"]');
    if (seedEl) seedEl.remove();
  }
  return submitSearch();
}

function cycleSort() {
  var sortEl = document.getElementById('search-sort');
  if (!sortEl) return false;
  var order = ['newest', 'filesize', 'order', 'random'];
  var i = order.indexOf(sortEl.value);
  var next = order[(i + 1) % order.length];
  if (next === 'random') {
    applyRandomSort();
    return true;
  }
  sortEl.value = next;
  return applySortChange(sortEl);
}

function flipSortDirection() {
  var orderEl = document.querySelector('#search-form select[name="order"]');
  if (!orderEl) return false;
  orderEl.value = orderEl.value === 'asc' ? 'desc' : 'asc';
  return submitSearch();
}

// Dispatch, not form.submit(): htmx listens for the submit event, which
// submit() never fires.
function submitSearch() {
  var form = document.getElementById('search-form');
  if (!form) return false;
  form.dispatchEvent(new Event('submit', { bubbles: true }));
  return true;
}

function focusFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { revealSidebarFor(el); el.focus(); if (el.select) el.select(); return true; }
  }
  return false;
}

function isGalleryPage()    { return !!document.querySelector('.thumb-grid, #gallery-grid'); }
function isDetailPage()     { return !!document.getElementById('detail-page'); }
function isTagDetailPage()  { return !!document.getElementById('tag-detail-page'); }
function isTagsPage()       { return !!document.getElementById('tags-page'); }
function isCategoriesPage() { return !!document.getElementById('categories-page'); }
function isSettingsPage()   { return !!document.getElementById('settings-page'); }
function batchBarVisible()  { return !!document.querySelector('#batch-bar.visible'); }

function activeSurfaces() {
  var s = ['anywhere'];
  if (isGalleryPage())    s.push('gallery');
  if (selectionArmed())   s.push('selection');
  if (isDetailPage())     s.push('detail');
  if (isTagsPage())       s.push('tags');
  if (isTagDetailPage())  s.push('tag-detail');
  if (isCategoriesPage()) s.push('categories');
  if (isSettingsPage())   s.push('settings');
  if (document.getElementById('pages-grid-page')) s.push('pages');
  if (document.getElementById('relations-session-page')) s.push('relations');
  if (document.querySelector('.detail-img-link')) s.push('lightbox');
  if (document.getElementById('slideshow-btn')) s.push('slideshow');
  return s;
}

// AltGraph, which types \ [ ] ? on AZERTY and QWERTZ, reports as Ctrl+Alt on Windows.
function chorded(e) {
  return (e.ctrlKey || e.metaKey || e.altKey) && !e.getModifierState('AltGraph');
}

function handlePagesGridKey(e) {
  var page = document.getElementById('pages-grid-page');
  if (!page || chorded(e)) return false;
  if (e.target.tagName.toLowerCase() === 'input' || e.target.isContentEditable) return false;
  if (document.querySelector('dialog[open]')) return false;
  var cells = Array.from(page.querySelectorAll('.manga-page-cell'));
  if (cells.length === 0) return false;
  var step = { ArrowRight: [1, 0], ArrowLeft: [-1, 0], ArrowDown: [0, 1], ArrowUp: [0, -1] }[e.key];
  if (step) {
    e.preventDefault(); moveGridCursor(step[0], step[1], cells); return true;
  }
  switch (e.key) {
    case 'Home':
      e.preventDefault(); jumpGridCursor('first', cells); return true;
    case 'End':
      e.preventDefault(); jumpGridCursor('last', cells); return true;
    case 'Enter':
    case 'o':
      var link = page.querySelector('.manga-page-cell.focused a');
      if (link && link.href) { window.location.href = link.href; return true; }
      return false;
  }
  return false;
}

var lightbox = (function () {
  var dlg = null, img = null, stage = null, zoomLbl = null, openLink = null;
  var scale = 1, tx = 0, ty = 0;
  var panning = false, px0 = 0, py0 = 0, tx0 = 0, ty0 = 0;
  // Past 4px (16 squared) a press is a pan, and its release must not
  // close the viewer.
  var pressMoved = false;
  var bound = false;

  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
  // scale multiplies the CSS fit, so 1:1 is read back off the painted box
  // rather than assumed to be 1.
  function oneToOneScale() {
    if (!img || !img.naturalWidth) return 0;
    var r = contentRect();
    if (!r || !r.width) return 0;
    return img.naturalWidth * scale / r.width;
  }
  // 1:1 on an image smaller than the stage sits below 0.5, and the floor
  // must not rule it out.
  function minScale() {
    var one = oneToOneScale();
    return one > 0 ? Math.min(0.5, one) : 0.5;
  }
  function apply() {
    if (!img) return;
    img.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    if (!zoomLbl) return;
    // Against the image's own pixels, not the fit: on a small image the
    // fit is already an upscale.
    var one = oneToOneScale();
    zoomLbl.textContent = Math.round((one > 0 ? scale / one : scale) * 100) + '%';
  }
  function reset() { scale = 1; tx = 0; ty = 0; apply(); }
  function zoomAt(cx, cy, factor) {
    var ns = clamp(scale * factor, minScale(), 4);
    var k = ns / scale;
    tx = cx - k * (cx - tx);
    ty = cy - k * (cy - ty);
    scale = ns;
    apply();
  }
  // object-fit: contain letterboxes the image inside the element, so the
  // painted rect is computed; the letterbox is not "on the image".
  function contentRect() {
    if (!img) return null;
    var r = img.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    var iw = img.naturalWidth, ih = img.naturalHeight;
    if (!iw || !ih) return r;
    var ia = iw / ih, ba = r.width / r.height;
    var w, h;
    if (ia > ba) { w = r.width; h = r.width / ia; }
    else { h = r.height; w = r.height * ia; }
    var left = r.left + (r.width - w) / 2;
    var top = r.top + (r.height - h) / 2;
    return { left: left, top: top, right: left + w, bottom: top + h, width: w, height: h };
  }
  function oneToOne() {
    var one = oneToOneScale();
    if (one <= 0) return;
    var ns = clamp(one, minScale(), 4);
    var k = ns / scale;
    tx = k * tx; ty = k * ty;
    scale = ns;
    apply();
  }
  function onWheel(e) {
    e.preventDefault();
    var sr = stage.getBoundingClientRect();
    var cx = e.clientX - (sr.left + sr.width / 2);
    var cy = e.clientY - (sr.top + sr.height / 2);
    zoomAt(cx, cy, e.deltaY < 0 ? 1.2 : 1 / 1.2);
  }
  function onPointerDown(e) {
    if (e.button !== 0) return;
    stage.setPointerCapture(e.pointerId);
    panning = true;
    pressMoved = false;
    px0 = e.clientX; py0 = e.clientY;
    tx0 = tx; ty0 = ty;
    stage.classList.add('grabbing');
  }
  function onPointerMove(e) {
    if (!panning) return;
    if (!pressMoved) {
      var dx = e.clientX - px0, dy = e.clientY - py0;
      if (dx * dx + dy * dy > 16) pressMoved = true;
    }
    tx = tx0 + (e.clientX - px0);
    ty = ty0 + (e.clientY - py0);
    apply();
  }
  function onPointerEnd(e) {
    if (!panning) return;
    panning = false;
    stage.releasePointerCapture(e.pointerId);
    stage.classList.remove('grabbing');
    // Only a release off the image closes: the first click of a dblclick
    // on it must not.
    if (pressMoved || !img) return;
    var r = contentRect();
    if (!r || !r.width || e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
      dlg.close();
    }
  }
  function onDblClick() {
    if (Math.abs(scale - 1) < 0.01) oneToOne();
    else reset();
  }
  function ensure() {
    if (dlg) return true;
    dlg = document.getElementById('image-lightbox');
    if (!dlg) return false;
    img = document.getElementById('lightbox-img');
    stage = document.getElementById('lightbox-stage');
    zoomLbl = document.getElementById('lightbox-zoom');
    openLink = document.getElementById('lightbox-open');
    if (!bound && stage) {
      stage.addEventListener('wheel', onWheel, { passive: false });
      stage.addEventListener('pointerdown', onPointerDown);
      stage.addEventListener('pointermove', onPointerMove);
      stage.addEventListener('pointerup', onPointerEnd);
      stage.addEventListener('pointercancel', onPointerEnd);
      stage.addEventListener('dblclick', onDblClick);
      // The readout needs the natural size, which only arrives after
      // open() sets the src.
      img.addEventListener('load', apply);
      dlg.addEventListener('close', reset);
      var closeBtn = document.getElementById('lightbox-close');
      if (closeBtn) closeBtn.addEventListener('click', function () { dlg.close(); });
      bound = true;
    }
    return true;
  }
  return {
    open: function (e, src) {
      // Modifier and middle clicks keep the link's own navigation: a
      // truthy return leaves the default alone.
      if (e && (e.button === 1 || e.ctrlKey || e.metaKey || e.shiftKey)) return true;
      if (!ensure()) return true;
      if (e && e.preventDefault) e.preventDefault();
      img.src = src;
      if (openLink) openLink.href = src;
      reset();
      if (!dlg.open) dlg.showModal();
      return false;
    },
    handleKey: function (e) {
      if (!dlg || !dlg.open || chorded(e)) return false;
      var t = e.target.tagName.toLowerCase();
      if (t === 'input' || t === 'textarea' || e.target.isContentEditable) return false;
      switch (e.key) {
        case '+': case '=':
          e.preventDefault(); zoomAt(0, 0, 1.2); return true;
        case '-':
          e.preventDefault(); zoomAt(0, 0, 1 / 1.2); return true;
        case '0':
          e.preventDefault(); reset(); return true;
        case '1':
          e.preventDefault(); oneToOne(); return true;
      }
      return false;
    },
  };
})();

var slideshow = (function () {
  var durations = [2, 3, 5, 10, 15, 30];
  var dlg = null, slots = [], message = null, position = null, durationSelect = null, documentBound = false;
  var ids = [], videos = new Set();
  var index = -1, current = null, visibleSlot = 0, prepared = null;
  var playing = true, muted = true, looping = false;
  var durationMs = 5000, remainingMs = 0, startedAt = null;
  var displayEpoch = 0, preloadEpoch = 0;
  var autoTimer = null, skipTimer = null;
  var leavingFullscreen = false, restacks = 0;
  var detailStart = 0, detailBack = '';
  var actions = {
    first: function () { go(-ids.length); },
    previous: function () { go(-1); },
    next: function () { go(1); },
    last: function () { go(ids.length); },
    play: function () { if (playing) pause(); else resume(); },
    restart: restart,
    loop: toggleLoop,
    mute: toggleMuted,
    fullscreen: toggleFullscreen,
    exit: function () { dlg.close(); },
  };
  var keys = { ' ': 'play', ArrowLeft: 'previous', ArrowRight: 'next', Home: 'first', End: 'last', r: 'restart', l: 'loop', m: 'mute', f: 'fullscreen' };

  function readDuration() {
    var seconds = 5;
    try { seconds = parseInt(localStorage.getItem('monbooru_slideshow_duration') || '5', 10); } catch (e) {}
    return durations.indexOf(seconds) >= 0 ? seconds : 5;
  }
  function itemAt(i) {
    var id = ids[i], video = videos.has(id);
    return { id: id, video: video, src: '/images/' + id + (video ? '/file' : '/view') };
  }
  function setMessage(text) { message.textContent = text; }
  function button(action) { return dlg.querySelector('[data-slideshow="' + action + '"]'); }
  function currentVideo() { return current && current.video ? slots[visibleSlot].querySelector('video') : null; }

  function clearSlot(slot) {
    var v = slot.querySelector('video');
    if (v) { v.pause(); v.removeAttribute('src'); v.load(); }
    slot.replaceChildren();
    slot.classList.remove('active');
  }
  function prepareMedia(slot, item) {
    clearSlot(slot);
    return new Promise(function (resolve, reject) {
      var media = document.createElement(item.video ? 'video' : 'img');
      var settled = false;
      var timeout = setTimeout(function () { finish('#' + item.id + ' took too long to load.'); }, 12000);
      function finish(err) {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        media.onload = media.onloadeddata = media.onerror = null;
        if (err) reject(new Error(err)); else resolve(media);
      }
      media.onerror = function () { finish('Could not load #' + item.id + '.'); };
      if (item.video) {
        media.preload = 'auto';
        media.muted = true;
        media.playsInline = true;
        media.onloadeddata = function () { finish(); };
      } else {
        media.alt = '';
        media.onload = function () { media.decode().then(function () { finish(); }, function () { finish(); }); };
      }
      slot.appendChild(media);
      media.src = item.src;
    });
  }

  function show(i) {
    if (i < 0 || i >= ids.length) return;
    clearTimeout(autoTimer);
    clearTimeout(skipTimer);
    var epoch = ++displayEpoch;
    preloadEpoch++;
    if (prepared && prepared.index === i) {
      var ready = prepared;
      prepared = null;
      activate(ready);
      return;
    }
    prepared = null;
    setMessage('Loading ' + (i + 1) + ' / ' + ids.length + '...');
    var slot = 1 - visibleSlot;
    var item = itemAt(i);
    prepareMedia(slots[slot], item).then(function (media) {
      if (epoch === displayEpoch) activate({ index: i, item: item, slot: slot, media: media });
    }, function (err) {
      if (epoch === displayEpoch) skip(i, err.message, epoch);
    });
  }
  function activate(slide) {
    var old = currentVideo();
    if (old) old.pause();
    slots[visibleSlot].classList.remove('active');
    slots[slide.slot].classList.add('active');
    visibleSlot = slide.slot;
    current = slide.item;
    index = slide.index;
    remainingMs = durationMs;
    startedAt = playing && !current.video ? Date.now() : null;
    setMessage('');
    if (current.video) {
      var v = slide.media;
      v.loop = looping;
      v.muted = muted;
      v.currentTime = 0;
      v.onended = function () { if (index === slide.index) advance(); };
      if (playing) playOrWait(v);
    } else {
      schedule();
    }
    updateControls();
    prefetch(index + 1);
  }
  function prefetch(i) {
    if (i >= ids.length) return;
    var epoch = ++preloadEpoch;
    var slot = 1 - visibleSlot;
    var item = itemAt(i);
    prepareMedia(slots[slot], item).then(function (media) {
      if (epoch === preloadEpoch) prepared = { index: i, item: item, slot: slot, media: media };
    }, function () {});
  }
  function skip(i, text, epoch) {
    setMessage(text + ' Skipping...');
    skipTimer = setTimeout(function () {
      if (epoch !== displayEpoch) return;
      if (i + 1 < ids.length) { show(i + 1); return; }
      pause();
      setMessage(text);
    }, 700);
  }

  function schedule() {
    clearTimeout(autoTimer);
    if (!dlg.open || !current || current.video || !playing) return;
    autoTimer = setTimeout(advance, remainingMs - (Date.now() - startedAt));
  }
  function advance() {
    if (!dlg.open || !playing) return;
    if (index + 1 >= ids.length) { pause(); return; }
    show(index + 1);
  }
  function pause() {
    if (!playing) return;
    if (startedAt !== null) remainingMs = Math.max(0, remainingMs - (Date.now() - startedAt));
    startedAt = null;
    playing = false;
    clearTimeout(autoTimer);
    var v = currentVideo();
    if (v) v.pause();
    updateControls();
  }
  function resume() {
    playing = true;
    if (current && !current.video) startedAt = Date.now();
    var v = currentVideo();
    if (v) playOrWait(v);
    schedule();
    updateControls();
  }
  // An AbortError is a play() that lost to a pause, a load or the end of a
  // short clip; only the browser refusing to play waits for Space.
  function playOrWait(v) {
    var at = index;
    v.play().catch(function (err) {
      if (err.name !== 'NotAllowedError' || index !== at) return;
      pause();
      setMessage('Press Space to play the video.');
    });
  }
  function go(delta) {
    var target = Math.max(0, Math.min(ids.length - 1, index + delta));
    if (index >= 0 && target !== index) show(target);
  }
  function restart() {
    var v = currentVideo();
    if (!v) return;
    v.currentTime = 0;
    if (playing) v.play().catch(function () {});
  }
  function toggleLoop() {
    looping = !looping;
    var v = currentVideo();
    if (v) v.loop = looping;
    updateControls();
  }
  function toggleMuted() {
    muted = !muted;
    var v = currentVideo();
    if (v) v.muted = muted;
    updateControls();
  }
  function toggleFullscreen() {
    if (document.fullscreenElement) {
      leavingFullscreen = true;
      document.exitFullscreen().catch(function () {});
    } else {
      enterFullscreen();
    }
  }
  // A close can land while the request is pending, and teardown then has no
  // fullscreen to leave.
  function enterFullscreen() {
    document.documentElement.requestFullscreen().then(function () {
      if (!dlg.open) { document.exitFullscreen().catch(function () {}); return; }
      // The fullscreen root joins the top layer above the open dialog and
      // paints the page over the show; reopening puts the show back on top.
      restacks++;
      dlg.close();
      dlg.showModal();
    }, function () {});
  }

  function updateControls() {
    var isVideo = !!(current && current.video);
    position.textContent = (index + 1) + ' / ' + ids.length;
    dlg.querySelector('.slideshow-play-label').textContent = playing ? 'Pause' : 'Play';
    dlg.querySelectorAll('[data-slideshow="first"], [data-slideshow="previous"]').forEach(function (b) { b.disabled = index <= 0; });
    dlg.querySelectorAll('[data-slideshow="next"], [data-slideshow="last"]').forEach(function (b) { b.disabled = index >= ids.length - 1; });
    button('restart').disabled = !isVideo;
    var loop = button('loop');
    loop.setAttribute('aria-pressed', String(looping));
    loop.textContent = looping ? 'Loop: on' : 'Loop: off';
    var mute = button('mute');
    mute.setAttribute('aria-pressed', String(!muted));
    mute.textContent = muted ? 'Sound: off' : 'Sound: on';
  }

  function teardown() {
    displayEpoch++;
    preloadEpoch++;
    clearTimeout(autoTimer);
    clearTimeout(skipTimer);
    prepared = null;
    current = null;
    slots.forEach(clearSlot);
    if (document.fullscreenElement) document.exitFullscreen().catch(function () {});
  }
  function ensure() {
    if (dlg && dlg.isConnected) return true;
    dlg = document.getElementById('slideshow-dialog');
    if (!dlg) return false;
    slots = Array.from(dlg.querySelectorAll('.slideshow-slot'));
    message = document.getElementById('slideshow-message');
    position = document.getElementById('slideshow-position');
    durationSelect = document.getElementById('slideshow-duration');
    dlg.addEventListener('click', function (e) {
      var b = e.target.closest('[data-slideshow]');
      if (b) actions[b.dataset.slideshow]();
    });
    onSwipe(dlg.querySelector('.slideshow-stage'), actions.previous, actions.next);
    dlg.addEventListener('close', function () {
      if (restacks) { restacks--; return; }
      var id = index >= 0 ? ids[index] : 0;
      teardown();
      if (!id) return;
      if (detailStart) {
        if (id !== detailStart) window.location.href = '/images/' + id + detailBack;
        return;
      }
      var cards = Array.from(document.querySelectorAll('.thumb-card'));
      var card = document.querySelector('.thumb-card[data-id="' + id + '"]');
      if (card) { setFocused(cards, cards.indexOf(card)); return; }
      var params = new URLSearchParams(window.location.search);
      params.delete('page');
      params.set('at', id);
      window.location.href = '/?' + params + '#img-' + id;
    });
    durationSelect.addEventListener('change', function () {
      try { localStorage.setItem('monbooru_slideshow_duration', durationSelect.value); } catch (e) {}
      durationMs = parseInt(durationSelect.value, 10) * 1000;
      if (!current || current.video) return;
      remainingMs = durationMs;
      startedAt = playing ? Date.now() : null;
      schedule();
    });
    if (!documentBound) bindDocument();
    return true;
  }
  function bindDocument() {
    documentBound = true;
    // On the document: a button disabled under focus drops it out of the dialog.
    document.addEventListener('keydown', function (e) {
      if (!dlg || !dlg.open || chorded(e) || e.target.tagName === 'SELECT') return;
      var action = keys[e.key];
      if (!action) return;
      e.preventDefault();
      actions[action]();
    });
    // In fullscreen the browser keeps Esc to itself, so a fullscreen exit
    // the slideshow did not ask for is that Esc.
    document.addEventListener('fullscreenchange', function () {
      if (document.fullscreenElement || !dlg) return;
      if (dlg.open && !leavingFullscreen) dlg.close();
      leavingFullscreen = false;
    });
  }

  return {
    start: function (btn) {
      if (!ensure() || dlg.open) return;
      var flash = document.querySelector('[data-action-flash]');
      var startID = Number(btn.dataset.start) || 0;
      var firstID = startID;
      var params;
      if (startID) {
        params = new URLSearchParams({ scope: 'search', q: btn.dataset.q, sort: btn.dataset.sort, order: btn.dataset.order, seed: btn.dataset.seed, next: btn.dataset.next });
      } else {
        var focused = document.querySelector('.thumb-card.focused');
        if (focused) firstID = Number(focused.dataset.id);
        var scope = selectAllMatching || pickedIDs.length === 0 ? 'search' : 'selection';
        var parts = batchScopeParams(scope, flash);
        if (!parts) return;
        params = new URLSearchParams(['scope=' + scope].concat(parts).join('&'));
      }
      params.set('start', firstID);
      postForm('/internal/slideshow', params, {
        flashId: flash.id,
        onOK: function (res) {
          res.json().then(function (data) {
            if (!data.ids.length) { setFlashText(flash, 'warn', 'No images to play.'); return; }
            ids = data.ids;
            videos = new Set(data.videos);
            index = -1;
            current = null;
            prepared = null;
            visibleSlot = 0;
            playing = false;
            muted = true;
            looping = false;
            durationMs = readDuration() * 1000;
            durationSelect.value = String(durationMs / 1000);
            updateControls();
            document.querySelectorAll('.detail-video').forEach(function (v) { v.pause(); });
            dlg.showModal();
            enterFullscreen();
            detailStart = startID;
            detailBack = btn.dataset.back || '';
            show(data.start);
          });
        }
      });
    },
  };
})();

function openReaderJumpDialog() {
  var dlg = document.getElementById('reader-jump-dialog');
  if (!dlg) return false;
  dlg.showModal();
  var inp = document.getElementById('reader-jump-input');
  if (inp) { inp.focus(); inp.select(); }
  return true;
}

function handleReaderKey(e) {
  var reader = document.getElementById('reader');
  if (!reader || chorded(e)) return false;
  if (e.target.tagName.toLowerCase() === 'input') return false;
  if (document.querySelector('dialog[open]')) return false;
  var page = parseInt(reader.dataset.page, 10) || 1;
  var total = parseInt(reader.dataset.total, 10) || 1;
  var imgID = reader.dataset.imageId;
  var detail = reader.dataset.detailUrl || ('/images/' + imgID);
  function go(p) {
    if (p < 1) p = 1; if (p > total) p = total;
    if (p === page) return;
    // Keep the current query: without from=pages the next Esc lands on
    // the detail page, not the pages grid.
    var url = new URL(window.location.href);
    url.searchParams.set('page', String(p));
    url.hash = '';
    window.location.href = url.pathname + url.search;
  }
  switch (e.key) {
    case 'ArrowRight': case 'l': case ' ':
      e.preventDefault(); go(page + 1); return true;
    case 'ArrowLeft': case 'h':
      e.preventDefault(); go(page - 1); return true;
    case 'Home':
      e.preventDefault(); go(1); return true;
    case 'End':
      e.preventDefault(); go(total); return true;
    case 'p':
      if (openReaderJumpDialog()) e.preventDefault();
      return true;
    case 'P':
      var pagesLink = document.querySelector('.reader-pages');
      if (pagesLink && pagesLink.href) {
        e.preventDefault();
        window.location.href = pagesLink.href;
        return true;
      }
      return false;
    case 'o':
      e.preventDefault();
      var openLink = document.querySelector('.reader-open');
      if (openLink) openLink.click();
      return true;
    case 'e':
      var extractForm = document.querySelector('.reader-extract');
      if (extractForm) {
        e.preventDefault();
        extractForm.requestSubmit();
        return true;
      }
      return false;
    case 'v':
      var pageLink = document.querySelector('.reader-page-link');
      if (pageLink) {
        e.preventDefault();
        lightbox.open(null, pageLink.href);
        return true;
      }
      return false;
    case 'Escape': case 'Backspace':
      e.preventDefault();
      window.location.href = detail;
      return true;
  }
  return false;
}

document.addEventListener('keydown', function(e) {
  var tag = e.target.tagName.toLowerCase();
  var isInput = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;

  if (lightbox.handleKey(e)) return;
  if (!isInput && !chorded(e) && chordStep(e)) return;
  if (handleReaderKey(e)) return;
  if (handlePagesGridKey(e)) return;

  if (e.key === 'Escape') {
    if (isInput) { e.target.blur(); return; }
    if (document.querySelector('dialog[open]')) return;
    if (selectionArmed()) {
      e.preventDefault();
      selectMode = false;
      clearSelection();
      return;
    }
    if (document.body.classList.contains('tag-focus') || focusedTagRow()) {
      e.preventDefault();
      exitTagFocusMode();
      return;
    }
    if (isDetailPage() || document.getElementById('pages-grid-page')) {
      e.preventDefault();
      detailBack();
      return;
    }
    // A page handler may already have used this Esc, e.g. to close the
    // compare slider.
    if (e.defaultPrevented) return;
    var escBackEl = document.querySelector('[data-esc-back]');
    if (escBackEl) {
      var dest = escBackEl.dataset.escBack;
      if (dest) {
        e.preventDefault();
        window.location.href = dest;
        return;
      }
    }
    return;
  }

  if (!isInput && document.querySelector('dialog[open]')) {
    if (e.key === '?' && document.getElementById('shortcuts-help') && document.getElementById('shortcuts-help').open) {
      e.preventDefault();
      document.getElementById('shortcuts-help').close();
    }
    return;
  }

  if (isInput) return;

  if ((e.ctrlKey || e.metaKey) && (e.key === 'a' || e.key === 'A')) {
    if (document.querySelector('.thumb-checkbox')) {
      e.preventDefault();
      selectAll();
      return;
    }
    var tagPageBox = document.getElementById('tag-select-page');
    if (tagPageBox) {
      e.preventDefault();
      if (!tagPageBox.checked) tagPageBox.click();
    }
    return;
  }

  if (chorded(e)) return;

  if (e.key === '?') {
    var helpDlg = document.getElementById('shortcuts-help');
    if (helpDlg) {
      e.preventDefault();
      if (helpDlg.open) helpDlg.close();
      else openShortcutSheet();
    }
    return;
  }

  if (e.key === 's' || e.key === '/') {
    if (focusFirstSelector(['#search-input', '#sidebar-inner input[name="q"]'])) {
      e.preventDefault();
      return;
    }
  }

  // Clicks the toggle even where CSS hides it: its handler picks the job
  // by viewport width.
  if (e.key === 'b') {
    if (clickFirstSelector(['#sidebar-toggle'])) { e.preventDefault(); return; }
  }

  if (e.key === 'Y') {
    var syncForm = document.querySelector('form[hx-post="/internal/sync"]');
    if (syncForm) { e.preventDefault(); syncForm.requestSubmit ? syncForm.requestSubmit() : syncForm.submit(); return; }
  }

  if (e.key === ',') { if (cycleRatingCeiling(-1)) { e.preventDefault(); return; } }
  if (e.key === '.') { if (cycleRatingCeiling(+1)) { e.preventDefault(); return; } }

  if (e.key === '\\') {
    var swDlg = document.getElementById('gallery-switch-dialog');
    if (swDlg) { e.preventDefault(); swDlg.showModal(); return; }
  }

  if (e.key === 'g') {
    e.preventDefault();
    enterChord(chordMap.g, 'g');
    return;
  }

  if (e.key === 'e' && isDetailPage()) {
    e.preventDefault();
    enterChord(chordMap.e, 'e');
    return;
  }

  if (e.key === 'x') {
    if (chordDigitTargets(chordMap.x).length) {
      e.preventDefault();
      enterChord(chordMap.x, 'x');
      return;
    }
  }

  if (e.key === 't' && (isTagsPage() || isTagDetailPage())) {
    e.preventDefault();
    enterChord(chordMap.t, 't');
    return;
  }

  if (isTagsPage()) {
    if (e.key === 'n') {
      if (clickFirstSelector(['#btn-create-tag'])) { e.preventDefault(); return; }
    }
    if (e.key === 'N') {
      if (clickFirstSelector(['#btn-create-alias'])) { e.preventDefault(); return; }
    }
  }

  if (handlePaginationKey(e)) { e.preventDefault(); return; }

  if (isCategoriesPage() && e.key === 'n') {
    if (focusFirstSelector(['.add-cat-form input[name="name"]'])) { e.preventDefault(); return; }
  }

  // Indexed off the rendered nav so a profile-gated section cannot shift
  // the digits.
  if (isSettingsPage() && /^[0-9]$/.test(e.key)) {
    var nth = e.key === '0' ? 9 : parseInt(e.key, 10) - 1;
    var link = document.querySelectorAll('.settings-nav a')[nth];
    var sec = link && document.querySelector(link.getAttribute('href'));
    if (sec) { e.preventDefault(); sec.scrollIntoView({ block: 'start' }); return; }
  }

  if (e.key === 'f') {
    if (batchBarVisible()) { e.preventDefault(); openBatchDialog('batch-fav', 'selection'); return; }
    if (clickFirstSelector(['.btn-fav'])) { e.preventDefault(); return; }
  }

  if (e.key === 'a') {
    if (batchBarVisible()) { e.preventDefault(); openTagDialog('selection'); return; }
    var tagInput = document.getElementById('tag-input');
    if (tagInput) { e.preventDefault(); tagInput.focus(); return; }
  }

  if (e.key === 'r') {
    if (batchBarVisible()) { e.preventDefault(); openStripDialog('selection'); return; }
    if (isDetailPage()) { e.preventDefault(); enterTagFocusMode(); return; }
  }

  if (e.key === 'R') {
    var readBtn = document.querySelector('.btn-manga-action.btn-read');
    if (readBtn) { e.preventDefault(); window.location.href = readBtn.href; return; }
    var firstPage = document.querySelector('#pages-grid-page #page-1 a');
    if (firstPage) { e.preventDefault(); window.location.href = firstPage.href; return; }
  }
  if (e.key === 'P' && isDetailPage()) {
    var pagesBtn = document.querySelector('.btn-manga-action.btn-pages');
    if (pagesBtn) { e.preventDefault(); window.location.href = pagesBtn.href; return; }
  }
  if ((e.key === '[' || e.key === ']') && isDetailPage()) {
    var step = document.querySelector('.coll-nav-keyed .coll-step[data-dir="' + (e.key === '[' ? 'prev' : 'next') + '"]');
    if (step) { e.preventDefault(); window.location.href = step.href; return; }
  }
  if (e.key === 'C' && isDetailPage()) {
    if (clickFirstSelector(['.btn-manga-action.btn-generate-collection'])) { e.preventDefault(); return; }
  }

  if (e.key === 'd' && isDetailPage()) {
    if (clickFirstSelector(['.detail-actions a[download]'])) { e.preventDefault(); return; }
  }

  if (e.key === 'B' && isDetailPage()) {
    var modeBtn = document.querySelector('.tag-mode-toggle');
    if (modeBtn) { e.preventDefault(); revealSidebarFor(modeBtn); modeBtn.click(); return; }
  }

  if (e.key === 'v' && isDetailPage()) {
    var lbTrigger = document.querySelector('.detail-img-link');
    if (lbTrigger) { e.preventDefault(); lightbox.open(null, lbTrigger.href); return; }
  }

  if (e.key === 't') {
    if (batchBarVisible()) {
      if (!document.querySelector('#batch-bar.visible .btn-autotag:not([disabled])')) return;
      e.preventDefault(); openBatchAutotagDialog('selection'); return;
    }
    if (isDetailPage()) {
      if (clickFirstSelector(['.btn-autotag:not([disabled])'])) { e.preventDefault(); return; }
    }
  }

  if (e.key === 'm') {
    if (batchBarVisible()) { e.preventDefault(); openBatchPlaceDialog('selection'); return; }
    if (isDetailPage() && document.getElementById('place-image-dialog')) {
      e.preventDefault(); openPlaceImageDialog(); return;
    }
  }

  if (e.key === 'i') {
    if (batchBarVisible()) { e.preventDefault(); openBatchDialog('batch-inbox', 'selection'); return; }
    if (isDetailPage()) {
      if (clickFirstSelector(['.btn-inbox'])) { e.preventDefault(); return; }
    }
  }

  if (e.key === 'c' && batchBarVisible()) { e.preventDefault(); openBatchCollectionDialog('selection'); return; }

  if (e.key === 'd' && batchBarVisible()) { e.preventDefault(); openBatchDownloadDialog('selection'); return; }

  if (e.key === 'T' && batchBarVisible()) {
    if (clickFirstSelector(['#batch-bar.visible #batch-transfer'])) { e.preventDefault(); return; }
  }

  // Not gated on a selection: invert and select-all-matching are used
  // with nothing picked yet.
  if (e.key === 'I' && selectionArmed()) {
    if (clickFirstSelector(['#batch-invert'])) { e.preventDefault(); return; }
  }

  if (e.key === 'A') {
    if (clickFirstSelector(['#batch-select-all-matching:not([hidden])'])) { e.preventDefault(); return; }
  }

  if (e.key === 'L') {
    if (batchBarVisible()) {
      if (document.querySelector('#batch-bar.visible .peer-accent:not([disabled])')) {
        e.preventDefault(); openBatchLookupDialog('selection'); return;
      }
    }
    if (clickFirstSelector(['.add-tag-form .btn-hash-lookup:not([disabled])'])) { e.preventDefault(); return; }
  }

  if (e.key === 'w') {
    if (clickFirstSelector(['#slideshow-btn'])) { e.preventDefault(); return; }
  }

  if (isGalleryPage()) {
    if (e.key === 'F') {
      if (clickFirstSelector(['#fav-filter-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === 'R') {
      if (clickFirstSelector(['#random-sort-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === 'V') {
      if (clickFirstSelector(['#select-mode-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === '-' || e.key === '=') {
      if (stepThumbSize(e.key === '=' ? 1 : -1)) { e.preventDefault(); return; }
    }
    if (e.key === 'O') { if (cycleSort()) { e.preventDefault(); return; } }
    if (e.key === 'D') { if (flipSortDirection()) { e.preventDefault(); return; } }
    if (e.key === 'S' && document.querySelector('#batch-bar:not(.visible)')) {
      if (openSaveSearchDialog()) { e.preventDefault(); return; }
    }
    if (e.key === 'Home') { if (jumpGridCursor('first')) { e.preventDefault(); return; } }
    if (e.key === 'End')  { if (jumpGridCursor('last'))  { e.preventDefault(); return; } }
  }

  if (e.key === 'Delete' || e.key === 'Del') {
    if (batchBarVisible()) { e.preventDefault(); openBatchDeleteDialog('selection'); return; }
    if (clickFirstSelector([
      '#delete-image-btn',
      '#tag-batch-bar:not([hidden]) #btn-batch-delete',
      '#btn-detail-delete',
    ])) { e.preventDefault(); return; }
  }

  if (e.key === ' ') {
    var vid = document.querySelector('.detail-video');
    if (vid) {
      e.preventDefault();
      if (vid.paused) vid.play(); else vid.pause();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused');
    if (focusedCard) {
      var cb = focusedCard.querySelector('.thumb-checkbox');
      if (cb) {
        e.preventDefault();
        cb.checked = !cb.checked;
        pickCheckbox(cb);
        writeSelection();
        updateBatchBar();
      }
      return;
    }
    // A click, not .checked: the tags page wires selection in its own script.
    var focusedRow = document.querySelector('.tag-row.focused');
    if (focusedRow) {
      var rowBox = focusedRow.querySelector('.tag-select');
      if (rowBox) { e.preventDefault(); rowBox.click(); }
    }
    return;
  }

  if (e.key === 'Backspace' && (isDetailPage() || document.getElementById('pages-grid-page'))) {
    e.preventDefault();
    detailBack();
    return;
  }
  if (e.key === 'Backspace' && isTagDetailPage()) {
    var tdBack = document.getElementById('tag-detail-page');
    e.preventDefault();
    window.location.href = (tdBack && tdBack.dataset.escBack) || '/tags';
    return;
  }

  if (e.key === 'o') {
    if (isDetailPage()) {
      var dlA = document.querySelector('.detail-actions a[download]');
      if (dlA) { e.preventDefault(); window.open(dlA.href, '_blank'); return; }
    } else {
      var foc = document.querySelector('.thumb-card.focused a');
      if (foc) { e.preventDefault(); window.location.href = foc.href; return; }
    }
  }

  var navStep = {
    h: [-1, 0], l: [1, 0], k: [0, -1], j: [0, 1],
    ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1],
  }[e.key];
  if (navStep) {
    var dx = navStep[0], dy = navStep[1];
    var forward = dx + dy > 0;
    var arrow = e.key.startsWith('Arrow');
    if (focusedTagRow()) {
      e.preventDefault();
      cycleTagFocus(forward ? 1 : -1);
      return;
    }
    var kind = isDetailPage() ? 'image' : isTagDetailPage() ? 'tag' : '';
    if (kind) {
      // Up and Down arrows keep their native scroll; j and k step
      // prev/next like h and l.
      if ((!arrow || dy === 0) && navDetailArrow(forward ? 'next' : 'prev', kind)) {
        e.preventDefault();
        return;
      }
      if (arrow) return;
    } else if (isGalleryPage() && moveGridCursor(dx, dy)) {
      e.preventDefault();
      return;
    } else if (isTagsPage() && dy !== 0 && moveTagRowCursor(dy)) {
      e.preventDefault();
      return;
    }
  }

  if (e.key === 'Enter') {
    var focusedTag = focusedTagRow();
    if (focusedTag) {
      e.preventDefault();
      var idx = tagFocusRows().indexOf(focusedTag);
      if (idx >= 0) document.body.dataset.tagFocusIdx = String(idx);
      var tagBtn = focusedTag.querySelector('.tag-entry-remove');
      if (tagBtn) tagBtn.click();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused a');
    if (focusedCard) { window.location.href = focusedCard.href; return; }
    var tagLink = document.querySelector('.tag-row.focused .tag-name-cell a');
    if (tagLink) { window.location.href = tagLink.href; return; }
  }
});

document.addEventListener('click', function(e) {
  var link = e.target.closest('.back-link');
  if (!link) return;
  if (detailRefBack()) e.preventDefault();
});

// history.back, not HX-Redirect: a redirect pushes a fresh entry and
// drops the ref chain behind it.
document.body.addEventListener('delete-go-back', function(e) {
  if (history.length > 1) { history.back(); return; }
  var fallback = e.detail && e.detail.fallback;
  if (fallback) window.location.href = fallback;
});

// The modal also keeps a second click off the button while the diff loads.
function ptrContribOpen() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) load.showModal();
}

// A loading modal already closed means Escape cancelled the wait, so the
// answer is dropped.
function ptrContribOpened() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) return;
  if (load) load.close();
  var dlg = document.getElementById('ptr-contrib-dialog');
  if (dlg) dlg.showModal();
  else htmx.trigger(document.body, 'ptr-contrib-closed');
}

function ptrContribRefresh(form) {
  var counts = [];
  form.querySelectorAll('.ptr-hunk-wrap').forEach(function(w) {
    var boxes = w.querySelectorAll('.ptr-row input[type="checkbox"]');
    var n = w.querySelectorAll('.ptr-row input[type="checkbox"]:checked').length;
    counts.push(n);
    var live = w.querySelector('.ptr-live');
    if (live) live.textContent = n + '/' + boxes.length;
    var box = w.querySelector('[data-reason]');
    if (box) {
      var was = box.hidden;
      box.hidden = n === 0;
      var input = box.querySelector('input');
      if (input) input.required = n > 0;
      if (n > 0 && was && input) input.focus();
    }
  });
  var adds = counts[0] || 0, pets = counts[1] || 0;
  var noun = form.dataset.noun, parts = [];
  if (adds) parts.push(form.dataset.verb + ' ' + adds + ' ' + noun + (adds > 1 ? 's' : ''));
  if (pets) parts.push('petition the removal of ' + pets + ' ' + noun + (pets > 1 ? 's' : ''));
  var summary = form.querySelector('#ptr-contrib-summary');
  if (summary) summary.textContent = parts.length ? 'asks to ' + parts.join(' and ') + '.' : 'nothing selected yet.';
  var send = form.querySelector('#ptr-contrib-send');
  if (send) send.disabled = (adds + pets) === 0;
}

document.body.addEventListener('change', function(e) {
  var form = e.target.closest && e.target.closest('#ptr-contrib-form');
  if (form) ptrContribRefresh(form);
});

document.addEventListener('click', function(e) {
  var form = e.target.closest('#ptr-contrib-form');
  if (!form) return;
  var bulk = e.target.closest('[data-bulk]');
  if (bulk) {
    bulk.closest('.ptr-hunk-wrap').querySelectorAll('label.ptr-row input[type="checkbox"]').forEach(function(c) {
      c.checked = bulk.dataset.bulk === 'all';
    });
    ptrContribRefresh(form);
    return;
  }
  // Keyed on the label: the click it forwards to its checkbox carries no
  // modifier flags.
  var row = e.target.closest('label.ptr-row');
  if (!row) return;
  var wrap = row.closest('.ptr-hunk-wrap');
  var rows = Array.prototype.slice.call(wrap.querySelectorAll('label.ptr-row'));
  var i = rows.indexOf(row);
  var last = wrap.ptrContribAnchor;
  if (e.shiftKey && last !== undefined && last !== i) {
    e.preventDefault();
    var state = rows[last].querySelector('input[type="checkbox"]').checked;
    var from = Math.min(last, i), to = Math.max(last, i);
    for (var k = from; k <= to; k++) {
      rows[k].querySelector('input[type="checkbox"]').checked = state;
    }
    ptrContribRefresh(form);
  }
  wrap.ptrContribAnchor = i;
});

// Captured: a dialog's close event does not bubble.
document.addEventListener('close', function(e) {
  if (e.target.id === 'ptr-contrib-dialog') htmx.trigger(document.body, 'ptr-contrib-closed');
}, true);

function restoreGalleryFocusFromHash() {
  var m = window.location.hash.match(/^#img-(\d+)$/);
  if (!m) return;
  var card = document.querySelector('.thumb-card[data-id="' + m[1] + '"]');
  if (!card) return;
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  setFocused(cards, cards.indexOf(card));
}
document.addEventListener('DOMContentLoaded', restoreGalleryFocusFromHash);

document.addEventListener('mouseover', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  const hoverSrc = card.dataset.hover;
  if (!img || !hoverSrc || img.dataset.hovering) return;
  img.dataset.orig = img.src;
  img.dataset.hovering = '1';
  img.onerror = function() {
    img.src = img.dataset.orig || '';
    delete img.dataset.orig;
    delete img.dataset.hovering;
    img.onerror = null;
  };
  img.src = hoverSrc;
});

document.addEventListener('mouseout', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card || (e.relatedTarget && card.contains(e.relatedTarget))) return;
  const img = card.querySelector('.thumb-img');
  if (!img || !img.dataset.orig) return;
  img.src = img.dataset.orig;
  delete img.dataset.orig;
  delete img.dataset.hovering;
  img.onerror = null;
});

document.addEventListener('input', function(e) {
  if (e.target.id !== 'tag-input') return;
  var tagsDiv = document.getElementById('image-tags');
  if (!tagsDiv) return;
  var err = tagsDiv.querySelector('.flash-err');
  if (err) err.remove();
});

// The live search leaves the sidebar's filter links on a stale q, and htmx
// fixes a boosted link's path at process time, so it is rewritten per request.
function sidebarFilterQ(url) {
  const input = document.querySelector('#sidebar-inner form input[name=q]');
  if (!input) return null;
  url.searchParams.set('q', input.value);
  return url.pathname + '?' + url.searchParams.toString();
}

document.body.addEventListener('htmx:configRequest', function(e) {
  if (activeGallery()) e.detail.headers['X-Monbooru-Gallery'] = activeGallery();
});

document.body.addEventListener('htmx:configRequest', function(e) {
  const elt = e.detail.elt;
  if (!elt || !elt.matches || !elt.matches('#sidebar-inner a.filter-btn')) return;
  const path = sidebarFilterQ(new URL(e.detail.path, window.location.origin));
  if (path) e.detail.path = path;
});

// A Back restores markup, not values: the boxes a swap never re-renders
// would keep the first page's query.
document.body.addEventListener('htmx:historyRestore', function() {
  const params = new URLSearchParams(window.location.search);
  const form = document.getElementById('search-form');
  if (form && document.getElementById('gallery-grid')) {
    const input = document.getElementById('search-input');
    if (input) input.value = params.get('q') || '';
    const orderEl = form.querySelector('select[name="order"]');
    const order = params.get('order');
    if (orderEl && (order === 'asc' || order === 'desc')) orderEl.value = order;
    let seedEl = form.querySelector('input[name="seed"]');
    const seed = params.get('seed');
    if (seed && !seedEl) {
      seedEl = document.createElement('input');
      seedEl.type = 'hidden';
      seedEl.name = 'seed';
      form.appendChild(seedEl);
    }
    if (seed) seedEl.value = seed;
    else if (seedEl) seedEl.remove();
    readSelection();
    syncSelectionBoxes();
    updateBatchBar();
    pruneSelection();
    initInboxUpload();
  }
  ['tags-q-input', 'collections-q-input'].forEach(function(id) {
    const el = document.getElementById(id);
    if (el) el.value = params.get('q') || '';
  });
});

// htmx does not swap error responses, but a job-conflict 409's body is
// the inline flash.
document.body.addEventListener('htmx:beforeSwap', function(e) {
  const xhr = e.detail.xhr;
  if (!xhr || xhr.status !== 409) return;
  if (xhr.getResponseHeader('X-Monbooru-Gallery')) {
    showPageNotice(xhr.responseText);
    return;
  }
  if (!/^text\/html/.test(xhr.getResponseHeader('Content-Type') || '')) e.detail.serverResponse = escapeHTML(xhr.responseText);
  e.detail.shouldSwap = true;
});

document.addEventListener('input', function(e) {
  if (!e.target.matches || !e.target.matches('#sidebar-inner form input[name=q]')) return;
  document.querySelectorAll('#sidebar-inner a.filter-btn').forEach(function(a) {
    const href = sidebarFilterQ(new URL(a.getAttribute('href'), window.location.origin));
    if (href) a.setAttribute('href', href);
  });
});

// The button's term is category:name, but the query may hold the bare
// name, so both forms are removed.
document.addEventListener('click', function(e) {
  const btn = e.target.closest('.tag-add-btn');
  if (!btn) return;
  e.preventDefault();
  const tagName = btn.dataset.tag;
  if (!tagName) return;
  const si = document.getElementById('search-input');
  if (!si) return;
  const colon = tagName.indexOf(':');
  const bare = colon >= 0 ? tagName.slice(colon + 1) : tagName;
  const terms = si.value.trim().split(/\s+/).filter(Boolean);
  const filtered = terms.filter(t => t !== tagName && t !== bare);
  if (filtered.length === terms.length) filtered.push(tagName);
  si.value = filtered.join(' ');
  submitSearch();
});

document.addEventListener('click', function(e) {
  const header = e.target.closest('.tag-group-header');
  if (!header) return;
  const group = header.closest('.tag-group');
  if (!group) return;
  const list = group.querySelector('.tag-list-sidebar');
  if (!list) return;
  const indicator = group.querySelector('.cat-collapse');
  const collapsed = list.style.display === 'none';
  list.style.display = collapsed ? '' : 'none';
  if (indicator) indicator.textContent = collapsed ? '▾' : '▸';
});

// Blurred so the keydown router, which ignores keys while an input has
// focus, still sees the next shortcut.
document.addEventListener('change', function(e) {
  if (!e.target.classList.contains('thumb-checkbox')) return;
  pickCheckbox(e.target);
  writeSelection();
  updateBatchBar();
  e.target.blur();
});

function forEachClusterCheckbox(header, fn) {
  var node = header.nextElementSibling;
  while (node && !node.classList.contains('thumb-cluster-header')) {
    var cb = node.querySelector ? node.querySelector('.thumb-checkbox') : null;
    if (cb) fn(cb);
    node = node.nextElementSibling;
  }
}

document.addEventListener('click', function(e) {
  var sel = e.target.closest('[data-cluster-select]');
  var uns = e.target.closest('[data-cluster-unselect]');
  if (!sel && !uns) return;
  e.preventDefault();
  var header = (sel || uns).closest('.thumb-cluster-header');
  if (!header) return;
  var target = !!sel;
  forEachClusterCheckbox(header, function(cb) { cb.checked = target; pickCheckbox(cb); });
  writeSelection();
  updateBatchBar();
});

// A cluster's batch may run past this page, so its ids come from a search
// of its own range.
document.addEventListener('click', function(e) {
  var btn = e.target.closest ? e.target.closest('[data-cluster-all]') : null;
  if (!btn) return;
  e.preventDefault();
  btn.disabled = true;
  fetch('/internal/search/ids?q=' + encodeURIComponent(btn.dataset.clusterAll), {
    headers: {'Accept': 'application/json'},
  }).then(function(res) {
    if (sessionLost(res)) {
      showLoginNotice();
      return null;
    }
    if (!res.ok) throw new Error('ids');
    return res.json();
  }).then(function(data) {
    if (!data) return;
    var ids = data.ids.map(String);
    ids.forEach(function(v) {
      if (pickedIDs.indexOf(v) === -1) pickedIDs.push(v);
    });
    var header = btn.closest('.thumb-cluster-header');
    if (header) header.clusterAllIDs = ids;
    selectAllMatching = false;
    writeSelection();
    syncSelectionBoxes();
    updateBatchBar();
    if (data.truncated) {
      setFlashText(document.getElementById('gallery-flash'), 'err',
        'Batch too large: the first ' + ids.length + ' are selected.');
    }
  }).catch(function() {
    setFlashText(document.getElementById('gallery-flash'), 'err', 'Could not read the batch.');
  }).finally(function() {
    btn.disabled = false;
  });
});

var lastPickedCard = null;

// Ctrl/Cmd toggles like a plain click rather than opening a tab; Alt
// belongs to the band, which reads it as subtract.
document.addEventListener('click', function(e) {
  if (e.button !== 0 || e.altKey) return;
  var grid = document.getElementById('gallery-grid');
  if (!grid || !grid.classList.contains('batch-active')) return;
  var link = e.target.closest('.thumb-link');
  if (!link) return;
  var card = link.closest('.thumb-card');
  if (!card) return;
  var cb = card.querySelector('.thumb-checkbox');
  if (!cb) return;
  e.preventDefault();
  var cards = Array.prototype.slice.call(grid.querySelectorAll('.thumb-card'));
  var to = cards.indexOf(card);
  var from = lastPickedCard ? cards.indexOf(lastPickedCard) : -1;
  if (e.shiftKey && from !== -1 && from !== to) {
    var want = !cb.checked;
    cards.slice(Math.min(from, to), Math.max(from, to) + 1).forEach(function(c) {
      var box = c.querySelector('.thumb-checkbox');
      if (!box) return;
      box.checked = want;
      pickCheckbox(box);
    });
  } else {
    cb.checked = !cb.checked;
    pickCheckbox(cb);
  }
  lastPickedCard = card;
  writeSelection();
  updateBatchBar();
});

// A drag only adds (Alt subtracts), so an accidental drag cannot wipe a
// selection.
var marquee = null;

function marqueeCells(grid, origin) {
  return Array.prototype.map.call(grid.querySelectorAll('.thumb-card'), function(card) {
    var r = card.getBoundingClientRect();
    return {
      cb: card.querySelector('.thumb-checkbox'),
      x: r.left - origin.left, y: r.top - origin.top, w: r.width, h: r.height,
    };
  });
}

function marqueePaint() {
  var m = marquee;
  var x = Math.min(m.x0, m.x1), y = Math.min(m.y0, m.y1);
  var w = Math.abs(m.x1 - m.x0), h = Math.abs(m.y1 - m.y0);
  if (!m.box) {
    m.box = document.createElement('div');
    m.box.className = 'marquee';
    m.grid.appendChild(m.box);
  }
  m.box.style.left = x + 'px';
  m.box.style.top = y + 'px';
  m.box.style.width = w + 'px';
  m.box.style.height = h + 'px';
  m.cells.forEach(function(c, i) {
    if (!c.cb) return;
    var hit = !(c.x > x + w || c.x + c.w < x || c.y > y + h || c.y + c.h < y);
    var on = m.mode === 'add' ? (m.was[i] || hit) : (m.was[i] && !hit);
    if (c.cb.checked !== on) {
      c.cb.checked = on;
      pickCheckbox(c.cb);
    }
  });
  updateBatchBar();
}

document.addEventListener('mousedown', function(e) {
  if (e.button !== 0 || window.matchMedia('(pointer: coarse)').matches) return;
  var grid = e.target.closest('.thumb-grid');
  // Links are fair game, or the band could only start in the gaps between
  // cards.
  if (!grid || e.target.closest('button, input')) return;
  var origin = grid.getBoundingClientRect();
  marquee = {
    grid: grid, box: null, moved: false,
    x0: e.clientX - origin.left, y0: e.clientY - origin.top,
    x1: e.clientX - origin.left, y1: e.clientY - origin.top,
    mode: e.altKey ? 'sub' : 'add',
    cells: marqueeCells(grid, origin),
  };
  marquee.was = marquee.cells.map(function(c) { return !!(c.cb && c.cb.checked); });
  // Without this the drag turns into a native link drag or a text selection.
  e.preventDefault();
  document.body.classList.add('marquee-active');
});

document.addEventListener('mousemove', function(e) {
  if (!marquee) return;
  // Re-read on every move: the grid may have scrolled under the pointer.
  var origin = marquee.grid.getBoundingClientRect();
  marquee.x1 = e.clientX - origin.left;
  marquee.y1 = e.clientY - origin.top;
  if (Math.abs(marquee.x1 - marquee.x0) > 4 || Math.abs(marquee.y1 - marquee.y0) > 4) {
    marquee.moved = true;
  }
  // Nothing is painted before the threshold, so a click's pixel or two of
  // movement stays a click.
  if (!marquee.moved) return;
  var content = document.getElementById('content');
  if (content) {
    var cr = content.getBoundingClientRect();
    if (e.clientY < cr.top + 40) content.scrollTop -= 12;
    else if (e.clientY > cr.bottom - 40) content.scrollTop += 12;
  }
  marqueePaint();
});

document.addEventListener('mouseup', function() {
  if (!marquee) return;
  var moved = marquee.moved;
  if (marquee.box) marquee.box.remove();
  marquee = null;
  document.body.classList.remove('marquee-active');
  writeSelection();
  updateBatchBar();
  // The release also fires a click on the card under it, which would undo
  // the band's last cell.
  if (moved) marqueeSwallowUntil = Date.now() + 100;
});

var marqueeSwallowUntil = 0;

document.addEventListener('click', function(e) {
  if (Date.now() >= marqueeSwallowUntil) return;
  if (!e.target.closest || !e.target.closest('.thumb-grid')) return;
  e.preventDefault();
  e.stopPropagation();
}, true);

// Null while the batch runs past this page and its ids have not been fetched.
function clusterAllIDs(header) {
  if (header.clusterAllIDs) return header.clusterAllIDs;
  if (!header.hasAttribute('data-cluster-whole')) return null;
  var ids = [];
  forEachClusterCheckbox(header, function(cb) { ids.push(cb.value); });
  return ids;
}

function updateClusterButtons() {
  document.querySelectorAll('.thumb-cluster-header[data-cluster-start]').forEach(function(header) {
    var total = 0, checked = 0;
    forEachClusterCheckbox(header, function(cb) {
      total++;
      if (cb.checked) checked++;
    });
    var sel = header.querySelector('[data-cluster-select]');
    var uns = header.querySelector('[data-cluster-unselect]');
    var all = header.querySelector('[data-cluster-all]');
    if (sel) sel.hidden = total > 0 && checked === total;
    if (uns) uns.hidden = checked === 0;
    if (all) {
      var ids = clusterAllIDs(header);
      all.hidden = !!ids && ids.length > 0 && ids.every(function(id) {
        return pickedIDs.indexOf(id) !== -1;
      });
    }
  });
}

// Ids, not checkboxes: a selection spans pages. Pick order is the order a
// numbering job walks, so a re-pick keeps its place.
var pickedIDs = [];
// When set, the scope is the query, re-resolved on the server, not the ids.
var selectAllMatching = false;
var selectMode = false;

// Read this, not selectMode: a selection arms the grid without the toggle.
function selectionArmed() {
  return selectMode || selectAllMatching || pickedIDs.length > 0;
}

// The toggle shows armed while anything is picked, so switching it off
// clears the selection.
function toggleSelectMode() {
  if (selectionArmed()) {
    selectMode = false;
    clearSelection();
    return;
  }
  selectMode = true;
  updateBatchBar();
}

// Does not persist: the caller writes once when done, so a whole page is
// one write.
function pickCheckbox(cb) {
  var at = pickedIDs.indexOf(cb.value);
  if (cb.checked) {
    if (at === -1) pickedIDs.push(cb.value);
  } else if (at !== -1) {
    pickedIDs.splice(at, 1);
  }
  // A single toggle narrows the pick, which the whole-search scope cannot
  // describe.
  selectAllMatching = false;
}

// Every box is set: across a reload a browser restores form state by
// position, which after rows drop lands on the wrong ones. A whole-search
// scope takes the ids on screen so a toggle can narrow from them.
function syncSelectionBoxes() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    var picked = pickedIDs.indexOf(cb.value) !== -1;
    if (selectAllMatching && !picked) {
      pickedIDs.push(cb.value);
      picked = true;
    }
    cb.checked = picked;
  });
}

function updateBatchBar() {
  const onPage = document.querySelectorAll('.thumb-checkbox:checked').length;
  const total = document.querySelectorAll('.thumb-checkbox').length;
  const active = selectAllMatching || pickedIDs.length > 0;
  // A pick enters the mode the toggle does, so None or an Invert that
  // empties the page leaves it up either way.
  if (active) selectMode = true;
  const armed = selectionArmed();
  const bar = document.getElementById('batch-bar');
  const pluginBar = document.getElementById('plugin-batch-bar');
  const grid = document.getElementById('gallery-grid');
  if (bar) {
    // Shown while armed, .visible only with a scope: the keyboard router
    // reads .visible.
    bar.hidden = !armed;
    bar.classList.toggle('visible', active);
  }
  // Plugins post a list of ids, so a whole-search scope leaves them unlit.
  if (pluginBar) {
    pluginBar.hidden = !armed;
    pluginBar.classList.toggle('visible', active && !selectAllMatching);
  }
  if (grid) grid.classList.toggle('batch-active', armed);
  const modeBtn = document.getElementById('select-mode-btn');
  if (modeBtn) modeBtn.setAttribute('aria-pressed', String(armed));
  const matching = matchingTotal();
  const countEl = document.getElementById('batch-count');
  if (countEl) {
    countEl.hidden = !active;
    countEl.textContent = selectAllMatching
      ? 'All ' + (matching === null ? '…' : matching) + ' matching selected'
      : pickedIDs.length + ' selected' +
        (pickedIDs.length > onPage ? ' · ' + onPage + ' on this page' : '');
  }
  // Offered only once the whole page is picked, as the step up from it.
  const escalate = document.getElementById('batch-select-all-matching');
  if (escalate) {
    escalate.textContent = 'Select all ' + matching + ' matching';
    escalate.hidden = selectAllMatching || total === 0 || onPage < total || matching <= total;
  }
  updateClusterButtons();
}

// Read off .result-count, which each htmx search swaps out of band; the
// batch bar is not re-rendered. null while a deferred count is loading.
function matchingTotal() {
  var el = document.querySelector('.result-count');
  var m = el && el.textContent.match(/(\d+)/);
  return m ? parseInt(m[1], 10) : null;
}

// A count that loads after the grid lands without a grid settle; a batch
// dialog opened before it still shows the ellipsis.
document.body.addEventListener('htmx:oobAfterSwap', function(e) {
  if (!e.detail || !e.detail.target || e.detail.target.id !== 'result-matches') return;
  updateBatchBar();
  document.querySelectorAll('dialog[open]').forEach(function(dlg) {
    var prefix = dlg.id.replace(/-dialog$/, '');
    var scopeEl = document.getElementById(prefix + '-scope');
    if (scopeEl && scopeEl.value === 'search') {
      scopeCount('search', document.getElementById(prefix + '-count'), document.getElementById(prefix + '-noun'));
    }
  });
});

function clearSelection() {
  pickedIDs = [];
  selectAllMatching = false;
  writeSelection();
  syncSelectionBoxes();
  updateBatchBar();
}

function selectAll() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = true;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// Flips the page only: picks on other pages are not on screen to reconsider.
function invertSelection() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = !cb.checked;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// Minutes, not seconds: a batch job's reload can come long after the click.
var selectionStashMs = 300000;

// Part of every stash key: ids are per gallery, and after a switch the
// same URL is another library.
function activeGallery() {
  return (document.body && document.body.dataset.gallery) || '';
}

// From the URL, not the box, which may hold an unsubmitted query. Sort
// and page stay out so paging keeps the pick.
function selectionKey() {
  return activeGallery() + location.pathname + '?q=' + (new URLSearchParams(location.search).get('q') || '');
}

function writeSelection() {
  writeStash('monbooru_selection', {ids: pickedIDs, all: selectAllMatching, url: selectionKey()});
}

function readSelection() {
  var stash = readStash('monbooru_selection', selectionStashMs);
  if (!stash || !stash.ids || stash.url !== selectionKey()) {
    pickedIDs = [];
    selectAllMatching = false;
    return;
  }
  pickedIDs = stash.ids;
  selectAllMatching = !!stash.all;
}

// A batch edit or another client can take a pick out of the listing while
// the URL that keys the stash stays the same. The rows on screen match, so
// only the others are asked about.
function pruneSelection() {
  if (selectAllMatching || !document.getElementById('gallery-grid')) return;
  var onScreen = {};
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) { onScreen[cb.value] = true; });
  var away = pickedIDs.filter(function(id) { return !onScreen[id]; });
  if (away.length === 0) return;
  var key = selectionKey();
  var q = new URLSearchParams(location.search).get('q') || '';
  var csrf = getCSRFToken();
  var headers = {
    'Accept': 'application/json',
    'Content-Type': 'application/x-www-form-urlencoded',
    'HX-Request': 'true',
    'X-CSRF-Token': csrf,
  };
  if (activeGallery()) headers['X-Monbooru-Gallery'] = activeGallery();
  fetch('/internal/search/ids', {
    method: 'POST',
    headers: headers,
    body: new URLSearchParams({q: q, ids: away.join(','), _csrf: csrf}).toString(),
  }).then(function(res) {
    return res.ok ? res.json() : null;
  }).then(function(data) {
    if (!data || selectAllMatching || key !== selectionKey()) return;
    var gone = {};
    away.forEach(function(id) { gone[id] = true; });
    data.ids.forEach(function(id) { delete gone[id]; });
    pickedIDs = pickedIDs.filter(function(id) { return !gone[id]; });
    writeSelection();
    updateBatchBar();
  }).catch(function() {});
}

// For the /tags rows only; the gallery persists its own selection on
// every change.
function stashSelection(ids) {
  writeStash('monbooru_tag_selection', {ids: ids, url: tagSelectionKey()});
}

function tagSelectionKey() {
  return activeGallery() + location.pathname + location.search;
}

function takeTagStash() {
  var stash = readStash('monbooru_tag_selection', selectionStashMs, true);
  if (!stash || !stash.ids || stash.url !== tagSelectionKey()) return [];
  return stash.ids;
}

document.addEventListener('DOMContentLoaded', function() {
  readSelection();
  var mode = readStash('monbooru_select_mode', 30000, true);
  if (mode && mode.url === selectionKey()) selectMode = true;
  syncSelectionBoxes();
  // Every box is set: across a reload a browser restores form state by
  // position.
  var tagIDs = takeTagStash();
  document.querySelectorAll('.tag-select').forEach(function(cb) {
    cb.checked = tagIDs.indexOf(cb.value) !== -1;
  });
  updateBatchBar();
  pruneSelection();
  if (typeof updateTagBatchBar === 'function') updateTagBatchBar();
});

document.addEventListener('click', function(e) {
  if (!e.target.closest || !e.target.closest('#batch-select-all-matching')) return;
  e.preventDefault();
  selectAllMatching = true;
  writeSelection();
  updateBatchBar();
});

function sidebarTagRows() {
  return Array.from(document.querySelectorAll('#tag-groups .tag-entry[data-tag-id]'));
}

// Implied tags are skipped: they cannot be removed apart from the tag
// that implies them.
function tagFocusRows() {
  return sidebarTagRows().filter(function(li) { return !li.classList.contains('tag-entry-implied'); });
}

function focusedTagRow() {
  return document.querySelector('#tag-groups .tag-entry.focused');
}

function enterTagFocusMode() {
  var items = tagFocusRows();
  if (!items.length) return;
  if (focusedTagRow()) return;
  revealSidebarFor(items[0]);
  items[0].classList.add('focused');
  items[0].scrollIntoView({block: 'nearest'});
  // On body so the mode survives an htmx swap that drops the row's .focused.
  document.body.classList.add('tag-focus');
}

function exitTagFocusMode() {
  document.querySelectorAll('#tag-groups .tag-entry.focused').forEach(function(li) {
    li.classList.remove('focused');
  });
  document.body.classList.remove('tag-focus');
}

function cycleTagFocus(step) {
  var items = tagFocusRows();
  if (!items.length) return;
  var current = focusedTagRow();
  var idx = current ? items.indexOf(current) : 0;
  idx = Math.max(0, Math.min(items.length - 1, idx + step));
  setFocused(items, idx);
}

function openSaveSearchDialog() {
  var dlg = document.getElementById('save-search-dialog');
  if (!dlg) return false;
  var si = document.getElementById('search-input');
  var sq = document.getElementById('save-search-query');
  var sp = document.getElementById('save-search-preview');
  if (si && sq) sq.value = si.value;
  if (si && sp) sp.textContent = si.value || '(empty)';
  var url = new URL(window.location.href);
  var ss = document.getElementById('save-search-sort');
  var so = document.getElementById('save-search-order');
  var se = document.getElementById('save-search-seed');
  var savedSort = url.searchParams.get('sort') || '';
  if (ss) ss.value = savedSort;
  if (so) so.value = url.searchParams.get('order') || '';
  if (se) se.value = savedSort === 'random' ? (url.searchParams.get('seed') || '') : '';
  dlg.showModal();
  return true;
}

function refreshJobStatus() {
  var el = document.getElementById('job-status');
  if (!el || !window.htmx) return;
  el.setAttribute('hx-trigger', 'every 2s');
  window.htmx.process(el);
  window.htmx.ajax('GET', '/internal/job/status', {target: '#job-status', swap: 'outerHTML'});
}

document.addEventListener('click', function(e) {
  var btn = e.target.closest('.page-jump');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('page-jump-dialog');
  var input = document.getElementById('page-jump-input');
  var totalSpan = document.getElementById('page-jump-total');
  if (!dlg || !input) return;
  var current = btn.dataset.current || '1';
  // Empty while the gallery's count is still loading: any page goes, and
  // the server clamps one past the end.
  var total = btn.dataset.total || '';
  input.value = current;
  // data-max, not max: constraint validation would block the submit
  // before the clamp runs.
  input.dataset.max = total;
  if (totalSpan) totalSpan.textContent = total || '…';
  dlg.showModal();
  setTimeout(function() { input.focus(); input.select(); }, 0);
});

['mouseover', 'mouseout', 'focusin', 'focusout'].forEach(function(type) {
  document.addEventListener(type, function(e) {
    var el = e.target.closest ? e.target.closest('[data-edge]') : null;
    var graph = el && el.closest('.deriv-graph');
    if (!graph) return;
    var on = type === 'mouseover' || type === 'focusin';
    graph.querySelectorAll('[data-edge="' + el.dataset.edge + '"]').forEach(function(peer) {
      peer.classList.toggle('deriv-edge-lit', on);
    });
  });
});

// Only the form's own 204 closes: htmx events bubble, and an empty
// suggest answer from an input inside is a 204 too.
function onExternalEditResponse(event, dialogID) {
  if (!event || !event.detail || !event.detail.xhr) return;
  if (!event.detail.elt || event.detail.elt.tagName !== 'FORM') return;
  if (event.detail.xhr.status !== 204) return;
  var dlg = document.getElementById(dialogID);
  if (dlg) dlg.close();
}

document.addEventListener('click', function(e) {
  var btn = e.target.closest && e.target.closest('.markup-bar [data-wrap]');
  if (!btn) return;
  var ta = document.getElementById(btn.closest('.markup-bar').dataset.target);
  if (!ta) return;
  var kind = btn.dataset.wrap, start = ta.selectionStart, end = ta.selectionEnd;
  var sel = ta.value.slice(start, end), open, close, caret;
  if (kind === 'image') {
    open = '[image:'; close = ']'; sel = ''; caret = start + open.length;
  } else if (kind === 'tag' || kind === 'url') {
    open = '[' + kind + '=]'; close = '[/' + kind + ']'; caret = start + open.length - 1;
  } else {
    open = '[' + kind + ']'; close = '[/' + kind + ']'; caret = start + open.length + sel.length;
  }
  ta.value = ta.value.slice(0, start) + open + sel + close + ta.value.slice(end);
  ta.focus();
  ta.setSelectionRange(caret, caret);
});

function toggleAnnotations(btn) {
  var media = btn.closest('.detail-media');
  if (!media) return;
  var hidden = media.classList.toggle('annotations-hidden');
  btn.textContent = hidden ? '[show annotations]' : '[hide annotations]';
}

// The form's _gallery field is for a page without JS; postForm sends the header.
function extractReaderPage(form) {
  postForm(form.action, null, {
    hx: true,
    onOK: function(res) { window.location.href = res.headers.get('HX-Redirect'); },
    onErr: function() { showPageNotice('Could not extract this page.'); }
  });
}

// The URL is copied, not rebuilt: it carries the back context.
function submitPageJump(inputId) {
  var input = document.getElementById(inputId || 'page-jump-input');
  if (!input) return;
  var p = parseInt(input.value, 10);
  if (!p || p < 1) p = 1;
  var max = parseInt(input.dataset.max, 10);
  if (max && p > max) p = max;
  var u = new URL(window.location.href);
  u.searchParams.set('page', String(p));
  ['at', 'after', 'before'].forEach(function(k) { u.searchParams.delete(k); });
  window.location.href = u.toString();
}

document.addEventListener('click', function(e) {
  if (!e.target.matches || !e.target.matches('#sidebar-toggle, #sidebar-rail')) return;
  if (window.matchMedia('(max-width: 768px)').matches) {
    const sidebar = document.getElementById('sidebar');
    if (sidebar) sidebar.classList.toggle('open');
    return;
  }
  const layout = document.getElementById('main-layout');
  if (layout) setSidebarCollapsed(!layout.classList.contains('sidebar-collapsed'));
});

// Page 1: the page number in the URL indexes the listing at the old size.
function setPageSize(size) {
  postForm('/internal/view-prefs', {page_size: size}, {
    onOK: function() {
      if (selectMode) writeStash('monbooru_select_mode', {url: selectionKey()});
      var u = new URL(location.href);
      u.searchParams.set('page', '1');
      ['at', 'after', 'before'].forEach(function(k) { u.searchParams.delete(k); });
      location.href = u.toString();
    }
  });
}

function setThumbSize(size) {
  var grid = document.querySelector('.thumb-grid');
  if (grid) {
    grid.classList.toggle('thumb-sm', size === 's');
    grid.classList.toggle('thumb-lg', size === 'l');
  }
  document.querySelectorAll('#thumb-size-ramp button').forEach(function(b) {
    b.setAttribute('aria-pressed', String(b.dataset.thumb === size));
  });
  postForm('/internal/view-prefs', {thumb: size});
}

function setSidebarCollapsed(collapsed) {
  var layout = document.getElementById('main-layout');
  if (!layout) return;
  layout.classList.toggle('sidebar-collapsed', collapsed);
  document.cookie = 'monbooru_sidebar=' + (collapsed ? 'collapsed' : '') +
    '; path=/; max-age=' + (collapsed ? 31536000 : 0);
  // A collapsed layout renders its lazy panels without a load trigger;
  // this event fetches them.
  if (!collapsed) htmx.trigger(document.body, 'sidebar-shown');
}

function revealSidebarFor(el) {
  if (el && el.closest && el.closest('#sidebar')) setSidebarCollapsed(false);
}

// Each path is encoded once and the list joined with commas; a value
// written encoded twice has no comma of its own and is read once more.
function getFolderCookie() {
  var m = document.cookie.match(/monbooru_folders=([^;]*)/);
  if (!m) return new Set();
  var raw = m[1];
  try {
    if (raw.indexOf(',') < 0 && /%2C|%25/i.test(raw)) raw = decodeURIComponent(raw);
    return new Set(raw.split(',').filter(Boolean).map(decodeURIComponent));
  } catch (err) { return new Set(); }
}

function setFolderCookie(set) {
  document.cookie = 'monbooru_folders=' + Array.from(set).map(encodeURIComponent).join(',') + '; path=/; max-age=31536000';
}

function toggleFolderItem(btn, targetId, path) {
  var list = document.getElementById(targetId);
  if (list) flipSection(btn, list, path || targetId, false);
}

// The cookie holds the sections toggled off their default, open or collapsed.
function flipSection(toggle, list, key, defaultOpen) {
  var opening = list.style.display === 'none';
  list.style.display = opening ? '' : 'none';
  toggle.textContent = opening ? '▼' : '▶';
  var state = getFolderCookie();
  if (opening !== !!defaultOpen) state.add(key);
  else state.delete(key);
  setFolderCookie(state);
}

function initSectionToggle(toggleId, listId, cookieKey, forceOpen, defaultOpen) {
  var toggle = document.getElementById(toggleId);
  var list = document.getElementById(listId);
  if (!toggle || !list) return;
  var offDefault = getFolderCookie().has(cookieKey);
  if (defaultOpen) {
    if (offDefault) { list.style.display = 'none'; toggle.textContent = '▶'; }
  } else if (forceOpen || offDefault) {
    list.style.display = '';
    toggle.textContent = '▼';
  }
  var clickToggle = function() { flipSection(toggle, list, cookieKey, defaultOpen); };
  // onclick, not addEventListener: this runs again on each settle that
  // holds the tree.
  toggle.onclick = clickToggle;
  var header = toggle.closest('.sidebar-section-header');
  var title = header && header.querySelector('.sidebar-section-title');
  if (title) title.onclick = clickToggle;
}

// folder: matches as SQLite's NOCASE does, A-Z only.
function asciiLower(s) {
  return s.replace(/[A-Z]/g, function(c) { return c.toLowerCase(); });
}

// Kept in JS, not the DOM: OOB swaps replace #sidebar-inner on every
// refresh, even with no URL change. Without it the URL-driven force-open
// undoes a collapse the operator just made.
var _lastInitedQuery = null;

function initFolderTree() {
  // DOMContentLoaded beats the lazy sidebar; an empty pass must not latch
  // the URL.
  if (!document.querySelector('.folder-toggle-btn')) return;

  var expanded = getFolderCookie();
  var currentQuery = window.location.search;
  var urlChanged = currentQuery !== _lastInitedQuery;
  _lastInitedQuery = currentQuery;

  var currentFolder = '';
  var urlParams = new URLSearchParams(currentQuery);
  var q = urlParams.get('q') || '';
  var folderMatch = q.match(/(?:^|\s)folder(?:only)?:(?:"([^"]+)"|([^\s]*))/);
  if (folderMatch) {
    currentFolder = asciiLower(folderMatch[1] || folderMatch[2]);
  }

  initSectionToggle('tags-toggle', 'tag-groups', '__tags__', false, true);
  initSectionToggle('folder-tree-toggle', 'folder-tree-list', '__tree__', false);
  var sourceLabelMatch = q.match(/(?:^|\s)source:(?:"([^"]+)"|([^\s]+))/);
  var sourceLabelOpen = urlChanged && !!sourceLabelMatch;
  initSectionToggle('source-labels-toggle', 'source-labels-list', '__sources__', sourceLabelOpen);
  var treeToggle = document.getElementById('folder-tree-toggle');
  var treeList = document.getElementById('folder-tree-list');

  // onclick, not addEventListener: this runs again on each settle that
  // holds the tree.
  document.querySelectorAll('.folder-toggle-btn[data-path]').forEach(function(btn) {
    var path = btn.dataset.path;
    var targetId = btn.dataset.target || ('fc-' + path);
    var list = document.getElementById(targetId);
    if (!list) return;

    var low = asciiLower(path);
    var urlDriven = currentFolder && (currentFolder === low || currentFolder.startsWith(low + '/'));
    var shouldExpand = expanded.has(path) || (urlChanged && urlDriven);

    if (shouldExpand) {
      list.style.display = '';
      btn.textContent = '▼';
      if (urlChanged && urlDriven && treeList) {
        treeList.style.display = '';
        if (treeToggle) treeToggle.textContent = '▼';
      }
    }

    btn.onclick = function(e) {
      e.stopPropagation();
      toggleFolderItem(btn, targetId, path);
    };
  });
}

document.addEventListener('DOMContentLoaded', initFolderTree);

function markInboxGrowth() {
  var nav = document.getElementById('inbox-nav');
  var count = document.getElementById('inbox-nav-count');
  if (!nav || !count) return;
  var n = parseInt((count.textContent.match(/\d+/) || ['0'])[0], 10);
  var key = 'monbooru_inbox_seen:' + (document.body.dataset.gallery || '');
  try {
    var seen = parseInt(localStorage.getItem(key) || '0', 10);
    if (nav.classList.contains('active') || n < seen) {
      localStorage.setItem(key, String(n));
    } else if (n > seen) {
      nav.classList.add('inbox-new');
    }
  } catch (e) {}
}
document.addEventListener('DOMContentLoaded', markInboxGrowth);
// An outerHTML swap, as [more] makes, settles each inserted row on its own.
var _folderSettledXhr = null;
document.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;
  if (!el || !el.querySelector || !el.querySelector('.folder-toggle-btn')) return;
  if (e.detail.xhr && e.detail.xhr === _folderSettledXhr) return;
  _folderSettledXhr = e.detail.xhr;
  initFolderTree();
});

function lastWordIndex(words) {
  for (var i = words.length - 1; i >= 0; i--) {
    if (words[i].trim() !== '') return i;
  }
  return -1;
}

function suggestInputOf(btn) {
  var dd = btn.closest('.suggest-dropdown');
  var input = dd && dd.parentElement && dd.parentElement.querySelector('input[type="text"]');
  if (!input) return null;
  dd.innerHTML = '';
  return input;
}

// The word the caret sits in or at the end of; an input keeps its caret
// while a click on a suggestion has the focus.
function wordAtCaret(input) {
  var v = input.value;
  var at = input.selectionStart == null ? v.length : input.selectionStart;
  var start = at, end = at;
  while (start > 0 && !/\s/.test(v[start - 1])) start--;
  while (end < v.length && !/\s/.test(v[end])) end++;
  return {start: start, end: end, word: v.slice(start, end)};
}

function applyTagSuggest(btn) {
  var tagName = btn.dataset.tagName;
  if (!tagName) return;
  var input = suggestInputOf(btn);
  if (!input) return;
  if (input.dataset.multiTags) {
    var w = wordAtCaret(input);
    var after = input.value.slice(w.end);
    if (!/^\s/.test(after)) after = ' ' + after;
    input.value = input.value.slice(0, w.start) + tagName + after;
    input.focus();
    var caret = w.start + tagName.length + 1;
    input.setSelectionRange(caret, caret);
    return;
  }
  input.value = tagName;
  input.focus();
}

function applyLabelSuggest(btn, key) {
  var label = btn.dataset[key];
  if (label == null) return;
  var input = suggestInputOf(btn);
  if (!input) return;
  input.value = label;
  input.focus();
}

// The chip cancels its mousedown so it never takes focus: activeElement
// must stay the field with the caret.
function insertToken(btn) {
  var ids = (btn.dataset.targets || '').split(/\s+/).filter(Boolean);
  var input = ids.indexOf((document.activeElement || {}).id) >= 0
    ? document.activeElement
    : document.getElementById(ids[0]);
  if (!input) return;
  var token = btn.dataset.token;
  var at = input.selectionStart == null ? input.value.length : input.selectionStart;
  var to = input.selectionEnd == null ? at : input.selectionEnd;
  input.value = input.value.slice(0, at) + token + input.value.slice(to);
  input.focus();
  input.setSelectionRange(at + token.length, at + token.length);
  input.dispatchEvent(new Event('input', {bubbles: true}));
}

// A search scope has no picked ids: the server reads its first rows in the
// order the job walks, which the grid shows only on page 1.
function namePreviewVals(prefix, scope) {
  var scopeEl = document.getElementById(prefix + '-scope');
  var kind = scopeEl && scopeEl.value ? scopeEl.value : 'selection';
  var slot = document.getElementById(prefix + '-preview');
  var rows = parseInt(slot && slot.dataset.rows, 10) || 5;
  var vals = {
    folder: document.getElementById(prefix + '-folder').value,
    name: document.getElementById(prefix + '-name').value,
    scope: scope,
    rows: rows,
    total: scopeCount(kind, null, null) || ''
  };
  if (kind === 'search') {
    new URLSearchParams(searchScopeParts().join('&')).forEach(function(v, k) { vals[k] = v; });
    vals.kind = 'search';
  } else {
    vals.ids = pickedIDs.slice(0, rows).join(',');
  }
  return vals;
}

// A click, not a keystroke: a {md5} template hashes files that have no
// digest yet.
function showMoreNamePreview(btn) {
  var slot = btn.closest('[hx-get]');
  if (!slot) return;
  slot.dataset.rows = btn.dataset.rows;
  if (window.htmx) window.htmx.trigger(slot, 'preview');
}

function applySearchSuggest(tagName) {
  var si = document.getElementById('search-input');
  if (!si) return;
  var words = si.value.split(/(\s+)/);
  var lastWordIdx = lastWordIndex(words);
  if (lastWordIdx >= 0) {
    var last = words[lastWordIdx];
    var prefix = last.startsWith('-') ? '-' : '';
    words[lastWordIdx] = prefix + tagName;
  } else {
    words.push(tagName);
  }
  // A row ending in a colon or an operator waits for its value, so no
  // trailing space.
  var keepCursor = /[:<>=]$|\.\.$/.test(tagName);
  si.value = words.join('') + (keepCursor ? '' : ' ');
  clearSlots('search-suggest');
  si.focus();
  // The synthetic input event makes htmx's input trigger fetch the next
  // level of hints.
  if (keepCursor) {
    si.dispatchEvent(new Event('input', { bubbles: true }));
  }
}

document.addEventListener('click', function (e) {
  var btn = e.target.closest('.btn-tagger-cmd');
  if (!btn) return;
  var dlg = document.getElementById('tagger-cmd-dialog');
  if (!dlg) return;
  var host = btn.dataset.hostCmd || '';
  document.getElementById('tagger-cmd-name').textContent = btn.dataset.taggerName || '';
  document.getElementById('tagger-cmd-desc').textContent = btn.dataset.taggerDesc || '';
  document.getElementById('tagger-cmd-gated').hidden = !btn.dataset.gated;
  document.getElementById('tagger-cmd-host').textContent = host;
  document.getElementById('tagger-cmd-host-copy').dataset.copy = host;
  dlg.showModal();
});

var _jobAutoClearTimer = null;
var _jobAutoClearFinishedAt = '';
var _lastReloadedFinishedAt = '';
function refreshGalleryGrid() {
  if (!document.getElementById('gallery-grid') || !window.htmx) return;
  var u = new URL(window.location.href);
  window.htmx.ajax('GET', u.pathname + u.search, {target: '#gallery-grid', swap: 'innerHTML'});
}

function inboxBusy() {
  var input = document.getElementById('inbox-upload-file-input');
  return !!(input && input.files && input.files.length);
}

// The first settle reports whatever job last finished, often before this
// page rendered, so its reload is skipped.
var _firstJobStatusSettle = true;
var _lastJobProcessed = -1;
var _lastChanges = -1;
// Set when an action starts a job, whose completion then reloads the
// page: the grid swap after a job intermittently fails to settle.
var _pendingGalleryReload = false;
var _pendingReloadShownAt = '';

function armGalleryReload() {
  _pendingGalleryReload = true;
  _pendingReloadShownAt = (document.getElementById('job-status') || {dataset: {}}).dataset.finishedAt || '';
  refreshJobStatus();
}

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;

  // By afterSettle htmx has pushed the new URL, so readSelection keys on
  // the new listing.
  if (el && el.id === 'gallery-grid') {
    readSelection();
    syncSelectionBoxes();
    updateBatchBar();
    pruneSelection();
    restoreGalleryFocusFromHash();
    initInboxUpload();
    return;
  }

  if (!el || el.id !== 'job-status') return;

  var firstSettle = _firstJobStatusSettle;
  _firstJobStatusSettle = false;

  var isDone = !!el.querySelector('.job-done');
  var isErr  = !!el.querySelector('.job-error');
  // On #job-status itself, where querySelector would never find it.
  var isRunning = el.classList.contains('job-running');
  var finishedAt = el.dataset.finishedAt || '';

  // The listed types advance Processed as they change the grid; changes
  // count what a sync or the watcher altered while any job runs.
  var jobType = el.dataset.jobType || '';
  var processed = parseInt(el.dataset.processed || '0', 10);
  var changes = parseInt(el.dataset.changes || '0', 10);
  var refreshDuringRun = jobType === 'delete' || jobType === 're-extract';
  if (!isRunning) {
    _lastJobProcessed = -1;
    _lastChanges = -1;
  } else {
    var needRefresh = false;
    if (refreshDuringRun && processed > 0 && processed !== _lastJobProcessed) {
      _lastJobProcessed = processed;
      needRefresh = true;
    }
    if (changes > 0 && changes !== _lastChanges) {
      _lastChanges = changes;
      needRefresh = true;
    }
    if (needRefresh && !inboxBusy()) {
      refreshGalleryGrid();
    }
  }

  var isIdle = !isDone && !isErr && !isRunning;

  if (isIdle) {
    _jobAutoClearFinishedAt = '';
    if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
    return;
  }

  if ((isDone || isErr) && finishedAt && finishedAt !== _jobAutoClearFinishedAt) {
    _jobAutoClearFinishedAt = finishedAt;
    if (_jobAutoClearTimer) clearTimeout(_jobAutoClearTimer);
    _jobAutoClearTimer = setTimeout(function() {
      _jobAutoClearFinishedAt = '';
      dismissJobStatus();
    }, 30000);
  }

  if (isDone && _pendingGalleryReload && finishedAt !== _pendingReloadShownAt) {
    if (finishedAt) _lastReloadedFinishedAt = finishedAt;
    if (document.getElementById('gallery-grid') || document.getElementById('tags-page') || document.getElementById('tag-detail-page') || document.getElementById('collections-page')) {
      var pendingDone = el.querySelector('.job-done');
      if (pendingDone) stashActionFlash(jobDoneFlash(pendingDone), 'ok');
      // Left armed: reload() does not stop this script, and a navigation
      // is still pending.
      window.location.reload();
      return;
    }
    _pendingGalleryReload = false;
  }

  if (isDone && finishedAt && finishedAt !== _lastReloadedFinishedAt) {
    _lastReloadedFinishedAt = finishedAt;
    if (firstSettle) return;

    // The relations hub computes its counters at render time, so only a
    // reload updates them.
    if (document.getElementById('relations-page') && (jobType === 'relations' || jobType === 'phash')) {
      window.location.reload();
      return;
    }
    // A walker's delete-all runs as a job that can outlast its form's reload.
    if (document.getElementById('relations-walker-page') && jobType === 'delete') {
      window.location.reload();
      return;
    }

    // A check changes nothing, so the grid is left alone.
    var grid = jobType === 'check' ? null : document.getElementById('gallery-grid');
    if (grid) {
      var doneEl = el.querySelector('.job-done');
      if (doneEl) showActionFlash(jobDoneFlash(doneEl), 'ok');
      if (!inboxBusy()) refreshGalleryGrid();
    } else if (window.htmx && document.getElementById('footer-count-tags')) {
      window.htmx.ajax('GET', '/internal/footer-counts', {target: '#footer', swap: 'none'});
    }

    var imageTags = document.getElementById('image-tags');
    if (imageTags) {
      var imageId = imageTags.dataset.imageId;
      if (imageId && window.htmx) {
        window.htmx.ajax('GET', '/images/' + imageId + '/tags', {target: '#image-tags', swap: 'outerHTML'});
      }
    }
  }
});

// showActionFlash colours only an error itself, so a partial run's line
// arrives already classed.
function jobDoneFlash(doneEl) {
  var text = escapeHTML(doneEl.title || '');
  return doneEl.classList.contains('job-partial') ? '<div class="flash flash-warn">' + text + '</div>' : text;
}

function scopeCount(scope, countEl, nounEl) {
  var n = scope === 'selection' ? pickedIDs.length : matchingTotal();
  if (countEl) countEl.textContent = n === null ? '…' : n;
  if (nounEl) {
    var suffix = n === 1 ? 'image' : 'images';
    nounEl.textContent = scope === 'selection' ? 'selected ' + suffix : suffix + ' in current search';
  }
  return n;
}

// From the URL, not the form, which may hold an unsubmitted query.
function searchScopeParts() {
  var params = new URLSearchParams(location.search);
  var sortEl = document.getElementById('search-sort');
  var orderEl = document.querySelector('#search-form select[name="order"]');
  var parts = ['q=' + encodeURIComponent(params.get('q') || ''),
               'sort=' + encodeURIComponent(params.get('sort') || (sortEl ? sortEl.value : 'newest')),
               'order=' + encodeURIComponent(params.get('order') || (orderEl ? orderEl.value : 'desc'))];
  // Without the seed, a job that numbers by position walks a different
  // shuffle than the one on screen.
  var seed = params.get('seed');
  if (seed) parts.push('seed=' + encodeURIComponent(seed));
  return parts;
}

// One comma-joined parameter: a selection built across pages can pass
// net/url's 10000-parameter cap, and the parser rejects the whole request.
function selectionScopeIds() {
  var ids = pickedIDs.slice();
  if (ids.length === 0) return null;
  return ['ids=' + encodeURIComponent(ids.join(','))];
}

function batchScopeParams(scope, flash) {
  if (scope === 'search') return searchScopeParts();
  var ids = selectionScopeIds();
  if (!ids) {
    setFlashText(flash, 'err', 'No images selected.');
    return null;
  }
  return ids;
}

function relayPlugin(btn) {
  var pinned = btn.closest('[data-image-id]');
  var ids = pinned ? [pinned.dataset.imageId] : pickedIDs.slice();
  if (!ids.length || !window.htmx) return;
  window.htmx.ajax('POST', '/internal/plugin/relay', {
    swap: 'none',
    values: {plugin: btn.dataset.plugin, button: btn.dataset.button, ids: ids.join(',')}
  });
}

// A plugin signals it is done by sending the frame to its {back_url}, off
// /plugins/.
function openPluginPage(btn) {
  var dlg = document.getElementById('plugin-open-dialog');
  var frame = document.getElementById('plugin-open-frame');
  if (!dlg || !frame) return;
  dlg.querySelector('.plugin-open-title').textContent = btn.dataset.peer + ': ' + btn.textContent.trim();
  dlg.querySelector('.plugin-open-tab').href = btn.dataset.href;
  if (!frame.pluginWired) {
    frame.pluginWired = true;
    frame.addEventListener('load', pluginPageNavigated);
    // Escape closes the dialog without passing through Close.
    dlg.addEventListener('close', function() { frame.src = 'about:blank'; });
  }
  frame.src = btn.dataset.href;
  dlg.showModal();
}

function pluginPageNavigated(e) {
  var dlg = document.getElementById('plugin-open-dialog');
  if (!dlg || !dlg.open) return;
  var path;
  try {
    path = e.target.contentWindow.location.pathname;
  } catch (err) {
    return; // the plugin sent the browser off monbooru's origin entirely
  }
  if (path.indexOf('/plugins/') === 0) return;
  closePluginPage();
  window.location.reload();
}

function closePluginPage() {
  var dlg = document.getElementById('plugin-open-dialog');
  if (dlg && dlg.open) dlg.close();
}

// opts: countId, requireMatches (refuse a search scope matching nothing),
// radio (re-checked as the default), clearIds (emptied on open),
// beforeShow, focusId.
function openBatchDialog(prefix, scope, opts) {
  opts = opts || {};
  // An escalated bar still opens dialogs with the selection scope;
  // flipping here lets each post the search.
  if (scope === 'selection' && selectAllMatching) scope = 'search';
  if (scope === 'selection' && pickedIDs.length === 0) return;
  var n = scopeCount(scope, document.getElementById(opts.countId || prefix + '-count'),
                     document.getElementById(prefix + '-noun'));
  if (opts.requireMatches && scope === 'search' && n === 0) return;
  document.getElementById(prefix + '-scope').value = scope;
  if (opts.radio) {
    var dflt = document.querySelector(opts.radio);
    if (dflt) dflt.checked = true;
  }
  (opts.clearIds || []).forEach(function(id) {
    var el = document.getElementById(id);
    if (!el) return;
    if (el.tagName === 'INPUT') el.value = ''; else el.innerHTML = '';
  });
  clearSlots(prefix + '-flash');
  var dlg = document.getElementById(prefix + '-dialog');
  if (opts.beforeShow) opts.beforeShow();
  dlg.showModal();
  var focusEl = opts.focusId ? document.getElementById(opts.focusId) : null;
  if (focusEl) focusEl.focus();
}

function clearSlots() {
  for (var i = 0; i < arguments.length; i++) {
    var el = document.getElementById(arguments[i]);
    if (el) el.innerHTML = '';
  }
}

// A suggest still in its debounce at submit lands after the answer and paints
// over it; these inputs keep focus, so the focus check alone misses it.
function disarmSuggest(inputId) {
  var input = document.getElementById(inputId);
  if (input) input.dataset.suggestStale = '1';
}

function suggestDisarmed(input) {
  if (!input.dataset.suggestStale) return false;
  delete input.dataset.suggestStale;
  return true;
}

function applyPTRSpelling(btn) {
  var input = document.getElementById('ptr-lookup-as');
  var form = document.getElementById('ptr-lookup-form');
  if (!input || !form) return;
  input.value = btn.dataset.spelling;
  form.requestSubmit();
}

function openHxRewriteDialog(formId, attr, url, dialogId) {
  var form = document.getElementById(formId);
  form.setAttribute(attr, url);
  if (window.htmx) window.htmx.process(form);
  document.getElementById(dialogId).showModal();
}

function openNameDialog(ids, name, url) {
  document.getElementById(ids.src).textContent = name;
  clearSlots(ids.err);
  var input = document.getElementById(ids.input);
  input.value = name;
  openHxRewriteDialog(ids.form, 'hx-post', url, ids.dialog);
  input.focus();
  input.select();
}

function openRenameDialog(btn) {
  openNameDialog({src: 'rename-src-name', err: 'rename-error', input: 'rename-new-name',
                  form: 'rename-form', dialog: 'rename-dialog'},
                 btn.dataset.name, '/tags/' + btn.dataset.id + '/rename');
  // An alias cannot keep its old name as an alias: that would point an
  // alias at an alias, so the row is hidden or not rendered for one.
  var keep = document.getElementById('rename-keep-alias');
  if (keep) {
    keep.checked = false;
    document.getElementById('rename-keep-alias-row').hidden = btn.dataset.isAlias === '1';
  }
}

function confirmBatchSimple(prefix, endpoint, extraParams, failMsg, consumesScope) {
  var scope = document.getElementById(prefix + '-scope').value;
  var flashId = prefix + '-flash';
  var scoped = batchScopeParams(scope, document.getElementById(flashId));
  if (!scoped) return;
  clearSlots(flashId);
  var parts = ['scope=' + encodeURIComponent(scope)].concat(extraParams || [], scoped);
  postForm(endpoint, new URLSearchParams(parts.join('&')), {
    dialogId: prefix + '-dialog',
    flashId: flashId,
    failMsg: failMsg || 'Action failed.',
    onOK: function() {
      if (consumesScope) clearSelection();
      armGalleryReload();
    }
  });
}

// opts: method (POST), hx (sends HX-Request), okStatus (the one success
// status), dialogId (closed on OK), onOK(res), flashId (gets the error
// body), failMsg, onErr(text) (instead of the flash), catchMsg (the
// network-failure text for the flash or onErr; with neither, the page
// notice says the server did not answer), onDone (when the page notice
// takes the answer).
function postForm(url, params, opts) {
  opts = opts || {};
  var csrf = getCSRFToken();
  var body = params instanceof URLSearchParams ? params : new URLSearchParams(params || {});
  body.append('_csrf', csrf);
  var headers = {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf};
  if (opts.hx) headers['HX-Request'] = 'true';
  if (activeGallery()) headers['X-Monbooru-Gallery'] = activeGallery();
  var flash = opts.flashId ? document.getElementById(opts.flashId) : null;
  var p = fetch(url, {
    method: opts.method || 'POST',
    headers: headers,
    body: body.toString()
  }).then(function(res) {
    if (sessionLost(res)) {
      showLoginNotice();
      if (opts.onDone) opts.onDone();
      return;
    }
    if (res.status === 403 || (res.status === 409 && res.headers.get('X-Monbooru-Gallery'))) {
      res.text().then(showPageNotice);
      if (opts.onDone) opts.onDone();
      return;
    }
    var ok = opts.okStatus ? res.status === opts.okStatus : res.ok;
    if (ok) {
      var dlg = opts.dialogId ? document.getElementById(opts.dialogId) : null;
      if (dlg) dlg.close();
      if (opts.onOK) opts.onOK(res);
    } else {
      res.text().then(function(t) {
        if (opts.onErr) { opts.onErr(t); return; }
        if (!flash) return;
        if (!t) {
          if (opts.failMsg) setFlashText(flash, 'err', opts.failMsg);
          else flash.textContent = '';
        } else if (/^text\/html/.test(res.headers.get('Content-Type') || '')) {
          flash.innerHTML = t;
        } else {
          setFlashText(flash, 'err', t);
        }
      });
    }
  });
  p.catch(function() {
    var msg = opts.catchMsg || 'Request failed.';
    if (flash) setFlashText(flash, 'err', msg);
    else if (opts.onErr) opts.onErr(msg);
    else showUnansweredNotice();
  });
}

function mirrorJobSummary(flashId) {
  var status = document.getElementById('job-status');
  if (!status || !status.parentNode) return;
  var startedAt = status.dataset.finishedAt || '';
  var obs = new MutationObserver(function() {
    var now = document.getElementById('job-status');
    if (!now || (now.dataset.finishedAt || '') === startedAt) return;
    var done = document.getElementById('job-done-msg');
    var failed = document.getElementById('job-error-msg');
    if (!done && !failed) return;
    obs.disconnect();
    var node = done || failed;
    setFlashText(document.getElementById(flashId), done ? 'ok' : 'err',
                 node.getAttribute('title') || node.textContent);
  });
  obs.observe(status.parentNode, {childList: true, subtree: true});
}

function dismissJobStatus() {
  _lastReloadedFinishedAt = '';
  _lastJobProcessed = -1;
  _lastChanges = -1;
  _jobAutoClearFinishedAt = '';
  if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
  clearSlots('job-status');
  postForm('/internal/job/dismiss', null, {onOK: refreshJobStatus});
}

function cancelJobStatus() {
  postForm('/internal/job/cancel');
}

function handleSuggestKey(e, dropdownId, inputId) {
  var dd = document.getElementById(dropdownId);
  if (!dd) return;
  var items = Array.from(dd.querySelectorAll('.suggest-item'));
  var focused = dd.querySelector('.suggest-item.kbd-focused');
  var idx = focused ? items.indexOf(focused) : -1;
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    items.forEach(function(i){ i.classList.remove('kbd-focused'); });
    idx = e.key === 'ArrowDown' ? Math.min(idx + 1, items.length - 1) : Math.max(idx - 1, 0);
    if (idx >= 0) {
      items[idx].classList.add('kbd-focused');
      items[idx].scrollIntoView({ block: 'nearest' });
    }
  } else if (e.key === 'Enter' && focused) {
    e.preventDefault();
    focused.click();
  } else if (e.key === 'Escape') {
    // An open list takes this Escape, so a dialog around the input stays open.
    if (dd.childElementCount > 0) e.preventDefault();
    dd.innerHTML = '';
  }
}

// blurOnSubmit: after Enter the arrow keys drive the gallery, not the
// input. clearOnEmpty: drop a swap landing on an input emptied by an add,
// which keeps focus; label inputs list on an empty prefix on purpose.
var suggestPairs = {
  'search-suggest': {input: 'search-input', blurOnSubmit: true, clearOnEmpty: true},
  'tag-suggest-dropdown': {input: 'tag-input', clearOnEmpty: true},
  'batch-place-suggest': {input: 'batch-place-folder'},
  'place-image-suggest': {input: 'place-image-folder'},
  'batch-tag-suggest': {input: 'batch-tag-input', clearOnEmpty: true},
  'batch-strip-suggest': {input: 'batch-strip-input', clearOnEmpty: true},
  'source-suggest': {input: 'source-site-input'},
  'batch-collection-suggest': {input: 'batch-collection-input'},
  'collection-suggest': {input: 'collection-name-input'},
  'generate-collection-suggest': {input: 'generate-collection-input'},
  'collection-rename-suggest': {input: 'collection-rename-input'},
  'alias-create-suggest': {input: 'alias-create-canon', clearOnEmpty: true},
  'batch-alias-suggest': {input: 'batch-alias-canon', clearOnEmpty: true},
  'batch-imply-suggest': {input: 'batch-imply-target', clearOnEmpty: true},
  'tag-create-suggest': {input: 'tag-create-name', clearOnEmpty: true},
  'detail-alias-suggest': {input: 'detail-alias-canon', clearOnEmpty: true},
  'implication-add-suggest': {input: 'implication-add-input', clearOnEmpty: true},
  'implied-by-add-suggest': {input: 'implied-by-add-input', clearOnEmpty: true},
  'ptr-lookup-suggest': {input: 'ptr-lookup-as', clearOnEmpty: true},
};

var suggestDropdownByInput = {};
for (var _suggestID in suggestPairs) suggestDropdownByInput[suggestPairs[_suggestID].input] = _suggestID;

// Resolved per event and delegated to document: some inputs live in
// re-rendered fragments, where listeners on the first nodes would be
// orphaned.
document.addEventListener('click', function(e) {
  for (var ddId in suggestPairs) {
    var dd = document.getElementById(ddId);
    if (dd && !dd.contains(e.target) && e.target.id !== suggestPairs[ddId].input) {
      dd.innerHTML = '';
    }
  }
});

document.addEventListener('submit', function(e) {
  for (var ddId in suggestPairs) {
    var input = document.getElementById(suggestPairs[ddId].input);
    if (!input || e.target !== input.form) continue;
    clearSlots(ddId);
    if (suggestPairs[ddId].blurOnSubmit) input.blur();
  }
});

// A late response can land after a submit or a focus move, so it is dropped
// unless the input still has focus. suggest-fresh holds off :hover until the
// mouse moves, or the item under a still cursor looks picked.
document.addEventListener('htmx:afterSwap', function(e) {
  var pair = suggestPairs[e.target.id];
  if (!pair) return;
  var input = document.getElementById(pair.input);
  if (document.activeElement !== input || (pair.clearOnEmpty && input.value === '')) { e.target.innerHTML = ''; return; }
  e.target.classList.add('suggest-fresh');
  e.target.addEventListener('mousemove', clearSuggestFresh, {once: true});
});

// Suggest endpoints answer 204 on no match, and htmx swaps nothing on a
// 204, which would leave stale matches showing.
document.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail || !e.detail.elt) return;
  var ddId = suggestDropdownByInput[e.detail.elt.id];
  if (!ddId) return;
  if (e.detail.xhr && e.detail.xhr.status === 204) clearSlots(ddId);
});

function mergeChoiceItem(name, value, checked, decorate) {
  var li = document.createElement('li');
  li.className = 'merge-dup-choice';
  var label = document.createElement('label');
  var radio = document.createElement('input');
  radio.type = 'radio';
  radio.name = name;
  radio.value = value;
  radio.checked = checked;
  label.appendChild(radio);
  decorate(label);
  li.appendChild(label);
  return li;
}

function clearSuggestFresh(e) {
  e.currentTarget.classList.remove('suggest-fresh');
}


var _initialTagIDs = null;
var _addedTagOrder = [];

function captureInitialTags() {
  if (_initialTagIDs !== null) return;
  _initialTagIDs = new Set();
  sidebarTagRows().forEach(function(li) { _initialTagIDs.add(li.dataset.tagId); });
}

// Only rows with the user's attribution are this session's adds; a
// tagger's or a source's join the baseline.
function separateNewTags() {
  var added = document.querySelector('.tag-list-added');
  if (!added) return;
  if (_initialTagIDs === null) { captureInitialTags(); }

  var rowsById = {};
  sidebarTagRows().forEach(function(li) {
    var id = li.dataset.tagId;
    if (_initialTagIDs.has(id)) return;
    if (li.dataset.source !== 'user') { _initialTagIDs.add(id); return; }
    rowsById[id] = li;
    if (_addedTagOrder.indexOf(id) === -1) _addedTagOrder.push(id);
  });
  _addedTagOrder = _addedTagOrder.filter(function(id) { return rowsById[id] !== undefined; });

  added.innerHTML = '';
  _addedTagOrder.forEach(function(id) { added.appendChild(justAddedChip(rowsById[id])); });

  var title = document.querySelector('.tag-list-added-title');
  if (title) title.hidden = _addedTagOrder.length === 0;
}

// Its x clicks the sidebar row's own button, which carries the
// server-rendered htmx wiring.
function justAddedChip(row) {
  var link = row.querySelector('.tag-link');
  var li = document.createElement('li');
  li.className = 'tag-item';
  li.dataset.tagId = row.dataset.tagId;
  if (link) li.style.color = link.style.color;
  var a = document.createElement('a');
  a.className = 'tag-chip-name';
  a.href = link ? link.getAttribute('href') : '#';
  a.textContent = link ? link.textContent.trim() : '';
  li.appendChild(a);
  var rm = document.createElement('button');
  rm.type = 'button';
  rm.className = 'tag-remove-btn';
  rm.title = 'Remove tag';
  rm.textContent = '×';
  rm.onclick = function() {
    var btn = row.querySelector('.tag-entry-remove');
    if (btn) btn.click();
  };
  li.appendChild(rm);
  return li;
}

// The swap drops .focused; put it back one row above the removed tag,
// from the index Enter stashed.
document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail ? e.detail.elt : null;
  if (!el || (el.id !== 'image-tags' && el.id !== 'sidebar-inner')) return;
  separateNewTags();
  if (!document.body.classList.contains('tag-focus')) return;
  var stash = document.body.dataset.tagFocusIdx;
  delete document.body.dataset.tagFocusIdx;
  if (focusedTagRow()) return;
  var items = tagFocusRows();
  if (items.length === 0) {
    document.body.classList.remove('tag-focus');
    return;
  }
  var at = stash === undefined ? 0 : Math.min(Math.max(0, parseInt(stash, 10) - 1), items.length - 1);
  items[at].classList.add('focused');
  items[at].scrollIntoView({block: 'nearest'});
});

document.addEventListener('DOMContentLoaded', function() {
  if (document.getElementById('image-tags')) captureInitialTags();
});


// The topbar wraps to several rows at narrow widths, so the drawer's top
// comes from its measured height.
(function () {
  var root = document.documentElement;
  function sync() {
    var tb = document.getElementById('topbar');
    if (!tb) return;
    root.style.setProperty('--topbar-h', Math.round(tb.getBoundingClientRect().height) + 'px');
  }
  document.addEventListener('DOMContentLoaded', sync);
  window.addEventListener('resize', sync);
  window.addEventListener('load', sync);
})();

document.addEventListener('DOMContentLoaded', function() {
  if (!document.getElementById('pages-grid-page')) return;
  var hash = window.location.hash;
  if (!hash || hash.indexOf('#page-') !== 0) return;
  var target = document.querySelector(hash + '.manga-page-cell');
  if (!target) return;
  var cells = Array.from(document.querySelectorAll('.manga-page-cell'));
  setFocused(cells, cells.indexOf(target), 'center');
});

function initInboxUpload() {
  var dz = document.getElementById('inbox-upload-drop');
  var input = document.getElementById('inbox-upload-file-input');
  var list = document.getElementById('inbox-upload-file-list');
  var pickBtn = document.getElementById('inbox-upload-pick-btn');
  var form = document.getElementById('inbox-upload-form');
  var submitBtn = document.getElementById('inbox-upload-submit-btn');
  var resetBtn = document.getElementById('inbox-upload-reset-btn');
  var result = document.getElementById('inbox-upload-result');
  // A property: a Back's snapshot would keep a data- flag, not the listeners.
  if (!dz || !input || !list || !form || dz.inboxWired) return;
  dz.inboxWired = true;

  // The file picker replaces input.files on every change, so picks
  // accumulate here and are copied back.
  var _pending = new DataTransfer();

  function renderList() {
    list.innerHTML = '';
    var files = _pending.files;
    for (var i = 0; i < files.length; i++) {
      var li = document.createElement('li');
      var kib = Math.round(files[i].size / 1024);
      var sizeText = kib > 0 ? (kib + ' KiB') : '<1 KiB';
      li.textContent = files[i].name + ' (' + sizeText + ')';
      list.appendChild(li);
    }
    input.files = _pending.files;
    var empty = files.length === 0;
    if (submitBtn) submitBtn.disabled = empty;
    if (resetBtn) resetBtn.disabled = empty;
  }

  function appendFiles(incoming) {
    for (var i = 0; i < incoming.length; i++) _pending.items.add(incoming[i]);
    renderList();
  }

  function clearPending() {
    _pending = new DataTransfer();
    renderList();
    if (result) result.innerHTML = '';
  }

  input.addEventListener('change', function() {
    if (input.files && input.files.length) appendFiles(input.files);
  });
  if (pickBtn) {
    pickBtn.addEventListener('click', function(e) { e.preventDefault(); input.click(); });
  }
  if (resetBtn) {
    resetBtn.addEventListener('click', function(e) { e.preventDefault(); clearPending(); });
  }
  renderList();

  ['dragenter', 'dragover'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.add('drag-over'); });
  });
  ['dragleave', 'drop'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.remove('drag-over'); });
  });
  dz.addEventListener('drop', function(e) {
    if (!e.dataTransfer || !e.dataTransfer.files) return;
    appendFiles(e.dataTransfer.files);
  });

  // htmx:xhr:progress fires for the response download too; the latch
  // keeps its events from rewinding the counter.
  var uploadDone = false;
  form.addEventListener('htmx:beforeRequest', function() {
    uploadDone = false;
    if (submitBtn) submitBtn.disabled = true;
    if (result) result.innerHTML = '<div class="field-hint">Uploading...</div>';
  });
  form.addEventListener('htmx:xhr:progress', function(e) {
    if (uploadDone || !result || !e.detail || !e.detail.lengthComputable || !e.detail.total) return;
    var pct = Math.round((e.detail.loaded / e.detail.total) * 100);
    if (pct >= 100) {
      uploadDone = true;
      result.innerHTML = '<div class="field-hint">Processing...</div>';
      return;
    }
    result.innerHTML = '<div class="field-hint">Uploading... ' + pct + '%</div>';
  });
  form.addEventListener('htmx:afterRequest', function(e) {
    if (submitBtn) submitBtn.disabled = false;
    if (!e.detail.successful) {
      if (result && !result.querySelector('.flash')) setFlashText(result, 'err', 'Upload failed: nothing was uploaded.');
      return;
    }
    // The result slot sits inside #gallery-grid, which the refresh below
    // replaces.
    if (result && result.innerHTML.trim() !== '') {
      showActionFlash(result.innerHTML, /flash-err/.test(result.innerHTML) ? 'err' : 'ok');
    }
    clearPending();
    refreshGalleryGrid();
  });
}

document.addEventListener('DOMContentLoaded', initInboxUpload);

function mergeCategoryCollision(tagID, catID) {
  showConfirm('Merge this tag into the existing one?', function() {
    postForm('/tags/' + tagID + '/category', {category_id: String(catID), merge: '1'}, {
      method: 'PATCH',
      flashId: 'tag-flash',
      catchMsg: 'Merge request failed.',
      onOK: function() {
        stashActionFlash('Merged into the existing tag.', 'ok');
        window.location.reload();
      },
      // The body is unescaped plain text, and tag names may hold < and >.
      onErr: function(t) { setFlashText(document.getElementById('tag-flash'), 'err', t || 'Merge failed.'); }
    });
  }, 'Its images and implications move onto the existing tag, and its name keeps working as an alias. This cannot be undone.', 'Merge');
}

function mergeRenameCollision(btn) {
  var target = btn.textContent.replace(/^Merge into /, '');
  showConfirm('Merge this tag into ' + target + '?', function() {
    postForm('/tags/batch-alias', {ids: btn.dataset.id, canonical_id: '#' + btn.dataset.target}, {
      dialogId: 'rename-dialog',
      flashId: 'rename-error',
      okStatus: 202,
      failMsg: 'Merge failed.',
      catchMsg: 'Merge request failed.',
      onOK: function() { armGalleryReload(); }
    });
  }, 'Its images and implications move onto ' + target + ', and its name keeps working as an alias. This cannot be undone.', 'Merge');
}

function dropAliasAndRename(btn) {
  postForm('/tags/' + btn.dataset.alias, {}, {
    method: 'DELETE',
    flashId: 'rename-error',
    catchMsg: 'Request failed.',
    onOK: function() { document.getElementById('rename-form').requestSubmit(); }
  });
}

function deleteTagFlow(btn, tagID, onSuccess) {
  var name = btn.dataset.name;
  var isRating = btn.dataset.category === 'rating';
  var msg = isRating
    ? 'Remove the rating "' + name + '" from every image?'
    : 'Delete tag "' + name + '" and remove it from every image?';
  var danger = isRating
    ? 'The rating itself stays and can be added again later.'
    : 'This cannot be undone.';
  var alt = null;
  // An alias follows its target, so only the target can be rejected.
  if (!isRating && !btn.dataset.rejected && !btn.dataset.alias) {
    danger += ' Delete and reject also drops the name on arrival, whatever brings it, until you un-reject it.';
    alt = {label: 'Delete and reject', run: function() { rejectTagFlow(btn, tagID, onSuccess); }};
  }
  showConfirm(msg, function() {
    btn.disabled = true;
    var el = document.getElementById('tag-flash');
    setFlashText(el, 'warn', (isRating ? 'Clearing' : 'Deleting') + ' "' + name + '"…');
    postForm('/tags/' + tagID, null, {
      method: 'DELETE',
      catchMsg: 'Delete request failed.',
      onDone: function() {
        btn.disabled = false;
        el.textContent = '';
      },
      onOK: function() {
        // A raw fetch does not process HX-Trigger, so the flash is
        // stashed by hand.
        stashActionFlash(escapeHTML(isRating
          ? 'Cleared "' + name + '" from every image.'
          : 'Tag "' + name + '" deleted.'), 'ok');
        onSuccess();
      },
      onErr: function(t) {
        btn.disabled = false;
        setFlashText(el, 'err', t || 'Delete failed.');
      }
    });
  }, danger, isRating ? 'Clear' : 'Delete', alt);
}

function rejectTagFlow(btn, tagID, onSuccess) {
  var name = btn.dataset.name;
  btn.disabled = true;
  var el = document.getElementById('tag-flash');
  setFlashText(el, 'warn', 'Rejecting "' + name + '"…');
  postForm('/tags/' + tagID + '/reject', null, {
    catchMsg: 'Reject request failed.',
    onDone: function() {
      btn.disabled = false;
      el.textContent = '';
    },
    onOK: function() {
      stashActionFlash(escapeHTML('Rejected "' + name + '": it is off every image, and nothing brings it back until you un-reject it.'), 'ok');
      onSuccess();
    },
    onErr: function(t) {
      btn.disabled = false;
      setFlashText(el, 'err', t || 'Reject failed.');
    }
  });
}
