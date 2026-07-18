/* Switchboard vend-form scope validation (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0013 REQ "Endpoints View and Vend Modal" ("MUST refuse submission until name, at least one
 * queue, and at least one verb are chosen"). Presentation-only: it disables the submit button,
 * counts the checked queue/verb toggle chips (and, on the free-text queue fallback shown when the
 * store knows no queues yet, mirrors typed queues as preview chips); the server re-validates every
 * submission and mints nothing on an invalid one, so no-JS clients still get the same guarantee.
 */
(function () {
  "use strict";

  function updateVend(form) {
    if (!form) return;
    var nameEl = form.querySelector("[data-sb-vend-name]");
    var queuesEl = form.querySelector("[data-sb-vend-queues]");
    var submit = form.querySelector("[data-sb-vend-submit]");
    var name = nameEl ? nameEl.value.trim() : "";
    var typed = queuesEl
      ? queuesEl.value.split(",").map(function (q) { return q.trim(); }).filter(Boolean)
      : [];
    var queues = typed.length + form.querySelectorAll("[data-sb-vend-queue-chips] input:checked").length;
    var verbs = form.querySelectorAll("[data-sb-vend-verbs] input:checked").length;

    var preview = form.querySelector("[data-sb-vend-queue-preview]");
    if (preview) {
      preview.textContent = "";
      typed.forEach(function (q) {
        var span = document.createElement("span");
        span.className = "sb-chip sb-chip--queue";
        span.textContent = q;
        preview.appendChild(span);
      });
    }
    if (submit) submit.disabled = !(name && queues && verbs);
  }

  function init() {
    function refresh(el) {
      var form = el && el.closest ? el.closest("[data-sb-vend]") : null;
      if (form) updateVend(form);
    }
    document.body.addEventListener("input", function (e) { refresh(e.target); });
    document.body.addEventListener("change", function (e) { refresh(e.target); });
    function initAll() {
      document.querySelectorAll("[data-sb-vend]").forEach(updateVend);
    }
    // Enhance the inline form now and every vend form swapped into the overlay. The overlay module
    // owns open/close; this module only re-runs its own scope check on overlay content changes.
    initAll();
    var overlay = document.getElementById("sb-overlay");
    if (overlay) {
      new MutationObserver(initAll).observe(overlay, { childList: true });
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
