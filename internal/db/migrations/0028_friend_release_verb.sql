-- 0028_friend_release_verb — re-broadens the release verb migration 0025 stripped
-- (closes #538).
--
-- 0025_friend_edges_own_authority narrows every friend-minted endpoint to a hardcoded list
-- of grantable verbs. That list predates the release verb (#507), so an approved edge that
-- already held release silently lost it, and its endpoint answered forbidden to a verb it
-- was correctly granted. The store-side list (store.friendGrantableVerbs) was corrected in
-- #507; the SQL was not. A migration that has already run cannot be edited in place, so
-- this one repairs its output.
--
-- The damage is reconstructable: a grant is always a subset of the request (SPEC-0010), and
-- release only became grantable in #507, so release could ever be granted to an edge whose
-- request carried it. This migration therefore re-adds release to every approved edge that
-- requested it and no longer holds it, and re-broadens the edge's vended endpoint with it.
-- One limit is accepted: an approver who deliberately withheld release while granting the
-- other requested verbs is indistinguishable from a stripped grant, and gets release back.
-- release sits in the same drain-verb family as complete and fail (it ends the friend's own
-- attempt on a todo it claimed and carries no webhook, rule, route or event authority), the
-- rebuilt grant still satisfies granted ⊆ requested, and the narrow-only approval path
-- stays the place where a narrower grant is expressed. An operator who needs that grant
-- narrowed again can revoke the edge and re-approve with a smaller scope.
--
-- The rebuild keeps every verb currently granted and reorders the array into the canonical
-- friendGrantableVerbs order, mirroring 0025's order-preserving style. Pending, denied and
-- revoked edges are untouched: a pending edge grants nothing and approval filters verbs
-- against the corrected store list; a revoked edge's grant is dead with its endpoint.
-- Nothing is narrowed here, and no verb outside the friend-grantable set is introduced.
--
-- Rollback: none meaningful. Narrowing again would re-apply the original bug; the previous
-- grants are no longer recoverable from the database.
UPDATE friend_edges f
   SET granted_verbs = ARRAY(
           SELECT c.v FROM unnest(ARRAY['create_for', 'list_todos', 'get_todo', 'claim', 'claim_next', 'complete', 'fail', 'release', 'heartbeat'])
            WITH ORDINALITY AS c(v, n)
            WHERE c.v = ANY(f.granted_verbs)
               OR (c.v = 'release' AND f.requested_verbs @> ARRAY['release'])
            ORDER BY c.n),
       updated_at = now()
 WHERE f.state = 'approved'
   AND f.requested_verbs @> ARRAY['release']
   AND NOT f.granted_verbs @> ARRAY['release'];

UPDATE endpoints e
   SET scope_verbs = ARRAY(
           SELECT c.v FROM unnest(ARRAY['create_for', 'list_todos', 'get_todo', 'claim', 'claim_next', 'complete', 'fail', 'release', 'heartbeat'])
            WITH ORDINALITY AS c(v, n)
            WHERE c.v = ANY(e.scope_verbs)
               OR (c.v = 'release' AND f.requested_verbs @> ARRAY['release'])
            ORDER BY c.n)
  FROM friend_edges f
 WHERE f.endpoint_id = e.id
   AND f.state = 'approved'
   AND f.requested_verbs @> ARRAY['release']
   AND NOT e.scope_verbs @> ARRAY['release'];
