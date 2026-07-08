-- M4 multi-node scheduling (master-spec 2026-07-08-m4, ADR-0005): a static
-- node registry with polled liveness, and per-sandbox placement.

CREATE TABLE IF NOT EXISTS nodes (
    id           text PRIMARY KEY,
    addr         text NOT NULL DEFAULT '',   -- nodeapi unix socket / address ('' = in-proc)
    state        text NOT NULL DEFAULT 'up', -- up | down
    capacity_mib int  NOT NULL DEFAULT 0,
    last_seen    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS node_id text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandboxes_node_state_idx ON sandboxes (node_id, state);
