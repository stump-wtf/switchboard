-- Dead-letter the work stranded on endpoints that were already revoked.
--
-- Revocation kills the credential, so no session can ever authenticate as that
-- endpoint again and nothing can claim its pending todos. Until now the cascade
-- stopped at the endpoint and its OAuth rows, leaving the todos pending forever.
--
-- That was not merely untidy. These rows accumulate at the OLD end of the table
-- (an endpoint is revoked after a life of receiving work), so every oldest-first
-- sweep reached for them first, and a doorbell that resolves to no session logs
-- nothing at all. 1,442 such rows silently absorbed the entire heartbeat budget
-- while live endpoints holding real work were never rung. Excluding them from the
-- sweep stopped the bleeding; this stops them existing.
--
-- Retiring them one-way as 'failed' with next_retry_at NULL is deliberate: there
-- is no future in which the revoked endpoint drains them, so a retry schedule
-- would be a lie. The result records why, so a row found later explains itself.
UPDATE todos t
SET state            = 'failed',
    next_retry_at    = NULL,
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
