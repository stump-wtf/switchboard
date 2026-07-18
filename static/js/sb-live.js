/* Switchboard live-region helpers (embedded, CSP script-src 'self' — no CDN, ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0013 REQ "Live Updates and Toasts", design.md ("thin vanilla-JS layer"): this module NEVER
 * mutates domain state — it expires toasts, trims the Board feed, animates lease countdowns toward
 * server-stamped deadlines, and decays the top-bar LIVE pill to its idle state when counts frames
 * stop arriving. The server renders truth; a reload discards everything here.
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

  // ---- Board feed: trim to the visible cap and toggle the empty-state label ----
  function initFeed() {
    var feed = document.querySelector("[data-sb-feed-cap]");
    var empty = document.querySelector("[data-sb-feed-empty]");
    watch(feed, function () {
      var cap = parseInt(feed.getAttribute("data-sb-feed-cap"), 10) || 8;
      while (feed.children.length > cap) feed.removeChild(feed.lastElementChild);
      if (empty) empty.hidden = feed.children.length > 0;
    });
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
    initFeed();
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
