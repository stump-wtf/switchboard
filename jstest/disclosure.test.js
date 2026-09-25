/* Behavioral tests for static/js/sb-disclosure.js — a <details data-sb-disclosure> mirrors its
 * open state onto its <summary>'s aria-expanded (the Quarantine view's payload disclosure), and
 * leaves any other <details> alone. Governing: SPEC-0026 REQ-9 (the payload is collapsed by
 * default; the disclosure toggles aria-expanded); ADR-0018.
 */
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createDOM, loadModule, el, dispatchEvent } = require("./dom.js");

function disclosure(env, attrs) {
  const d = env.document;
  const details = el(d, "details", attrs);
  const summary = el(d, "summary", { "aria-expanded": "false" }, details);
  el(d, "pre", {}, details);
  return { details, summary };
}

test("opening a disclosure sets aria-expanded, closing clears it", () => {
  const env = createDOM();
  loadModule(env, "sb-disclosure.js");
  const { details, summary } = disclosure(env, { "data-sb-disclosure": "" });

  details.open = true;
  dispatchEvent(details, "toggle");
  assert.equal(summary.getAttribute("aria-expanded"), "true");

  details.open = false;
  dispatchEvent(details, "toggle");
  assert.equal(summary.getAttribute("aria-expanded"), "false");
});

test("a disclosure swapped in after load is covered without re-binding", () => {
  const env = createDOM();
  loadModule(env, "sb-disclosure.js");
  const { details, summary } = disclosure(env, { "data-sb-disclosure": "" }); // created after load
  details.open = true;
  dispatchEvent(details, "toggle");
  assert.equal(summary.getAttribute("aria-expanded"), "true");
});

test("a plain details element is left alone", () => {
  const env = createDOM();
  loadModule(env, "sb-disclosure.js");
  const { details, summary } = disclosure(env, {});
  details.open = true;
  dispatchEvent(details, "toggle");
  assert.equal(summary.getAttribute("aria-expanded"), "false");
});
