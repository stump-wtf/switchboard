-- Todo lineage: the thread key, the statement table, and tags.
--
-- SPEC-0037's design and ADR-0044 are on main, but none of its schema is, so every story
-- downstream of this one (declaring what an attempt produced, spawning a child, tagging a todo)
-- has nothing to write into. This migration is that schema and nothing else: additive, no
-- backfill — history is filled on demand by the backfill command the design names — and every
-- link and tag dies with its todo so a pruned todo cannot leave a dangling edge behind.
--
-- subject_key is the normalized key of the thread a todo belongs to. It is nullable because
-- nothing written before this migration has one, and the index is partial for the same reason:
-- it serves the thread scan (key first, then age, which is how a thread is listed oldest
-- first) without indexing the large NULL half of the table.
ALTER TABLE todos ADD COLUMN subject_key text CHECK (octet_length(subject_key) <= 256);
CREATE INDEX idx_todos_subject_key ON todos (subject_key, created_at) WHERE subject_key IS NOT NULL;

-- todo_links holds one statement that someone made: this todo (and optionally this attempt of
-- it) produced a thread key, or spawned that other todo. The target column depends on the kind,
-- and the two table-level CHECKs below are what enforce it as an exact biconditional: a
-- `produced` row must carry a key and no todo id, a `spawned` row a todo id and no key, so a
-- row that carries both targets or neither is rejected rather than left ambiguous for the
-- graph to guess at. The `spawned` target is a real todo id because deleting it must delete
-- the edge pointing at it; a thread key has no row to cascade from.
CREATE TABLE todo_links (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_todo_id      text        NOT NULL REFERENCES todos(id) ON DELETE CASCADE,
    from_attempt_seq  int,
    kind              text        NOT NULL CHECK (kind IN ('produced','spawned')),
    to_subject_key    text        CHECK (octet_length(to_subject_key) <= 256),
    to_todo_id        text        REFERENCES todos(id) ON DELETE CASCADE,
    provenance        text        NOT NULL CHECK (provenance IN ('declared','api','tagged','trace')),
    actor_ref         text        NOT NULL CHECK (octet_length(actor_ref) <= 128),
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'produced') = (to_subject_key IS NOT NULL)),
    CHECK ((kind = 'spawned')  = (to_todo_id IS NOT NULL))
);

-- Duplicate statements are idempotent (an agent that retries a heartbeat must not draw the same
-- edge twice), so the statement is keyed on who said it, about what, of what kind, and how it is
-- known. COALESCE collapses the two target columns into the one column each kind fills.
CREATE UNIQUE INDEX uq_todo_links_stmt ON todo_links
    (from_todo_id, kind, COALESCE(to_subject_key, to_todo_id), provenance);
-- Both walks: find the statements that name this key, and the todos that spawned this one.
CREATE INDEX idx_todo_links_to_key  ON todo_links (to_subject_key) WHERE to_subject_key IS NOT NULL;
CREATE INDEX idx_todo_links_to_todo ON todo_links (to_todo_id)     WHERE to_todo_id IS NOT NULL;

-- Tags are the hand-written labels a todo carries alongside its routing queue. The grammar CHECK
-- is what keeps a tag shareable across surfaces — the same characters an OpenSpec or Cairn tag
-- accepts — so a tag typed on the web is the same tag a rule or a producer writes.
CREATE TABLE todo_tags (
    todo_id   text        NOT NULL REFERENCES todos(id) ON DELETE CASCADE,
    tag       text        NOT NULL CHECK (tag ~ '^[a-z0-9._:/#-]{1,64}$'),
    origin    text        NOT NULL CHECK (origin IN ('rule','agent','human','producer')),
    added_by  text        NOT NULL CHECK (octet_length(added_by) <= 128),
    added_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (todo_id, tag)
);
CREATE INDEX idx_todo_tags_tag ON todo_tags (tag);

-- Governing: SPEC-0037 REQ-4/REQ-18, ADR-0044
