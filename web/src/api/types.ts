// Wire types mirror pkg/controlplane JSON exactly; the Go structs are the
// schema truth.

export type SandboxState =
  | "PENDING"
  | "STARTING"
  | "RUNNING"
  | "PAUSING"
  | "PAUSED_HOT"
  | "PAUSED_WARM"
  | "ARCHIVED_COLD"
  | "RESUMING"
  | "STOPPING"
  | "STOPPED"
  | "RECYCLED"
  | "FAILED";

export interface Sandbox {
  id: string;
  template_id: string;
  state: SandboxState;
  vcpus: number;
  memory_mib: number;
  data_disk_gib: number;
  netns?: string;
  owner?: string;
  error?: string;
  created_at: string;
  updated_at: string;
  paused_at?: string;
  node_id?: string;
  parent_id?: string;
  forked_from?: string;
  max_memory_mib?: number;
  max_vcpus?: number;
  base_memory_mib?: number;
  base_vcpus?: number;
  autoscale?: boolean;
}

export interface CreateSandboxRequest {
  template_id: string;
  vcpus?: number;
  memory_mib?: number;
  data_disk_gib?: number;
  max_memory_mib?: number;
  max_vcpus?: number;
  autoscale?: boolean;
  egress?: "nat" | "none";
}

export interface Template {
  id: string;
  name: string;
  image: string;
  state: "BUILDING" | "READY" | "ERROR";
  error?: string;
  created_at: string;
  ready_at?: string;
}

export interface NodeView {
  id: string;
  addr?: string;
  state: "up" | "down";
  capacity_mib: number;
  cpu_cores?: number;
  last_seen: string;
  used_mib: number;
  used_vcpus: number;
  active_sandboxes: number;
}

export interface Checkpoint {
  tag: string;
  layer: string;
  seq: number;
  created_at: string;
}

export interface ExecResponse {
  exit_code: number;
  stdout?: string; // base64 ([]byte over JSON)
  stderr?: string; // base64
  timed_out?: boolean;
  truncated?: boolean;
  duration_ms: number;
}

export interface StorageReport {
  sandbox_id: string;
  state: string;
  tier: "hot" | "warm" | "cold" | "recycled" | "none";
  logical_bytes: number;
  stored_bytes: number;
  chunk_count: number;
  stored_ratio: number;
  artifact_bytes?: number;
  layers: number;
}

// The aggregate report is an object with pre-summed totals, NOT a bare
// array — the server does the reduction (storageReportAll).
export interface StorageReportAll {
  sandboxes: StorageReport[];
  total_logical_bytes: number;
  total_stored_bytes: number;
  total_artifact_bytes: number;
  total_chunks: number;
}
