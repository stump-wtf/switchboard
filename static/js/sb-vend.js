/* Switchboard vend-wizard step validation (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0015 REQ "Endpoints View And Vend Wizard" / "Wizard Interaction Pattern". Presentation-only
 * progressive enhancement over the wizard's routed step pages: it disables the step's submit until
 * the fields PRESENT IN THAT FORM are satisfied (name on the persona step; ≥1 queue via chips or
 * the free-text field on the queues step; ≥1 verb chip on the verbs step) and mirrors typed queues
 * as preview chips. Each check applies only when its field exists, so the same module serves every
 * step page. The server re-validates every submission and re-renders the step with an inline error
 * on an invalid one, so no-JS clients get the same guarantee (SPEC-0015 no-JS completion).
 */
(function () {
  "use strict";

  function updateVend(form) {
    if (!form) return;
    var nameEl = form.querySelector("[data-sb-vend-name]");
    var queuesEl = form.querySelector("[data-sb-vend-queues]");
    var queueChips = form.querySelector("[data-sb-vend-queue-chips]");
    var verbsEl = form.querySelector("[data-sb-vend-verbs]");
    var submit = form.querySelector("[data-sb-vend-submit]");

    var typed = queuesEl
      ? queuesEl.value.split(",").map(function (q) { return q.trim(); }).filter(Boolean)
      : [];

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

    var ok = true;
    if (nameEl) ok = ok && nameEl.value.trim().length > 0;
    if (queuesEl || queueChips) {
      var checked = queueChips ? queueChips.querySelectorAll("input:checked").length : 0;
      ok = ok && typed.length + checked > 0;
    }
    if (verbsEl) ok = ok && verbsEl.querySelectorAll("input:checked").length > 0;
    if (submit) submit.disabled = !ok;
  }

  function init() {
    function refresh(el) {
      var form = el && el.closest ? el.closest("[data-sb-vend]") : null;
      if (form) updateVend(form);
    }
    document.body.addEventListener("input", function (e) { refresh(e.target); });
    document.body.addEventListener("change", function (e) { refresh(e.target); });
    document.querySelectorAll("[data-sb-vend]").forEach(updateVend);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
