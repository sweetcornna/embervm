-- EmberVM M1 control-plane schema (master-spec D6). PostgreSQL is the single
-- source of truth for templates and sandboxes; sandbox_events is the audit
-- trail of lifecycle transitions.

CREATE TABLE IF NOT EXISTS templates (
    id         uuid PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    image      text NOT NULL,
    state      text NOT NULL,             -- BUILDING | READY | ERROR
    error      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    ready_at   timestamptz
);

CREATE TABLE IF NOT EXISTS sandboxes (
    id            uuid PRIMARY KEY,
    template_id   uuid NOT NULL REFERENCES templates (id),
    state         text NOT NULL,          -- pkg/lifecycle state name
    vcpus         int  NOT NULL,
    memory_mib    int  NOT NULL,
    data_disk_gib int  NOT NULL,
    netns         text NOT NULL DEFAULT '',
    owner         text NOT NULL DEFAULT '',
    error         text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    paused_at     timestamptz
);

CREATE INDEX IF NOT EXISTS sandboxes_owner_state_idx ON sandboxes (owner, state);

CREATE TABLE IF NOT EXISTS sandbox_events (
    id         bigserial PRIMARY KEY,
    sandbox_id uuid NOT NULL,
    from_state text NOT NULL DEFAULT '',
    to_state   text NOT NULL,
    at         timestamptz NOT NULL DEFAULT now(),
    detail     jsonb
);

CREATE INDEX IF NOT EXISTS sandbox_events_sandbox_idx ON sandbox_events (sandbox_id, at);
