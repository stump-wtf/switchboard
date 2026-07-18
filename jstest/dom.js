/* Hand-rolled DOM stub for the sb.js behavioral tests (jstest/*.test.js, run by node's built-in
 * test runner via TestJSModules in jsharness_test.go — no dependencies, no build step; shipped
 * assets under static/js/ are loaded UNMODIFIED).
 *
 * Implements ONLY the surface the sb.js modules touch: elements + attributes, a small selector
 * engine (tag, #id, .class, [attr], [attr="value"], :checked, the descendant combinator, comma
 * lists), bubbling event dispatch, focus/click, localStorage, and matchMedia. Anything a module
 * starts using that this stub lacks fails the tests loudly, which is the point — the stub grows
 * with the modules, never silently behind them.
 *
 * Governing: SPEC-0015 REQ "Global Keyboard Map", REQ "Theme Toggle", REQ "Wizard Interaction
 * Pattern"; ADR-0018 (keyboard-first interaction language); ADR-0001 (no framework, hand-rolled).
 */
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// --- nodes -------------------------------------------------------------------------------------

class SBText {
  constructor(text) {
    this.nodeType = 3;
    this.textContent = String(text);
    this.parentNode = null;
  }
}

class SBElement {
  constructor(doc, tagName) {
    this.nodeType = 1;
    this.ownerDocument = doc;
    this.tagName = String(tagName).toUpperCase();
    this.attrs = new Map();
    this.childNodes = [];
    this.parentNode = null;
    this.listeners = Object.create(null);
    // form-control state the modules read/write
    this.value = "";
    this.checked = false;
    this.disabled = false;
    // instrumentation for assertions
    this.clicks = 0;
  }

  get id() {
    return this.attrs.get("id") || "";
  }
  get className() {
    return this.attrs.get("class") || "";
  }
  set className(v) {
    this.attrs.set("class", String(v));
  }
  get isContentEditable() {
    const v = this.attrs.get("contenteditable");
    return v === "" || v === "true";
  }

  getAttribute(name) {
    return this.attrs.has(name) ? this.attrs.get(name) : null;
  }
  setAttribute(name, v) {
    this.attrs.set(name, String(v));
  }
  hasAttribute(name) {
    return this.attrs.has(name);
  }
  removeAttribute(name) {
    this.attrs.delete(name);
  }

  appendChild(child) {
    child.parentNode = this;
    this.childNodes.push(child);
    return child;
  }

  get textContent() {
    return this.childNodes.map((n) => n.textContent).join("");
  }
  set textContent(v) {
    this.childNodes = [];
    if (v !== "" && v !== null && v !== undefined) this.appendChild(new SBText(v));
  }

  matches(selector) {
    return matchesSelector(this, selector);
  }
  closest(selector) {
    let node = this;
    while (node && node.nodeType === 1) {
      if (matchesSelector(node, selector)) return node;
      node = node.parentNode;
    }
    return null;
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
  querySelectorAll(selector) {
    return descendants(this).filter((el) => matchesSelector(el, selector));
  }

  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  removeEventListener(type, fn) {
    const fns = this.listeners[type] || [];
    const i = fns.indexOf(fn);
    if (i >= 0) fns.splice(i, 1);
  }

  click() {
    this.clicks++;
    return dispatchEvent(this, "click");
  }
  focus() {
    this.ownerDocument.activeElement = this;
  }
}

class SBDocument {
  constructor() {
    this.nodeType = 9;
    this.readyState = "complete"; // modules init synchronously on load, like a deferred script
    this.listeners = Object.create(null);
    this.documentElement = new SBElement(this, "html");
    this.documentElement.ownerDocument = this;
    this.documentElement.parentNode = this; // bubbling terminates at the document
    this.head = this.documentElement.appendChild(new SBElement(this, "head"));
    this.body = this.documentElement.appendChild(new SBElement(this, "body"));
    this.activeElement = this.body;
  }

