-- 0008_todo_retry_backoff — scheduled exponential backoff between failed attempts.
-- A fail below the attempt cap no longer returns the todo to `pending` immediately: it parks in
-- `failed` with `next_retry_at` stamped (attempt 1 → 30s, doubling per attempt, capped at 15m —
-- constants live in internal/store/todos.go). The claim scan and the reaper-adjacent retry
-- scheduler re-queue the todo once the backoff elapses; a dead-lettered todo (attempts exhausted)
-- keeps `next_retry_at` NULL and moves only on an explicit operator/agent retry.
-- Governing: SPEC-0003 REQ "Bounded Retries via max_attempts" (scheduled backoff), design canvas
-- (drawer failed-card "retry with backoff · attempt N" + live ↻ countdown).
ALTER TABLE todos ADD COLUMN next_retry_at timestamptz;
-- Hot scheduler scan: failed rows with a due retry window.
CREATE INDEX idx_todos_retry_due ON todos (next_retry_at)
    WHERE state = 'failed' AND next_retry_at IS NOT NULL;
