// Tiny toast store: module-level queue + useSyncExternalStore, so any code
// (mutations, WS handlers) can raise a toast without prop drilling. The
// viewport component lives in components/toast.tsx.

import { useSyncExternalStore } from "react";

export interface Toast {
  id: number;
  kind: "success" | "error" | "info";
  title: string;
  detail?: string;
  action?: { label: string; onClick: () => void };
}

const MAX_VISIBLE = 3;
const TTL_MS = 5_000;
const ERROR_TTL_MS = 8_000;

let queue: Toast[] = [];
let nextID = 1;
const listeners = new Set<() => void>();

function publish() {
  for (const l of listeners) l();
}

function push(t: Omit<Toast, "id">) {
  const toast = { ...t, id: nextID++ };
  queue = [...queue.slice(-(MAX_VISIBLE - 1)), toast];
  publish();
  const ttl = t.kind === "error" ? ERROR_TTL_MS : TTL_MS;
  setTimeout(() => dismiss(toast.id), ttl);
  return toast.id;
}

export function dismiss(id: number) {
  const before = queue.length;
  queue = queue.filter((t) => t.id !== id);
  if (queue.length !== before) publish();
}

export const toast = {
  success: (title: string, detail?: string) =>
    push({ kind: "success", title, detail }),
  error: (title: string, detail?: string) =>
    push({ kind: "error", title, detail }),
  info: (title: string, detail?: string) => push({ kind: "info", title, detail }),
  action: (
    kind: Toast["kind"],
    title: string,
    action: { label: string; onClick: () => void },
  ) => push({ kind, title, action }),
};

/** Convenience for mutation onError callbacks. */
export function toastError(prefix: string) {
  return (err: Error) => {
    toast.error(prefix, err.message);
  };
}

export function useToasts(): Toast[] {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => queue,
  );
}
