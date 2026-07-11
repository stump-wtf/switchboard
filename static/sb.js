/* Switchboard presentation-only helpers (embedded, CSP script-src 'self' — no CDN, ADR-0001).
 *
 * Governing: SPEC-0013 REQ "Live Updates and Toasts" (design.md "thin vanilla-JS layer"): this
 * file never mutates domain state — it expires toasts after a short TTL, trims the Board feed to
 * its visible cap as SSE rows enter, and toggles the feed's empty-state label. Everything here is
 * cosmetic; the server renders truth and a reload discards all of it.
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

  function init() {
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

    var feed = document.querySelector("[data-sb-feed-cap]");
    var empty = document.querySelector("[data-sb-feed-empty]");
    watch(feed, function () {
      var cap = parseInt(feed.getAttribute("data-sb-feed-cap"), 10) || 8;
      while (feed.children.length > cap) feed.removeChild(feed.lastElementChild);
      if (empty) empty.hidden = feed.children.length > 0;
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
