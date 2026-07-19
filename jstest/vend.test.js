/* Behavioral tests for static/js/sb-vend.js — the vend wizard's presentation-only step gating:
 * each step's submit is disabled until the fields PRESENT IN THAT FORM are satisfied, typed
 * queues mirror into the preview chipset, and every check applies only when its field exists
 * (one module serves all step pages). This is enhancement only — the server re-validates every
 * submission and the whole flow completes with JS disabled, bound end-to-end in
 * internal/server/vend_wizard_test.go (value-preserving back nav included).
 * Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard", REQ "Wizard Interaction Pattern"
 * (scenario "JavaScript disabled"); ADR-0018.
 */
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createDOM, loadModule, el, dispatchEvent } = require("./dom.js");

// stepForm builds one wizard step page: a [data-sb-vend] form holding the given fields plus its
// submit, mirroring templates/vend.html's data hooks.
function stepForm(env, build) {
  const form = el(env.document, "form", { "data-sb-vend": "" });
  build(form);
  const submit = el(env.document, "button", { type: "submit", "data-sb-vend-submit": "" }, form);
  return { form, submit };
}

function input(env, form, attrs) {
  return el(env.document, "input", attrs, form);
}

// edit sets a field's value and fires the input event sb-vend.js listens for.
function edit(field, value) {
  field.value = value;
  dispatchEvent(field, "input");
}

// toggleCheck flips a checkbox and fires change.
function toggleCheck(box, checked) {
  box.checked = checked;
  dispatchEvent(box, "change");
}

test("persona step: submit gates on a non-blank name", () => {
  const env = createDOM();
  const { submit } = stepForm(env, (form) => {
    input(env, form, { name: "name", "data-sb-vend-name": "" });
  });
  loadModule(env, "sb-vend.js");
  assert.equal(submit.disabled, true, "empty name must disable the submit at load");
  const name = env.document.querySelector("[data-sb-vend-name]");
  edit(name, "   ");
  assert.equal(submit.disabled, true, "whitespace is not a name");
  edit(name, "release-bot");
  assert.equal(submit.disabled, false);
  edit(name, "");
  assert.equal(submit.disabled, true, "clearing the name re-disables the submit");
});

test("queues step: chips OR typed queues satisfy the gate", () => {
  const env = createDOM();
  let chipBox;
  const { submit } = stepForm(env, (form) => {
    const chips = el(env.document, "div", { "data-sb-vend-queue-chips": "" }, form);
    chipBox = el(env.document, "input", { type: "checkbox", name: "queues", value: "reviews" }, chips);
    input(env, form, { name: "queues_extra", "data-sb-vend-queues": "" });
    el(env.document, "div", { "data-sb-vend-queue-preview": "" }, form);
  });
  loadModule(env, "sb-vend.js");
  assert.equal(submit.disabled, true, "no queues chosen yet");
  toggleCheck(chipBox, true);
  assert.equal(submit.disabled, false, "a checked chip satisfies the gate");
  toggleCheck(chipBox, false);
  assert.equal(submit.disabled, true);
  edit(env.document.querySelector("[data-sb-vend-queues]"), "hotfixes");
  assert.equal(submit.disabled, false, "a typed queue satisfies the gate too");
});

test("queues step: typed queues mirror into the preview chipset", () => {
  const env = createDOM();
  stepForm(env, (form) => {
    input(env, form, { name: "queues_extra", "data-sb-vend-queues": "" });
    el(env.document, "div", { "data-sb-vend-queue-preview": "" }, form);
  });
  loadModule(env, "sb-vend.js");
  const field = env.document.querySelector("[data-sb-vend-queues]");
  const preview = env.document.querySelector("[data-sb-vend-queue-preview]");
  edit(field, " reviews , deploys ,, ");
  assert.deepEqual(
    preview.querySelectorAll("span").map((c) => c.textContent),
    ["reviews", "deploys"],
    "preview chips are the trimmed, non-empty typed queues",
  );
  edit(field, "");
  assert.equal(preview.querySelectorAll("span").length, 0, "clearing the field clears the preview");
});

test("verbs step: submit gates on at least one checked verb", () => {
  const env = createDOM();
  let claim, complete;
  const { submit } = stepForm(env, (form) => {
    const verbs = el(env.document, "div", { "data-sb-vend-verbs": "" }, form);
    claim = el(env.document, "input", { type: "checkbox", name: "verbs", value: "claim" }, verbs);
    complete = el(env.document, "input", { type: "checkbox", name: "verbs", value: "complete" }, verbs);
  });
  loadModule(env, "sb-vend.js");
  assert.equal(submit.disabled, true);
  toggleCheck(claim, true);
  toggleCheck(complete, true);
  assert.equal(submit.disabled, false);
  toggleCheck(claim, false);
  assert.equal(submit.disabled, false, "one verb still satisfies the gate");
  toggleCheck(complete, false);
  assert.equal(submit.disabled, true, "unchecking the last verb re-disables the submit");
});

test("steps without gated fields (lifetime, confirm) never disable their submit", () => {
  // The same module loads on every step page; a form with none of the gated fields — the
  // lifetime radios or the confirm summary — must keep its submit live.
  const env = createDOM();
  const { submit } = stepForm(env, (form) => {
    el(env.document, "div", { "data-sb-vend-lifetime": "" }, form);
  });
  loadModule(env, "sb-vend.js");
  assert.equal(submit.disabled, false, "no gated fields present — the submit stays enabled");
});

test("checked chips pre-rendered by back nav satisfy the gate at load", () => {
  // Back navigation re-renders the draft server-side (the chip arrives checked); the module's
  // load-time pass must honor that state, not reset it.
  const env = createDOM();
  const { submit } = stepForm(env, (form) => {
    const chips = el(env.document, "div", { "data-sb-vend-queue-chips": "" }, form);
    const box = el(env.document, "input", { type: "checkbox", name: "queues", value: "reviews" }, chips);
    box.checked = true; // server rendered `checked`
    input(env, form, { name: "queues_extra", "data-sb-vend-queues": "" });
  });
  loadModule(env, "sb-vend.js");
  assert.equal(submit.disabled, false, "the draft's checked chip counts without any user event");
});

test("edits outside a wizard form are ignored", () => {
  const env = createDOM();
  const stray = el(env.document, "input", { type: "search" }); // not inside [data-sb-vend]
  loadModule(env, "sb-vend.js");
  assert.doesNotThrow(() => edit(stray, "anything"));
});
