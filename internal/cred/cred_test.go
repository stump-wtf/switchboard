package cred

import (
	"strings"
	"testing"
)

func TestMint(t *testing.T) {
	tok, hash, disp, err := Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(tok, "sbk_") {
		t.Fatalf("token prefix: %q", tok)
	}
	if hash != Hash(tok) {
		t.Fatal("Mint hash must equal Hash(token)")
	}
	if len(hash) != 64 { // sha256 hex
		t.Fatalf("hash length: %d", len(hash))
	}
	// Mint's third value is a clean, non-secret prefix: a genuine prefix of the token (so an operator
	// can visually match the one-time reveal) that is never the full plaintext.
	if !strings.HasPrefix(disp, "sbk_") || !strings.HasPrefix(tok, disp) || disp == tok {
		t.Fatalf("mint prefix %q must be a clean, partial prefix of %q", disp, tok)
	}
	// Display (UI helper) still elides with an ellipsis.
	if d := Display(tok); !strings.HasSuffix(d, "…") {
		t.Fatalf("Display should elide with an ellipsis, got %q", d)
	}
	// Distinct each time.
	tok2, _, _, _ := Mint()
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
}

func TestHashDeterministic(t *testing.T) {
	a, b := Hash("sbk_abc"), Hash("sbk_abc")
	if a != b {
		t.Fatal("Hash must be deterministic")
	}
	if a == Hash("sbk_abd") {
		t.Fatal("different tokens must hash differently")
	}
}
