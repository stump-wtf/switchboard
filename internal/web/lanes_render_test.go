package web

// Companion render coverage for the patch-panel lanes (#37, companion to #25): the board_lanes
// fragment renders the full three-lane panel from its view models (each card inside its own lane
// list, header counts, empty-state toggling), the card chip matrix covers every ephemeral and
// durable state, and the lane surface is THEME-COMPLETE — every CSS custom property the lane
// styles consume resolves in both the day and night themes, and the lane markup carries no inline
// styling that could pin one theme's colors. Assertions key on data-sb-* attributes and ids, never
// on classes alone (the board_dom_test.go convention).
// Governing: SPEC-0015 REQ "Patch Panel Board", REQ "Live Fragment Architecture", REQ "Design
// Token System" (both themes complete); ADR-0018.

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// laneList extracts the inner HTML of one lane's card list (<ul id="sb-lane-<key>-cards">…</ul>)
// so a card can be asserted INSIDE its own lane, not merely somewhere on the page.
func laneList(t *testing.T, body, key string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)<ul id="sb-lane-` + key + `-cards"[^>]*>(.*?)</ul>`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("lane list #sb-lane-%s-cards missing from %q", key, body)
	}
	return m[1]
}

// TestBoardLanesFragmentRendersFromViewModels renders the full three-lane panel from a populated
// lanesView: every card lands inside its own lane list, the lane headers carry their counts (the
// received count DOM-derived from the rendered cards, verified/patched from the durable numbers),
// populated lanes hide their empty-state labels, and a server render is never OOB.
func TestBoardLanesFragmentRendersFromViewModels(t *testing.T) {
	h := newTestHandler(t)
	received := inflightCard("github", "push", "signed", "gh-d-77", time.Now())
	queued := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_q1", Source: "github", Kind: "push", Title: "PR #7 opened", State: "pending",
		IdempotencyKey: "gh-d-7", CreatedAt: time.Now().Add(-2 * time.Minute),
	}, TrustMode: "signed"})
	claimed := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_c1", Source: "stripe", Kind: "invoice.paid", Title: "stripe invoice.paid",
		State: "claimed", Owner: "op:h1", CreatedAt: time.Now().Add(-1 * time.Hour),
	}, TrustMode: "signed"})

	body := renderFrag(t, h, "board_lanes", lanesView{
		Received: []laneCard{received},
		Verified: []laneCard{queued},
		Patched:  []laneCard{claimed},
		Counts:   laneCounts{Verified: 3, Patched: 2},
	})

	// Lanes render in board order: received, verified, patched through.
	iR := strings.Index(body, `data-sb-lane="received"`)
	iV := strings.Index(body, `data-sb-lane="verified"`)
	iP := strings.Index(body, `data-sb-lane="patched"`)
	if iR < 0 || iV < 0 || iP < 0 || iR >= iV || iV >= iP {
		t.Errorf("lanes out of order: received=%d verified=%d patched=%d", iR, iV, iP)
	}

	// Each card sits inside its OWN lane list.
	if l := laneList(t, body, "received"); !strings.Contains(l, `id="`+received.DomID+`"`) {
		t.Errorf("received lane missing its ephemeral card: %q", l)
	}
	if l := laneList(t, body, "verified"); !strings.Contains(l, `id="sb-td-td_q1"`) {
		t.Errorf("verified lane missing the queued card: %q", l)
	}
	if l := laneList(t, body, "patched"); !strings.Contains(l, `id="sb-td-td_c1"`) {
		t.Errorf("patched lane missing the claimed card: %q", l)
	}
	if l := laneList(t, body, "verified"); strings.Contains(l, `id="sb-td-td_c1"`) {
		t.Error("claimed card leaked into the verified lane")
	}

	// Header counts: received DOM-derived (renders the card count), verified/patched from the
	// durable counts with their OOB swap-target ids.
	for _, want := range []string{
		`data-sb-lane-count="received">1<`,
		`id="sb-lane-count-verified" class="sb-lane__n"><a href="/todos?filter=pending" class="sb-lane__link">3</a></span> · pending todos`,
		`id="sb-lane-count-patched" class="sb-lane__n"><a href="/todos?filter=claimed" class="sb-lane__link">2</a></span> · claimed, done, failed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lane header counts: missing %q", want)
		}
	}

	// Populated lanes hide their empty-state labels.
	for _, lane := range []string{"received", "verified", "patched"} {
		if !strings.Contains(body, `data-sb-lane-empty="`+lane+`" hidden`) {
			t.Errorf("populated lane %q must hide its empty-state label", lane)
		}
	}

	// A server render is page content, never an OOB swap.
	if strings.Contains(body, "hx-swap-oob") {
		t.Error("server-rendered board_lanes must not carry hx-swap-oob")
	}

	// An empty board renders zero counts, empty lists, and VISIBLE empty states.
	empty := renderFrag(t, h, "board_lanes", lanesView{})
	for _, want := range []string{
		`data-sb-lane-count="received">0<`,
		`id="sb-lane-count-verified" class="sb-lane__n"><a href="/todos?filter=pending" class="sb-lane__link">0<`,
		`id="sb-lane-count-patched" class="sb-lane__n"><a href="/todos?filter=claimed" class="sb-lane__link">0<`,
		"quiet · no lines in flight",
		"none waiting · claims are keeping up",
		"nothing claimed yet",
	} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty board_lanes: missing %q", want)
		}
	}
	for _, lane := range []string{"received", "verified", "patched"} {
		if strings.Contains(empty, `data-sb-lane-empty="`+lane+`" hidden`) {
			t.Errorf("empty lane %q must show its empty-state label", lane)
		}
	}
	for _, lane := range []string{"received", "verified", "patched"} {
		if l := laneList(t, empty, lane); strings.TrimSpace(l) != "" {
			t.Errorf("empty %s lane must render no cards: %q", lane, l)
		}
	}
}

