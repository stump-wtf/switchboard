package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// attachHook rings only when the stream actually opens: a 200 head (explicit, or implicit via
// the first unheaded Write) fires onHead exactly once, while an explicit non-200 head — the SDK's
// 409 on a second GET of an open stream, or a 400 on the GET path — suppresses it even when a
// body follows. Ringing for a stream that never opened would charge the shared, permanently
// consumable ring budget for rows the pump drops, silencing those todos' pushes until pulled
// (SPEC-0011 scenario "Reconnecting session is rung for waiting work" — the converse).
func TestAttachHookFiresOnlyOn200Head(t *testing.T) {
	cases := []struct {
		name    string
		do      func(a *attachHook)
		wantHit bool
	}{
		{"explicit 200 head fires once", func(a *attachHook) {
			a.WriteHeader(http.StatusOK)
			_, _ = a.Write([]byte("x"))
			_, _ = a.Write([]byte("y"))
		}, true},
		{"implicit 200 via first Write", func(a *attachHook) {
			_, _ = a.Write([]byte("x"))
			_, _ = a.Write([]byte("y"))
		}, true},
		{"explicit 409 head suppresses, body write included", func(a *attachHook) {
			a.WriteHeader(http.StatusConflict)
			_, _ = a.Write([]byte("stream already open"))
		}, false},
		{"explicit 400 head suppresses", func(a *attachHook) {
			a.WriteHeader(http.StatusBadRequest)
			_, _ = a.Write([]byte("bad request"))
		}, false},
		{"409 head with no body", func(a *attachHook) {
			a.WriteHeader(http.StatusConflict)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired := 0
			hook := &attachHook{
				ResponseWriter: httptest.NewRecorder(),
				onHead:         func() { fired++ },
			}
			tc.do(hook)
			if tc.wantHit && fired != 1 {
				t.Fatalf("onHead fired %d times, want exactly 1", fired)
			}
			if !tc.wantHit && fired != 0 {
				t.Fatalf("onHead fired %d times after a non-200 head, want 0", fired)
			}
		})
	}
}
