// Per-sandbox overview: live pressure gauges (fed by the 2.5s health poll's
// rolling window), the resize panel, identity metadata, storage footprint,
// and a one-shot exec disclosure for quick non-interactive commands.

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ApiError, decodeBytes, fmtAge, fmtBytes, fmtKiB, fmtMiB, fmtPct } from "../../api/client";
import { useSandboxAction, useSandboxEvents, useStorage, verbs } from "../../api/hooks";
import type { ExecResult } from "../../api/hooks";
import type { Sandbox, SandboxState } from "../../api/types";
import { Sparkline } from "../../components/charts";
import { AutoscaleBadge, CpuGauge, MemGauge, STATE_META, stateLabel } from "../../components/status";
import { Button, Card, Empty, ErrorNote, Field, Mono, Toggle, inputCls } from "../../components/ui";
import { slotRoundingHint } from "../../lib/geometry";
import type { HealthSample } from "../../lib/health";
import { useSandboxHealth } from "../../lib/health";
import { useI18n } from "../../lib/i18n";
import { describeResourceEvent, parseResourceEvent } from "../../lib/resourceEvents";
import { toast, toastError } from "../../lib/toast";

export function OverviewTab(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  return (
    <div className="mx-auto max-w-6xl space-y-4 p-4">
      <Gauges sb={sb} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t("Resources", "资源")}>
          <ResourcesPanel sb={sb} />
        </Card>
        <div className="space-y-4">
          <Card title={t("About", "关于")}>
            <MetaGrid sb={sb} />
          </Card>
          <ScalingActivity sb={sb} />
        </div>
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card title={t("Storage", "存储")}>
          <StorageCard id={sb.id} />
        </Card>
        <Card title={t("One-shot exec", "一次性执行")}>
          <ExecPanel sb={sb} />
        </Card>
      </div>
      <RecentEvents sb={sb} />
    </div>
  );
}

/* ── Scaling activity (M7): resize / autoscale / migrate history ──────── */

const EVENT_ICONS: Record<string, string> = {
  grow: "↑",
  shrink: "↓",
  migrate: "⇄",
  config: "⚙",
  deferred: "⏳",
};

