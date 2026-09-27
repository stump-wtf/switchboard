package store

// Attempt History
//
// Every committed claim opens one todo_attempts row and the transition that ends the lease closes
// it with an outcome, in the same statement as the todos update (data-modifying CTEs). The lifecycle
// statements live in todos.go and todos_a2a.go; this file holds the shared pieces: the record type,
// the claim/report inputs, the SQL fragments every statement splices in, and the scoped read.
//
// Attempts carry no scope column. Every read reaches them through the todo and filters on the
// todo's owner scope in the same statement, so an attempt can never be more visible than its todo.
// The claimer fields (kind, endpoint, session) are provenance only: nothing here or anywhere else
// authorizes on them.
//
// Governing: ADR-0039; SPEC-0034 REQ-1 "Attempt Record", REQ-2 "Opening an Attempt on Every
// Committed Claim", REQ-3 "Closing an Attempt", REQ-10 "Tenant Isolation", REQ-11 "Retention and
// Bounds", REQ-18 "Database Operation Standards".
//
// @joestump-agent 09/23/2026 - Added for #315 (epic #313).

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Text bounds on attempt inputs (SPEC-0034 REQ-5, REQ-11). The schema's CHECK constraints enforce
// the same numbers, so a caller that skips the helpers below fails loudly rather than storing more.
const (
	AttemptSummaryMax  = 2048
	AttemptArtifactMax = 512
	AttemptClaimantMax = 128
)

// Attempt history limits (SPEC-0034 REQ-11): the per-todo cap is the settings row
// attempt_history_max_per_todo, defaulting to 50 and never below 5.
const (
	attemptCapSetting = "attempt_history_max_per_todo"
	attemptCapDefault = 50
	attemptCapMin     = 5
)

// Attempt is one claim-to-close span of a todo, in the shape every read returns (SPEC-0034 REQ-7).
// It deliberately omits the claimer endpoint, the MCP session and the lease-token hash: no read
// surface may return them, and leaving them off the type means none can by accident.
type Attempt struct {
	Seq              int
	Attempt          int
	ClaimerKind      string // endpoint | owner
	Claimant         string // caller-supplied label; "" when none
	ClaimedAt        time.Time
	LastHeartbeatAt  *time.Time
	LeaseExpiresAt   time.Time
	EndedAt          *time.Time
	Outcome          string // "" while open
	Disposition      string // "" while open
	Died             bool   // derived: outcome is lease_expired or reaped (REQ-4)
	Summary          string
	SummaryTruncated bool
	Artifact         string
}

// ClaimOpts are the claim inputs beyond the lease owner. The zero value (plus a TTL) is a claim
// exactly as it was before attempt history existed.
type ClaimOpts struct {
	TTL       time.Duration
	Claimant  string // caller label; clipped to 128 bytes with control characters removed
	Session   string // MCP session id, "" when the claim did not arrive over MCP
	TokenHash []byte // SHA-256 of the lease token when the claim asked for a fence (REQ-6), else nil
}

// ClaimedAttempt describes the attempt a committed claim opened.
type ClaimedAttempt struct {
	Seq           int
	AttemptsTotal int
}

// Report is what a holder says when it ends its attempt: the todo's result (stored on the todo, as
// before) and the attempt's summary and artifact. Summary is clipped, never rejected (REQ-5).
// Artifact is stored as given; validating its shape is the caller's job, because a malformed handle
// is a caller bug that deserves an `invalid` error rather than silent truncation.
//
// TokenHash is the SHA-256 of the lease token the caller presented, or nil when it presented none.
// It is checked against the open attempt's fence (leaseFence) and never stored.
type Report struct {
	Result    []byte
	Summary   string
	Artifact  string
	TokenHash []byte
}

// ClipSummary cuts s to AttemptSummaryMax bytes at the last complete UTF-8 rune, reporting whether
// it cut (SPEC-0034 REQ-5: a verdict is never lost because its prose ran long).
func ClipSummary(s string) (string, bool) {
	return clipUTF8(s, AttemptSummaryMax)
}

// ClipClaimant removes control characters from s and cuts it to AttemptClaimantMax bytes on a rune
// boundary (SPEC-0034 REQ-5).
func ClipClaimant(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s, _ = clipUTF8(s, AttemptClaimantMax)
	return s
}

func clipUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// attemptCapSQL is the per-todo attempt cap read inline from settings, so the claim statement needs
// no second round trip. A value that is not a small non-negative integer falls back to the default,
// and anything below the minimum is raised to it.
var attemptCapSQL = fmt.Sprintf(`GREATEST(COALESCE((SELECT CASE WHEN value ~ '^[0-9]{1,6}$' THEN value::int END
		FROM settings WHERE key = '%s'), %d), %d)`, attemptCapSetting, attemptCapDefault, attemptCapMin)

