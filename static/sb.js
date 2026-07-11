/* Switchboard presentation-only helpers (embedded, CSP script-src 'self' — no CDN, ADR-0001).
 *
 * Governing: SPEC-0013 REQ "Live Updates and Toasts", REQ "Todo Detail Drawer", design.md ("thin
 * vanilla-JS layer"): this file NEVER mutates domain state — it expires toasts, trims the Board feed,
 * animates lease countdowns toward server-stamped deadlines, and manages the drawer overlay (open on
 * swap-in, close on Escape / overlay-click / close button, with a focus trap and focus return). The
 * server renders truth; a reload discards everything here.
 */
(function () {
  "use strict";

  var TOAST_TTL_MS = 4000;
  var FOCUSABLE =
    'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

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

  // ---- lease countdowns: animate toward a server-stamped deadline (data-sb-deadline, unix ms) ----
  // Purely cosmetic: the store owns the lease; this only re-labels the time remaining once a second
  // and drains the progress bar. It re-syncs whenever an SSE swap re-stamps the deadline.
  function tickCountdowns() {
    var now = Date.now();
    document.querySelectorAll("[data-sb-countdown][data-sb-deadline]").forEach(function (el) {
      var deadline = parseInt(el.getAttribute("data-sb-deadline"), 10);
      if (!deadline) return;
      var secs = Math.max(0, Math.round((deadline - now) / 1000));
      var suffix = el.getAttribute("data-sb-suffix") || "";
      el.textContent = secs > 0 ? secs + "s" + suffix : "expired";
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

  // ---- drawer overlay: open on swap-in, trap focus, close on Escape / scrim / close button ----
  var overlay, lastFocused;

  function overlayOpen() {
    return overlay && !overlay.hidden && overlay.children.length > 0;
  }

  function firstFocusable() {
    if (!overlay) return null;
    var drawer = overlay.querySelector("[data-sb-drawer]");
    var scope = drawer || overlay;
    var els = scope.querySelectorAll(FOCUSABLE);
    return els.length ? els[0] : drawer;
  }

  function openOverlay() {
    overlay.hidden = false;
    var focus = firstFocusable();
    if (focus) focus.focus();
  }

  function closeOverlay() {
    if (!overlay) return;
    overlay.innerHTML = "";
    overlay.hidden = true;
    if (lastFocused && document.contains(lastFocused)) {
      lastFocused.focus();
    }
    lastFocused = null;
  }

  // trapTab keeps Tab focus cycling within the open drawer.
  function trapTab(e) {
    if (e.key !== "Tab" || !overlayOpen()) return;
    var els = Array.prototype.filter.call(overlay.querySelectorAll(FOCUSABLE), function (el) {
      return el.offsetParent !== null || el === document.activeElement;
    });
    if (!els.length) return;
    var first = els[0];
    var last = els[els.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }

  function initOverlay() {
    overlay = document.getElementById("sb-overlay");
    if (!overlay) return;

    // Record the trigger before HTMX swaps the drawer in, so focus can return to it on close.
    document.body.addEventListener("htmx:beforeRequest", function (e) {
      var t = e.detail && e.detail.elt;
      if (t && t.getAttribute && t.getAttribute("hx-target") === "#sb-overlay") {
        lastFocused = t;
      }
    });

    // Open (and focus) whenever the overlay gains drawer content.
    watch(overlay, function () {
      if (overlay.children.length > 0) {
        openOverlay();
      } else if (!overlay.hidden) {
        overlay.hidden = true;
      }
    });

    // Click on the scrim (the overlay itself, not its children) closes.
    overlay.addEventListener("click", function (e) {
      if (e.target === overlay) closeOverlay();
    });
  }

  // A close control anywhere: inside the overlay it closes the overlay; on the standalone drawer
  // page (no overlay content) it returns to the Todos list.
  function initCloseControls() {
    document.body.addEventListener("click", function (e) {
      var close = e.target.closest ? e.target.closest("[data-sb-close]") : null;
      if (!close) return;
      e.preventDefault();
      if (overlay && overlay.contains(close)) {
        closeOverlay();
      } else {
        window.location.assign("/todos");
      }
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && overlayOpen()) {
        e.preventDefault();
        closeOverlay();
      } else {
        trapTab(e);
      }
    });
  }

  function init() {
    initToasts();
    initFeed();
    initOverlay();
    initCloseControls();
    tickCountdowns();
    setInterval(tickCountdowns, 1000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
