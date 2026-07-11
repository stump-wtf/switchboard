-- 0006_persona_discoverable — reconcile the persona discoverability flag name with SPEC-0009.
-- The persona record (0004_personas) shipped the owner-controlled discoverability flag as `published`,
-- but SPEC-0009 REQ "Discoverability Is Owner-Controlled" and design.md call it `discoverable`. Rename
-- the column so schema and spec agree; the semantics are unchanged (owner explicitly marks a persona
-- discoverable before it appears in any bounded directory). SPEC-0009 REQ "Discoverability Is
-- Owner-Controlled".
ALTER TABLE personas RENAME COLUMN published TO discoverable;
