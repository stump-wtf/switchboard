-- Doorbell heartbeat: re-ring a pending todo nobody picked up.
--
-- The push is a hint and the queue is the ledger (ADR-0013), but under push-only
-- delivery that promise was only half true: a doorbell that arrived while every
-- worker was busy — or that was dropped by a transport fault, or landed during a
-- restart — was never repeated, so the todo sat pending forever with nothing to
-- surface it again. A 50-row backlog accumulated exactly that way.
--
-- last_ringed_at is when a doorbell was last published for the row (NULL = never
-- rung since this column existed). ring_attempts counts those publishes, so the
-- sweep can back off and eventually stop rather than ringing a todo nobody wants
-- until the end of time.
ALTER TABLE todos ADD COLUMN IF NOT EXISTS last_ringed_at timestamptz;
ALTER TABLE todos ADD COLUMN IF NOT EXISTS ring_attempts integer NOT NULL DEFAULT 0;

-- The sweep's predicate: pending rows, oldest ring first. Partial on state so the
-- index stays small — claimed and done rows are never candidates.
CREATE INDEX IF NOT EXISTS idx_todos_pending_ring
  ON todos (last_ringed_at NULLS FIRST, created_at)
  WHERE state = 'pending';