// claimTail completes a claim statement whose `cand` CTE exposes cand_id, prior_state and
// prior_total (the todo's attempts_total before this claim). It closes a lapsed lease's attempt
// (a takeover), prunes the oldest closed attempts past the cap, claims the todo, and opens the new
// attempt: all one statement, so all or none commit (REQ-2, REQ-3, REQ-11, REQ-18).
//
// Fixed parameters: $3 owner, $4 lease TTL, $5 claimer kind, $6 claimer endpoint id ("" = none),
// $7 MCP session ("" = none), $8 claimant ("" = none), $9 lease-token hash (nil = unfenced).
//
// The `closed` and `pruned` arms read only `cand`, never `upd`, so nothing depends on the
// unspecified order Postgres runs data-modifying CTEs in. The one-open-attempt rule is the deferred
// exclusion constraint, checked at commit, which sees the close and the open together.
var claimTail = `,
		cap AS (SELECT ` + attemptCapSQL + ` AS n),
		closed AS (
			UPDATE todo_attempts a SET ended_at = now(), outcome = 'lease_expired', disposition = 'requeued'
			FROM cand
			WHERE a.todo_id = cand.cand_id AND a.ended_at IS NULL AND cand.prior_state = 'claimed'
			RETURNING a.todo_id
		),
		pruned AS (
			DELETE FROM todo_attempts a USING cand, cap
			WHERE a.todo_id = cand.cand_id AND a.ended_at IS NOT NULL
				AND a.seq <= cand.prior_total + 1 - cap.n
			RETURNING a.seq
		),
		upd AS (
			UPDATE todos SET state='claimed', owner=$3, lease_expires_at=now()+$4::interval,
				attempt=attempt+1, attempts_total=attempts_total+1,
				attempts_pruned=attempts_pruned+(SELECT count(*) FROM pruned),
				claimed_at=now(), next_retry_at=NULL, updated_at=now()
			FROM cand WHERE todos.id = cand.cand_id
			RETURNING todos.*, cand.prior_state
		),
		opened AS (
			INSERT INTO todo_attempts (todo_id, seq, attempt, claimer_kind, claimer_endpoint_id,
				claimer_session, owner, claimant, claimed_at, lease_expires_at, lease_token_hash)
			SELECT upd.id, upd.attempts_total, upd.attempt, $5::text, NULLIF($6::text, '')::uuid,
				NULLIF($7::text, ''), $3, NULLIF($8::text, ''), now(), upd.lease_expires_at, $9::bytea
			FROM upd
			RETURNING seq
		)
		SELECT ` + todoCols + `, prior_state = 'claimed', (SELECT seq FROM opened), attempts_total,
			EXISTS (SELECT 1 FROM closed) FROM upd`

// claimArgs returns the claimTail parameters $3..$9 for a claim by owner.
func claimArgs(owner string, o ClaimOpts, kind, claimerEndpointID string) []any {
	return []any{owner, o.TTL.String(), kind, claimerEndpointID, o.Session, ClipClaimant(o.Claimant), o.TokenHash}
}

// closedArm is the data-modifying CTE that closes the open attempt of every row the statement's
// `upd` CTE changed. outcome is a literal; disposition and the report columns are SQL expressions
// over upd or bound parameters (NULL for a close nobody reported). Only constants reach the
// string: every caller-supplied value stays a bound parameter.
func closedArm(outcome, disposition, summary, truncated, artifact string) string {
	return `closed AS (
			UPDATE todo_attempts a SET ended_at = now(), outcome = '` + outcome + `',
				disposition = ` + disposition + `, summary = ` + summary + `,
				summary_truncated = ` + truncated + `, artifact = ` + artifact + `
			FROM upd
			WHERE a.todo_id = upd.id AND a.ended_at IS NULL
			RETURNING a.todo_id
		)`
}

// closedSelect ends a statement that spliced a closedArm: the todo row, then whether this row's
// attempt actually closed. The flag, not the transition, is what the attempts-closed counter counts
// (SPEC-0034 REQ-14), so a todo that somehow had no open attempt is never counted as a close.
const closedSelect = `SELECT ` + todoCols + `, EXISTS (SELECT 1 FROM closed c WHERE c.todo_id = upd.id) FROM upd`

// closedArmUnreported closes with no summary or artifact: deaths, cancels, revocations, and the
// Board's actions, where no holder reported anything.
func closedArmUnreported(outcome, disposition string) string {
	return closedArm(outcome, disposition, "NULL", "false", "NULL")
}

// failDisposition is the disposition of a fail or reap close, read from the committed todo: no
// scheduled retry on a failed row means it dead-lettered.
const failDisposition = `CASE WHEN upd.state = 'failed' AND upd.next_retry_at IS NULL
				THEN 'dead_lettered' ELSE 'retry_scheduled' END`