  createElement(tag) {
    return new SBElement(this, tag);
  }
  createTextNode(text) {
    return new SBText(text);
  }
  getElementById(id) {
    return this.querySelectorAll("#" + id)[0] || null;
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
  querySelectorAll(selector) {
    return [this.documentElement].concat(descendants(this.documentElement)).filter((el) => matchesSelector(el, selector));
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  removeEventListener(type, fn) {
    const fns = this.listeners[type] || [];
    const i = fns.indexOf(fn);
    if (i >= 0) fns.splice(i, 1);
  }
}

// descendants returns el's element descendants in tree order (not el itself).
function descendants(el) {
  const out = [];
  for (const child of el.childNodes) {
    if (child.nodeType === 1) {
      out.push(child);
      out.push(...descendants(child));
    }
  }
  return out;
}

// --- selector engine ---------------------------------------------------------------------------

// matchesSimple matches one compound selector (no combinators): optional tag name plus any run of
// #id, .class, [attr], [attr=value], [attr="value"], and :checked. Unsupported syntax throws —
// a module adopting fancier selectors must extend the stub, not silently match nothing.
function matchesSimple(el, simple) {
  let rest = simple;
  const tag = /^[a-zA-Z][\w-]*/.exec(rest);
  if (tag) {
    if (el.tagName !== tag[0].toUpperCase()) return false;
    rest = rest.slice(tag[0].length);
  }
  const partRE = /([#.][\w-]+)|\[([\w-]+)(?:=(?:"([^"]*)"|([^\]"]*)))?\]|(:checked)/g;
  let consumed = 0;
  let m;
  while ((m = partRE.exec(rest)) !== null) {
    if (m.index !== consumed) break; // gap = syntax the engine does not speak
    consumed += m[0].length;
    if (m[1] !== undefined) {
      if (m[1][0] === "#") {
        if (el.id !== m[1].slice(1)) return false;
      } else if (!el.className.split(/\s+/).includes(m[1].slice(1))) {
        return false;
      }
    } else if (m[2] !== undefined) {
      if (!el.attrs.has(m[2])) return false;
      const want = m[3] !== undefined ? m[3] : m[4];
      if (want !== undefined && el.attrs.get(m[2]) !== want) return false;
    } else if (m[5] !== undefined) {
      if (!el.checked) return false;
    }
  }
  if (consumed !== rest.length) {
    throw new Error("jstest/dom.js: unsupported selector syntax " + JSON.stringify(simple));
  }
  return true;
}

// matchesSelector supports comma lists of descendant chains ("a b c" = c inside b inside a).
function matchesSelector(el, selector) {
  return String(selector)
    .split(",")
    .some((part) => {
      const simples = part.trim().split(/\s+/);
      if (!matchesSimple(el, simples[simples.length - 1])) return false;
      let i = simples.length - 2;
      let node = el.parentNode;
      while (i >= 0 && node && node.nodeType === 1) {
        if (matchesSimple(node, simples[i])) i--;
        node = node.parentNode;
      }
      return i < 0;
    });
}

// --- events ------------------------------------------------------------------------------------

// dispatchEvent bubbles a synthetic event from target through its ancestors to the document and
// returns the event so tests can assert on defaultPrevented.
function dispatchEvent(target, type, props) {
  const event = Object.assign(
    {
      type,
      target,
      defaultPrevented: false,
      preventDefault() {
        this.defaultPrevented = true;
      },
      stopPropagation() {},
    },
    props || {},
  );
  let node = target;
  while (node) {
    const fns = (node.listeners && node.listeners[type]) || [];
    for (const fn of fns.slice()) fn(event);
    node = node.nodeType === 9 ? null : node.parentNode;
  }
  return event;
}

// keydown dispatches a keyboard event; opts.target defaults to body (nothing focused), modifiers
// default unpressed — exactly the shape sb-keys.js reads.
function keydown(document, key, opts) {
  const o = opts || {};
  const target = o.target || document.body;
  return dispatchEvent(target, "keydown", {
    key,
    ctrlKey: !!o.ctrlKey,
    metaKey: !!o.metaKey,
    altKey: !!o.altKey,
    target,
  });
}

// --- environment -------------------------------------------------------------------------------

function memStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => {
      m.set(k, String(v));
    },
    removeItem: (k) => {
      m.delete(k);
    },
  };
}

// brokenStorage models private-mode storage: every access throws (the modules must swallow it).
function brokenStorage() {
  const deny = () => {
    throw new Error("storage disabled");
  };
  return { getItem: deny, setItem: deny, removeItem: deny };
}

// createDOM builds one isolated page environment. opts: prefersDark (OS scheme), brokenStorage,
// storage (share a storage across "reloads" to test persistence round-trips). env.now, when set,
// pins Date.now() inside loaded modules (the `g` chord window). env.window.location.assigned
// records navigations.
function createDOM(opts) {
  const o = opts || {};
  const env = {
    prefersDark: !!o.prefersDark,
    now: null,
    document: new SBDocument(),
    localStorage: o.storage || (o.brokenStorage ? brokenStorage() : memStorage()),
  };
  env.window = {
    document: env.document,
    localStorage: env.localStorage,
    matchMedia(query) {
      return { matches: /dark/.test(query) && env.prefersDark };
    },
    location: {
      assigned: [],
      assign(url) {
        env.window.location.assigned.push(url);
      },
    },
  };
  return env;
}

// loadModule evaluates a shipped static/js module, unmodified, against the environment.
function loadModule(env, name) {
  const src = fs.readFileSync(path.join(__dirname, "..", "static", "js", name), "utf8");
  const ctx = vm.createContext({
    document: env.document,
    window: env.window,
    localStorage: env.localStorage,
    Date: { now: () => (env.now !== null ? env.now : Date.now()) },
    console,
  });
  vm.runInContext(src, ctx, { filename: "static/js/" + name });
}

// el creates an element with attributes and appends it (to body by default).
function el(document, tag, attrs, parent) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, v);
  (parent || document.body).appendChild(node);
  return node;
}

module.exports = { createDOM, loadModule, el, keydown, dispatchEvent, memStorage };