// TestLaneCardChipMatrix pins the state chip across the FULL taxonomy — the ephemeral received
// states (verifying · rejected · deduped) and the durable states (pending→queued · claimed · done
// · failed) — plus the pulse-vs-dot split: only the in-flight verifying card pulses, every settled
// state carries the static dot. Complements live_test.go's per-frame coverage with the one matrix
// the SPEC-0015 card anatomy promises ("trust chip, state chip").
func TestLaneCardChipMatrix(t *testing.T) {
	h := newTestHandler(t)
	cases := []struct {
		state string
		label string // the chip text laneStateLabel renders
		lane  string
		ttl   int
	}{
		{"verifying", "verifying", laneReceived, inflightTTLMS},
		{"rejected", "rejected", laneReceived, resolvedTTLMS},
		{"deduped", "deduped", laneReceived, resolvedTTLMS},
		{"pending", "queued", laneVerified, 0},
		{"claimed", "claimed", lanePatched, 0},
		{"done", "done", lanePatched, 0},
		{"failed", "failed", lanePatched, 0},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			card := laneCard{DomID: "sb-x-" + c.state, Lane: c.lane, Source: "github", Kind: "push",
				TrustMode: "signed", State: c.state, At: time.Now(), TTLMS: c.ttl}
			out := renderFrag(t, h, "lane_card", card)
			if !strings.Contains(out, "sb-status--"+c.state) || !strings.Contains(out, ">"+c.label+"<") {
				t.Errorf("state %q: chip must carry sb-status--%s and the %q label: %q", c.state, c.state, c.label, out)
			}
			if !strings.Contains(out, `data-sb-lane-card="`+c.lane+`"`) {
				t.Errorf("state %q: card must stamp its lane %q", c.state, c.lane)
			}
			if c.state == "verifying" {
				if !strings.Contains(out, "sb-lcard__pulse") {
					t.Errorf("verifying card must pulse: %q", out)
				}
			} else if !strings.Contains(out, "sb-status__dot") {
				t.Errorf("settled state %q must carry the static status dot: %q", c.state, out)
			}
			if c.ttl > 0 {
				if !strings.Contains(out, `data-sb-ephemeral="`+strconv.Itoa(c.ttl)+`"`) {
					t.Errorf("ephemeral state %q must stamp its TTL %d: %q", c.state, c.ttl, out)
				}
			} else if strings.Contains(out, "data-sb-ephemeral") {
				t.Errorf("durable state %q must not stamp a client TTL: %q", c.state, out)
			}
			// The trust chip names its mode in words on every card.
			if !strings.Contains(out, ">signed</span>") {
				t.Errorf("state %q: trust chip must carry readable text: %q", c.state, out)
			}
		})
	}
}

// cssTopBlock extracts the body of the first top-level block whose opening is introduced by
// marker (e.g. `:root {`), tracking brace depth so nested blocks stay intact.
func cssTopBlock(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("tokens.css: marker %q not found", marker)
	}
	j := strings.Index(src[i:], "{")
	if j < 0 {
		t.Fatalf("tokens.css: no block after %q", marker)
	}
	depth, start := 1, i+j+1
	for k := start; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start:k]
			}
		}
	}
	t.Fatalf("tokens.css: unbalanced block after %q", marker)
	return ""
}

var cssPropDef = regexp.MustCompile(`(--sb-[a-z0-9-]+)\s*:`)

// cssProps returns the set of custom properties DEFINED in a block body.
func cssProps(block string) map[string]bool {
	set := map[string]bool{}
	for _, m := range cssPropDef.FindAllStringSubmatch(block, -1) {
		set[m[1]] = true
	}
	return set
}

