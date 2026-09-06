// Family signature verifiers shared by the self-managed webhook path: the same HMAC schemes the
// retired operator-configured receivers used, kept because self-managed signed webhooks verify
// per-provider EXACTLY as those receivers did (SPEC-0006 "Signed Webhook Verification"). Salvaged
// from the removal of the shared receivers.
//
// @joestump-agent 09/06/2026 - Salvaged during the shared-receiver teardown (#176 follow-ups).

package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// bodyHash derives a stable dedup key from a body that carries no delivery id.
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verifyStripe verifies a Stripe v1 signature header ("t=...,v1=...") with a replay tolerance.
func verifyStripe(secret string, body []byte, header string, now time.Time, tolerance time.Duration) bool {
	var ts string
	sigs := []string{}
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	if ts == "" || len(sigs) == 0 {
		return false
	}
	tsUnix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if now.Sub(time.Unix(tsUnix, 0)) > tolerance || time.Unix(tsUnix, 0).Sub(now) > tolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, got := range sigs {
		if hmac.Equal([]byte(want), []byte(got)) {
			return true
		}
	}
	return false
}

// verifySlack verifies a Slack v0 signature header ("t=...,v0=...") with a replay tolerance.
func verifySlack(secret string, body []byte, timestamp, sig string, now time.Time, tolerance time.Duration) bool {
	tsUnix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if now.Sub(time.Unix(tsUnix, 0)) > tolerance || time.Unix(tsUnix, 0).Sub(now) > tolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return sig != "" && hmac.Equal([]byte(want), []byte(sig))
}
