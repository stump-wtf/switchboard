// Ephemeral received-lane instrumentation (SPEC-0015 REQ "Patch Panel Board"): the receivers
// report in-flight deliveries — arrival, rejection, dedup collapse — to an observer so the board's
// *received* lane can render the moment of verification live. Presentation-only by contract:
// nothing here persists state, the observer must never block, and a rejected payload is reported
// REDACTED (provider, event type, trust mode, idempotency key, and a client-safe reason — never
// the body, headers, signature, or token; SPEC-0001 rejection doctrine unchanged). Accepted
// deliveries are NOT reported as accepted here — the store's committed-transition hooks own that
// (the todo_created frame advances the card to the verified lane), so a card can never advance on
// an uncommitted write.
//
// Governing: SPEC-0015 REQ "Patch Panel Board" (received lane is ephemeral, SSE-only), design.md
// "Received lane is ephemeral by design"; ADR-0018.
package ingest

// Instrument observes in-flight deliveries for the board's ephemeral received lane. Implementations
// must be non-blocking and best-effort (the web layer enqueues onto its lossy live queue); a nil
// instrument disables observation entirely.
type Instrument interface {
	// DeliveryReceived reports a delivery entering verification (the card appears in *received*).
	DeliveryReceived(provider, eventType, trust, key string)
	// DeliveryRejected reports a delivery that failed verification and was NOT persisted. reason is
	// the client-safe rejection message (already redacted — no signature/token/body detail).
	DeliveryRejected(provider, eventType, trust, key, reason string)
	// DeliveryDeduped reports an accepted delivery that collapsed onto an existing live todo
	// (idempotency dedup) — the in-flight card resolves without a lane advance.
	DeliveryDeduped(provider, eventType, trust, key string)
}

// SetInstrument registers the received-lane observer. Wire it before the receivers serve traffic;
// passing nil clears it.
func (i *Ingest) SetInstrument(ins Instrument) { i.instrument = ins }

// observeReceived reports an in-flight delivery, if an instrument is wired.
func (i *Ingest) observeReceived(provider, eventType, trust, key string) {
	if i.instrument != nil {
		i.instrument.DeliveryReceived(provider, eventType, trust, key)
	}
}

// observeRejected reports a rejected (never-persisted) delivery, if an instrument is wired.
func (i *Ingest) observeRejected(provider, eventType, trust, key, reason string) {
	if i.instrument != nil {
		i.instrument.DeliveryRejected(provider, eventType, trust, key, reason)
	}
}

// observeDeduped reports an idempotency-collapsed delivery, if an instrument is wired.
func (i *Ingest) observeDeduped(provider, eventType, trust, key string) {
	if i.instrument != nil {
		i.instrument.DeliveryDeduped(provider, eventType, trust, key)
	}
}