// TestLaneStylesResolveInBothThemes is the server-side "renders in both themes" gate for the lane
// surface (SPEC-0015 REQ "Design Token System": day and night both complete). There is no browser
// in CI, so the contract is pinned where it lives: (1) every CSS custom property the lane/lcard
// styles consume is defined in the base day `:root` block, so a day render resolves every token;
// (2) the explicit `[data-theme="night"]` override set is IDENTICAL to the
// `prefers-color-scheme: dark` set, so the toggle and the OS preference can never drift apart;
// (3) every night override re-defines a day token (no night-only property a day render would
// miss). Together: any token the lanes consume resolves to a value under BOTH themes by either
// selection path.
func TestLaneStylesResolveInBothThemes(t *testing.T) {
	tokens, err := os.ReadFile("../../static/tokens.css")
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	sheet, err := os.ReadFile("../../static/switchboard.css")
	if err != nil {
		t.Fatalf("read switchboard.css: %v", err)
	}

	day := cssProps(cssTopBlock(t, string(tokens), ":root {"))
	osNight := cssProps(cssTopBlock(t, string(tokens), `:root:not([data-theme="day"])`))
	night := cssProps(cssTopBlock(t, string(tokens), `[data-theme="night"]`))
	if len(day) == 0 || len(night) == 0 || len(osNight) == 0 {
		t.Fatalf("token extraction broke: day=%d osNight=%d night=%d", len(day), len(osNight), len(night))
	}

	// Every var(--sb-…) consumed by a lane/lcard rule must be defined in the day base set.
	ruleRe := regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	varRe := regexp.MustCompile(`var\((--sb-[a-z0-9-]+)`)
	laneRefs := map[string]bool{}
	for _, m := range ruleRe.FindAllStringSubmatch(string(sheet), -1) {
		sel := m[1]
		if !strings.Contains(sel, "sb-lane") && !strings.Contains(sel, "sb-lcard") {
			continue
		}
		for _, v := range varRe.FindAllStringSubmatch(m[2], -1) {
			laneRefs[v[1]] = true
		}
	}
	if len(laneRefs) == 0 {
		t.Fatal("no var(--sb-…) references found in lane/lcard rules — extraction or CSS moved")
	}
	for ref := range laneRefs {
		if !day[ref] {
			t.Errorf("lane styles consume %s but the day :root block never defines it", ref)
		}
	}

	// The explicit night toggle and the OS dark preference must carry the SAME override set.
	for p := range night {
		if !osNight[p] {
			t.Errorf("%s defined for [data-theme=night] but missing from the prefers-color-scheme dark block", p)
		}
	}
	for p := range osNight {
		if !night[p] {
			t.Errorf("%s defined for the prefers-color-scheme dark block but missing from [data-theme=night]", p)
		}
	}

	// No night-only tokens: every night override re-defines a token day also carries.
	for p := range night {
		if !day[p] {
			t.Errorf("%s exists only in the night theme — a day render would fail to resolve it", p)
		}
	}
}

// TestLaneMarkupCarriesNoInlineStyling: lane markup must style itself exclusively through the
// token-driven class layer, never through inline style attributes — an inline color would pin one
// theme's palette and survive the theme toggle. (The stat band's activity bars legitimately carry
// a height style; they are not part of the lane surface.)
func TestLaneMarkupCarriesNoInlineStyling(t *testing.T) {
	h := newTestHandler(t)
	queued := laneCardFromItem(store.TodoItem{Todo: store.Todo{
		ID: "td_s1", Source: "github", Kind: "push", State: "pending", CreatedAt: time.Now(),
	}, TrustMode: "signed"})
	body := renderFrag(t, h, "board_lanes", lanesView{
		Received: []laneCard{inflightCard("github", "push", "signed", "k1", time.Now())},
		Verified: []laneCard{queued},
		Counts:   laneCounts{Verified: 1},
	})
	if strings.Contains(body, "style=") {
		t.Errorf("lane markup carries an inline style attribute: %q", body)
	}
}

// TestSbLiveDrivesTheEphemeralAndCountHooks guards the OTHER side of the template↔JS contract
// board_dom_test.go pins from the template side: sb-live.js must actually query the data-sb-*
// hooks that make the rejected-caller card transient ("appears and expires" — SPEC-0015 scenario
// "Rejected caller") and the received count DOM-derived. No browser runs in CI; if these attribute
// names disappear from the module, expiry silently stops and this fails instead.
func TestSbLiveDrivesTheEphemeralAndCountHooks(t *testing.T) {
	js, err := os.ReadFile("../../static/js/sb-live.js")
	if err != nil {
		t.Fatalf("read sb-live.js: %v", err)
	}
	src := string(js)
	for _, hook := range []string{
		"data-sb-ephemeral",  // TTL stamp the module expires (rejected/deduped/lost in-flight cards)
		"data-sb-lane-cards", // the lane lists it watches
		"data-sb-lane-empty", // empty-state labels it toggles
		"data-sb-lane-count", // the DOM-derived received count
		"data-sb-lane-cap",   // the visible per-lane cap
	} {
		if !strings.Contains(src, hook) {
			t.Errorf("sb-live.js no longer references %q — the board's client contract broke", hook)
		}
	}
	// Expiry must actually remove the card, not merely hide it (a hidden node would keep the
	// DOM-derived received count wrong).
	if !strings.Contains(src, ".remove()") {
		t.Error("sb-live.js must remove expired ephemeral cards from the DOM")
	}
}
