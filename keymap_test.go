package switchboard

// Keymap-registry contract (SPEC-0015 REQ "Global Keyboard Map", scenario "Hints match behavior"):
// static/js/sb-keys.js declares ONE registry that both drives the bindings and renders the
// key-hint footer (#sb-keys), so hints can never drift from behavior. These tests parse the
// registry out of the module source and assert the contract: the spec's four bindings are all
// declared (`g`+view, `/` filter, `enter` open, `t` theme); every entry pairs its key with a hint
// whose <kbd> label is the key itself (change a binding, the hint changes with it — no second
// source of truth); the footer renders from the registry alone (no other module touches the slot
// or binds the registry's keys); and bindings never shadow text inputs.
// Governing: ADR-0018 (keyboard-first interaction language), SPEC-0015 REQ "Global Keyboard Map".

import (
	"regexp"
	"strings"
	"testing"
)

// keymapSource returns the embedded sb-keys.js module source.
func keymapSource(t *testing.T) string {
	t.Helper()
	b, err := StaticFS.ReadFile("static/js/sb-keys.js")
	if err != nil {
		t.Fatalf("read static/js/sb-keys.js: %v", err)
	}
	return string(b)
}

// keymapEntry is one parsed registry binding: the key it handles and the hint it renders.
type keymapEntry struct {
	key, hint string
}

// registryEntryRE matches the adjacent key/hint pair each registry entry declares. Keeping them
// adjacent is part of the contract — the pair IS the single source of truth the footer renders.
var registryEntryRE = regexp.MustCompile(`key: "([^"]+)",\s*\n\s*hint: "([^"]+)",`)

// parseRegistry extracts the single keymap registry literal and its entries from the module.
func parseRegistry(t *testing.T, src string) []keymapEntry {
	t.Helper()
	const decl = "var registry = ["
	if got := strings.Count(src, decl); got != 1 {
		t.Fatalf("sb-keys.js declares %d keymap registries, want exactly 1 (SPEC-0015: one registry)", got)
	}
	block := src[strings.Index(src, decl):]
	if end := strings.Index(block, "\n  ];"); end >= 0 {
		block = block[:end]
	} else {
		t.Fatal("sb-keys.js: registry literal is not terminated")
	}
	var entries []keymapEntry
	for _, m := range registryEntryRE.FindAllStringSubmatch(block, -1) {
		entries = append(entries, keymapEntry{key: m[1], hint: m[2]})
	}
	if len(entries) == 0 {
		t.Fatal("sb-keys.js: no key/hint pairs found in the registry literal")
	}
	return entries
}

// TestKeymapRegistryDeclaresSpecBindings: the registry declares exactly the SPEC-0015 global map —
// `g` then a view key (go to view), `/` (focus filter), `enter` (open selection), `t` (theme) —
// and every entry's hint leads with its own key, so the rendered <kbd> label can never disagree
// with the binding it describes.
func TestKeymapRegistryDeclaresSpecBindings(t *testing.T) {
	entries := parseRegistry(t, keymapSource(t))
	want := []string{"g", "/", "Enter", "t"}
	if len(entries) != len(want) {
		t.Fatalf("registry declares %d bindings, want %d (g, /, enter, t)", len(entries), len(want))
	}
	for i, key := range want {
		e := entries[i]
		if e.key != key {
			t.Errorf("registry[%d]: key = %q, want %q", i, e.key, key)
			continue
		}
		if e.hint == "" {
			t.Errorf("registry[%d] (%q): empty hint — the footer would render nothing for it", i, key)
			continue
		}
		// The hint's leading <kbd> label must BE the key (case-insensitively: "Enter" renders as
		// "enter"), so changing a binding forcibly changes its hint with it.
		if !strings.HasPrefix(strings.ToLower(e.hint), strings.ToLower(key)) {
			t.Errorf("registry[%d]: hint %q does not lead with its key %q — hint and binding have drifted", i, e.hint, key)
		}
	}
}

// TestKeymapFooterRendersFromRegistryOnly: the key-hint footer is drawn by iterating the registry,
// and no other JS module writes the #sb-keys slot or binds the registry's keys — so there is no
// second source of truth for either behavior or hints.
func TestKeymapFooterRendersFromRegistryOnly(t *testing.T) {
	src := keymapSource(t)
	// The footer render must target the slot and iterate the registry itself.
	if !strings.Contains(src, `document.getElementById("sb-keys")`) {
		t.Error("sb-keys.js: renderHints does not target the #sb-keys footer slot")
	}
	if !strings.Contains(src, "registry.forEach(") {
		t.Error("sb-keys.js: footer hints are not rendered by iterating the registry")
	}
	// No sibling module may touch the footer slot or claim the registry's keys.
	entries, err := StaticFS.ReadDir("static/js")
	if err != nil {
		t.Fatalf("read static/js: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "sb-keys.js" {
			continue
		}
		b, err := StaticFS.ReadFile("static/js/" + e.Name())
		if err != nil {
			t.Fatalf("read static/js/%s: %v", e.Name(), err)
		}
		other := string(b)
		if strings.Contains(other, `"sb-keys"`) {
			t.Errorf("static/js/%s writes the #sb-keys footer slot — hints must come from the sb-keys.js registry only", e.Name())
		}
		// The registry's printable keys (`g`, `/`, `t`) are dispatched through the registry loop;
		// `Enter` is the one a sibling could plausibly re-bind (row opening), so assert it stays
		// declared in the registry alone.
		if strings.Contains(other, `"Enter"`) {
			t.Errorf("static/js/%s binds \"Enter\" — the open-selection key is declared in the sb-keys.js registry (SPEC-0015: one registry drives behavior)", e.Name())
		}
	}
}

// TestKeymapNeverShadowsTextInputs: bindings must never fire while a text input, select, textarea,
// or contenteditable element has focus, and never while a modifier is held — both guards must sit
// before the registry dispatch (SPEC-0015: "Bindings SHALL never shadow text inputs").
func TestKeymapNeverShadowsTextInputs(t *testing.T) {
	src := keymapSource(t)
	guard := strings.Index(src, "if (typingContext(e.target)) return;")
	modifiers := strings.Index(src, "if (e.ctrlKey || e.metaKey || e.altKey) return;")
	dispatch := strings.Index(src, "for (var i = 0; i < registry.length; i++)")
	if guard < 0 {
		t.Fatal("sb-keys.js: missing the typing-context guard — bindings would shadow text inputs")
	}
	if modifiers < 0 {
		t.Fatal("sb-keys.js: missing the modifier guard — bindings would shadow browser shortcuts")
	}
	if dispatch < 0 {
		t.Fatal("sb-keys.js: registry dispatch loop not found")
	}
	if guard > dispatch || modifiers > dispatch {
		t.Error("sb-keys.js: guards must run BEFORE registry dispatch, or a binding can fire mid-typing")
	}
	// The guard must cover every text-entry surface.
	for _, want := range []string{`"input"`, `"textarea"`, `"select"`, "isContentEditable"} {
		if !strings.Contains(src, want) {
			t.Errorf("sb-keys.js: typing-context guard does not cover %s", want)
		}
	}
}
