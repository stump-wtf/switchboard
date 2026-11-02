package oauthsrv

// RFC 8628 device authorization grant for the operator API (issue #308). A headless client — an
// SSH session, a Harness daemon host — cannot receive a loopback redirect, so it starts a device
// flow here: POST /oauth/device_authorization mints the two codes, the human types the short one
// at the consent screen on any device (the screen itself renders in internal/web behind the human
// session), and the client polls the token endpoint with the device-code grant until the decision
// lands. The grant this flow can produce is fixed at the row level — the device row has no
// endpoint column, only a human_id stamped at approval — so a device flow can only ever mint the
// same operator-bound access/refresh pair the code flow issues (ADR-0023's resource binding,
// unchanged), and never an endpoint credential. Governing: ADR-0019, ADR-0023, ADR-0057 (proposed).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// The device-grant URN (RFC 8628 §6.1) and the two paths this flow adds. DeviceAuthorizationPath
// is served by this package like the rest of the public AS surface; DeviceConsentPath is served
// by internal/web behind the human session, exactly like AuthorizePath.
const (
	DeviceGrantType         = "urn:ietf:params:oauth:grant-type:device_code"
	DeviceAuthorizationPath = "/oauth/device_authorization"
	DeviceConsentPath       = "/oauth/device"
)

// DeviceCodeTTL is how long a device request lives before the polling client reads
// expired_token. RFC 8628 recommends ten minutes; it is long enough for a human walking to a
// phone, short enough that an abandoned request is inert before the next coffee.
// Governing: ADR-0057 (proposed) — the numbers the issue left to the implementer.
const DeviceCodeTTL = 10 * time.Minute

// DevicePollInterval is the interval advertised as `interval` and enforced against every poll:
// a poll arriving sooner than the last one plus this (minus the skew margin) answers slow_down,
// which is what stops a impatient loop from turning consent polling into a hammering job.
const DevicePollInterval = 5 * time.Second

// DevicePollSkewMargin is what the slow_down comparison subtracts from the interval, so client
// and server clock drift of a couple of seconds cannot make an honest 5s poller read slow_down.
const DevicePollSkewMargin = time.Second

// userCodeLen is the length of the human-typeable code, and userCodeAlphabet is the RFC 8628
// "User Code" safe set with the visually ambiguous characters removed (no 0/O/1/I/L): eight
// characters from this alphabet are ~2.6e10 values, and every entry lookup is charged to the
// signed-in human, so guessing one is a refusal rather than a race. Governing: issue #308 AC
// "short, human-typeable, single-use, and rate-limited on entry".
const (
	userCodeLen     = 8
	userCodeDisplay = 4 // rendered as XXXX-XXXX
)

var userCodeAlphabet = []rune("BCDFGHJKLMNPQRSTVWXZ")

// DeviceStore is the slice of the store the device surface needs. *store.Store satisfies it;
// tests substitute a fake.
type DeviceStore interface {
	CreateOAuthDeviceCode(ctx context.Context, deviceHash, userHash, clientID string, expiresAt time.Time, intervalSeconds int) error
	OAuthDeviceCodeByDeviceHash(ctx context.Context, deviceHash string) (store.OAuthDeviceCode, error)
	SpendOAuthDeviceCode(ctx context.Context, id string) error
}

// DeviceCodes mints the pair a device request is identified by: the device_code (32 random bytes,
// base64url — the same entropy class as every other OAuth credential here, persisted only as a
// SHA-256 hash) and the user_code (8 alphabet characters, displayed XXXX-XXXX, persisted only as
// a hash). The plaintexts exist only in the response body that carries them to the client and the
// human.
func DeviceCodes() (deviceCode, userCode, deviceHash, userHash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", "", fmt.Errorf("oauthsrv: read random: %w", err)
	}
	deviceCode = base64.RawURLEncoding.EncodeToString(b)
	userRunes := make([]rune, userCodeLen)
	for i := range userRunes {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		if err != nil {
			return "", "", "", "", fmt.Errorf("oauthsrv: read random: %w", err)
		}
		userRunes[i] = userCodeAlphabet[n.Int64()]
	}
	userCode = string(userRunes)
	return deviceCode, userCode, cred.Hash(deviceCode), cred.Hash(userCode), nil
}

