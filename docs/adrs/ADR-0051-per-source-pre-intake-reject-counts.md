---
status: proposed
date: 2026-10-24
decision-makers: Proposed by joestump-agent, pending Joe
extends: [ADR-0028]
related: [ADR-0012, ADR-0038]
---

# ADR-0051: Count Deliveries Refused Before Intake Per Source — The Losses The Queue Cannot See

**Status note:** this ADR is **proposed**. It ships with an implementation so the choice can be argued
against working code rather than prose; nothing here is settled until Joe accepts it. The requirement
text it implies for SPEC-0026 REQ-9 is listed under Consequences rather than applied.

## Context and Problem Statement

Switchboard guarantees that work it accepted is not lost: a claim whose lease lapses is requeued. It
guarantees nothing about work it never accepted. A delivery the ingest throttle answers `429`, one
whose signature fails verification, one whose body is refused — none of those becomes an event row, a
todo, or a quarantine item. The rejection doctrine is deliberate about that: a refused payload is
never stored. But the reporting that started this thread lost six issues that way, because the
producers most likely to be throttled (Gitea deliveries among them) do not retry a `429`: the item
stays in the tracker in a state no consumer will ever look at again, and Switchboard reports a quiet,
green queue while it sits there (#612, from upstream stump-wtf/switchboard#38).

Two things are wrong and only one of them is fixable inside this slice. The loss itself needs the
reconcile pass — read the provider back and re-drive what never arrived — and that is #587 with its
child #616, which needs provider read access, a credential path, and a design of its own. What is
fixable now is the blindness: today an operator cannot tell *which source* is leaking, or whether it
is leaking at all, from anywhere in the product. The reporter asked for exactly that minimum: a
per-source count of deliveries that were rejected (`429`) or failed signature checks, on the endpoint
card.

## Decision

**Count every pre-intake refusal in one in-process ledger, keyed by the webhook it was addressed to,
and read that same ledger from both `/metrics` and the endpoint card.**

- One counter family: `switchboard_webhook_preintake_rejects_total{webhook,reason}`. `reason` carries
  the bounded failure mode the receivers already name (`bad_signature`, `missing_signature`,
  `awaiting_secret`, `too_large`, `unknown_webhook`, …) plus `rate_limited` for the throttle.
- The ledger is the single source. The Prometheus family is collected from it at scrape time, the same
  way the queue-liveness gauges read the store at scrape time, so the card and a graph cannot drift
  apart, and the card read costs no database query.
- **Nothing is persisted.** A refused delivery has no row to hang a count on, and adding a table written
  on every refusal would put a database write on the one path whose job is to shed load. The honest
  cost is stated where it is read: the card says the counts begin when the process starts.
- Cardinality reuses the existing sticky webhook limiter (SPEC-0023 REQ-5): a webhook past the cap
  folds into `__other__`, and a card for a folded webhook says nothing rather than quoting the shared
  overflow number. Reading a card never claims a limiter slot.
- **The throttle is attributed from a pairing the receiver already made.** The `429` is written by the
  limiter's middleware, before any handler runs, so nothing there knows the source. When the receiver
  resolves an ingest token it records `(hash of the token → webhook id)`; the throttle hook reads that
  map. A rejected request therefore costs one SHA-256 and a map read — never a lookup, never on the
  path that is already being hammered. A token this process has never resolved is counted under
  `webhook="__other__"`, so the refusal is still counted even when no source can be named yet.

## Considered Options

- **Persist rejects as `events` rows.** Rejected: it stores metadata about payloads the doctrine says
  must not be kept, needs a migration and a new disposition value, and writes on the reject path.
- **Reconcile against the provider here.** Rejected: that is #587/#616. It needs provider read access
  and a credential path, which is precisely what this slice exists to avoid.
- **A DB read on the reject path to resolve the token.** Rejected: a `429` is by definition the busiest
  moment of a surface; making it query the database turns a throttle into an amplification vector.
- **Per-IP throttle counters only, no source.** Rejected: it answers "were we throttling someone" and
  not the question asked, which is *which source is losing work*.
- **Store counters in a `CounterVec` and read them back for the card.** Rejected: it is two numbers
  (a vector and a card-reading path over label values) where the ledger is one, and reading values out
  of a vector needs the exposition-format plumbing.

## Consequences

**Gains.** An operator can name the leaking source from the card that already lists their webhooks, and
graph it: `rate_limited` says throttling (raise `SWITCHBOARD_WEBHOOK_RATE_RPS`/`_BURST`, #586), a
verification reason says the producer's secret drifted. Nothing new is stored, so there is no new
retention question and no new secret. The reconcile pass (#616) inherits these counts as the signal for
what to re-drive.

**Costs and limits.** The counts reset on restart, and the card says so on the chip. A `429` that
arrives before its token has been resolved once since startup is counted without a source. A deployment
with more than the label cap of webhooks gets metrics overflow and no card number for the folded ones.
Attribution is best-effort by construction: it is a visibility aid, not an audit trail, and it must not
be cited as proof that a specific delivery was lost.

**Implied requirement changes.** SPEC-0023 REQ-4 gains the family above, and this PR applies that
wording in `docs/openspec/specs/metrics/spec.md`: the family is exactly what the implementation
counts, so its requirement ships with the code rather than after it. SPEC-0026 REQ-9's third count
per webhook row — pre-intake rejects since process start, withheld when the webhook folded into the
cardinality overflow bucket — remains listed here, not applied, until this ADR is accepted. Neither
changes what is persisted, and neither grants any source new access.
