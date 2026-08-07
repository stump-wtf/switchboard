-- 0012_todo_a2a_states — A2A task semantics for the Todo state machine.
-- Governing: ADR-0021 (A2A task-delegation transport), SPEC-0018 REQ "Task State Machine Extension".
--
-- The Todo state machine (ADR-0007) grows four new states so A2A's task semantics have a durable
-- home, WITHOUT touching any existing pending/claimed/done/failed row:
--   canceled       — terminal; explicitly canceled (CancelTask), NOT retried out. Distinct from
--                    `failed`: it never re-enters `pending` and is never dead-lettered.
--   rejected       — terminal; never claimed (e.g. an intake policy check fails). NOT eligible for
--                    the retry/backoff behavior that applies to `failed`.
--   input-required — interrupt state entered from `claimed`; returns to `claimed` (retaining owner
--                    + lease) once the required input is supplied.
--   auth-required  — interrupt state entered from `claimed`; returns to `claimed` (retaining owner
--                    + lease) once the required auth is resolved.
-- The existing four states project onto A2A's TaskState in Go (internal/store/a2a_state.go); the DB
-- keeps their internal names unchanged (ADR-0021: "Task" is wire vocabulary, not a rename).
--
-- The `todos.state` column was declared plain `text` with no CHECK (0001_init). This migration is
-- additive: it stamps a CHECK enumerating the full valid domain (the four existing + four new
-- states). Every existing row is already `pending`/`claimed`/`done`/`failed`, so all satisfy it and
-- no row changes; NOT VALID + VALIDATE keeps the ADD lock brief and never rewrites the table. It
-- mirrors the friend_edges precedent (0005) of a CHECK-enumerated state domain.
ALTER TABLE todos
    ADD CONSTRAINT todos_state_check
    CHECK (state IN ('pending', 'claimed', 'done', 'failed',
                     'canceled', 'rejected', 'input-required', 'auth-required'))
    NOT VALID;
ALTER TABLE todos VALIDATE CONSTRAINT todos_state_check;
