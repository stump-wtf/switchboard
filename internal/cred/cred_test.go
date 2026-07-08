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
	if !strings.HasPrefix(disp, "sbk_") || !strings.HasSuffix(disp, "…") {
		t.Fatalf("display: %q", disp)
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
