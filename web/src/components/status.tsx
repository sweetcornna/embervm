// Sandbox lifecycle as flat, professional status. Each state maps to a
// semantic color and a short label; the lifecycle is genuinely a temperature
// ramp (hot pool → warm write-through → cold archive → ash), so the hues
// follow it, but they render as standard operator badges — a dot plus a
// tinted pill — not decoration.

import type { SandboxState } from "../api/types";
import { fmtMiB } from "../api/client";
import { useI18n } from "../lib/i18n";
import type { TFn } from "../lib/i18n";

type Meta = { color: string; label: string; zh: string };

export const STATE_META: Record<SandboxState, Meta> = {
  RUNNING: { color: "var(--color-ok)", label: "Running", zh: "运行中" },
  PAUSED_HOT: { color: "var(--color-hot)", label: "Paused · hot", zh: "已暂停 · 热" },
  PAUSED_WARM: { color: "var(--color-warm)", label: "Paused · warm", zh: "已暂停 · 温" },
  ARCHIVED_COLD: { color: "var(--color-cold)", label: "Archived · cold", zh: "已归档 · 冷" },
  STOPPED: { color: "var(--color-idle)", label: "Stopped", zh: "已停止" },
  RECYCLED: { color: "var(--color-idle)", label: "Recycled", zh: "已回收" },
  FAILED: { color: "var(--color-danger)", label: "Failed", zh: "失败" },
  PENDING: { color: "var(--color-transit)", label: "Pending", zh: "等待中" },
  STARTING: { color: "var(--color-transit)", label: "Starting", zh: "启动中" },
  RESUMING: { color: "var(--color-transit)", label: "Resuming", zh: "恢复中" },
  PAUSING: { color: "var(--color-transit)", label: "Pausing", zh: "暂停中" },
  STOPPING: { color: "var(--color-transit)", label: "Stopping", zh: "停止中" },
};

function meta(state: SandboxState): Meta {
  return STATE_META[state] ?? { color: "var(--color-idle)", label: state.toLowerCase(), zh: state };
}

/** Translate a sandbox state's display label with a bound t(). */
export function stateLabel(state: SandboxState, t: TFn): string {
  const m = meta(state);
  return t(m.label, m.zh);
}

/** Hook variant for components rendering state labels directly. */
export function useStateLabel(): (state: SandboxState) => string {
  const { t } = useI18n();
  return (state: SandboxState) => stateLabel(state, t);
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
  const { t } = useI18n();
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
      {t(m.label, m.zh)}
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

/** Effective vCPUs inside the [base, max] track — MemGauge's CPU sibling
    (M7). Fixed geometry renders the plain value. */
export function CpuGauge(props: {
  state: SandboxState;
  vcpus: number;
  baseVCPUs?: number;
  maxVCPUs?: number;
  wide?: boolean;
}) {
  const max = props.maxVCPUs && props.maxVCPUs > 0 ? props.maxVCPUs : props.vcpus;
  const base = props.baseVCPUs && props.baseVCPUs > 0 ? props.baseVCPUs : props.vcpus;
  const resizable = max > base;
  const pct = Math.max(3, Math.min(100, (props.vcpus / max) * 100));
  const basePct = Math.max(0, Math.min(100, (base / max) * 100));
  const c = meta(props.state).color;
  return (
    <div className={props.wide ? "w-full" : "w-40"}>
      <div
        className="relative h-1.5 overflow-hidden rounded-full bg-overlay"
        role="meter"
        aria-label="effective vcpus"
        aria-valuenow={props.vcpus}
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
            title={`base ${base} vCPU`}
          />
        )}
      </div>
      <div className="mt-1 flex justify-between font-mono text-[11px] text-muted tabular-nums">
        <span className="text-ink">{props.vcpus} vCPU</span>
        {resizable ? <span>/ {max}</span> : <span className="text-faint">fixed</span>}
      </div>
    </div>
  );
}

/** Autoscale state pill (M7): on (engine in control), off-but-resizable
    (manual), or deferred (engine wants to grow, node is full). Fixed
    geometry should not render this at all. */
export function AutoscaleBadge(props: { on: boolean; deferred?: boolean }) {
  const { t } = useI18n();
  const c = props.deferred
    ? "var(--color-warm)"
    : props.on
      ? "var(--color-transit)"
      : "var(--color-idle)";
  const label = props.deferred
    ? t("autoscale · deferred", "自动伸缩 · 已推迟")
    : props.on
      ? t("autoscale", "自动伸缩")
      : t("manual", "手动");
  return (
    <span
      className="inline-flex items-center gap-1.5 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium"
      style={{ color: c, borderColor: color(c, "35%"), background: color(c, "12%") }}
      title={
        props.on
          ? "pressure-driven resize between base and ceiling"
          : "resize via the workspace panel"
      }
    >
      <span aria-hidden className="inline-block h-1.5 w-1.5 shrink-0 rounded-full" style={{ background: c }} />
      {label}
    </span>
  );
}
