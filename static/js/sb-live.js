/* Switchboard live-region helpers (embedded, CSP script-src 'self' — no CDN, ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0015 REQ "Patch Panel Board", design.md ("thin vanilla-JS layer"): this module NEVER
 * mutates domain state — it expires toasts and ephemeral received-lane cards, trims and counts
 * the board lanes, animates lease countdowns toward server-stamped deadlines, and decays the
 * top-bar LIVE pill to its idle state when counts frames stop arriving. The server renders truth;
 * a reload discards everything here (the received lane is ephemeral BY DESIGN — SSE-only, nothing
 * persisted behind it).
 */
(function () {
  "use strict";

  var TOAST_TTL_MS = 4000;

  // watch observes childList mutations on el and runs fn now and on every change.
  function watch(el, fn) {
    if (!el) return;
    new MutationObserver(fn).observe(el, { childList: true });
    fn();
  }

  // ---- toasts: auto-expire after a short TTL ----
  function initToasts() {
    var toasts = document.getElementById("sb-toasts");
    watch(toasts, function () {
      Array.prototype.forEach.call(toasts.children, function (toast) {
        if (toast.dataset.sbTtl) return; // already scheduled
        toast.dataset.sbTtl = "1";
        setTimeout(function () {
          toast.remove();
        }, TOAST_TTL_MS);
      });
    });
  }

  // ---- patch-panel lanes: trim to the visible cap, toggle empty states, count received ----
  // Each lane list ([data-sb-lane-cards]) is trimmed to its server-stamped cap, its empty-state
  // label ([data-sb-lane-empty=<lane>]) toggled, and — for the ephemeral received lane, whose
  // cards exist only in the DOM — the header count ([data-sb-lane-count=<lane>]) is derived from
  // the DOM. Verified/patched counts come from server counts frames, never from here.
  function initLanes() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-sb-lane-cards]"), function (lane) {
      var key = lane.getAttribute("data-sb-lane-cards");
      var empty = document.querySelector('[data-sb-lane-empty="' + key + '"]');
      var count = document.querySelector('[data-sb-lane-count="' + key + '"]');
      watch(lane, function () {
        var cap = parseInt(lane.getAttribute("data-sb-lane-cap"), 10) || 8;
        while (lane.children.length > cap) lane.removeChild(lane.lastElementChild);
        scheduleEphemerals(lane);
        if (empty) empty.hidden = lane.children.length > 0;
        if (count) count.textContent = String(lane.children.length);
      });
    });
  }

  // ---- ephemeral cards: expire after the server-stamped TTL (data-sb-ephemeral, ms) ----
  // Received-lane cards are SSE-only: an in-flight card whose resolution frame was lost, and the
  // transient rejected/deduped surfaces, self-clean here. Removal re-fires the lane watcher, so
  // counts and empty states stay honest.
  function scheduleEphemerals(root) {
    Array.prototype.forEach.call(root.querySelectorAll("[data-sb-ephemeral]"), function (card) {
      if (card.dataset.sbTtl) return; // already scheduled
      card.dataset.sbTtl = "1";
      var ttl = parseInt(card.getAttribute("data-sb-ephemeral"), 10) || 8000;
      setTimeout(function () {
        card.remove();
      }, ttl);
    });
  }

  // ---- todos table: filter fidelity for live-inserted rows + empty-state upkeep ----
  // todo_created/todo_resurfaced frames insert rows OOB into #sb-todos-body (data-sb-live-row,
  // fragments/todos.html) because the SSE stream is one broadcast and the server cannot know each
  // viewer's filter. This sweep keeps the view honest: a live-inserted row is dropped when a
  // search is active (the query can't be evaluated here) or when its state chip misses the active
  // filter pill; the empty-state label follows the row count. Presentation-only — the durable
  // queue is untouched and a reload renders truth. Governing: SPEC-0015 REQ "Todos View And
  // Drawer" (#96).
  function initTodos() {
    var panel = document.getElementById("sb-todos-panel");
    if (!panel) return;
    function sweep() {
      var body = document.getElementById("sb-todos-body");
      if (!body) return;
      var active = panel.querySelector(".sb-fpill--active");
      var filter = active ? active.getAttribute("data-sb-filter") : "all";
      var search = document.getElementById("sb-todos-search");
      var querying = !!(search && search.value);
      Array.prototype.forEach.call(body.querySelectorAll("[data-sb-live-row]"), function (row) {
        var chip = row.querySelector("[data-sb-state]");
        var state = chip ? chip.getAttribute("data-sb-state") : "";
        if (querying || (filter !== "all" && state !== filter)) row.remove();
      });
      var empty = panel.querySelector("[data-sb-todos-empty]");
      if (empty) empty.hidden = body.children.length > 0;
    }
    // The panel's inner regions are swapped wholesale by filter/search GETs, so observe the
    // stable panel node with subtree to survive every swap.
    new MutationObserver(sweep).observe(panel, { childList: true, subtree: true });
    sweep();
  }

  // ---- lease/retry countdowns: animate toward a server-stamped deadline (data-sb-deadline, unix ms) ----
  // Purely cosmetic: the store owns the lease and the retry schedule; this only re-labels the time
  // remaining once a second and drains the progress bar. It re-syncs whenever an SSE swap re-stamps
  // the deadline. data-sb-prefix prepends label text (the retry countdown's "↻ retry ");
  // data-sb-expired overrides the at-zero label (a due retry reads "re-queuing…" until the
  // todo_resurfaced frame swaps the row, while a lease keeps the default "expired").
  function tickCountdowns() {
    var now = Date.now();
    document.querySelectorAll("[data-sb-countdown][data-sb-deadline]").forEach(function (el) {
      var deadline = parseInt(el.getAttribute("data-sb-deadline"), 10);
      if (!deadline) return;
      var secs = Math.max(0, Math.round((deadline - now) / 1000));
      var prefix = el.getAttribute("data-sb-prefix") || "";
      var suffix = el.getAttribute("data-sb-suffix") || "";
      el.textContent = secs > 0 ? prefix + secs + "s" + suffix : (el.getAttribute("data-sb-expired") || "expired");
    });
    document.querySelectorAll("[data-sb-leasebar][data-sb-deadline]").forEach(function (el) {
      var deadline = parseInt(el.getAttribute("data-sb-deadline"), 10);
      if (!deadline) return;
      var remaining = deadline - now;
      var total = parseInt(el.getAttribute("data-sb-total"), 10);
      if (!total || total < remaining) {
        total = remaining > 0 ? remaining : 1;
        el.setAttribute("data-sb-total", total);
      }
      var pct = Math.max(0, Math.min(100, Math.round((remaining / total) * 100)));
      el.style.width = pct + "%";
    });
  }

  // ---- LIVE pill decay: return the top-bar pill to its idle zero-state when traffic stops ----
  // Counts frames are published only on committed events/transitions (live.go), so a quiet board
  // would pin the last nonzero rate forever. The server stamps data-sb-live-decay (ms) on the pill
  // (fragments/shared.html "live_pill"); the window exceeds the rate's trailing 1-minute DB window,
  // so by the time it elapses with no swap the true rate IS zero — this only catches the display
  // up. Every SSE counts swap replaces the element wholesale (new node → the timer re-arms), and
  // the next frame replaces our decayed pill with server truth. Presentation-only, like everything
  // here.
  var livePillNode = null;
  var livePillSince = 0;

  function tickLiveDecay() {
    var pill = document.getElementById("sb-live");
    if (!pill) return;
    var decay = parseInt(pill.getAttribute("data-sb-live-decay"), 10);
    if (!decay) return;
    if (pill !== livePillNode) {
      // Fresh render (page load or an SSE counts swap): re-arm the silence window.
      livePillNode = pill;
      livePillSince = Date.now();
      return;
    }
    if (pill.classList.contains("sb-live--idle") || Date.now() - livePillSince < decay) return;
    pill.classList.add("sb-live--idle");
    Array.prototype.forEach.call(pill.childNodes, function (n) {
      if (n.nodeType === Node.TEXT_NODE) n.nodeValue = "LIVE · 0/min";
    });
  }

  function init() {
    initToasts();
    initLanes();
    initTodos();
    tickCountdowns();
    tickLiveDecay();
    setInterval(function () {
      tickCountdowns();
      tickLiveDecay();
    }, 1000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
