// Per-sandbox overview: live pressure gauges (fed by the 2.5s health poll's
// rolling window), the resize panel, identity metadata, storage footprint,
// and a one-shot exec disclosure for quick non-interactive commands.

import { useMutation } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { decodeBytes, fmtAge, fmtBytes, fmtKiB, fmtMiB, fmtPct } from "../../api/client";
import { useSandboxAction, useStorage, verbs } from "../../api/hooks";
import type { ExecResult } from "../../api/hooks";
import type { Sandbox } from "../../api/types";
import { Sparkline } from "../../components/charts";
import { MemGauge } from "../../components/status";
import { Button, Card, Empty, ErrorNote, Field, Mono, inputCls } from "../../components/ui";
import type { HealthSample } from "../../lib/health";
import { useSandboxHealth } from "../../lib/health";
import { toast, toastError } from "../../lib/toast";

export function OverviewTab(props: { sb: Sandbox }) {
  const { sb } = props;
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <Gauges sb={sb} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Resources">
          <ResizePanel sb={sb} />
        </Card>
        <Card title="About">
          <MetaGrid sb={sb} />
        </Card>
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title="Storage">
          <StorageCard id={sb.id} />
        </Card>
        <Card title="One-shot exec">
          <ExecPanel sb={sb} />
        </Card>
      </div>
    </div>
  );
}

/* ── Live gauges ─────────────────────────────────────────────────────── */

function series(
  samples: HealthSample[],
  pick: (s: HealthSample) => number | undefined,
) {
  const out: { at: number; value: number }[] = [];
  for (const s of samples) {
    if (!s.health.ok) continue;
    const v = pick(s);
    if (v !== undefined) out.push({ at: s.at, value: v });
  }
  return out;
}

function Gauges(props: { sb: Sandbox }) {
  const { sb } = props;
  const { samples, latest, unreachable } = useSandboxHealth(sb.id);

  const mem = useMemo(
    () =>
      series(samples, (s) =>
        s.health.mem_total_kib
          ? 1 - (s.health.mem_available_kib ?? 0) / s.health.mem_total_kib
          : undefined,
      ),
    [samples],
  );
  const psiMem = useMemo(() => series(samples, (s) => s.health.psi_mem_some10 ?? 0), [samples]);
  const psiCPU = useMemo(() => series(samples, (s) => s.health.psi_cpu_some10 ?? 0), [samples]);

  if (sb.state !== "RUNNING")
    return (
      <Card title="Live guest telemetry">
        <Empty>Gauges resume with the sandbox — the guest is not running.</Empty>
      </Card>
    );

  const h = latest?.health;
  const psiMax = Math.max(10, ...psiMem.map((p) => p.value), ...psiCPU.map((p) => p.value));
  return (
    <div className="grid gap-4 md:grid-cols-3">
      <GaugeCard
        title="Memory used"
        value={h?.mem_total_kib ? fmtPct(1 - (h.mem_available_kib ?? 0) / h.mem_total_kib) : "—"}
        sub={
          h?.mem_total_kib
            ? `${fmtKiB((h.mem_total_kib ?? 0) - (h.mem_available_kib ?? 0))} of ${fmtKiB(h.mem_total_kib)}`
            : unreachable
              ? "guest unreachable"
              : "waiting for first sample"
        }
      >
        <Sparkline points={mem} label="memory used" format={fmtPct} yMin={0} yMax={1} />
      </GaugeCard>
      <GaugeCard
        title="Memory pressure"
        value={h ? (h.psi_mem_some10 ?? 0).toFixed(1) : "—"}
        sub="PSI some avg10 — what autoscale watches"
      >
        <Sparkline
          points={psiMem}
          label="memory pressure"
          format={(v) => v.toFixed(1)}
          yMin={0}
          yMax={psiMax}
          trendWords={["easing", "steady", "climbing"]}
        />
      </GaugeCard>
      <GaugeCard
        title="CPU pressure"
        value={h ? (h.psi_cpu_some10 ?? 0).toFixed(1) : "—"}
        sub={h?.resumes !== undefined ? `${h.resumes} resumes · seq ${h.seq}` : undefined}
      >
        <Sparkline
          points={psiCPU}
          label="cpu pressure"
          format={(v) => v.toFixed(1)}
          yMin={0}
          yMax={psiMax}
          trendWords={["easing", "steady", "climbing"]}
        />
      </GaugeCard>
    </div>
  );
}

