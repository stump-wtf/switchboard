-- Adapter poll-loop health tracking (SPEC-0002 REQ "Poll-Loop Lifecycle — Concurrency Safety"):
-- the runner stamps each consume attempt so an operator can see when an adapter last polled and
-- whether it is degraded (backing off on a broker error) without shell access to the process.
-- last_error holds the wrapped (credential-free) error text of the most recent failed attempt and
-- is cleared on the next healthy one.
ALTER TABLE adapters
    ADD COLUMN last_poll_at timestamptz,
    ADD COLUMN last_error   text;
