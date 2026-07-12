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

  // ---- drawer overlay: open on swap-in, trap focus, close on Escape / scrim / close button ----
  var overlay, lastFocused;

  function overlayOpen() {
    return overlay && !overlay.hidden && overlay.children.length > 0;
  }

  function firstFocusable() {
    if (!overlay) return null;
    // The drawer (todo detail) and the modals (vend/…) both scope focus to their panel; fall back to
    // the overlay so any future overlay content still focuses sanely.
    var panel = overlay.querySelector("[data-sb-drawer],[data-sb-modal]");
    var scope = panel || overlay;
    var els = scope.querySelectorAll(FOCUSABLE);
    return els.length ? els[0] : panel;
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

  // ---- persona modal: open a hidden <template> into the overlay and drive the verb/queue chip
  // constraint. Presentation only — the server validates every subset against the backing agent's
  // vended grant (SPEC-0009); this just hides what the human cannot choose. The agent-card URL under
  // the name is server-rendered (the id-based shape the well-known endpoint resolves; ids are
  // assigned on create) so there is no client-side URL derivation to drift from the endpoint.
  // Governing: SPEC-0013 REQ "Personas View" (create/edit modal), SPEC-0009 REQ "Well-Known Card
  // Endpoint". ----

  // renderChips rebuilds a chip container's checkboxes from a verb/queue list, preserving any values
  // that were checked before the swap. name is the form field ("verbs" / "queues").
  function renderChips(container, values, name) {
    if (!container) return;
    var checked = {};
    container.querySelectorAll("input[type=checkbox]").forEach(function (cb) {
      if (cb.checked) checked[cb.value] = true;
    });
    if (!values.length) {
      container.innerHTML = '<span class="sb-muted">the backing agent vends no ' + name + "</span>";
      return;
    }
    container.innerHTML = values
      .map(function (v) {
        return (
          '<label class="sb-chip sb-chip--check"><input type="checkbox" name="' +
          name +
          '" value="' +
          v +
          '"' +
          (checked[v] ? " checked" : "") +
          "> " +
          v +
          "</label>"
        );
      })
      .join("");
  }

  // wireModal binds the agent-driven chip constraint on a freshly opened modal.
  function wireModal(modal) {
    if (!modal) return;
    var select = modal.querySelector("[data-sb-agent-select]");
    if (select) {
      select.addEventListener("change", function () {
        var opt = select.options[select.selectedIndex];
        var verbs = (opt.getAttribute("data-verbs") || "").split(",").filter(Boolean);
        var queues = (opt.getAttribute("data-queues") || "").split(",").filter(Boolean);
        renderChips(modal.querySelector("[data-sb-verb-chips]"), verbs, "verbs");
        renderChips(modal.querySelector("[data-sb-queue-chips]"), queues, "queues");
      });
    }
  }

  // initModals opens a persona modal (referenced by data-sb-open-modal → a hidden <template> id) into
  // the shared overlay; the overlay watcher then shows and focuses it.
  function initModals() {
    document.body.addEventListener("click", function (e) {
      var trigger = e.target.closest ? e.target.closest("[data-sb-open-modal]") : null;
      if (!trigger) return;
      e.preventDefault();
      if (!overlay) return;
      var tpl = document.getElementById(trigger.getAttribute("data-sb-open-modal"));
      if (!tpl || !("content" in tpl)) return;
      lastFocused = trigger; // return focus here on close
      overlay.innerHTML = "";
      overlay.appendChild(tpl.content.cloneNode(true));
      wireModal(overlay.querySelector("[data-sb-modal]"));
    });
  }

  // ---- vend modal: live scope validation + queue chip preview ----
  // Governing: SPEC-0013 REQ "Endpoints View and Vend Modal" ("MUST refuse submission until name, at
  // least one queue, and at least one verb are chosen"). Presentation-only: it disables the submit
  // button and mirrors typed queues as chips; the server re-validates every submission and mints
  // nothing on an invalid one, so no-JS clients still get the same guarantee.
  function updateVend(form) {
    if (!form) return;
    var nameEl = form.querySelector("[data-sb-vend-name]");
    var queuesEl = form.querySelector("[data-sb-vend-queues]");
    var submit = form.querySelector("[data-sb-vend-submit]");
    var name = nameEl ? nameEl.value.trim() : "";
    var queues = queuesEl
      ? queuesEl.value.split(",").map(function (q) { return q.trim(); }).filter(Boolean)
      : [];
    var verbs = form.querySelectorAll("[data-sb-vend-verbs] input:checked").length;

    var chips = form.querySelector("[data-sb-vend-queue-chips]");
    if (chips) {
      chips.textContent = "";
      queues.forEach(function (q) {
        var span = document.createElement("span");
        span.className = "sb-chip sb-chip--queue";
        span.textContent = q;
        chips.appendChild(span);
      });
    }
    if (submit) submit.disabled = !(name && queues.length && verbs);
  }

  function initVend() {
    function refresh(el) {
      var form = el && el.closest ? el.closest("[data-sb-vend]") : null;
      if (form) updateVend(form);
    }
    document.body.addEventListener("input", function (e) { refresh(e.target); });
    document.body.addEventListener("change", function (e) { refresh(e.target); });
    function initAll() {
      document.querySelectorAll("[data-sb-vend]").forEach(updateVend);
    }
    // Enhance the inline form now and every vend form swapped into the overlay.
    initAll();
    if (overlay) watch(overlay, initAll);
  }

  function init() {
    initToasts();
    initFeed();
    initOverlay();
    initCloseControls();
    initModals();
    initVend();
    tickCountdowns();
    setInterval(tickCountdowns, 1000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
