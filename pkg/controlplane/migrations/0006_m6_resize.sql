-- M6 runtime resize (ADR-0007): immutable per-sandbox resize bounds.
-- memory_mib/vcpus become the CURRENT effective values (moved by the resize
-- verb; NodeUsage keeps summing them); 0 ceilings mean fixed geometry.
-- base_* record the create-time floors (the boot geometry); autoscale opts
-- the sandbox into the engine's pressure-driven resize loop.
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS max_memory_mib int NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS max_vcpus int NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS base_memory_mib int NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS base_vcpus int NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS autoscale boolean NOT NULL DEFAULT false;