// NormalizeUserCode canonicalizes what a human typed: upper-case, and any separator characters
// they included (spaces, dashes) dropped. The hash lookup compares against the stored PLAINTEXT
// hash, so this is the only place formatting may differ.
func NormalizeUserCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// UserCodeDisplay renders a user code the way the consent screen and verification_uri_complete
// show it: XXXX-XXXX.
func UserCodeDisplay(code string) string {
	if len(code) <= userCodeDisplay {
		return code
	}
	return code[:userCodeDisplay] + "-" + code[userCodeDisplay:]
}

// DeviceAuthorization serves POST /oauth/device_authorization (RFC 8628 §3.1). The requesting
// client must be registered (DCR is the only way a client exists here); the request is bound to
// the operator API resource — a resource indicator naming anything else is refused rather than
// ignored, so the "operator API only" scope of this flow is enforced at the front door, and the
// row itself cannot express any other binding. Answers the standard document: device_code,
// user_code, verification_uri, verification_uri_complete, expires_in and interval. Every response
// is Cache-Control: no-store — these carry credentials. Governing: ADR-0023, ADR-0057 (proposed).
func (h *Handler) DeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "request body must be application/x-www-form-urlencoded")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if clientID == "" || len(clientID) > maxTokenParamLen {
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" && res != strings.TrimRight(h.base, "/")+OperatorResourcePath {
		writeTokenError(w, http.StatusBadRequest, "invalid_target",
			"the device flow grants only onto the operator API resource of this switchboard")
		return
	}
	// The client is an identifier, not a secret — but an unregistered one would mint a request no
	// consent screen can name, so the device endpoint enforces registration like the authorize
	// screen does.
	client, err := h.store.OAuthClientByClientID(r.Context(), clientID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeTokenError(w, http.StatusBadRequest, "invalid_client", "the client_id was never registered with this switchboard")
			return
		}
		h.log.Error("oauth device_authorization: client lookup", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the request could not be completed")
		return
	}

	devStore, ok := h.store.(DeviceStore)
	if !ok {
		h.log.Error("oauth device_authorization: store does not support device codes")
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the request could not be completed")
		return
	}

	deviceCode, userCode, deviceHash, userHash, err := DeviceCodes()
	if err != nil {
		h.log.Error("oauth device_authorization: mint codes", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the request could not be completed")
		return
	}
	// A hash collision on either unique index is re-minted, never adopted: one attempt of retry
	// is plenty at this entropy, and a second collision is a broken database.
	if err := devStore.CreateOAuthDeviceCode(r.Context(), deviceHash, userHash, client.ClientID,
		time.Now().Add(DeviceCodeTTL), int(DevicePollInterval.Seconds())); errors.Is(err, store.ErrConflict) {
		deviceCode, userCode, deviceHash, userHash, err = DeviceCodes()
		if err != nil {
			h.log.Error("oauth device_authorization: remint codes", "err", err)
			writeTokenError(w, http.StatusInternalServerError, "server_error", "the request could not be completed")
			return
		}
		err = devStore.CreateOAuthDeviceCode(r.Context(), deviceHash, userHash, client.ClientID,
			time.Now().Add(DeviceCodeTTL), int(DevicePollInterval.Seconds()))
	}
	if err != nil {
		h.log.Error("oauth device_authorization: persist request", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the request could not be completed")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          h.base + DeviceConsentPath,
		"verification_uri_complete": h.base + DeviceConsentPath + "?user_code=" + UserCodeDisplay(userCode),
		"expires_in":                int64(DeviceCodeTTL.Seconds()),
		"interval":                  int64(DevicePollInterval.Seconds()),
	})
}

