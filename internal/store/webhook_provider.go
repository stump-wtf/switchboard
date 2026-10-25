package store

// Per-Webhook Provider Connection Config
//
// A reconcile pass (#616) reads a provider back to find deliveries that never arrived. To do that it
// needs three things per webhook, and this file is the only place that writes or opens them: the
// provider API address, the repository on it, and a read-only credential. They live on the webhook row
// (migration 0035) because the webhook is the unit that knows which provider installation it came
// from, and because ownership of that decision belongs to the webhook's human, not to the agent that
// owns the ingest endpoint (ADR-0052 proposes exactly that split, and nothing here lets an agent write
// it).
//
// The credential is stored the way a held signing secret is — sealed through the internal/cred envelope
// when SWITCHBOARD_SECRET_ENCRYPTION_KEY is configured — and it follows the same rules: a write of a
// non-empty value with no at-rest key is refused instead of storing a live token in the clear, no read
// that builds a response selects the column, and the plaintext surfaces in exactly one accessor, meant
// for the reconcile caller that has to put it in an Authorization header.
//
// The address is validated where it is stored, not where it is dialed: NormalizeBaseURL rejects an
// address that could hide credentials or a query, and when the operator configured
// SWITCHBOARD_PROVIDER_READ_ALLOWED_ORIGINS an address outside that list is refused here, so nothing
// switchboard dials was ever chosen by a caller who could not be checked.
//
// Nothing in the delivery path calls any of this. A webhook whose connection is configured is not
// reconciled by anything until #616 lands; until then these columns describe a capability the server
// has, not one it uses.
//
// Governing: stump.wtf/switchboard#614 (this config), #615 (the credential a human provisions),
// #616 (the pass that reads it); ADR-0002 (PostgreSQL is the trust boundary), ADR-0037 (the envelope),
// ADR-0022 (endpoint-scoped ownership), SPEC-0032 REQ-3 (no caller-supplied secret in the clear).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stump-wtf/switchboard/internal/provider"
)

// ErrProviderTokenEncryptionRequired is a provider read token refused because no at-rest key is
// configured. Storing a live read credential in the clear is not a supported configuration the way an
// unencrypted minted signing secret is: this one is pasted in by a human from a token someone handed
// them, and the key that protects signing secrets protects it just as well.
var ErrProviderTokenEncryptionRequired = errors.New("store: a provider read token requires SWITCHBOARD_SECRET_ENCRYPTION_KEY")

// ErrProviderOriginRefused is a provider address outside the operator's
// SWITCHBOARD_PROVIDER_READ_ALLOWED_ORIGINS. It is its own sentinel so the surface that asked can
// answer "not allowed by this instance" rather than a generic invalid argument.
var ErrProviderOriginRefused = errors.New("store: provider base url is outside this instance's allowed origins")

// ProviderConnectionEdit is one write of a webhook's provider connection: the address, the repository,
// and optionally the credential.
type ProviderConnectionEdit struct {
	// BaseURL is the provider API root (a Gitea install's includes /api/v1). It is canonicalized on
	// write; "" clears the address, and the repository with it, because they are one decision.
	BaseURL string
	// Repo is "owner/name" on that provider. Required with a base URL, ignored without one.
	Repo string
	// Token is the read credential: nil leaves whatever is stored alone, "" clears it, and a non-empty
	// value replaces it. Sealed here — callers hand plaintext and never see it again.
	Token *string
	// AllowedOrigins is the operator's origin allowlist (config.Config.ProviderReadOrigins); empty
	// admits any valid address.
	AllowedOrigins []string
}

// SetWebhookProviderConnection writes the connection config for a webhook the given endpoint owns. The
// endpoint_id guard is the ownership check: another endpoint's or an unknown id affects no rows and
// returns ErrNotFound. The address pair is replaced whole on every write, so a stored value can never
// be half-updated; the credential is touched only when Token is non-nil.
func (s *Store) SetWebhookProviderConnection(ctx context.Context, webhookID, endpointID string, edit ProviderConnectionEdit) (Webhook, error) {
	if !isUUID(webhookID) || !isUUID(endpointID) {
		return Webhook{}, ErrNotFound
	}
	baseURL, repo, token, err := s.canonicalizeConnection(edit)
	if err != nil {
		return Webhook{}, err
	}
	return s.applyProviderConnection(ctx, `WHERE w.id = $1 AND w.endpoint_id = $2`,
		[]any{webhookID, endpointID, baseURL, repo, token})
}

// SetWebhookProviderConnectionForHuman is SetWebhookProviderConnection authorized by a human's reach
// rather than by an endpoint credential (SPEC-0035 REQ "Reach on Every Route"): an unknown, malformed,
// or another human's webhook id is all ErrNotFound, so the human API cannot be used to probe what
// exists.
func (s *Store) SetWebhookProviderConnectionForHuman(ctx context.Context, webhookID, ownerHumanID string, edit ProviderConnectionEdit) (Webhook, error) {
	if !isUUID(webhookID) || !isUUID(ownerHumanID) {
		return Webhook{}, ErrNotFound
	}
	baseURL, repo, token, err := s.canonicalizeConnection(edit)
	if err != nil {
		return Webhook{}, err
	}
	return s.applyProviderConnection(ctx, `WHERE w.id = $1 AND EXISTS (
			SELECT 1 FROM endpoints e JOIN agents a ON a.id = e.agent_id
			WHERE e.id = w.endpoint_id AND a.owner_human_id = $2)`,
		[]any{webhookID, ownerHumanID, baseURL, repo, token})
}

