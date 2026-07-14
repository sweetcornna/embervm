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
  // M7 oversell view — absent on pre-M7 servers; every consumer must
  // degrade to the plain used/capacity bars when these are undefined.
  base_mib?: number;
  base_vcpus?: number;
  ceiling_mib?: number;
  ceiling_vcpus?: number;
  mem_budget_mib?: number; // capacity × MemOvercommit; 0/absent = unlimited
  vcpu_budget?: number; // cores × CPUOvercommit; 0/absent = unconstrained
}

// sandbox_events.detail payload for M7 resource events (resize / migrate /
// autoscale_config). Mirrors controlplane.ResourceEventDetail; unknown
// kinds must render as a generic row.
export interface ResourceEventDetail {
  kind: "resize" | "migrate" | "autoscale_config" | string;
  actor?: "user" | "autoscale" | string;
  reason?: "manual" | "pressure" | "deferred" | string;
  memory_mib?: [number, number]; // [old, new]
  vcpus?: [number, number];
  from_node?: string;
  to_node?: string;
  psi_mem?: number;
  avail_pct?: number;
  enabled?: boolean;
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

// GET /sandboxes/:id/health — live guest pressure. Non-RUNNING states answer
// ok:false with no guest numbers (that is a state, not an error).
export interface SandboxHealth {
  state: SandboxState;
  ok: boolean;
  seq?: number;
  pid?: number;
  version?: string;
  resumes?: number;
  mem_total_kib?: number;
  mem_available_kib?: number;
  psi_mem_some10?: number;
  psi_cpu_some10?: number;
}

// GET /sandboxes/:id/files?op=list — guest directory listing.
export interface DirEntry {
  name: string;
  size: number;
  mode: string; // fs.FileMode.String(), e.g. "drwxr-xr-x"
  mtime: string;
  is_dir: boolean;
  symlink?: string;
}

export interface ListDirResponse {
  path: string;
  entries: DirEntry[] | null;
  truncated?: boolean;
}

// GET /sandboxes/:id/events — lifecycle timeline (newest first, id-cursored).
export interface SandboxEvent {
  id: number;
  sandbox_id: string;
  from_state?: SandboxState | "";
  to_state: SandboxState;
  at: string;
  detail?: Record<string, unknown>;
}

export interface SandboxEvents {
  events: SandboxEvent[] | null;
  next_before?: number;
}

// Interactive terminal wire protocol (GET /sandboxes/:id/term, WebSocket).
// Binary frames = raw PTY bytes; text frames = JSON TermControl.
export const TERM_SUBPROTOCOL = "embervm-term.v1";

export interface TermControl {
  type: "resize" | "exit";
  cols?: number;
  rows?: number;
  code?: number;
}