// deviceGrant serves the device_code grant at the token endpoint (RFC 8628 §3.4/§3.5). Order of
// checks is the RFC's: polling faster than the interval is slow_down before anything is read;
// then the clocks answer expired_token / access_denied / authorization_pending; then the client
// binding; then the grant itself. A spent row is invalid_grant — the pair this request bought is
// already out there, and the flow, like the code flow, is single-use. Approval spent the row at
// consent time, so the winning poll is the one that sees HumanID set and UsedAt already stamped;
// there is no second spend to race. The pair is minted exactly like the code flow's, bound to the
// approving human and nothing else. Governing: ADR-0023, ADR-0057 (proposed), issue #308 AC.
func (h *Handler) deviceGrant(w http.ResponseWriter, r *http.Request) {
	devStore, ok := h.store.(DeviceStore)
	if !ok {
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	deviceCode := r.PostForm.Get("device_code")
	clientID := r.PostForm.Get("client_id")
	switch {
	case deviceCode == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "device_code is required")
		return
	case clientID == "":
		writeTokenError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	d, err := devStore.OAuthDeviceCodeByDeviceHash(r.Context(), cred.Hash(deviceCode))
	if errors.Is(err, store.ErrNotFound) {
		writeTokenError(w, http.StatusBadRequest, "expired_token", "the device code is unknown or expired")
		return
	}
	if err != nil {
		h.log.Error("oauth token: load device request", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}
	// The polling interval is what a client WAITING on a human must respect: while nobody has decided
	// anything, every poll is a request that can only answer "not yet", and slow_down is what keeps
	// that from becoming a hammering job. Once the row carries a decision or has expired there is
	// nothing left to wait for, so the answer goes out immediately; a client that hammers after
	// approval still gets exactly one grant, because the spend below is a CAS. Governing: RFC 8628
	// §3.5, ADR-0057 (proposed).
	if d.DeniedAt == nil && d.AuthorizedAt == nil && time.Now().Before(d.ExpiresAt) {
		if interval := time.Duration(d.IntervalSeconds) * time.Second; d.LastPolledAt != nil &&
			time.Now().Before(d.LastPolledAt.Add(interval-DevicePollSkewMargin)) {
			writeTokenError(w, http.StatusBadRequest, "slow_down", "poll no faster than every "+fmt.Sprintf("%ds", d.IntervalSeconds))
			return
		}
	}
	switch {
	case time.Now().After(d.ExpiresAt):
		writeTokenError(w, http.StatusBadRequest, "expired_token", "the device code expired before anyone approved it")
		return
	case d.DeniedAt != nil:
		writeTokenError(w, http.StatusBadRequest, "access_denied", "the operator denied the request")
		return
	case d.AuthorizedAt == nil:
		writeTokenError(w, http.StatusBadRequest, "authorization_pending", "the user has not yet completed the authorization")
		return
	}
	if d.ClientID != clientID {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "the device code was not issued to this client")
		return
	}
	// The spend is the CAS: exactly one concurrent poll flips used_at and mints; the losers read
	// the conflict and answer like any other spent grant.
	if err := devStore.SpendOAuthDeviceCode(r.Context(), d.ID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeTokenError(w, http.StatusBadRequest, "invalid_grant", "the device grant has already been redeemed")
			return
		}
		h.log.Error("oauth token: spend device grant", "err", err)
		writeTokenError(w, http.StatusInternalServerError, "server_error", "the exchange could not be completed")
		return
	}

	h.issue(w, r, func(accessHash, refreshHash string) (store.OAuthToken, error) {
		return h.tokens.CreateOAuthToken(r.Context(), accessHash, refreshHash, d.ClientID, "", d.HumanID,
			time.Now().Add(AccessTokenTTL))
	})
}