function GaugeCard(props: {
  title: string;
  value: string;
  sub?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-[var(--radius)] border border-border bg-surface p-4">
      <div className="flex items-baseline justify-between">
        <div className="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
          {props.title}
        </div>
        <div className="font-mono text-lg font-semibold tabular-nums text-ink">{props.value}</div>
      </div>
      <div className="mt-2">{props.children}</div>
      {props.sub && <div className="mt-1.5 text-[11px] text-faint">{props.sub}</div>}
    </div>
  );
}

/* ── Resize (ported from the old detail page, optimistic + toast) ────── */

function ResizePanel(props: { sb: Sandbox }) {
  const { sb } = props;
  const base = sb.base_memory_mib || sb.memory_mib;
  const maxMem = sb.max_memory_mib ?? 0;
  const maxCPU = sb.max_vcpus ?? 0;
  const resizable = maxMem > base || maxCPU > (sb.base_vcpus || sb.vcpus);
  const [mem, setMem] = useState(sb.memory_mib);
  const [cpu, setCPU] = useState(sb.vcpus);
  const resize = useSandboxAction(
    () =>
      verbs.resize(sb.id, {
        memory_mib: mem !== sb.memory_mib ? mem : undefined,
        vcpus: cpu !== sb.vcpus ? cpu : undefined,
      }),
    {
      sandboxId: sb.id,
      optimistic: () => ({ memory_mib: mem, vcpus: cpu }),
      onSuccess: (out) => toast.success("Resized", `${fmtMiB(out.memory_mib)} · ${out.vcpus} vCPU`),
      onError: toastError("Resize failed"),
    },
  );

  if (!resizable)
    return (
      <p className="text-[13px] text-faint">
        Fixed geometry — create with <Mono>max_memory_mib</Mono> / <Mono>max_vcpus</Mono> to enable
        runtime resize.
      </p>
    );

  const dirty = mem !== sb.memory_mib || cpu !== sb.vcpus;
  return (
    <div className="space-y-4">
      <MemGauge
        wide
        state={sb.state}
        memoryMiB={sb.memory_mib}
        baseMiB={sb.base_memory_mib}
        maxMiB={sb.max_memory_mib}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        {maxMem > base && (
          <Field label={`Memory · ${fmtMiB(base)} – ${fmtMiB(maxMem)}`}>
            <div className="flex items-center gap-3">
              <input
                type="range"
                min={base}
                max={maxMem}
                step={128}
                value={mem}
                onChange={(e) => setMem(Number(e.target.value))}
                className="w-full accent-(--color-accent)"
              />
              <Mono className="w-20 text-right tabular-nums">{fmtMiB(mem)}</Mono>
            </div>
          </Field>
        )}
        {maxCPU > 0 && (
          <Field label={`vCPUs · 1 – ${maxCPU}`}>
            <input
              className={inputCls}
              type="number"
              min={1}
              max={maxCPU}
              value={cpu}
              onChange={(e) => setCPU(Number(e.target.value))}
            />
          </Field>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <Button
          kind="primary"
          onClick={() => resize.mutate()}
          busy={resize.isPending}
          disabled={!dirty || sb.state !== "RUNNING"}
        >
          Apply resize
        </Button>
        {sb.autoscale && (
          <span className="font-mono text-xs text-transit">
            autoscale on — the engine also moves these on guest pressure
          </span>
        )}
        {sb.state !== "RUNNING" && <span className="text-xs text-faint">resize needs RUNNING</span>}
      </div>
      <ErrorNote error={resize.error} />
    </div>
  );
}

/* ── Metadata ────────────────────────────────────────────────────────── */

function MetaGrid(props: { sb: Sandbox }) {
  const { sb } = props;
  const rows: Array<[string, React.ReactNode]> = [
    ["node", sb.node_id || "—"],
    ["template", sb.template_id.slice(0, 8)],
    ["disk", `${sb.data_disk_gib} GiB`],
    ["vcpus", `${sb.vcpus}${sb.max_vcpus ? ` / ${sb.max_vcpus}` : ""}`],
    ["age", `${fmtAge(sb.created_at)}`],
    ["updated", `${fmtAge(sb.updated_at)} ago`],
  ];
  if (sb.paused_at) rows.push(["paused", `${fmtAge(sb.paused_at)} ago`]);
  if (sb.autoscale) rows.push(["autoscale", "on"]);
  return (
    <div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5 sm:grid-cols-3">
        {rows.map(([k, v]) => (
          <div key={k}>
            <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
            <dd className="mt-0.5 font-mono text-[13px] text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      {sb.forked_from && (
        <p className="mt-3 text-[12px] text-muted">
          forked from{" "}
          <Link to={`/sandboxes/${sb.parent_id ?? ""}`} className="font-mono text-accent hover:underline">
            {sb.forked_from}
          </Link>
        </p>
      )}
      {sb.error && <ErrorNote error={new Error(sb.error)} />}
    </div>
  );
}

/* ── Storage (ported) ────────────────────────────────────────────────── */

function StorageCard(props: { id: string }) {
  const { data } = useStorage(props.id);
  if (!data) return <Empty>Loading…</Empty>;
  const rows: Array<[string, string]> = [
    ["tier", data.tier],
    ["logical", fmtBytes(data.logical_bytes)],
    ["stored", fmtBytes(data.stored_bytes)],
    ["stored / logical", data.logical_bytes > 0 ? `${(data.stored_ratio * 100).toFixed(1)}%` : "—"],
    ["chunks", String(data.chunk_count)],
    ["layers", String(data.layers)],
  ];
  return (
    <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5 sm:grid-cols-3">
      {rows.map(([k, v]) => (
        <div key={k}>
          <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
          <dd className="mt-0.5 font-mono text-[13px] text-ink">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

/* ── One-shot exec (buffered; the Terminal tab is the interactive path) ─ */

function ExecPanel(props: { sb: Sandbox }) {
  const { sb } = props;
  const [cmdline, setCmdline] = useState("");
  const [result, setResult] = useState<ExecResult | null>(null);
  const exec = useMutation({
    mutationFn: () => {
      const [cmd, ...args] = cmdline.trim().split(/\s+/);
      return verbs.exec(sb.id, cmd, args);
    },
    onSuccess: setResult,
  });
  return (
    <div className="space-y-3">
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (cmdline.trim()) exec.mutate();
        }}
      >
        <input
          className={`${inputCls} font-mono`}
          value={cmdline}
          onChange={(e) => setCmdline(e.target.value)}
          placeholder="uname -a"
          aria-label="Command"
        />
        <Button kind="primary" type="submit" busy={exec.isPending} disabled={sb.state !== "RUNNING"}>
          Run
        </Button>
      </form>
      <p className="text-[11px] text-faint">
        Buffered request/response — for a live shell use the Terminal tab.
      </p>
      <ErrorNote error={exec.error} />
      {result && (
        <div className="rounded-md border border-hairline bg-bg p-3">
          <div className="mb-2 font-mono text-[11px] text-muted tabular-nums">
            exit{" "}
            <span className={result.exit_code === 0 ? "text-ok" : "text-danger"}>
              {result.exit_code}
            </span>
            {" · "}
            {result.duration_ms}ms
            {result.timed_out && <span className="text-danger"> · timed out</span>}
            {result.truncated && <span className="text-transit"> · truncated</span>}
          </div>
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap font-mono text-xs text-ink">
            {decodeBytes(result.stdout) || <span className="text-faint">(no stdout)</span>}
          </pre>
          {result.stderr && (
            <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap font-mono text-xs text-[#f3a6a2]">
              {decodeBytes(result.stderr)}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}
