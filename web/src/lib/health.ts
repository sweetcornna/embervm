// Per-sandbox guest-health poller, deliberately OUTSIDE react-query: the
// ring of samples IS the chart datasource, and react-query would discard
// history on every refetch. Components subscribe via useSyncExternalStore;
// polling runs while at least one subscriber exists and pauses itself when
// the tab is hidden. Entries are garbage-collected shortly after the last
// subscriber leaves so an abandoned workspace stops polling.

import { useCallback, useSyncExternalStore } from "react";
import { api } from "../api/client";
import type { SandboxHealth } from "../api/types";

export interface HealthSample {
  at: number; // Date.now()
  health: SandboxHealth;
}

/** Immutable snapshot handed to subscribers. */
export interface HealthSeries {
  samples: HealthSample[]; // oldest → newest
  latest?: HealthSample;
  /** Last poll failed while the sandbox row said RUNNING. */
  unreachable: boolean;
}

const POLL_MS = 2_500;
const CAPACITY = 240; // ~10 minutes of history
const GC_AFTER_MS = 60_000;

const EMPTY: HealthSeries = { samples: [], unreachable: false };

interface Entry {
  samples: HealthSample[];
  unreachable: boolean;
  snapshot: HealthSeries;
  listeners: Set<() => void>;
  timer: ReturnType<typeof setInterval> | null;
  gcTimer: ReturnType<typeof setTimeout> | null;
  inFlight: boolean;
}

const entries = new Map<string, Entry>();

function entryFor(id: string): Entry {
  let e = entries.get(id);
  if (!e) {
    e = {
      samples: [],
      unreachable: false,
      snapshot: EMPTY,
      listeners: new Set(),
      timer: null,
      gcTimer: null,
      inFlight: false,
    };
    entries.set(id, e);
  }
  return e;
}

function publish(e: Entry) {
  e.snapshot = {
    samples: e.samples,
    latest: e.samples[e.samples.length - 1],
    unreachable: e.unreachable,
  };
  for (const l of e.listeners) l();
}

async function poll(id: string, e: Entry) {
  if (e.inFlight || document.hidden) return;
  e.inFlight = true;
  try {
    const health = await api<SandboxHealth>("GET", `/sandboxes/${id}/health`);
    // ok:false (paused etc.) is state, not history — keep the ramp clean by
    // only charting live samples, but surface the state via `latest`.
    const next = e.samples.slice(Math.max(0, e.samples.length - CAPACITY + 1));
    next.push({ at: Date.now(), health });
    e.samples = next;
    e.unreachable = false;
  } catch {
    e.unreachable = true;
  } finally {
    e.inFlight = false;
    publish(e);
  }
}

function subscribe(id: string, listener: () => void): () => void {
  const e = entryFor(id);
  e.listeners.add(listener);
  if (e.gcTimer) {
    clearTimeout(e.gcTimer);
    e.gcTimer = null;
  }
  if (!e.timer) {
    void poll(id, e);
    e.timer = setInterval(() => void poll(id, e), POLL_MS);
  }
  return () => {
    e.listeners.delete(listener);
    if (e.listeners.size === 0) {
      if (e.timer) {
        clearInterval(e.timer);
        e.timer = null;
      }
      e.gcTimer = setTimeout(() => {
        if (e.listeners.size === 0) entries.delete(id);
      }, GC_AFTER_MS);
    }
  };
}

/** Live guest health + rolling history for one sandbox. Poll cadence and
    retention are module-level policy; a second subscriber shares the feed. */
export function useSandboxHealth(id: string): HealthSeries {
  const sub = useCallback(
    (listener: () => void) => subscribe(id, listener),
    [id],
  );
  const get = useCallback(() => entries.get(id)?.snapshot ?? EMPTY, [id]);
  return useSyncExternalStore(sub, get);
}

/** Drop all pollers (logout). */
export function resetHealthStore() {
  for (const e of entries.values()) {
    if (e.timer) clearInterval(e.timer);
    if (e.gcTimer) clearTimeout(e.gcTimer);
  }
  entries.clear();
}

window.addEventListener("embervm:unauthorized", resetHealthStore);
