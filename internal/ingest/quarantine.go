package ingest

// Quarantine release (ADR-0031, SPEC-0026 REQ-7). A held delivery is released by routing it AGAIN
// through the webhook's current rules. The trust gate counts as satisfied, .actor carries the
// original verdict unchanged, and .release says who let it out. A release that names a queue skips
// the rules, but that queue must be within the owner's webhook-queue ceiling. If routing sends it
// back to quarantine, faults, or drops it, the release fails with ErrReleaseConflict and the item
// stays held.
//
// This lives beside the receiver because it IS the receiver's routing, rerun: the same router (the
// out-of-process sandbox), the same grant, and the same work orders and once keys. The plan is
// computed with no transaction open, since the sandbox can take seconds and the store reads
// need pooled connections. store.ApplyQuarantineRelease then locks the held row and applies the
// plan only if the row is still held.
//
// Callers are the owner's Quarantine view (#387) and, later, classifier endpoints (SPEC-0026 REQ-8);
// both pass by as "human:<id>" or "classifier:<slug>".
//
// Governing: ADR-0031, SPEC-0026 REQ-7 "Release and Discard", REQ-10, REQ-13.
//
// @joestump-agent 09/25/2026 - Added for #386.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Release refusals. Callers map ErrReleaseConflict to the stable "conflict" code, ErrQueueNotGranted
// to "forbidden", and ErrRoutingUnavailable to "unavailable". store.ErrNotFound passes through for an
// item that is unknown, foreign, or no longer held.
var (
	ErrReleaseConflict    = errors.New("ingest: release refused")
	ErrQueueNotGranted    = errors.New("ingest: release queue is not within the owner's webhook queues")
	ErrRoutingUnavailable = errors.New("ingest: routing is unavailable")
)

