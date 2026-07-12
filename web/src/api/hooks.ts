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
  NodeView,
  Sandbox,
  StorageReport,
  Template,
} from "./types";

export function useSandboxes() {
  return useQuery({
    queryKey: ["sandboxes"],
    queryFn: () => api<Sandbox[]>("GET", "/sandboxes"),
    refetchInterval: 5000,
  });
}

export function useSandbox(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id],
    queryFn: () => api<Sandbox>("GET", `/sandboxes/${id}`),
    refetchInterval: 5000,
  });
}

export function useNodes() {
  return useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<NodeView[]>("GET", "/nodes"),
    refetchInterval: 10000,
  });
}

export function useTemplates() {
  return useQuery({
    queryKey: ["templates"],
    queryFn: () => api<Template[]>("GET", "/templates"),
    refetchInterval: 10000,
  });
}

export function useCheckpoints(id: string) {
  return useQuery({
    queryKey: ["sandboxes", id, "checkpoints"],
    queryFn: () => api<Checkpoint[]>("GET", `/sandboxes/${id}/checkpoints`),
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
    queryFn: () => api<StorageReport[]>("GET", "/storage-report"),
  });
}

/** A mutation on a sandbox that invalidates fleet queries when done. */
export function useSandboxAction<TArgs = void, TOut = Sandbox>(
  fn: (args: TArgs) => Promise<TOut>,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      void qc.invalidateQueries({ queryKey: ["nodes"] });
    },
  });
}

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
  exec: (id: string, cmd: string, args: string[]) =>
    api<ExecResponse>("POST", `/sandboxes/${id}/exec`, { cmd, args }),
  createSandbox: (body: CreateSandboxRequest) =>
    api<Sandbox>("POST", "/sandboxes", body),
  createTemplate: (name: string, image: string) =>
    api<Template>("POST", "/templates", { name, image }),
  deleteTemplate: (id: string) => api<void>("DELETE", `/templates/${id}`),
};
