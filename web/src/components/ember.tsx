// The console's signature: sandbox lifecycle rendered as temperature. Every
// state maps onto a thermal ramp (the product's own mental model — hot pool,
// warm write-through, cold archive, ash), and RUNNING embers breathe.

import type { SandboxState } from "../api/types";
import { fmtMiB } from "../api/client";

export const STATE_HEAT: Record<SandboxState, { color: string; label: string }> = {
  RUNNING: { color: "var(--color-ember)", label: "running" },
  PAUSED_HOT: { color: "var(--color-heat)", label: "paused · hot" },
  PAUSED_WARM: { color: "var(--color-rust)", label: "paused · warm" },
  ARCHIVED_COLD: { color: "var(--color-cold)", label: "archived · cold" },
  STOPPED: { color: "var(--color-ash)", label: "stopped" },
  RECYCLED: { color: "var(--color-ash)", label: "recycled" },
  FAILED: { color: "var(--color-alarm)", label: "failed" },
  PENDING: { color: "var(--color-transit)", label: "pending" },
  STARTING: { color: "var(--color-transit)", label: "starting" },
  RESUMING: { color: "var(--color-transit)", label: "resuming" },
  PAUSING: { color: "var(--color-transit)", label: "pausing" },
  STOPPING: { color: "var(--color-transit)", label: "stopping" },
};

function heat(state: SandboxState) {
  return STATE_HEAT[state] ?? { color: "var(--color-ash)", label: state.toLowerCase() };
}

export function EmberDot(props: { state: SandboxState; size?: number }) {
  const h = heat(props.state);
  const s = props.size ?? 8;
  return (
    <span
      aria-hidden
      className={props.state === "RUNNING" ? "ember-live rounded-full" : "rounded-full"}
      style={{ width: s, height: s, background: h.color, display: "inline-block", flexShrink: 0 }}
    />
  );
}

export function StateMark(props: { state: SandboxState }) {
  const h = heat(props.state);
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <EmberDot state={props.state} />
      <span className="font-mono text-xs" style={{ color: h.color }}>
        {h.label}
      </span>
    </span>
  );
}

/** The ember gauge: effective memory as heat inside the [base, max] track.
    Fixed-geometry sandboxes render as a plain filled bar. */
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
  const pct = Math.max(2, Math.min(100, (props.memoryMiB / max) * 100));
  const basePct = Math.max(0, Math.min(100, (base / max) * 100));
  const color = heat(props.state).color;
  return (
    <div className={props.wide ? "w-full" : "w-36"}>
      <div
        className="relative h-1.5 overflow-hidden rounded-full bg-raised"
        role="meter"
        aria-label="effective memory"
        aria-valuenow={props.memoryMiB}
        aria-valuemin={base}
        aria-valuemax={max}
      >
        <div
          className="h-full rounded-full transition-[width] duration-500"
          style={{ width: `${pct}%`, background: color }}
        />
        {resizable && (
          <div
            aria-hidden
            className="absolute top-0 h-full w-px bg-ink/40"
            style={{ left: `${basePct}%` }}
            title={`base ${fmtMiB(base)}`}
          />
        )}
      </div>
      <div className="mt-0.5 flex justify-between font-mono text-[11px] text-muted">
        <span className="text-ink">{fmtMiB(props.memoryMiB)}</span>
        {resizable ? <span>/ {fmtMiB(max)}</span> : <span className="text-faint">fixed</span>}
      </div>
    </div>
  );
}