// leaseFence is the lease-token predicate on the agent's heartbeat, complete, fail and release
// statements. param is the bound lease-token hash ($N, nil for no token). The statement applies only
// when the todo's open attempt carries exactly that hash: a matching token on a fenced attempt, or no
// token on an unfenced one. A token on an unfenced attempt, no token on a fenced one, or another
// attempt's token all make the UPDATE miss, and classifyMiss reports the row that is still there as
// ErrConflict: the same answer as "not the owner", so a mismatch never reveals that a fence exists.
// The hashes are fixed-length digests compared in SQL, so timing reveals nothing about the token.
// The Board's ...OperatorOwned statements omit it, so the owning human can always recover a stuck
// attempt.
//
// Governing: SPEC-0034 REQ-6 "Lease Token Fence", REQ-19 "Error Handling Standards"; design.md
// "The fence is a hash on the open attempt".
func leaseFence(param string) string {
	return `
			AND NOT EXISTS (SELECT 1 FROM todo_attempts a
				WHERE a.todo_id = todos.id AND a.ended_at IS NULL
					AND a.lease_token_hash IS DISTINCT FROM ` + param + `::bytea)`
}

// reportArgs returns the bound summary, truncated flag and artifact for a report.
func reportArgs(r Report) (summary string, truncated bool, artifact string) {
	summary, truncated = ClipSummary(r.Summary)
	return summary, truncated, r.Artifact
}

const attemptCols = `a.seq, a.attempt, a.claimer_kind, COALESCE(a.claimant, ''), a.claimed_at,
	a.last_heartbeat_at, a.lease_expires_at, a.ended_at, COALESCE(a.outcome, ''),
	COALESCE(a.disposition, ''), COALESCE(a.outcome IN ('lease_expired', 'reaped'), false),
	COALESCE(a.summary, ''), COALESCE(a.summary_truncated, false), COALESCE(a.artifact, '')`

// TodoAttempts returns up to limit attempts of one todo, newest first and including the open one,
// with the todo's attempts_total and attempts_pruned. endpointID is the tenant scope (ADR-0022):
// the todo and its attempts are read in ONE statement filtered on the todo's endpoint, so a foreign
// id and a never-minted id are both ErrNotFound and nothing reveals whether a foreign todo has
// attempts (SPEC-0034 REQ-10). limit defaults to 20 and is capped at 50 (REQ-8).
func (s *Store) TodoAttempts(ctx context.Context, endpointID, id string, limit int) ([]Attempt, int, int, error) {
	if err := endpointScope(endpointID); err != nil {
		return nil, 0, 0, err
	}
	return s.todoAttempts(ctx, `t.endpoint_id = $3`, id, limit, endpointID)
}

// todoAttempts runs the scoped attempt read; scope is the todo predicate over alias t, and its
// parameter is $3.
func (s *Store) todoAttempts(ctx context.Context, scope, id string, limit int, scopeArg any) ([]Attempt, int, int, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.attempts_total, t.attempts_pruned, a.seq IS NOT NULL, `+attemptCols+`
		FROM todos t
		LEFT JOIN LATERAL (
			SELECT * FROM todo_attempts WHERE todo_id = t.id ORDER BY seq DESC LIMIT $2
		) a ON true
		WHERE t.id = $1 AND `+scope+`
		ORDER BY a.seq DESC`, id, limit, scopeArg)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: todo attempts %s: %w", id, err)
	}
	defer rows.Close()
	var (
		out           []Attempt
		total, pruned int
		seen, hasAtt  bool
	)
	for rows.Next() {
		var a Attempt
		var seq, att *int
		var kind *string
		var claimedAt, leaseExp *time.Time
		if err := rows.Scan(&total, &pruned, &hasAtt, &seq, &att, &kind, &a.Claimant, &claimedAt,
			&a.LastHeartbeatAt, &leaseExp, &a.EndedAt, &a.Outcome, &a.Disposition, &a.Died,
			&a.Summary, &a.SummaryTruncated, &a.Artifact); err != nil {
			return nil, 0, 0, fmt.Errorf("store: todo attempts %s scan: %w", id, err)
		}
		seen = true
		if !hasAtt {
			continue
		}
		a.Seq, a.Attempt, a.ClaimerKind = *seq, *att, *kind
		a.ClaimedAt, a.LeaseExpiresAt = *claimedAt, *leaseExp
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: todo attempts %s: %w", id, err)
	}
	if !seen {
		return nil, 0, 0, ErrNotFound
	}
	return out, total, pruned, nil
}

// AttemptMetrics is the optional sink for switchboard_todo_attempts_closed_total (SPEC-0034 REQ-14).
// It is separate from Metrics so the lifecycle seam keeps its shape: a sink that also implements
// this receives one call per closed attempt, after commit; one that does not is simply not asked.
// *metrics.Metrics implements both. outcome is one of the REQ-3 outcomes.
type AttemptMetrics interface {
	AttemptClosed(queue, outcome string)
}

// countAttemptClosed reports one committed attempt close to the sink, when it takes them.
func (s *Store) countAttemptClosed(queue, outcome string) {
	if m, ok := s.metricsOrNop().(AttemptMetrics); ok {
		m.AttemptClosed(queue, outcome)
	}
}