// ReleaseQuarantined releases one held delivery that ownerHumanID owns, as by, optionally straight
// to queue. It returns the todos the release produced: the held todo first (same id, new queue and
// target), then any further fan-out targets.
func (i *Ingest) ReleaseQuarantined(ctx context.Context, ownerHumanID, todoID, by, queue string) ([]store.CreatedTodo, error) {
	// Errors name the webhook and the stage (SPEC-0026 REQ-13), plus the item, once the webhook is
	// known; before that only the item id is.
	webhookID := ""
	wrap := func(err error) error {
		if webhookID == "" {
			return fmt.Errorf("todo %s: %s: %w", todoID, stageRelease, err)
		}
		return fmt.Errorf("webhook %s: todo %s: %s: %w", webhookID, todoID, stageRelease, err)
	}

	item, err := i.store.QuarantinedForHuman(ctx, ownerHumanID, todoID)
	if errors.Is(err, store.ErrHeldEventGone) {
		return nil, wrap(fmt.Errorf("%w: the held delivery's event is gone; discard it instead", ErrReleaseConflict))
	}
	if err != nil {
		return nil, wrap(err)
	}
	ev := item.Event
	webhookID = ev.WebhookID
	if ev.WebhookID == "" {
		return nil, wrap(fmt.Errorf("%w: the delivery's webhook no longer exists; discard it instead", ErrReleaseConflict))
	}
	rt, err := i.store.WebhookRoutingByID(ctx, ev.WebhookID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, wrap(fmt.Errorf("%w: the delivery's webhook no longer exists; discard it instead", ErrReleaseConflict))
	}
	if err != nil {
		return nil, wrap(err)
	}
	targets, err := i.store.ResolveWebhookTargets(ctx, rt.WebhookID, rt.EndpointID)
	if err != nil {
		return nil, wrap(err)
	}
	if len(targets) == 0 {
		return nil, wrap(fmt.Errorf("%w: the webhook has no live delivery target", ErrReleaseConflict))
	}
	scopes, err := i.store.EndpointScopeQueues(ctx, targets)
	if err != nil {
		return nil, wrap(err)
	}

	var detail quarantineDetail
	_ = json.Unmarshal(item.Todo.QuarantineDetail, &detail) // a missing detail releases with a null .actor
	in := routing.EnvelopeInput{
		Source: rt.SourceType, Kind: ev.EventType, WebhookID: rt.WebhookID, TrustMode: rt.TrustMode,
		Verified: ev.Verified, ContentType: ev.ContentType, Body: ev.Payload, Actor: detail.Actor,
		Release: &routing.Release{By: by, At: i.now().UTC().Format(time.RFC3339)},
	}
	_ = json.Unmarshal(ev.Headers, &in.Headers)
	g := routing.Grant{TargetQueue: rt.TargetQueue, Queues: rt.WebhookQueues, Endpoints: targets, EndpointQueues: scopes}

	var d routing.Decision
	if queue != "" {
		if !routing.QueueGranted(queue, g) {
			return nil, wrap(ErrQueueNotGranted)
		}
		d = routing.Decision{Queue: queue, Endpoints: slices.Clone(targets),
			Trace: routing.Trace{Stage: stageRelease, Cause: "named_queue", Action: routing.Action{Queue: queue}}}
	} else {
		router := i.router
		if router == nil {
			router = routing.Unavailable{}
		}
		d = router.Route(ctx, rt.Config, g, in)
		switch {
		case d.Unavailable:
			return nil, wrap(ErrRoutingUnavailable)
		case d.Faulted:
			return nil, wrap(fmt.Errorf("%w: the webhook's rules fault on this delivery (%s)", ErrReleaseConflict, d.Fault.Cause))
		case d.Quarantine:
			return nil, wrap(fmt.Errorf("%w: the webhook's rules send this delivery back to quarantine; name a queue to release it", ErrReleaseConflict))
		case d.Drop:
			return nil, wrap(fmt.Errorf("%w: the webhook's rules drop this delivery; discard it, or name a queue", ErrReleaseConflict))
		}
	}

	subject := routing.SubjectOf(rt.SourceType, in.Headers, ev.Payload)
	var onceKey string
	if d.Once {
		onceKey = routing.OnceKey(subject, d.Queue)
		d.Trace.OnceKey = onceKey
	}
	var workOrder []byte
	if d.WorkOrder {
		if workOrder, err = json.Marshal(routing.BuildWorkOrder(d, in, subject)); err != nil {
			return nil, wrap(err)
		}
	}
	trace, err := json.Marshal(d.Trace)
	if err != nil {
		return nil, wrap(err)
	}
	out, err := i.store.ApplyQuarantineRelease(ctx, store.ReleasePlan{
		TodoID: todoID, OwnerHumanID: ownerHumanID, By: by, Queue: d.Queue, Endpoints: d.Endpoints,
		Trace: trace, WorkOrder: workOrder, OnceKey: onceKey, WebhookID: rt.WebhookID,
		Verified: ev.Verified, TrustMode: rt.TrustMode,
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, wrap(fmt.Errorf("%w: the item is no longer held, its work order was already issued, or the target "+
			"already holds a live todo for this delivery (if it is still held, discard it)", ErrReleaseConflict))
	}
	if err != nil {
		return nil, wrap(err)
	}
	for _, ct := range out {
		if ct.New {
			i.hub.Publish(ct.Todo) // as the receiver does for freshly routed work
		}
	}
	return out, nil
}

// DiscardQuarantined completes one held delivery that ownerHumanID owns, recording who discarded
// it and why. The loser of a race with a release gets ErrReleaseConflict. Governing: SPEC-0026 REQ-7.
func (i *Ingest) DiscardQuarantined(ctx context.Context, ownerHumanID, todoID, by, reason string) (store.Todo, error) {
	t, err := i.store.DiscardQuarantined(ctx, ownerHumanID, todoID, by, reason)
	if errors.Is(err, store.ErrConflict) {
		return store.Todo{}, fmt.Errorf("todo %s: discard: %w: the item is no longer held", todoID, ErrReleaseConflict)
	}
	if err != nil {
		return store.Todo{}, fmt.Errorf("todo %s: discard: %w", todoID, err)
	}
	return t, nil
}
