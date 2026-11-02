package web

// The device-authorization consent surface (RFC 8628, issue #308, ADR-0057): GET /oauth/device
// asks a signed-in human for the short user code a headless CLI printed, and shows the same
// consent content the code-flow screen shows for an operator grant — the client name, the
// operator-API resource, and the principal the grant acts as. The POST records the decision:
// approval binds the request to the approving human (the row's only possible principal), denial
// marks it dead. No redirect ever leaves this page: the device flow's whole point is that the
// client cannot receive a callback, so the CLI learns the outcome by polling the token endpoint,
// never by being bounced. CSRF applies like every other session mutation, and every user-code
// lookup is charged to the signed-in human by the store's entry rate limiter, so the entry page
// cannot be used to grind guesses against the short code. Governing: ADR-0019, ADR-0023.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
)

// deviceEntryView is the render model for templates/device.html — the code-entry step, the consent
// step (which reuses the authorize view's fields for its content), or the finished state.
type deviceEntryView struct {
	Error    string // entry/lookup problem; re-renders the entry form with the callout
	Code     string // the user code as displayed (XXXX-XXXX), round-tripped through the form
	Consent  bool   // true = a live request resolved; render client name + operator scope
	Done     string // non-empty = decision recorded; render the "back to your device" state
	NotFound bool   // denial/expiry path still shows the entry form afterwards
}

// OAuthDeviceEntry renders the user-code step, or the consent step when ?user_code= already names
// a live request. Requires human — the router bounces an anonymous hit through /login first, the
// same gate the code-flow consent has.
func (h *Handler) OAuthDeviceEntry(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	h.renderDevice(w, r, &human, r.URL.Query().Get("user_code"), "")
}

// OAuthDeviceDecision executes the human's decision. The POST re-resolves the user code from
// scratch — the rendered page is convenience, never trust — and only then records approve or
// deny. Requires human; CSRF enforced by the route group.
func (h *Handler) OAuthDeviceDecision(w http.ResponseWriter, r *http.Request) {
	human, _ := auth.FromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.PostFormValue("user_code")
	if r.PostFormValue("decision") != "approve" {
		d, err := h.resolveDeviceCode(r, code, human.ID)
		if err == nil {
			err = h.store.DenyOAuthDeviceCode(r.Context(), d.ID)
		}
		if err != nil {
			h.renderDevice(w, r, &human, code, deviceEntryError(err))
			return
		}
		h.renderDeviceDone(w, r, &human, code, "Denied — the device will be told the request was refused.")
		return
	}
	d, err := h.resolveDeviceCode(r, code, human.ID)
	if err != nil {
		h.renderDevice(w, r, &human, code, deviceEntryError(err))
		return
	}
	if err := h.store.ApproveOAuthDeviceCode(r.Context(), d.ID, human.ID); err != nil {
		h.renderDevice(w, r, &human, code, deviceEntryError(err))
		return
	}
	h.renderDeviceDone(w, r, &human, code, "Approved — return to your device, the command picks the grant up on its next poll.")
}

// resolveDeviceCode normalizes a typed code and looks it up, which charges the lookup to the
// acting human's entry window in the store (the entry surface is session-gated, so there is always
// a principal to charge). A wrong, unknown, decided or expired code is uniformly ErrNotFound.
func (h *Handler) resolveDeviceCode(r *http.Request, raw, gateKey string) (store.OAuthDeviceCode, error) {
	normalized := oauthsrv.NormalizeUserCode(raw)
	if len(normalized) != 8 {
		return store.OAuthDeviceCode{}, store.ErrNotFound
	}
	d, err := h.store.OAuthDeviceCodeByUserCode(r.Context(), cred.Hash(normalized), gateKey)
	if err != nil {
		return store.OAuthDeviceCode{}, err
	}
	return d, nil
}

// deviceEntryError maps a lookup/decision failure to the one fixed string shown on the entry
// page. Rate limiting and "no such request" share a message on purpose — telling a guesser which
// one fired would be a leak.
func deviceEntryError(err error) string {
	if errors.Is(err, store.ErrConflict) {
		return "too many attempts with this code — wait a minute, then type it again."
	}
	return "no pending device request matches that code. Check the code the command printed, and that the request has not expired."
}

func (h *Handler) renderDevice(w http.ResponseWriter, r *http.Request, human *store.Human, rawCode, errMsg string) {
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	dv := &deviceEntryView{Error: errMsg, Code: oauthsrv.UserCodeDisplay(oauthsrv.NormalizeUserCode(rawCode))}
	v := view{Title: "Authorize device", Human: human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		Device: dv}
	if errMsg == "" && dv.Code != "" {
		if d, err := h.resolveDeviceCode(r, rawCode, human.ID); err == nil {
			name := h.deviceClientName(r.Context(), d.ClientID)
			v.Authorize = &authorizeView{
				ClientName: name,
				Workspace:  h.workspaceHost(),
				Principal:  principalLabel(human),
				Initials:   initials(human),
				Operator:   true,
				Resource:   strings.TrimRight(h.cfg.BaseURL, "/") + OperatorResourcePath,
				Bullets:    authorizeScopeBullets(oauthAuthzRequest{Operator: true}),
			}
			dv.Consent = true
		} else {
			dv.Error = deviceEntryError(err)
		}
	}
	h.render(w, "device", v)
}

func (h *Handler) renderDeviceDone(w http.ResponseWriter, r *http.Request, human *store.Human, code, msg string) {
	sh, _ := h.buildShell(r.Context(), "endpoints", human)
	h.render(w, "device", view{Title: "Authorize device", Human: human, CSRF: auth.CSRFFromContext(r.Context()), Shell: sh,
		Device: &deviceEntryView{Done: msg, Code: oauthsrv.UserCodeDisplay(oauthsrv.NormalizeUserCode(code))}})
}

// deviceClientName is the registered client_name for the consent line; an unregistered client_id
// (which the device endpoint cannot mint, but stored data could rot) renders the same fallback
// the code-flow screen uses.
func (h *Handler) deviceClientName(ctx context.Context, clientID string) string {
	c, err := h.store.OAuthClientByClientID(ctx, clientID)
	if err != nil || c.Name == "" {
		return "a client"
	}
	return c.Name
}
