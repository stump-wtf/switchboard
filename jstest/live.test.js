/* Behavioral tests for static/js/sb-live.js initTodos — the filter-fidelity sweep behind live
 * row insertion (#96): todo_created/todo_resurfaced frames insert rows OOB into #sb-todos-body
 * (stamped data-sb-live-row), and because the SSE stream is one broadcast the client drops an
 * inserted row that misses the viewer's active filter pill or arrives while a search is active,
 * and keeps the empty-state label in step with the row count.
 * Governing: SPEC-0015 REQ "Todos View And Drawer"; issue #96.
 */
"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const { createDOM, loadModule, el } = require("./dom.js");

// todosPage builds the minimal Todos view: the stable panel, the filter pills (active = the
// given filter), the search input, the table body, and the empty-state label.
function todosPage(env, filter) {
  const d = env.document;
  const panel = el(d, "div", { id: "sb-todos-panel" });
  for (const key of ["all", "pending", "claimed", "done", "failed"]) {
    el(d, "a", {
      class: "sb-fpill" + (key === filter ? " sb-fpill--active" : ""),
      "data-sb-filter": key,
    }, panel);
  }
  const search = el(d, "input", { id: "sb-todos-search" }, panel);
  const body = el(d, "tbody", { id: "sb-todos-body" }, panel);
  const empty = el(d, "p", { "data-sb-todos-empty": "" }, panel);
  return { panel, search, body, empty };
}

// liveRow appends an SSE-inserted row (data-sb-live-row, as fragments/todos.html stamps it)
// whose state chip carries the given state.
function liveRow(env, body, id, state) {
  const row = el(env.document, "tr", { id: "sb-tr-" + id, "data-sb-live-row": "" }, body);
  el(env.document, "span", { "data-sb-state": state }, row);
  return row;
}

test("a live-inserted row matching the active filter stays, and the empty label hides", () => {
  const env = createDOM({});
  const page = todosPage(env, "pending");
  loadModule(env, "sb-live.js");
  liveRow(env, page.body, "td_1", "pending");
  env.flushObservers();
  assert.equal(page.body.children.length, 1, "matching row must survive the sweep");
  assert.equal(page.empty.hidden, true, "empty label hides once a row exists");
});

test("a live-inserted row missing the active filter is dropped", () => {
  const env = createDOM({});
  const page = todosPage(env, "done");
  loadModule(env, "sb-live.js");
  liveRow(env, page.body, "td_2", "pending"); // a creation is always pending — wrong lane for "done"
  env.flushObservers();
  assert.equal(page.body.children.length, 0, "non-matching row must be dropped");
  assert.equal(page.empty.hidden, false, "empty label returns with zero rows");
});

test("the all filter accepts every state", () => {
  const env = createDOM({});
  const page = todosPage(env, "all");
  loadModule(env, "sb-live.js");
  liveRow(env, page.body, "td_3", "pending");
  liveRow(env, page.body, "td_4", "claimed"); // a re-surface flavor — still welcome under all
  env.flushObservers();
  assert.equal(page.body.children.length, 2);
});

test("an active search drops every live insert — the query cannot be evaluated client-side", () => {
  const env = createDOM({});
  const page = todosPage(env, "all");
  loadModule(env, "sb-live.js");
  page.search.value = "stripe";
  liveRow(env, page.body, "td_5", "pending");
  env.flushObservers();
  assert.equal(page.body.children.length, 0, "inserts are suppressed while searching");
});

test("server-rendered rows are never swept, even when stale against the filter", () => {
  const env = createDOM({});
  const page = todosPage(env, "pending");
  loadModule(env, "sb-live.js");
  // A server-rendered row (no data-sb-live-row) whose state moved on via an in-place OOB update:
  // the sweep must leave it alone — staleness against the filter is accepted by design there.
  const row = el(env.document, "tr", { id: "sb-tr-td_6" }, page.body);
  el(env.document, "span", { "data-sb-state": "claimed" }, row);
  env.flushObservers();
  assert.equal(page.body.children.length, 1, "server-rendered rows are out of scope");
});
