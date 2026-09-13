-- Session provenance (SPEC-0021 REQ "Session Parity and Provenance", ADR-0026):
-- each session records the trusted issuer and the provider-side subject that
-- established it, so provenance is auditable and the issuer gate (passkey-only
-- consent actions) reads the live session rather than re-deriving it. Nullable
-- + COALESCE'd: sessions minted before this migration carry no provenance, and
-- rewriting them would be a lie about who established them.
ALTER TABLE sessions ADD COLUMN issuer      text;
ALTER TABLE sessions ADD COLUMN provider_sub text;