function ScalingActivity(props: { sb: Sandbox }) {
  const { data } = useSandboxEvents(props.sb.id);
  const { t } = useI18n();
  const nav = useNavigate();
  const rows = (data?.events ?? [])
    .map((ev) => ({ ev, d: parseResourceEvent(ev) }))
    .filter((r) => r.d !== null)
    .slice(0, 6);
  return (
    <Card title={t("Scaling activity", "伸缩记录")} pad={false}>
      {rows.length === 0 ? (
        <Empty>{t("No scaling activity recorded.", "暂无伸缩记录。")}</Empty>
      ) : (
        <ul className="divide-y divide-hairline/60">
          {rows.map(({ ev, d }) => {
            const view = describeResourceEvent(d!, t);
            return (
              <li key={ev.id} className="flex items-center gap-3 px-4 py-2">
                <span
                  aria-hidden
                  className="w-4 shrink-0 text-center font-mono text-[13px]"
                  style={{ color: view.tone === "warn" ? "var(--color-warm)" : "var(--color-ok)" }}
                >
                  {EVENT_ICONS[view.icon] ?? "·"}
                </span>
                <span className="min-w-0 flex-1 text-[13px] text-ink">
                  {view.text}
                  {view.actor && (
                    <span className="ml-2 rounded-full border border-hairline px-1.5 py-px font-mono text-[10px] text-faint">
                      {view.actor}
                    </span>
                  )}
                </span>
                {view.icon === "deferred" && (
                  <button
                    className="shrink-0 text-xs text-accent hover:underline"
                    onClick={() => nav(`/sandboxes/${props.sb.id}/settings`)}
                  >
                    {t("migrate →", "迁移 →")}
                  </button>
                )}
                <span className="shrink-0 font-mono text-[11px] text-faint">
                  {fmtAge(ev.at)} {t("ago", "前")}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </Card>
  );
}

/* ── Recent lifecycle events (full history on the Checkpoints tab) ────── */

function RecentEvents(props: { sb: Sandbox }) {
  const { data } = useSandboxEvents(props.sb.id);
  const { t } = useI18n();
  const events = (data?.events ?? []).slice(0, 5);
  return (
    <Card
      title={t("Recent events", "近期事件")}
      actions={
        <Link
          to={`/sandboxes/${props.sb.id}/checkpoints`}
          className="text-xs text-accent hover:underline"
        >
          {t("full timeline →", "完整时间线 →")}
        </Link>
      }
      pad={false}
    >
      {events.length === 0 ? (
        <Empty>{t("No transitions recorded yet.", "暂无状态转换记录。")}</Empty>
      ) : (
        <ul className="divide-y divide-hairline/60">
          {events.map((ev) => {
            const res = parseResourceEvent(ev);
            if (res) {
              const view = describeResourceEvent(res, t);
              return (
                <li key={ev.id} className="flex items-center gap-3 px-4 py-2">
                  <span
                    aria-hidden
                    className="w-2 shrink-0 text-center font-mono text-[12px]"
                    style={{ color: view.tone === "warn" ? "var(--color-warm)" : "var(--color-ok)" }}
                  >
                    {EVENT_ICONS[view.icon] ?? "·"}
                  </span>
                  <span className="min-w-0 flex-1 text-[13px] text-ink">
                    {view.text}
                    {view.actor && (
                      <span className="ml-2 font-mono text-[11px] text-faint">{view.actor}</span>
                    )}
                  </span>
                  <span className="shrink-0 font-mono text-[11px] text-faint">
                    {fmtAge(ev.at)} {t("ago", "前")}
                  </span>
                </li>
              );
            }
            const meta = STATE_META[ev.to_state as SandboxState];
            return (
              <li key={ev.id} className="flex items-center gap-3 px-4 py-2">
                <span
                  aria-hidden
                  className="size-2 shrink-0 rounded-full"
                  style={{ background: meta?.color ?? "var(--color-idle)" }}
                />
                <span className="min-w-0 flex-1 text-[13px] text-ink">
                  {stateLabel(ev.to_state as SandboxState, t)}
                  {ev.from_state && (
                    <span className="ml-2 font-mono text-[11px] text-faint">
                      {t("from", "来自")} {ev.from_state}
                    </span>
                  )}
                </span>
                <span className="shrink-0 font-mono text-[11px] text-faint">
                  {fmtAge(ev.at)} {t("ago", "前")}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </Card>
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
  const { t } = useI18n();
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
      <Card title={t("Live guest telemetry", "实时 guest 遥测")}>
        <Empty>{t("Gauges resume with the sandbox — the guest is not running.", "仪表随沙箱恢复 —— guest 未在运行。")}</Empty>
      </Card>
    );

  const h = latest?.health;
  const psiMax = Math.max(10, ...psiMem.map((p) => p.value), ...psiCPU.map((p) => p.value));
  return (
    <div className="grid gap-4 md:grid-cols-3">
      <GaugeCard
        title={t("Memory used", "内存占用")}
        value={h?.mem_total_kib ? fmtPct(1 - (h.mem_available_kib ?? 0) / h.mem_total_kib) : "—"}
        sub={
          h?.mem_total_kib
            ? `${fmtKiB((h.mem_total_kib ?? 0) - (h.mem_available_kib ?? 0))} ${t("of", "/")} ${fmtKiB(h.mem_total_kib)}`
            : unreachable
              ? t("guest unreachable", "guest 不可达")
              : t("waiting for first sample", "等待首个采样")
        }
      >
        <Sparkline points={mem} label={t("memory used", "内存占用")} format={fmtPct} yMin={0} yMax={1} />
      </GaugeCard>
      <GaugeCard
        title={t("Memory pressure", "内存压力")}
        value={h ? (h.psi_mem_some10 ?? 0).toFixed(1) : "—"}
        sub={t("PSI some avg10 — what autoscale watches", "PSI some avg10 —— 自动伸缩的观测指标")}
      >
        <Sparkline
          points={psiMem}
          label={t("memory pressure", "内存压力")}
          format={(v) => v.toFixed(1)}
          yMin={0}
          yMax={psiMax}
          trendWords={[t("easing", "回落"), t("steady", "平稳"), t("climbing", "攀升")]}
        />
      </GaugeCard>
      <GaugeCard
        title={t("CPU pressure", "CPU 压力")}
        value={h ? (h.psi_cpu_some10 ?? 0).toFixed(1) : "—"}
        sub={h?.resumes !== undefined ? `${h.resumes} ${t("resumes", "恢复次数")} · seq ${h.seq}` : undefined}
      >
        <Sparkline
          points={psiCPU}
          label={t("cpu pressure", "CPU 压力")}
          format={(v) => v.toFixed(1)}
          yMin={0}
          yMax={psiMax}
          trendWords={[t("easing", "回落"), t("steady", "平稳"), t("climbing", "攀升")]}
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

/* ── Resources (M7): geometry, history, resize, autoscale, capacity UX ── */

function ResourcesPanel(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const qc = useQueryClient();
  const base = sb.base_memory_mib || sb.memory_mib;
  const baseCPU = sb.base_vcpus || 1;
  const maxMem = sb.max_memory_mib ?? 0;
  const maxCPU = sb.max_vcpus ?? 0;
  const resizable = maxMem > base || maxCPU > baseCPU;
  const [mem, setMem] = useState(sb.memory_mib);
  const [cpu, setCPU] = useState(sb.vcpus);
  const [noCapacity, setNoCapacity] = useState(false);
  // Pre-M7 servers have no POST /autoscale — a 404/405 hides the toggle
  // and leaves the read-only badge.
  const [toggleSupported, setToggleSupported] = useState(true);
  const sliderRef = useRef<HTMLInputElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  // ⌘K "Resize this sandbox" lands here.
  useEffect(() => {
    const onFocus = () => {
      panelRef.current?.scrollIntoView({ behavior: "smooth", block: "center" });
      sliderRef.current?.focus();
    };
    window.addEventListener("embervm:focus-resize", onFocus);
    return () => window.removeEventListener("embervm:focus-resize", onFocus);
  }, []);

  const { samples } = useSandboxHealth(sb.id);
  const memHistory = useMemo(
    () =>
      series(samples, (s) =>
        s.health.mem_total_kib ? s.health.mem_total_kib / 1024 : undefined,
      ),
    [samples],
  );

  const resize = useSandboxAction(
    () =>
      verbs.resize(sb.id, {
        memory_mib: mem !== sb.memory_mib ? mem : undefined,
        vcpus: cpu !== sb.vcpus ? cpu : undefined,
      }),
    {
      sandboxId: sb.id,
      optimistic: () => ({ memory_mib: mem, vcpus: cpu }),
      onSuccess: (out) => {
        setNoCapacity(false);
        toast.success(t("Resized", "已调整"), `${fmtMiB(out.memory_mib)} · ${out.vcpus} vCPU`);
      },
      onError: (err) => {
        // A 409 while inside the ceiling means the NODE is full — surface
        // the honest options instead of a generic error (ceiling 409s
        // cannot happen from here; the controls are bounded).
        if (err instanceof ApiError && err.status === 409 && !err.message.includes("outside resize ceiling")) {
          setNoCapacity(true);
          return;
        }
        toastError(t("Resize failed", "调整失败"))(err);
      },
    },
  );

  const setAutoscale = useSandboxAction(
    (on: boolean) => verbs.setAutoscale(sb.id, on),
    {
      sandboxId: sb.id,
      onSuccess: (out) =>
        toast.success(
          out.autoscale
            ? t("Autoscale on — grows/shrinks between base and ceiling", "自动伸缩已开启 —— 在基础与上限之间自动调整")
            : t("Autoscale off — geometry only moves when you resize", "自动伸缩已关闭 —— 仅手动调整时改变规格"),
        ),
      onError: (err) => {
        if (err instanceof ApiError && (err.status === 404 || err.status === 405)) {
          setToggleSupported(false);
          toast.info(t("This server predates the autoscale toggle", "当前服务端不支持运行时切换自动伸缩"));
          return;
        }
        toastError(t("Autoscale toggle failed", "切换自动伸缩失败"))(err);
      },
    },
  );
  // Optimistic patch needs the TARGET value, which useSandboxAction's
  // optimistic() cannot see — patch by hand, roll back by hand. Returns the
  // toggled sandbox (or undefined on failure) so callers can SEQUENCE a
  // follow-up mutation instead of racing it against this one's rollback.
  const flipAutoscale = (on: boolean) => {
    const prev = qc.getQueryData<Sandbox>(["sandboxes", sb.id]);
    qc.setQueryData<Sandbox>(["sandboxes", sb.id], (p) => (p ? { ...p, autoscale: on } : p));
    return setAutoscale.mutateAsync(on).catch(() => {
      if (prev) qc.setQueryData(["sandboxes", sb.id], prev);
      return undefined; // onError already surfaced the failure
    });
  };

  const migrateRetry = useMutation({
    mutationFn: async () => {
      await verbs.migrate(sb.id);
      return verbs.resize(sb.id, {
        memory_mib: mem !== sb.memory_mib ? mem : undefined,
        vcpus: cpu !== sb.vcpus ? cpu : undefined,
      });
    },
    onSuccess: (out) => {
      setNoCapacity(false);
      toast.success(t("Migrated & resized", "已迁移并调整"), `${fmtMiB(out.memory_mib)} · ${out.vcpus} vCPU`);
      void qc.invalidateQueries({ queryKey: ["sandboxes"] });
      void qc.invalidateQueries({ queryKey: ["nodes"] });
    },
    onError: toastError(t("Migrate & retry failed", "迁移重试失败")),
  });

  if (!resizable)
    return (
      <p className="text-[13px] text-faint">
        {t(
          "Fixed geometry — this sandbox was created with explicit vcpus/memory_mib. Omit them (elastic is the default) or set ",
          "固定规格 —— 该沙箱创建时显式指定了 vcpus/memory_mib。省略它们（弹性是默认行为）或设置 ",
        )}
        <Mono>max_memory_mib</Mono> / <Mono>max_vcpus</Mono>
        {t(" to get runtime resize.", " 即可获得运行时调整。")}
      </p>
    );

  const dirty = mem !== sb.memory_mib || cpu !== sb.vcpus;
  const roundHint = maxMem > base ? slotRoundingHint(base, mem, t) : null;
  return (
    <div ref={panelRef} className="space-y-4">
      <div className="flex items-start justify-between gap-3">
        <div className="grid flex-1 gap-3 sm:grid-cols-2">
          <MemGauge
            wide
            state={sb.state}
            memoryMiB={sb.memory_mib}
            baseMiB={sb.base_memory_mib}
            maxMiB={sb.max_memory_mib}
          />
          <CpuGauge
            wide
            state={sb.state}
            vcpus={sb.vcpus}
            baseVCPUs={sb.base_vcpus}
            maxVCPUs={sb.max_vcpus}
          />
        </div>
        <AutoscaleBadge on={!!sb.autoscale} />
      </div>

      {sb.state === "RUNNING" && memHistory.length >= 2 && (
        <div>
          <div className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
            {t("Effective memory · ~10 min", "有效内存 · 约 10 分钟")}
          </div>
          <Sparkline
            points={memHistory}
            label={t("effective memory", "有效内存")}
            format={(v) => fmtMiB(Math.round(v))}
            yMin={0}
            yMax={Math.max(maxMem, sb.memory_mib) * 1.06}
            refLines={[
              ...(maxMem > base ? [{ value: maxMem, label: `max ${fmtMiB(maxMem)}` }] : []),
              { value: base, label: `base ${fmtMiB(base)}` },
            ]}
            trendWords={[t("shrinking", "收缩"), t("steady", "平稳"), t("growing", "增长")]}
          />
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-2">
        {maxMem > base && (
          <Field
            label={`${t("Memory", "内存")} · ${fmtMiB(base)} – ${fmtMiB(maxMem)}`}
            hint={roundHint ?? undefined}
          >
            <div className="flex items-center gap-3">
              <input
                ref={sliderRef}
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
          <Field label={`vCPUs · ${baseCPU} – ${maxCPU}`}>
            <input
              className={inputCls}
              type="number"
              min={baseCPU}
              max={maxCPU}
              value={cpu}
              onChange={(e) => setCPU(Number(e.target.value))}
            />
          </Field>
        )}
      </div>

      {sb.autoscale && dirty && (
        <p className="rounded-md border border-hairline bg-bg p-2.5 text-[12px] text-muted">
          {t(
            "Autoscale stays in control: it treats your new size as the current point — it may grow it further under pressure, and shrinks it back toward base within ~5 minutes when idle.",
            "自动伸缩仍在接管：它把你的新规格视为当前值 —— 压力下可能继续扩容，空闲约 5 分钟后会向基础规格回缩。",
          )}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <Button
          kind="primary"
          onClick={() => resize.mutate()}
          busy={resize.isPending}
          disabled={!dirty || sb.state !== "RUNNING"}
        >
          {t("Apply resize", "应用调整")}
        </Button>
        {sb.autoscale && dirty && toggleSupported && (
          <Button
            onClick={() =>
              void flipAutoscale(false).then((out) => {
                if (out) resize.mutate(); // only after the toggle actually landed
              })
            }
            busy={setAutoscale.isPending}
            disabled={sb.state !== "RUNNING"}
          >
            {t("Turn off autoscale & apply", "关闭自动伸缩并应用")}
          </Button>
        )}
        {sb.state !== "RUNNING" && (
          <span className="text-xs text-faint">{t("resize needs RUNNING", "调整需要 RUNNING")}</span>
        )}
        {toggleSupported && !dirty && (
          <Toggle
            checked={!!sb.autoscale}
            onChange={flipAutoscale}
            label={t("Autoscale on guest pressure", "按 guest 压力自动伸缩")}
          />
        )}
      </div>

      {noCapacity && (
        <div className="space-y-2 rounded-md border border-[color-mix(in_srgb,var(--color-warm)_40%,transparent)] bg-[color-mix(in_srgb,var(--color-warm)_8%,transparent)] p-3">
          <p className="text-[13px] text-ink">
            {t(
              "The node is out of budget for this growth. Your options:",
              "当前节点预算不足以完成此次扩容。可选项：",
            )}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <Button kind="primary" onClick={() => migrateRetry.mutate()} busy={migrateRetry.isPending}>
              {t("Migrate & retry (scheduler picks)", "迁移并重试（调度器选点）")}
            </Button>
            <Link to={`/sandboxes/${sb.id}/settings`} className="text-xs text-accent hover:underline">
              {t("Choose target node…", "选择目标节点…")}
            </Link>
            <span className="text-[11px] text-faint">
              {t("or pause/resume — resume re-places on a roomier node", "或 暂停/恢复 —— 恢复时会重新放置到更宽裕的节点")}
            </span>
          </div>
        </div>
      )}
      {!noCapacity && <ErrorNote error={resize.error} />}
    </div>
  );
}

/* ── Metadata ────────────────────────────────────────────────────────── */

function MetaGrid(props: { sb: Sandbox }) {
  const { sb } = props;
  const { t } = useI18n();
  const rows: Array<[string, React.ReactNode]> = [
    [t("node", "节点"), sb.node_id || "—"],
    [t("template", "模板"), sb.template_id.slice(0, 8)],
    [t("disk", "磁盘"), `${sb.data_disk_gib} GiB`],
    ["vcpus", `${sb.vcpus}${sb.max_vcpus ? ` / ${sb.max_vcpus}` : ""}`],
    [t("age", "创建时长"), `${fmtAge(sb.created_at)}`],
    [t("updated", "更新于"), `${fmtAge(sb.updated_at)} ${t("ago", "前")}`],
  ];
  if (sb.paused_at) rows.push([t("paused", "暂停于"), `${fmtAge(sb.paused_at)} ${t("ago", "前")}`]);
  if (sb.autoscale) rows.push([t("autoscale", "自动伸缩"), t("on", "开启")]);
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
          {t("forked from", "派生自")}{" "}
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
  const { t } = useI18n();
  const { data } = useStorage(props.id);
  if (!data) return <Empty>{t("Loading…", "加载中…")}</Empty>;
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
  const { t } = useI18n();
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
          aria-label={t("Command", "命令")}
        />
        <Button kind="primary" type="submit" busy={exec.isPending} disabled={sb.state !== "RUNNING"}>
          {t("Run", "运行")}
        </Button>
      </form>
      <p className="text-[11px] text-faint">
        {t(
          "Buffered request/response — for a live shell use the Terminal tab.",
          "缓冲式请求/响应 —— 需要交互式 shell 请用「终端」标签。",
        )}
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
            {result.timed_out && <span className="text-danger">{t(" · timed out", " · 已超时")}</span>}
            {result.truncated && <span className="text-transit">{t(" · truncated", " · 已截断")}</span>}
          </div>
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap font-mono text-xs text-ink">
            {decodeBytes(result.stdout) || <span className="text-faint">{t("(no stdout)", "（无标准输出）")}</span>}
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
