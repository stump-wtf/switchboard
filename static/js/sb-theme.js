/* Switchboard theme control (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0015 REQ "Theme Toggle": the visible control (and the `t` key via sb-keys.js, which
 * clicks it) cycles day/night, persists the choice to localStorage ("sb-theme"), and stamps
 * <html data-theme> — the same override contract tokens.css and theme-boot.js honor.
 */
(function () {
  "use strict";

  var KEY = "sb-theme";

  // currentTheme resolves the ACTIVE theme: the explicit override if present, else the OS scheme
  // (tokens.css: day default, night via prefers-color-scheme).
  function currentTheme() {
    var t = document.documentElement.getAttribute("data-theme");
    if (t === "day" || t === "night") return t;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "night"
      : "day";
  }

  function applyTheme(t) {
    document.documentElement.setAttribute("data-theme", t);
    try {
      localStorage.setItem(KEY, t);
    } catch (e) {
      /* storage unavailable: the attribute still themes this page */
    }
  }

  function init() {
    document.body.addEventListener("click", function (e) {
      var toggle = e.target.closest ? e.target.closest("[data-sb-theme-toggle]") : null;
      if (!toggle) return;
      applyTheme(currentTheme() === "night" ? "day" : "night");
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