// canonicalizeConnection turns an edit into the values that go into the row: a canonical address and
// repository (or two empty strings, which store NULL), and the sealed credential. Address and repo are
// checked as a pair because the row's CHECK treats them as one decision, and a Go-side error beats a
// constraint violation in the response.
func (s *Store) canonicalizeConnection(edit ProviderConnectionEdit) (baseURL, repo string, token *string, err error) {
	raw := strings.TrimSpace(edit.BaseURL)
	if raw == "" {
		if strings.TrimSpace(edit.Repo) != "" {
			return "", "", nil, fmt.Errorf("%w: a repository needs a provider base url", ErrInvalidArgument)
		}
	} else {
		baseURL, err = provider.NormalizeBaseURL(raw)
		if err != nil {
			return "", "", nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
		if err := provider.CheckAllowedBaseURL(baseURL, edit.AllowedOrigins); err != nil {
			return "", "", nil, fmt.Errorf("%w: %v", ErrProviderOriginRefused, err)
		}
		r, perr := provider.ParseRepoSpec(edit.Repo)
		if perr != nil {
			return "", "", nil, fmt.Errorf("%w: %v", ErrInvalidArgument, perr)
		}
		repo = r.String()
	}
	if edit.Token == nil {
		return baseURL, repo, nil, nil
	}
	plain := strings.TrimSpace(*edit.Token)
	if plain == "" {
		clear := ""
		return baseURL, repo, &clear, nil
	}
	if s.secretCipher == nil {
		return "", "", nil, ErrProviderTokenEncryptionRequired
	}
	sealed, serr := s.sealSecret(plain)
	if serr != nil {
		return "", "", nil, serr
	}
	return baseURL, repo, &sealed, nil
}

// applyProviderConnection is the one UPDATE both surfaces share. The address pair is written whole
// (empty stores NULL); the credential follows the three-way rule the edit documents, which is why the
// CASE is here and not in the callers. provider_connection_set_at moves on every write, including the
// ones that only replace a credential.
func (s *Store) applyProviderConnection(ctx context.Context, where string, args []any) (Webhook, error) {
	var w Webhook
	err := s.pool.QueryRow(ctx, `
		UPDATE endpoint_webhooks w
		SET provider_base_url = NULLIF($3, ''),
		    provider_repo     = NULLIF($4, ''),
		    provider_read_token = CASE WHEN $5::text IS NULL THEN w.provider_read_token
		                              ELSE NULLIF($5, '') END,
		    provider_connection_set_at = now()
		`+where+`
		RETURNING `+webhookRowColumns,
		args...).Scan(w.scanTargets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: set webhook provider connection: %w", err)
	}
	return w, nil
}

// ProviderConnection is what a reconcile read needs to reach one provider repository: the address, the
// repository, and the plaintext read credential ("" when none is stored, which is the
// awaiting_credential state). It is returned by exactly one accessor and by no listing, so the token
// has one path from the database to the caller that will put it in a header.
type ProviderConnection struct {
	BaseURL string
	Repo    string
	Token   string
	State   string
}

// ProviderReadConnection opens one webhook's connection config for a caller that is about to read that
// provider. It takes a webhook id, not a credential or a human: whoever holds the pool is already
// inside the trust boundary (ADR-0002), and the caller that needs this (#616) is choosing rows it has
// already enumerated. An unknown or malformed id is ErrNotFound; a webhook with no address configured
// answers State "unset" with empty fields rather than an error, because "there is nothing to reconcile
// for this webhook" is a normal answer.
//
// Governing: stump.wtf/switchboard#614, #616; ADR-0052 (proposed).
func (s *Store) ProviderReadConnection(ctx context.Context, webhookID string) (ProviderConnection, error) {
	if !isUUID(webhookID) {
		return ProviderConnection{}, ErrNotFound
	}
	var w Webhook
	var stored *string
	err := s.pool.QueryRow(ctx, `
		SELECT `+webhookRowColumns+`, w.provider_read_token
		FROM endpoint_webhooks w WHERE w.id = $1`, webhookID,
	).Scan(append(w.scanTargets(), &stored)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderConnection{}, ErrNotFound
	}
	if err != nil {
		return ProviderConnection{}, fmt.Errorf("store: provider read connection: %w", err)
	}
	out := ProviderConnection{BaseURL: w.ProviderBaseURL, Repo: w.ProviderRepo}
	if stored != nil {
		token, derr := s.openSecret(*stored)
		if derr != nil {
			return ProviderConnection{}, derr
		}
		out.Token = token
	}
	out.State = w.ProviderConnectionState()
	return out, nil
}
