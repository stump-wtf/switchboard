/* Switchboard disclosure state (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). A collapsed
 * <details data-sb-disclosure> (the Quarantine view's payload, SPEC-0026 REQ-9) opens and closes
 * natively, keyboard included, with or without this module. This module only mirrors the open state
 * onto the <summary>'s aria-expanded, so the toggle is announced the same way everywhere. The
 * server renders aria-expanded="false" to match the collapsed default.
 *
 * The toggle event does not bubble, so the listener is registered in the capture phase on the
 * document: it covers disclosures that HTMX swaps in after load without re-binding.
 */
(function () {
  "use strict";

  function sync(details) {
    if (!details || !details.matches || !details.matches("details[data-sb-disclosure]")) return;
    var summary = details.querySelector("summary");
    if (summary) summary.setAttribute("aria-expanded", details.open ? "true" : "false");
  }

  document.addEventListener("toggle", function (e) {
    sync(e.target);
  }, true);
})();
