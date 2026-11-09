-- Issue-dependency holds: a todo whose issue is blocked by open issues waits as `blocked`.
--
-- Until now switchboard could not see that an issue waits on another issue, so a dependent todo was
-- handed to a worker, which discovered the blocker had not merged and parked the issue by hand. The
-- hold lives in the queue instead: the row is recorded, counted, and visible, but it is not handed
-- out and rings nothing. Closing a blocker releases it (see internal/store/todo_blockers.go).
--
-- The state domain is widened the way 0012_todo_a2a_states widened it: rebuild the CHECK with
-- NOT VALID and validate it separately, so the ADD takes only a brief lock and never rewrites the
-- table. Every existing row is in the old domain, which the new domain includes.
ALTER TABLE todos DROP CONSTRAINT IF EXISTS todos_state_check;
ALTER TABLE todos
    ADD CONSTRAINT todos_state_check
    CHECK (state IN ('pending', 'claimed', 'done', 'failed',
                     'canceled', 'rejected', 'input-required', 'auth-required',
                     'blocked'))
    NOT VALID;
ALTER TABLE todos VALIDATE CONSTRAINT todos_state_check;

-- issue_ref is the tracker issue this todo was minted from, canonicalized as "owner/repo#number"
-- (empty for every todo that did not come from a forge issue delivery). It is what a hold is keyed
-- by: one held todo per issue per endpoint, so events that arrive while an issue is held fold into
-- the item that is already waiting instead of queueing one todo per event.
ALTER TABLE todos ADD COLUMN issue_ref     text,
                  ADD COLUMN coalesced_count int NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX uq_todos_held_per_issue
    ON todos (endpoint_id, queue, issue_ref)
    WHERE state = 'blocked' AND issue_ref IS NOT NULL;
CREATE INDEX idx_todos_blocked ON todos (endpoint_id, created_at) WHERE state = 'blocked';

-- todo_blockers records, for one held todo, the tracker issues that keep it waiting. Refs are stored
-- per todo (never shared) because links can cross repositories and each todo's hold is resolved
-- against the tracker on its own schedule; the blocker index is how a close event finds the
-- dependents to release, since the tracker sends nothing for a dependent when its blocker closes.
CREATE TABLE todo_blockers (
    todo_id    text NOT NULL REFERENCES todos(id) ON DELETE CASCADE,
    owner      text NOT NULL,
    repo       text NOT NULL,
    number     int  NOT NULL,
    -- state is the blocker's own tracker state verbatim ("open" | "closed"), as of checked_at. A
    -- blocker counts as done when it is closed, however it was closed; a reopen flips it back and
    -- re-holds the dependent.
    state      text NOT NULL DEFAULT 'open'
        CONSTRAINT todo_blockers_state_check CHECK (state IN ('open', 'closed')),
    html_url   text,
    checked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (todo_id, owner, repo, number)
);

-- Release lookup: which held todos is this issue blocking? Cross-repository by construction: the
-- key is the blocker's own owner/repo/number, not the dependent's.
CREATE INDEX todo_blockers_blocker ON todo_blockers (owner, repo, number);
