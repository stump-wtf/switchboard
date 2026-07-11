-- 0004_friend_edges — the human-vended friend-edge lifecycle (SPEC-0010, ADR-0010).
-- A friend edge is a per-direction, revocable, non-transitive request for one persona to hand work
-- to another. The pending edge grants NOTHING; the target human's approval is the sole act that
-- mints a scoped MCP endpoint on the existing `endpoints` substrate (ADR-0008), and revocation kills
-- that endpoint. Lifecycle: pending → approved | denied, and approved → revoked (both terminal).

CREATE TABLE friend_edges (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Directional identity: an A→B edge is distinct from B→A and opaque to it (non-transitive).
    from_persona        text NOT NULL,                          -- requesting persona identifier/handle
    to_persona          text NOT NULL,                          -- target persona identifier/handle
    direction           text NOT NULL DEFAULT 'outbound',       -- per-direction marker; the grant flows requester→target
    -- Principals. to_human is the owning/target human who authorizes approve/deny/revoke and is the
    -- ownership-isolation key (cross-owner reads are ErrNotFound). from_human is the OIDC-attested
    -- human behind the requester, carried for the legible who/why approval todo (SPEC-0010).
    from_human          uuid REFERENCES humans(id) ON DELETE SET NULL,
    to_human            uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    -- The local agent the minted endpoint is vended for. Null until approval, since a request may
    -- arrive before any local agent record stands in for the remote requester. Set at approval time.
    from_agent_id       uuid REFERENCES agents(id) ON DELETE SET NULL,
    state               text NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'approved', 'denied', 'revoked')),
    -- requested_* is the ceiling the requester asked for; granted_* is what the human approved and
    -- MUST be a subset (narrow-only, enforced in the store). The minted endpoint carries granted_*.
    requested_queues    text[] NOT NULL DEFAULT '{}',
    requested_verbs     text[] NOT NULL DEFAULT '{}',
    granted_queues      text[] NOT NULL DEFAULT '{}',
    granted_verbs       text[] NOT NULL DEFAULT '{}',
    reason              text,                                   -- why (legible approval-todo context)
    provenance_verified boolean NOT NULL DEFAULT false,         -- true only when OIDC provenance validated
    endpoint_id         uuid REFERENCES endpoints(id) ON DELETE SET NULL,  -- the vend; null until approved
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    decided_at          timestamptz,                            -- approve/deny stamp
    revoked_at          timestamptz
);

-- At most one LIVE edge per (from_persona, to_persona, direction): a second pending/approved request
-- for the same directional pair collides, enforcing the pending-edge-grants-nothing invariant and
-- blunting flooding. Denied/revoked edges are terminal and do not block a fresh re-request (SPEC-0010).
CREATE UNIQUE INDEX idx_friend_edges_live
    ON friend_edges (from_persona, to_persona, direction)
    WHERE state IN ('pending', 'approved');

-- Owner-scoped listing (the target human's inbound edges) filtered by state.
CREATE INDEX idx_friend_edges_owner ON friend_edges (to_human, state);

-- Per-target pending backlog: supports quota counting of outstanding approvals per target human.
CREATE INDEX idx_friend_edges_pending_target ON friend_edges (to_human) WHERE state = 'pending';
