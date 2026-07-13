// Sandbox lifecycle as flat, professional status. Each state maps to a
// semantic color and a short label; the lifecycle is genuinely a temperature
// ramp (hot pool → warm write-through → cold archive → ash), so the hues
// follow it, but they render as standard operator badges — a dot plus a
// tinted pill — not decoration.

import type { SandboxState } from "../api/types";
import { fmtMiB } from "../api/client";

type Meta = { color: string; label: string };

export const STATE_META: Record<SandboxState, Meta> = {
  RUNNING: { color: "var(--color-ok)", label: "Running" },
  PAUSED_HOT: { color: "var(--color-hot)", label: "Paused · hot" },
  PAUSED_WARM: { color: "var(--color-warm)", label: "Paused · warm" },
  ARCHIVED_COLD: { color: "var(--color-cold)", label: "Archived · cold" },
  STOPPED: { color: "var(--color-idle)", label: "Stopped" },
  RECYCLED: { color: "var(--color-idle)", label: "Recycled" },
  FAILED: { color: "var(--color-danger)", label: "Failed" },
  PENDING: { color: "var(--color-transit)", label: "Pending" },
  STARTING: { color: "var(--color-transit)", label: "Starting" },
  RESUMING: { color: "var(--color-transit)", label: "Resuming" },
  PAUSING: { color: "var(--color-transit)", label: "Pausing" },
  STOPPING: { color: "var(--color-transit)", label: "Stopping" },
};

function meta(state: SandboxState): Meta {
  return STATE_META[state] ?? { color: "var(--color-idle)", label: state.toLowerCase() };
}

export function StatusDot(props: { state: SandboxState; size?: number }) {
  const s = props.size ?? 7;
  return (
    <span
      aria-hidden
      className="inline-block shrink-0 rounded-full"
      style={{ width: s, height: s, background: meta(props.state).color }}
    />
  );
}

export function StateBadge(props: { state: SandboxState }) {
  const m = meta(props.state);
  return (
    <span
      className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium"
      style={{
        color: m.color,
        borderColor: color(m.color, "35%"),
        background: color(m.color, "12%"),
      }}
    >
      <StatusDot state={props.state} size={6} />
      {m.label}
    </span>
  );
}

function color(c: string, pct: string) {
  return `color-mix(in srgb, ${c} ${pct}, transparent)`;
}

/** Effective memory as a fill inside the [base, max] track. Fixed-geometry
    sandboxes render as a plain bar with a "fixed" marker. */
export function MemGauge(props: {
  state: SandboxState;
  memoryMiB: number;
  baseMiB?: number;
  maxMiB?: number;
  wide?: boolean;
}) {
  const max = props.maxMiB && props.maxMiB > 0 ? props.maxMiB : props.memoryMiB;
  const base = props.baseMiB && props.baseMiB > 0 ? props.baseMiB : props.memoryMiB;
  const resizable = max > base;
  const pct = Math.max(3, Math.min(100, (props.memoryMiB / max) * 100));
  const basePct = Math.max(0, Math.min(100, (base / max) * 100));
  const c = meta(props.state).color;
  return (
    <div className={props.wide ? "w-full" : "w-40"}>
      <div
        className="relative h-1.5 overflow-hidden rounded-full bg-overlay"
        role="meter"
        aria-label="effective memory"
        aria-valuenow={props.memoryMiB}
        aria-valuemin={base}
        aria-valuemax={max}
      >
        <div
          className="h-full rounded-full transition-[width] duration-500"
          style={{ width: `${pct}%`, background: c }}
        />
        {resizable && (
          <div
            aria-hidden
            className="absolute top-[-1px] h-[calc(100%+2px)] w-px bg-ink/50"
            style={{ left: `${basePct}%` }}
            title={`base ${fmtMiB(base)}`}
          />
        )}
      </div>
      <div className="mt-1 flex justify-between font-mono text-[11px] text-muted tabular-nums">
        <span className="text-ink">{fmtMiB(props.memoryMiB)}</span>
        {resizable ? <span>/ {fmtMiB(max)}</span> : <span className="text-faint">fixed</span>}
      </div>
    </div>
  );
}
