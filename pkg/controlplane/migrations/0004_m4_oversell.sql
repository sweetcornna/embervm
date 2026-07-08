-- M4 oversell: nodes report physical cores; placement budgets vCPUs at
-- cores × CPUOvercommit (docs/zh/03 M4: CPU 3x 起步). 0 = unknown/unlimited.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cpu_cores int NOT NULL DEFAULT 0;
