package ingest

// Issue Dependencies At Intake
//
// Two questions, asked once per delivery that names a tracker issue:
//
//   - Does this issue wait on any open issue? If yes, the todo this delivery would mint is minted as
//     `blocked` instead of `pending`: recorded, visible, counted, never handed out, and silent. The
//     work waits in the queue rather than in a worker run that would only discover the blocker is
//     unmerged.
//   - Did this delivery just close an issue that something else is waiting on? If yes, its dependents
//     are resolved and released, because the tracker sends a close for the blocker only and nothing at
//     all for the issues that were waiting behind it.
//
// Both are answered by internal/deps, which fails open: no provider connection, an unreachable
// tracker, or a provider with no dependency model all mean "unknown", and unknown mints the todo
// exactly as this build never held it. A hold is asserted only on evidence.
//
// The delivery body is untrusted: it says which issue the delivery is about, and nothing more. The
// connection dialed is the one the webhook's owner stored (internal/store/webhook_provider.go), never
// an address taken from the payload.
//
// Governing: stump.wtf/switchboard#583, ADR-0055 (proposed: what a provider read may be used for).

import (
	"context"

	"github.com/stump-wtf/switchboard/internal/deps"
	"github.com/stump-wtf/switchboard/internal/store"
)

// DependencyGate is the intake's dependence on internal/deps, as an interface so the receivers can be
// tested without a provider to dial. *deps.Checker satisfies it.
type DependencyGate interface {
	// BlockersFor asks the tracker what is blocking the issue, through the webhook's own connection.
	// ok false means nothing is known: the caller must not hold.
	BlockersFor(ctx context.Context, webhookID string, ref deps.IssueRef) ([]store.BlockerRef, bool)
	// ReleaseBlocker records that an issue closed and returns the dependents that were released.
	ReleaseBlocker(ctx context.Context, ref deps.IssueRef) ([]store.Todo, error)
}

// SetDependencyGate wires the dependency gate (nil clears it, which restores the pre-feature
// behaviour: nothing is ever held, and a close event resolves nothing).
func (i *Ingest) SetDependencyGate(g DependencyGate) { i.deps = g }

// hold is the intake's dependency verdict for one delivery: the refs to hold on, and the issue identity
// to key the hold by. Both empty means mint the todo as usual.
type hold struct {
	issueRef string
	blockers []store.BlockerRef
}

// dependencyVerdict asks the two questions above. It is called before the todos are minted, so the
// state the rows are inserted in is decided by the answer rather than corrected afterwards — that order
// is what keeps a held delivery from ever ringing a doorbell.
//
// A delivery that closes its own issue releases whatever waited on it and is not itself held: the work
// it describes is finished, whatever else is linked to it.
func (i *Ingest) dependencyVerdict(ctx context.Context, webhookID string, body []byte) hold {
	if i.deps == nil {
		return hold{}
	}
	ref, ok := deps.IssueRefFromPayload(body)
	if !ok {
		return hold{}
	}
	if ref.Closed() {
		if _, err := i.deps.ReleaseBlocker(ctx, ref); err != nil {
			// The close is already recorded as an event; the dependents stay held and the periodic
			// pass re-resolves them. A failed release must never fail the delivery the tracker sent
			// for the blocker, which is already accepted work.
			i.log.Warn("release of dependents failed; the recheck pass will retry",
				"issue", ref.RefString(), "webhook", webhookID, "err", err)
		}
		return hold{}
	}
	open, known := i.deps.BlockersFor(ctx, webhookID, ref)
	if !known || len(open) == 0 {
		return hold{}
	}
	return hold{issueRef: ref.RefString(), blockers: open}
}
