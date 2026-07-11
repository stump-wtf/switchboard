-- 0004_personas — persona records: named, scoped faces of a single registered agent (ADR-0009).
-- A persona is exactly three authored parts: the base agent it is a face of, a human-authored
-- system prompt, and a subset of that agent's vended verbs/queues (its capability slice). The
-- verb_subset/queues are validated as a subset of the agent's vended grant at write time
-- (enforced in the store, not the schema, since the grant is the dynamic union of the agent's
-- active endpoints). SPEC-0009 REQ "Persona Record".

CREATE TABLE personas (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_human_id uuid NOT NULL REFERENCES humans(id) ON DELETE CASCADE,
    agent_id       uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,  -- the one backing agent, ADR-0008
    name           text NOT NULL,
    slug           text NOT NULL,                                          -- public URL segment (/a/{slug}); not a secret
    system_prompt  text NOT NULL,                                          -- human-authored; the system never derives it
    verb_subset    text[] NOT NULL DEFAULT '{}',                           -- capability slice ⊆ agent's vended verbs
    queues         text[] NOT NULL DEFAULT '{}',                           -- queue slice ⊆ agent's vended queues
    description    text,                                                   -- optional; surfaced on the Agent Card
    published      boolean NOT NULL DEFAULT false,                         -- owner-controlled discoverability (SPEC-0009)
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Personas are listed per owner; the slug must resolve to exactly one of a human's personas.
CREATE INDEX idx_personas_owner ON personas (owner_human_id);
CREATE INDEX idx_personas_agent ON personas (agent_id);
CREATE UNIQUE INDEX idx_personas_owner_slug ON personas (owner_human_id, slug);
