/* Behavioral tests for static/js/theme-boot.js + sb-theme.js — the SPEC-0015 theme contract:
 * the pre-paint boot stamps <html data-theme> from localStorage ("sb-theme") synchronously at
 * evaluation (before stylesheets paint), never stamps garbage, and survives disabled storage;
 * the toggle module cycles day/night from the ACTIVE theme (override, else OS scheme), persists
 * the choice, and round-trips it back through the boot on the next load.
 * Governing: SPEC-0015 REQ "Theme Toggle" (scenario "Returning operator"); ADR-0018.
 */
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createDOM, loadModule, el, memStorage } = require("./dom.js");

// --- theme-boot.js: the pre-paint stamp --------------------------------------------------------

test("boot stamps a stored night choice at evaluation time (pre-paint)", () => {
  const env = createDOM({ prefersDark: false }); // a day-preferring OS…
  env.localStorage.setItem("sb-theme", "night"); // …but the operator chose night
  loadModule(env, "theme-boot.js");
  // The attribute is set synchronously by module evaluation itself — no event, no callback —
  // which is what "before first paint" means for a synchronous <head> script.
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "night");
});

test("boot stamps a stored day choice on a dark-preferring OS", () => {
  const env = createDOM({ prefersDark: true });
  env.localStorage.setItem("sb-theme", "day");
  loadModule(env, "theme-boot.js");
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "day");
});

test("boot leaves no attribute with nothing stored (prefers-color-scheme rules)", () => {
  const env = createDOM();
  loadModule(env, "theme-boot.js");
  assert.equal(env.document.documentElement.getAttribute("data-theme"), null);
});

test("boot never stamps an unknown stored value", () => {
  const env = createDOM();
  env.localStorage.setItem("sb-theme", "purple");
  loadModule(env, "theme-boot.js");
  assert.equal(env.document.documentElement.getAttribute("data-theme"), null);
});

test("boot survives disabled storage (private mode)", () => {
  const env = createDOM({ brokenStorage: true });
  assert.doesNotThrow(() => loadModule(env, "theme-boot.js"));
  assert.equal(env.document.documentElement.getAttribute("data-theme"), null);
});

// --- sb-theme.js: the toggle -------------------------------------------------------------------

function toggleDOM(opts) {
  const env = createDOM(opts);
  el(env.document, "button", { "data-sb-theme-toggle": "" });
  return env;
}

test("toggle cycles from the OS default and persists each choice", () => {
  const env = toggleDOM({ prefersDark: false }); // active theme: day (no override)
  loadModule(env, "sb-theme.js");
  const btn = env.document.querySelector("[data-sb-theme-toggle]");
  btn.click();
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "night");
  assert.equal(env.localStorage.getItem("sb-theme"), "night");
  btn.click();
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "day");
  assert.equal(env.localStorage.getItem("sb-theme"), "day");
});

test("toggle on a dark-preferring OS starts from night", () => {
  const env = toggleDOM({ prefersDark: true });
  loadModule(env, "sb-theme.js");
  env.document.querySelector("[data-sb-theme-toggle]").click();
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "day");
});

test("toggle cycles from a boot-stamped override, not the OS scheme", () => {
  const env = toggleDOM({ prefersDark: true }); // OS says dark…
  env.document.documentElement.setAttribute("data-theme", "day"); // …but the boot stamped day
  loadModule(env, "sb-theme.js");
  env.document.querySelector("[data-sb-theme-toggle]").click();
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "night");
});

test("clicks outside the toggle change nothing", () => {
  const env = toggleDOM();
  const bystander = el(env.document, "button", { "data-sb-new": "" });
  loadModule(env, "sb-theme.js");
  bystander.click();
  assert.equal(env.document.documentElement.getAttribute("data-theme"), null);
  assert.equal(env.localStorage.getItem("sb-theme"), null);
});

test("toggle still themes the page when storage is disabled", () => {
  const env = toggleDOM({ brokenStorage: true });
  loadModule(env, "sb-theme.js");
  assert.doesNotThrow(() => env.document.querySelector("[data-sb-theme-toggle]").click());
  assert.equal(env.document.documentElement.getAttribute("data-theme"), "night");
});

// --- the round trip: toggle → reload → boot ----------------------------------------------------

test("returning operator: a toggled choice paints from the first frame of the next load", () => {
  const storage = memStorage(); // one browser profile across two page loads
  // Load 1: a day-preferring OS; the operator toggles to night.
  const first = createDOM({ prefersDark: false, storage });
  el(first.document, "button", { "data-sb-theme-toggle": "" });
  loadModule(first, "sb-theme.js");
  first.document.querySelector("[data-sb-theme-toggle]").click();
  assert.equal(storage.getItem("sb-theme"), "night");
  // Load 2 (reload on the same day-preferring OS): the boot alone stamps night pre-paint —
  // SPEC-0015 scenario "Returning operator", no flash of day.
  const second = createDOM({ prefersDark: false, storage });
  loadModule(second, "theme-boot.js");
  assert.equal(second.document.documentElement.getAttribute("data-theme"), "night");
});
