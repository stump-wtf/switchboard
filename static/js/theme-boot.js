/* Pre-paint theme boot (SPEC-0015 REQ "Theme Toggle"; ADR-0018).
 *
 * Loaded synchronously in <head> BEFORE the stylesheets — a tiny external script, so the
 * same-origin CSP (script-src 'self', no inline JS) holds and a stored theme choice paints from
 * the first frame: an operator who chose night reloading on a day-preferring OS never flashes
 * day. With no stored choice the document keeps no data-theme attribute and tokens.css follows
 * prefers-color-scheme (day default). sb-theme.js owns the toggle + persistence after load.
 */
(function () {
  "use strict";
  try {
    var t = localStorage.getItem("sb-theme");
    if (t === "day" || t === "night") {
      document.documentElement.setAttribute("data-theme", t);
    }
  } catch (e) {
    /* storage unavailable (private mode): fall through to prefers-color-scheme */
  }
})();
