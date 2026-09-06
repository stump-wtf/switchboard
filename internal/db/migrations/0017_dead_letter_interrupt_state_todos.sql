-- Backfill: dead-letter todos stranded in A2A interrupt states on revoked endpoints.
--
-- 0016 dead-lettered 'pending' and 'claimed' rows, but the A2A interrupt states
-- ('input-required'/'auth-required') are also non-terminal and equally unrecoverable once the
-- endpoint's credential dies: never rung (the sweep rings pending only), never reaped, not
-- cancelable, never pruned by retention. 0016 shipped with the narrower predicate, so this
-- migration catches up anything it missed; on a database that never had interrupt-state rows on
-- revoked endpoints it updates zero rows.
--
-- Same one-way shape as 0016: state='failed' with next_retry_at NULL, and a result naming the
-- cause so a row found later explains itself without archaeology.
UPDATE todos t
SET state           = 'failed',
    next_retry_at   = NULL,
    lease_expires_at = NULL,
    owner            = NULL,
    result           = jsonb_build_object(
                         'error', 'endpoint_revoked',
                         'detail', 'the endpoint this todo was routed to was revoked; no session can claim it'
                       ),
    updated_at       = now(),
    completed_at     = now()
FROM endpoints ep
WHERE ep.id = t.endpoint_id
  AND ep.state = 'revoked'
  AND t.state IN ('pending', 'claimed', 'input-required', 'auth-required');
