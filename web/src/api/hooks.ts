import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { api } from "./client";
import type {
  Checkpoint,
  CreateSandboxRequest,
  ExecResponse,
  ListDirResponse,
  NodeView,
  Sandbox,
  SandboxEvents,
  StorageReport,
  StorageReportAll,
  Template,
} from "./types";

/** Every polling cadence in one place. TanStack's default
    refetchIntervalInBackground=false already stops all of these while the
    tab is hidden. */
export const INTERVALS = {
  sandboxes: 5_000,
  sandboxTransit: 2_000, // a *ING state is about to change — watch closely
  sandboxSettled: 15_000, // STOPPED/RECYCLED/FAILED move rarely
  nodes: 10_000,
  templates: 10_000,
  storage: 10_000,
  events: 5_000,
} as const;

const TRANSIT = /ING$|^PENDING$/;
const SETTLED = new Set(["STOPPED", "RECYCLED", "FAILED"]);

export function useSandboxes() {
  return useQuery({
    queryKey: ["sandboxes"],
    queryFn: () => api<Sandbox[]>("GET", "/sandboxes"),
    refetchInterval: INTERVALS.sandboxes,
  });
}

export function useSandbox(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id],
    queryFn: () => api<Sandbox>("GET", `/sandboxes/${id}`),
    refetchInterval: (q) => {
      const st = q.state.data?.state ?? "";
      if (TRANSIT.test(st)) return INTERVALS.sandboxTransit;
      if (SETTLED.has(st)) return INTERVALS.sandboxSettled;
      return INTERVALS.sandboxes;
    },
  });
}

export function useNodes() {
  return useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<NodeView[]>("GET", "/nodes"),
    refetchInterval: INTERVALS.nodes,
  });
}

export function useTemplates() {
  return useQuery({
    queryKey: ["templates"],
    queryFn: () => api<Template[]>("GET", "/templates"),
    refetchInterval: INTERVALS.templates,
  });
}

export function useCheckpoints(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id, "checkpoints"],
    queryFn: () => api<Checkpoint[]>("GET", `/sandboxes/${id}/checkpoints`),
  });
}

export function useSandboxEvents(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id, "events"],
    queryFn: () => api<SandboxEvents>("GET", `/sandboxes/${id}/events`),
    refetchInterval: INTERVALS.events,
  });
}

/** Owner-wide activity feed for the Overview page. */
export function useFleetEvents(limit = 30) {
  return useQuery({
    queryKey: ["events", limit],
    queryFn: () => api<SandboxEvents>("GET", `/events?limit=${limit}`),
    refetchInterval: INTERVALS.events,
  });
}

export function useStorage(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id, "storage"],
    queryFn: () => api<StorageReport>("GET", `/sandboxes/${id}/storage`),
  });
}

export function useStorageReport() {
  return useQuery({
    queryKey: ["storage-report"],
    queryFn: () => api<StorageReportAll>("GET", "/storage-report"),
    refetchInterval: INTERVALS.storage,
  });
}

/** Guest directory listing for the file browser. Keyed by path; manual
    refresh via invalidation, mild staleTime so tree re-expansion is free. */
export function useDirectory(id: string, path: string, enabled = true) {
  return useQuery({
    queryKey: ["sandboxes", id, "dir", path],
    queryFn: () =>
      api<ListDirResponse>(
        "GET",
        `/sandboxes/${id}/files?op=list&path=${encodeURIComponent(path)}`,
      ),
    staleTime: 10_000,
    enabled,
  });
}

/** A mutation on a sandbox that invalidates fleet queries when done.
    `optimistic` patches the sandbox row (detail + list) immediately —
    pause shows PAUSING before the server answers — and rolls back on error. */
export function useSandboxAction<TArgs = void, TOut = Sandbox>(
  fn: (args: TArgs) => Promise<TOut>,
  opts?: {
    sandboxId?: string;
    optimistic?: (sb: Sandbox) => Partial<Sandbox>;
    onSuccess?: (out: TOut) => void;
    onError?: (err: Error) => void;
  },
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onMutate: () => {
      const id = opts?.sandboxId;
      if (!id || !opts?.optimistic) return {};
      const prevOne = qc.getQueryData<Sandbox>(["sandboxes", id]);
      const prevList = qc.getQueryData<Sandbox[]>(["sandboxes"]);
      const patch = (sb: Sandbox) => ({ ...sb, ...opts.optimistic!(sb) });
      if (prevOne) qc.setQueryData(["sandboxes", id], patch(prevOne));
      if (prevList)
        qc.setQueryData(
          ["sandboxes"],
          prevList.map((sb) => (sb.id === id ? patch(sb) : sb)),
        );
      return { prevOne, prevList };
    },
    onError: (err, _args, ctx) => {
      const id = opts?.sandboxId;
      if (id && ctx?.prevOne) qc.setQueryData(["sandboxes", id], ctx.prevOne);
      if (ctx?.prevList) qc.setQueryData(["sandboxes"], ctx.prevList);
      opts?.onError?.(err as Error);
    },
    onSuccess: (out) => opts?.onSuccess?.(out),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      void qc.invalidateQueries({ queryKey: ["nodes"] });
    },
  });
}

export interface ExecOptions {
  env?: string[];
  cwd?: string;
  timeout_s?: number;
  checkpoint?: boolean;
}

/** ExecResponse plus the checkpoint tag when exec ran with checkpoint:true
    (the M5 time-travel step). */
export type ExecResult = ExecResponse & { checkpoint?: string };

export const verbs = {
  pause: (id: string) => api<Sandbox>("POST", `/sandboxes/${id}/pause`),
  resume: (id: string) => api<Sandbox>("POST", `/sandboxes/${id}/resume`),
  kill: (id: string) => api<void>("DELETE", `/sandboxes/${id}`),
  snapshot: (id: string, tag: string) =>
    api<{ snapshot_id: string }>("POST", `/sandboxes/${id}/snapshot`, { tag }),
  resize: (id: string, body: { memory_mib?: number; vcpus?: number }) =>
    api<Sandbox>("POST", `/sandboxes/${id}/resize`, body),
  migrate: (id: string, nodeID?: string) =>
    api<Sandbox>(
      "POST",
      `/sandboxes/${id}/migrate`,
      nodeID ? { node_id: nodeID } : {},
    ),
  checkpoint: (id: string, tag?: string) =>
    api<Checkpoint>(
      "POST",
      `/sandboxes/${id}/checkpoints`,
      tag ? { tag } : {},
    ),
  fork: (id: string, checkpoint?: string) =>
    api<Sandbox>(
      "POST",
      `/sandboxes/${id}/fork`,
      checkpoint ? { checkpoint } : {},
    ),
  rollback: (id: string, checkpoint: string) =>
    api<Sandbox>("POST", `/sandboxes/${id}/rollback`, { checkpoint }),
  exec: (id: string, cmd: string, args: string[], opts?: ExecOptions) =>
    api<ExecResult>("POST", `/sandboxes/${id}/exec`, {
      cmd,
      args,
      ...opts,
    }),
  restoreArtifacts: (id: string) =>
    api<{ sandbox: Sandbox; restored_from: string }>(
      "POST",
      `/sandboxes/${id}/restore-artifacts`,
    ),
  createSandbox: (body: CreateSandboxRequest) =>
    api<Sandbox>("POST", "/sandboxes", body),
  createTemplate: (name: string, image: string) =>
    api<Template>("POST", "/templates", { name, image }),
  deleteTemplate: (id: string) => api<void>("DELETE", `/templates/${id}`),
};
