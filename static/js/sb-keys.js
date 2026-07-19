/* Switchboard global keymap + key-hint footer (embedded, CSP script-src 'self' — ADR-0001).
 *
 * One of the sb.js feature modules (split per SPEC-0015 foundation; ADR-0018). Governing:
 * SPEC-0015 REQ "Global Keyboard Map" and REQ "Application Shell And Navigation": ONE registry
 * both drives the bindings and renders the key-hint footer (#sb-keys), so hints can never drift
 * from behavior. Bindings never fire while a text input, select, textarea, or contenteditable
 * element has focus, and never while a modifier is held.
 *
 *   g <view>  go to view (the second key comes from the nav's data-sb-nav stamps)
 *   /         focus the view's filter/search input
 *   enter     open the selection (forwards the focused data-sb-row-open row to its trigger)
 *   t         cycle the day/night theme (clicks the visible control — one code path)
 */
(function () {
  "use strict";

  var GOTO_WINDOW_MS = 1500;
  var pendingGoto = 0;

  // The single keymap registry (SPEC-0015: no second source of truth). `when` gates rendering and
  // handling on page state, so e.g. `g` hints only show where the nav exists.
  var registry = [
    {
      key: "g",
      hint: "g+view go to",
      when: function () {
        return document.querySelector("[data-sb-nav]") !== null;
      },
      handle: function () {
        pendingGoto = Date.now();
      },
    },
    {
      key: "/",
      hint: "/ filter",
      when: function () {
        return filterInput() !== null;
      },
      handle: function (e) {
        e.preventDefault();
        filterInput().focus();
      },
    },
    {
      // Open the selection: forwards a focused list row ([data-sb-row-open], tabindex="0") to its
      // accessible, HTMX-wired trigger ([data-sb-row-trigger]) — same forwarding path as the
      // whole-row click in sb-overlay.js, so drawer wiring and focus return stay on one path.
      key: "Enter",
      hint: "enter open",
      when: function () {
        return document.querySelector("[data-sb-row-open]") !== null;
      },
      handle: function (e) {
        var el = document.activeElement;
        var row = el && el.closest ? el.closest("[data-sb-row-open]") : null;
        if (!row || el !== row) return; // only when the ROW is focused — controls keep their keys
        var trigger = row.querySelector("[data-sb-row-trigger]");
        if (trigger) {
          e.preventDefault();
          trigger.click();
        }
      },
    },
    {
      key: "t",
      hint: "t theme",
      when: function () {
        return document.querySelector("[data-sb-theme-toggle]") !== null;
      },
      handle: function () {
        document.querySelector("[data-sb-theme-toggle]").click();
      },
    },
  ];

  function filterInput() {
    return document.querySelector("input[type=search], [role=search] input");
  }

  // typingContext: keys must never shadow text entry (SPEC-0015 "never shadow text inputs").
  function typingContext(el) {
    if (!el) return false;
    var tag = (el.tagName || "").toLowerCase();
    return tag === "input" || tag === "textarea" || tag === "select" || el.isContentEditable;
  }

  function onKeydown(e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (typingContext(e.target)) return;

    // Second key of a pending `g <view>` chord: navigate via the nav's own data-sb-nav stamp, so
    // the chord can only reach views the shell actually offers.
    if (pendingGoto && Date.now() - pendingGoto < GOTO_WINDOW_MS) {
      pendingGoto = 0;
      var target = document.querySelector('[data-sb-nav="' + e.key.toLowerCase() + '"]');
      if (target) {
        e.preventDefault();
        window.location.assign(target.getAttribute("href"));
      }
      return;
    }
    pendingGoto = 0;

    for (var i = 0; i < registry.length; i++) {
      var b = registry[i];
      if (e.key === b.key && (!b.when || b.when())) {
        b.handle(e);
        return;
      }
    }
  }

  // renderHints draws the footer from the registry — the ONLY place hints come from.
  function renderHints() {
    var slot = document.getElementById("sb-keys");
    if (!slot) return;
    slot.textContent = "";
    registry.forEach(function (b, i) {
      if (b.when && !b.when()) return;
      if (slot.childNodes.length) {
        var sep = document.createElement("span");
        sep.className = "sb-keys__sep";
        sep.setAttribute("aria-hidden", "true");
        sep.textContent = "•";
        slot.appendChild(sep);
      }
      var pair = document.createElement("span");
      pair.className = "sb-keys__pair";
      var parts = b.hint.split(" ");
      var key = document.createElement("kbd");
      key.className = "sb-keys__key";
      key.textContent = parts[0];
      pair.appendChild(key);
      pair.appendChild(document.createTextNode(" " + parts.slice(1).join(" ")));
      slot.appendChild(pair);
    });
  }

  function init() {
    document.addEventListener("keydown", onKeydown);
    renderHints();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
