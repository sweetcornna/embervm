-- M5 fork/branch/rollback (ADR-0006): checkpoints are named pause layers;
-- fork lineage lives on the sandbox row (the ZFS clone dependency made
-- queryable — destroy and rollback guards need it).

ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS parent_id uuid;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS forked_from text;

CREATE TABLE IF NOT EXISTS checkpoints (
    sandbox_id uuid NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    tag        text NOT NULL,
    layer      text NOT NULL, -- memory layer name "p<N>"
    seq        int  NOT NULL, -- N, for "newer than" comparisons
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sandbox_id, tag)
);

CREATE INDEX IF NOT EXISTS checkpoints_seq ON checkpoints (sandbox_id, seq);
CREATE INDEX IF NOT EXISTS sandboxes_parent ON sandboxes (parent_id) WHERE parent_id IS NOT NULL;
