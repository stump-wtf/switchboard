/* Switchboard overlay/drawer/modal machinery (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0013 REQ "Todo Detail Drawer": manages the shared overlay (open on swap-in, close on
 * Escape / scrim / close control, focus trap + focus return), forwards whole-row clicks/Space on
 * todo rows to the row's drawer trigger (Enter-opens-selection lives in the sb-keys.js keymap
 * registry — SPEC-0015 "Global Keyboard Map", one source of truth), and opens generic modals from
 * hidden <template> elements (data-sb-open-modal). The persona modal is retired — personas
 * create/edit through the full-page wizard (SPEC-0015 REQ "Personas View And Wizard").
 * Presentation-only; the server renders truth and validates every mutation.
 */
(function () {
  "use strict";

  var FOCUSABLE =
    'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

  // watch observes childList mutations on el and runs fn now and on every change.
  function watch(el, fn) {
    if (!el) return;
    new MutationObserver(fn).observe(el, { childList: true });
    fn();
  }

  // ---- whole-row drawer open (Todos table) ----
  // The design record makes the entire todo row clickable. The id cell's button
  // ([data-sb-row-trigger]) stays the accessible, HTMX-wired trigger; a click on the row body (not
  // on a real control) or Space while the row itself is focused (tabindex="0" on the <tr>) just
  // forwards to it, so focus return and the drawer wiring stay on one path. Enter on a focused row
  // takes the SAME forwarding path but is declared in the sb-keys.js registry (`enter` open) so
  // the key-hint footer and the binding share one source of truth (SPEC-0015).
  function rowTrigger(target) {
    var row = target && target.closest ? target.closest("[data-sb-row-open]") : null;
    if (!row) return null;
    return row.querySelector("[data-sb-row-trigger]");
  }

  function initRowOpen() {
    document.body.addEventListener("click", function (e) {
      if (e.target.closest && e.target.closest("button,a,form,input,select,textarea,label")) return;
      var trigger = rowTrigger(e.target);
      if (trigger) trigger.click();
    });
    document.body.addEventListener("keydown", function (e) {
      if (e.key !== " ") return;
      var row = e.target.closest ? e.target.closest("[data-sb-row-open]") : null;
      if (!row || e.target !== row) return; // only when the ROW is focused — controls keep their keys
      e.preventDefault();
      var trigger = row.querySelector("[data-sb-row-trigger]");
      if (trigger) trigger.click();
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

  // A close control anywhere: inside the overlay it closes the overlay; outside it, an anchor
  // close control (e.g. the inline vend form's Cancel → /endpoints) navigates to its own href,
  // and anything else (the standalone drawer page's close) returns to the Todos list.
  function initCloseControls() {
    document.body.addEventListener("click", function (e) {
      var close = e.target.closest ? e.target.closest("[data-sb-close]") : null;
      if (!close) return;
      if (overlay && overlay.contains(close)) {
        e.preventDefault();
        closeOverlay();
      } else if (!close.getAttribute("href")) {
        e.preventDefault();
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

  // initModals opens a generic modal (referenced by data-sb-open-modal → a hidden <template> id)
  // into the shared overlay; the overlay watcher then shows and focuses it. (The persona modal's
  // agent-driven chip constraint is retired with the modal itself — the persona wizard renders its
  // scope chips server-side from the wizard draft.)
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
    });
  }

  function init() {
    initOverlay();
    initCloseControls();
    initModals();
    initRowOpen();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
