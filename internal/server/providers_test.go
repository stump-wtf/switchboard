package server

// Unit tests for the env→registry boot seed and the registry-backed provider enumeration
// (ADR-0020, SPEC-0017 REQ "Environment Config Import" / "Runtime Provider Registry"). Both are
// pure wiring over the store seam, so no database is needed here — the create-if-absent semantics
// themselves are proven DB-backed in internal/store.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/joestump/switchboard/internal/ingest"
	"github.com/joestump/switchboard/internal/store"
)

// fakeSeeder records every seed offered to it, simulating an already-populated registry for the
// names in existing (SeedProvider then reports created=false, exactly like ON CONFLICT DO NOTHING).
type fakeSeeder struct {
	seeds    []store.ProviderSeed
	existing map[string]bool
	failErr  error
}

func (f *fakeSeeder) SeedProvider(_ context.Context, seed store.ProviderSeed) (bool, error) {
	if f.failErr != nil {
		return false, f.failErr
	}
	f.seeds = append(f.seeds, seed)
	return !f.existing[seed.Name], nil
}

// seedEnvProviders offers every env-configured provider as a create-if-absent seed with the
// effective (normalized) queues, correct kinds/trust modes, and the plaintext secret for the store
// to seal — and skips signed providers with no env secret (nothing to import for an unreachable
// route). Governing: SPEC-0017 REQ "Environment Config Import".
func TestSeedEnvProvidersComposition(t *testing.T) {
	f := &fakeSeeder{}
	cfg := ingest.Config{
		GitHubSecret: "gh-secret", // GitHubQueue empty → Normalized defaults it to "reviews"
		Generic: map[string]ingest.GenericProvider{
			"dockerhub": {Mode: "token", Token: "tok", Queue: "builds"},
			"lan":       {Mode: "open"},
		},
	}.Normalized()
	if err := seedEnvProviders(context.Background(), f, cfg, discardLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	want := map[string]store.ProviderSeed{
		"github":    {Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed", Secret: "gh-secret"},
		"dockerhub": {Name: "dockerhub", Family: "webhook", Kind: "generic", TrustMode: "token", Secret: "tok"},
		"lan":       {Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open", Secret: ""},
	}
	wantQueue := map[string]string{"github": "reviews", "dockerhub": "builds", "lan": "lan"}
	if len(f.seeds) != len(want) {
		t.Fatalf("seeds = %+v, want %d entries (stripe/slack unconfigured must be skipped)", f.seeds, len(want))
	}
	for _, got := range f.seeds {
		w, ok := want[got.Name]
		if !ok {
			t.Fatalf("unexpected seed %+v", got)
		}
		if got.Family != w.Family || got.Kind != w.Kind || got.TrustMode != w.TrustMode || got.Secret != w.Secret {
			t.Fatalf("seed %s = %+v, want %+v", got.Name, got, w)
		}
		var c struct {
			Queue string `json:"queue"`
		}
		if err := json.Unmarshal(got.Config, &c); err != nil || c.Queue != wantQueue[got.Name] {
			t.Fatalf("seed %s config = %s (err %v), want queue %q", got.Name, got.Config, err, wantQueue[got.Name])
		}
	}

	// Idempotent re-run against a registry that now holds every row: still no error, and the seeds
	// remain offers only — create-if-absent is the store's contract (proven in internal/store).
	f2 := &fakeSeeder{existing: map[string]bool{"github": true, "dockerhub": true, "lan": true}}
	if err := seedEnvProviders(context.Background(), f2, cfg, discardLogger()); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// A seed failure fails boot loudly — never a half-imported registry.
	f3 := &fakeSeeder{failErr: errors.New("pg down")}
	if err := seedEnvProviders(context.Background(), f3, cfg, discardLogger()); err == nil {
		t.Fatal("seed failure must surface")
	}
}

// providerStatuses projects registry rows into the UNCHANGED SPEC-0005 list_providers shape:
// unreachable routes (signed/token with no held secret) are skipped, open and queue rows carry
// none-by-design, the enabled flag comes from the row, and nothing secret-shaped can appear —
// the input rows hold only a presence flag. Governing: SPEC-0005 REQ "Provider Enumeration Without
// Secrets"; ADR-0020, SPEC-0017 REQ "Runtime Provider Registry".
func TestProviderStatusesFromRegistry(t *testing.T) {
	rows := []store.Adapter{
		{Name: "redis-main", Family: "queue", Kind: "redis", TrustMode: "queue", Enabled: true,
			Config: []byte(`{"transport":"redis","mode":"stream","stream":"jobs"}`)},
		{Name: "dockerhub", Family: "webhook", Kind: "generic", TrustMode: "token", Enabled: true,
			SecretConfigured: true},
		{Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed", Enabled: false,
			SecretConfigured: true},
		{Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open", Enabled: true},
		{Name: "pending", Family: "webhook", Kind: "generic", TrustMode: "token", Enabled: true}, // no token yet
		{Name: "stripe", Family: "webhook", Kind: "stripe", TrustMode: "signed", Enabled: true},  // no secret
		{Name: "mystery", Family: "webhook", Kind: "generic", TrustMode: "weird", Enabled: true}, // unknown trust
		{Name: "foreign", Family: "carrier-pigeon", Kind: "x", TrustMode: "open", Enabled: true}, // unknown family
	}
	got := providerStatuses(rows)

	if len(got) != 4 {
		t.Fatalf("statuses = %+v, want 4 (unreachable/unknown rows skipped)", got)
	}
	byName := map[string]int{}
	for i, ps := range got {
		byName[ps.Name] = i
	}
	q := got[byName["redis-main"]]
	if q.Family != "queue" || q.TrustMode != "queue" || !q.Enabled ||
		q.SecretStatus != "none-by-design" || q.Channel != "jobs" || q.Path != "" {
		t.Fatalf("queue row = %+v", q)
	}
	d := got[byName["dockerhub"]]
	if d.SecretStatus != "configured" || d.Path != "/webhooks/generic/dockerhub" || !d.Enabled {
		t.Fatalf("token row = %+v", d)
	}
	g := got[byName["github"]]
	if g.TrustMode != "signed" || g.SecretStatus != "configured" || g.Path != "/webhooks/github" || g.Enabled {
		t.Fatalf("signed row must carry the registry's enabled flag: %+v", g)
	}
	l := got[byName["lan"]]
	if l.SecretStatus != "none-by-design" || l.Path != "/webhooks/generic/lan" {
		t.Fatalf("open row = %+v", l)
	}
}
