-- M3 tiered archive & lifecycle (master-spec 2026-07-08-m3, ADR-0004):
-- artifact_paths drive the RECYCLED extraction; prewarmed_at records the
-- last pre-warm pull so the engine does not repeat it.

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS artifact_paths text[] NOT NULL DEFAULT '{}';
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS prewarmed_at timestamptz;

CREATE INDEX IF NOT EXISTS sandboxes_state_paused_at_idx ON sandboxes (state, paused_at);
