/* Behavioral tests for static/js/sb-keys.js — the global keymap registry: dispatch of every
 * SPEC-0015 binding (`g`+view chord, `/` filter, `enter` open, `t` theme), the input-shadowing
 * and modifier guards, and the key-hint footer rendering from the registry alone with `when`
 * gating (no second source of truth). The registry's SOURCE contract (entry shape, sibling
 * modules keeping out) is pinned separately in keymap_test.go; these tests exercise the module
 * running. Governing: SPEC-0015 REQ "Global Keyboard Map" (scenario "Hints match behavior");
 * ADR-0018.
 */
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createDOM, loadModule, el, keydown } = require("./dom.js");

// shellDOM builds the full shell surface every binding's `when` gate looks for: nav stamps, a
// search input, a theme toggle, an openable row with its trigger, and the footer slot.
function shellDOM() {
  const env = createDOM();
  const d = env.document;
  const nav = el(d, "nav", { "data-sb-nav-rail": "" });
  el(d, "a", { "data-sb-nav": "t", href: "/todos" }, nav);
  el(d, "a", { "data-sb-nav": "e", href: "/endpoints" }, nav);
  el(d, "button", { "data-sb-theme-toggle": "" });
  el(d, "input", { type: "search" });
  const row = el(d, "tr", { "data-sb-row-open": "", tabindex: "0" });
  el(d, "button", { "data-sb-row-trigger": "" }, row);
  el(d, "span", { id: "sb-keys", "data-sb-keys": "" });
  return env;
}

test("t clicks the visible theme control (one code path)", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  keydown(env.document, "t");
  assert.equal(env.document.querySelector("[data-sb-theme-toggle]").clicks, 1);
});

test("t + sb-theme.js together cycle the theme end to end", () => {
  // The keymap forwards to the SAME control sb-theme.js listens on — pressing `t` must actually
  // move the theme, proving the two modules share one code path.
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  loadModule(env, "sb-theme.js");
  keydown(env.document, "t");
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "night");
  assert.equal(env.localStorage.getItem("sb-theme"), "night");
});

test("/ focuses the filter input and suppresses the keystroke", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const e = keydown(env.document, "/");
  assert.equal(env.document.activeElement, env.document.querySelector("input[type=search]"));
  assert.equal(e.defaultPrevented, true, "the / must not leak into the freshly focused input");
});

test("g then a nav key navigates via the nav's own data-sb-nav stamp", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  keydown(env.document, "g");
  const e = keydown(env.document, "t");
  assert.deepEqual(env.window.location.assigned, ["/todos"]);
  assert.equal(e.defaultPrevented, true);
  // The chord consumed `t` for navigation — the theme binding must NOT also fire.
  assert.equal(env.document.querySelector("[data-sb-theme-toggle]").clicks, 0);
});

test("g chord only reaches views the shell offers, then resets", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  keydown(env.document, "g");
  keydown(env.document, "z"); // no data-sb-nav="z" anywhere
  assert.deepEqual(env.window.location.assigned, []);
  // The failed chord is spent: the next `t` is the theme binding again.
  keydown(env.document, "t");
  assert.equal(env.document.querySelector("[data-sb-theme-toggle]").clicks, 1);
  assert.deepEqual(env.window.location.assigned, []);
});

test("g chord expires after its window", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  env.now = 100000;
  keydown(env.document, "g");
  env.now = 100000 + 1501; // past GOTO_WINDOW_MS
  keydown(env.document, "t");
  assert.deepEqual(env.window.location.assigned, [], "a stale chord must not navigate");
  assert.equal(env.document.querySelector("[data-sb-theme-toggle]").clicks, 1);
});

test("enter forwards a FOCUSED row to its trigger", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const row = env.document.querySelector("[data-sb-row-open]");
  const trigger = row.querySelector("[data-sb-row-trigger]");
  row.focus();
  const e = keydown(env.document, "Enter", { target: row });
  assert.equal(trigger.clicks, 1);
  assert.equal(e.defaultPrevented, true);
});

test("enter leaves controls inside the row alone", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const row = env.document.querySelector("[data-sb-row-open]");
  const trigger = row.querySelector("[data-sb-row-trigger]");
  trigger.focus(); // focus is on a control WITHIN the row, not the row itself
  keydown(env.document, "Enter", { target: trigger });
  assert.equal(trigger.clicks, 0, "controls keep their own Enter behavior");
});

test("bindings never fire from a typing context (input shadowing guard)", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const d = env.document;
  const toggle = d.querySelector("[data-sb-theme-toggle]");
  const surfaces = [
    el(d, "input", { type: "text" }),
    el(d, "textarea", {}),
    el(d, "select", {}),
    el(d, "div", { contenteditable: "true" }),
  ];
  for (const surface of surfaces) {
    keydown(d, "t", { target: surface });
    keydown(d, "g", { target: surface });
    keydown(d, "/", { target: surface });
  }
  assert.equal(toggle.clicks, 0, "typing must never trigger bindings");
  assert.deepEqual(env.window.location.assigned, []);
  assert.notEqual(d.activeElement, d.querySelector("input[type=search]"), "/ must not steal focus mid-typing");
});

test("bindings never fire while a modifier is held", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const toggle = env.document.querySelector("[data-sb-theme-toggle]");
  keydown(env.document, "t", { ctrlKey: true });
  keydown(env.document, "t", { metaKey: true });
  keydown(env.document, "t", { altKey: true });
  assert.equal(toggle.clicks, 0, "browser shortcuts must pass through untouched");
});

test("key-hint footer renders every active binding from the registry", () => {
  const env = shellDOM();
  loadModule(env, "sb-keys.js");
  const slot = env.document.getElementById("sb-keys");
  const kbds = slot.querySelectorAll("kbd").map((k) => k.textContent);
  assert.deepEqual(kbds, ["g+view", "/", "enter", "t"], "footer <kbd> labels must be the registry's own keys");
  const seps = slot.querySelectorAll("span.sb-keys__sep");
  assert.equal(seps.length, kbds.length - 1, "one separator between each hint pair");
  for (const sep of seps) assert.equal(sep.getAttribute("aria-hidden"), "true");
});

test("key-hint footer honors the registry's when gates", () => {
  // A page offering ONLY the theme control: the other bindings are inactive there, so their
  // hints must not render — same gate, same registry entry, one source of truth.
  const env = createDOM();
  el(env.document, "button", { "data-sb-theme-toggle": "" });
  el(env.document, "span", { id: "sb-keys", "data-sb-keys": "" });
  loadModule(env, "sb-keys.js");
  const kbds = env.document.getElementById("sb-keys").querySelectorAll("kbd").map((k) => k.textContent);
  assert.deepEqual(kbds, ["t"], "only the active binding's hint renders");
});
