package provider

// Tests for the address, repository and allowlist rules that guard what a reconcile read would dial
// (#614). Nothing here dials anything: the contract is that a stored value is canonical, or refused.
//
// Governing: stump.wtf/switchboard#614; ADR-0002.

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"https://api.github.com", "https://api.github.com", false},
		{"https://api.github.com/", "https://api.github.com", false},
		{"  https://API.GitHub.com  ", "https://api.github.com", false},
		{"https://gitea.stump.rocks/api/v1", "https://gitea.stump.rocks/api/v1", false},
		{"https://gitea.stump.rocks/api/v1/", "https://gitea.stump.rocks/api/v1", false},
		{"https://git.internal:8443/api/v1", "https://git.internal:8443/api/v1", false},
		// Loopback http is the one cleartext exception: no path, and the httptest servers the read
		// clients are tested against depend on it.
		{"http://127.0.0.1:4321/api/v1", "http://127.0.0.1:4321/api/v1", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{"http://api.github.com", "", true},
		{"https://token:x@api.github.com", "", true},
		{"https://api.github.com?access_token=abc", "", true},
		{"https://api.github.com#frag", "", true},
		{"ftp://api.github.com", "", true},
		{"api.github.com", "", true},
		{"https://", "", true},
		{"", "", true},
		{strings.Repeat("https://a.example/", 500), "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeBaseURL(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("NormalizeBaseURL(%q) = %q, want an error", tc.in, got)
				continue
			}
			if !errors.Is(err, ErrInvalidBaseURL) {
				t.Errorf("NormalizeBaseURL(%q) err = %v, want ErrInvalidBaseURL", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOriginOf(t *testing.T) {
	got, err := OriginOf("https://gitea.stump.rocks/api/v1")
	if err != nil {
		t.Fatalf("OriginOf: %v", err)
	}
	if got != "https://gitea.stump.rocks" {
		t.Fatalf("OriginOf = %q, want the origin with the API root gone", got)
	}
	if _, err := OriginOf("http://api.github.com"); !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("OriginOf http err = %v, want ErrInvalidBaseURL", err)
	}
}

func TestParseAllowedOrigins(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{"", nil, false},
		{"https://api.github.com", []string{"https://api.github.com"}, false},
		{" https://a.example , https://b.example:8443 ,, ", []string{"https://a.example", "https://b.example:8443"}, false},
		{"https://a.example/", []string{"https://a.example"}, false},
		{"https://a.example/api/v1", nil, true},
		{"https://user@a.example", nil, true},
		{"http://a.example", nil, true},
		{"not-an-origin", nil, true},
	}
	for _, tc := range cases {
		got, err := ParseAllowedOrigins(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseAllowedOrigins(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAllowedOrigins(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("ParseAllowedOrigins(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ParseAllowedOrigins(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestCheckAllowedBaseURLMatchesOnOrigin is the reason the comparison is not a prefix match: an
// allowlisted host must not admit a lookalike that extends it.
func TestCheckAllowedBaseURLMatchesOnOrigin(t *testing.T) {
	allowed, err := ParseAllowedOrigins("https://gitea.stump.rocks")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := CheckAllowedBaseURL("https://gitea.stump.rocks/api/v1", allowed); err != nil {
		t.Fatalf("an allowed origin's API root was refused: %v", err)
	}
	for _, bad := range []string{
		"https://gitea.stump.rocks.evil.test/api/v1",
		"https://evil.test",
		"http://gitea.stump.rocks",
	} {
		if err := CheckAllowedBaseURL(bad, allowed); !errors.Is(err, ErrOriginNotAllowed) && !errors.Is(err, ErrInvalidBaseURL) {
			t.Errorf("CheckAllowedBaseURL(%q) err = %v, want a refusal", bad, err)
		}
	}
	// An unset allowlist restricts nothing.
	if err := CheckAllowedBaseURL("https://anything.example", nil); err != nil {
		t.Fatalf("empty allowlist refused %v", err)
	}
}

func TestParseRepoSpec(t *testing.T) {
	r, err := ParseRepoSpec("stump.wtf/switchboard")
	if err != nil {
		t.Fatalf("ParseRepoSpec: %v", err)
	}
	if r.Owner != "stump.wtf" || r.Name != "switchboard" || r.String() != "stump.wtf/switchboard" {
		t.Fatalf("parsed %+v", r)
	}
	for _, bad := range []string{"", "justname", "a/b/c", "a//b", "../etc", "/b"} {
		if _, err := ParseRepoSpec(bad); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("ParseRepoSpec(%q) err = %v, want ErrInvalidRepo", bad, err)
		}
	}
}
