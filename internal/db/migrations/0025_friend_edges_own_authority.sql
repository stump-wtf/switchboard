-- 0025_friend_edges_own_authority — friend-vended endpoints act with their own authority
-- (SPEC-0033 REQ "Closing the Audited Surfaces" F3 and F13; ADR-0038; closes #420).
--
-- F13: the live-edge uniqueness key was (from_persona, to_persona, direction) across the whole
-- instance, so two tenants each sending from a persona named "reviewer" to the same target persona
-- collided, and the second sender learned that the first existed. The humans join the key. Widening a
-- unique key cannot invalidate existing rows. NULLS NOT DISTINCT keeps an unattested requester
-- (from_human NULL) colliding with itself exactly as before, so the anti-flood property holds.
--
-- F3: a friend endpoint is vended on the approver's agent, so every verb on it acts with the
-- approver's authority. Friend grants are now limited to create_for and the drain verbs at intake and
-- at approval (internal/store/friends.go). Endpoints already minted from an approved edge are
-- narrowed to the same set here, and their edge's recorded grant with them, so the fix reaches live
-- friendships and not only new ones. Pending requests keep their requested verbs; approval filters
-- them. Nothing is widened.
--
-- Rollback: recreate idx_friend_edges_live on (from_persona, to_persona, direction). The narrowed
-- verbs are not restored: re-granting webhook or event verbs to a friend is the vulnerability.
DROP INDEX idx_friend_edges_live;
CREATE UNIQUE INDEX idx_friend_edges_live
    ON friend_edges (from_human, to_human, from_persona, to_persona, direction) NULLS NOT DISTINCT
    WHERE state IN ('pending', 'approved');

UPDATE endpoints e
   SET scope_verbs = ARRAY(
           SELECT v FROM unnest(e.scope_verbs) WITH ORDINALITY AS s(v, n)
            WHERE v IN ('create_for', 'list_todos', 'claim', 'claim_next', 'complete', 'fail', 'heartbeat')
            ORDER BY n)
  FROM friend_edges f
 WHERE f.endpoint_id = e.id
   AND f.state = 'approved'
   AND NOT e.scope_verbs <@ ARRAY['create_for', 'list_todos', 'claim', 'claim_next', 'complete', 'fail', 'heartbeat'];

UPDATE friend_edges f
   SET granted_verbs = ARRAY(
           SELECT v FROM unnest(f.granted_verbs) WITH ORDINALITY AS s(v, n)
            WHERE v IN ('create_for', 'list_todos', 'claim', 'claim_next', 'complete', 'fail', 'heartbeat')
            ORDER BY n),
       updated_at = now()
 WHERE f.state = 'approved'
   AND NOT f.granted_verbs <@ ARRAY['create_for', 'list_todos', 'claim', 'claim_next', 'complete', 'fail', 'heartbeat'];
