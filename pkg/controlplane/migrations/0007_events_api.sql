-- Console timeline (GET /v0/sandboxes/:id/events) reads events newest-first
-- by id with an exclusive id cursor; make that scan index-native.
CREATE INDEX IF NOT EXISTS sandbox_events_sandbox_id_idx
    ON sandbox_events (sandbox_id, id DESC);
