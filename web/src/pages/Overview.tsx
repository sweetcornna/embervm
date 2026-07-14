import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useFleetEvents, useNodes, useSandboxes } from "../api/hooks";
import type { Sandbox, SandboxState } from "../api/types";
import { CreateSandboxDialog } from "../components/createSandbox";
import { STATE_META, StatusDot, stateLabel } from "../components/status";
import { Button, CapacityBar, Card, Empty, Mono, PageHeader, Skeleton, Stat } from "../components/ui";
import { useI18n } from "../lib/i18n";
import { describeResourceEvent, parseResourceEvent } from "../lib/resourceEvents";

type FeedFilter = "all" | "lifecycle" | "resources";

// Distribution legend in thermal order — hot to ash.
const LEGEND: SandboxState[] = [
  "RUNNING",
  "PAUSED_HOT",
  "PAUSED_WARM",
  "ARCHIVED_COLD",
  "RECYCLED",
  "FAILED",
];

function FleetGrid(props: {
  sandboxes: Sandbox[];
  filter: SandboxState | null;
  onCreate: () => void;
}) {
  const { t } = useI18n();
  const shown = props.filter ? props.sandboxes.filter((s) => s.state === props.filter) : props.sandboxes;
  if (props.sandboxes.length === 0)
    return (
      <div className="flex flex-col items-center gap-3 py-8 text-center">
        <p className="text-[13px] text-faint">{t("No sandboxes yet.", "暂无沙箱")}</p>
        <Button kind="primary" onClick={props.onCreate}>
          {t("New sandbox", "新建沙箱")}
        </Button>
      </div>
    );
  if (shown.length === 0)
    return (
      <Empty>
        {t("No", "暂无")} {props.filter ? stateLabel(props.filter, t) : ""} {t("sandboxes.", "沙箱")}
      </Empty>
    );
  return (
    <div className="flex flex-wrap gap-1.5">
      {shown.map((sb) => (
        <Link
          key={sb.id}
          to={`/sandboxes/${sb.id}`}
          title={`${sb.id.slice(0, 8)} · ${stateLabel(sb.state, t)} · ${fmtMiB(sb.memory_mib)}`}
          className="grid size-6 place-items-center rounded border border-hairline transition-colors hover:border-accent hover:bg-raised"
        >
          <StatusDot state={sb.state} size={10} />
        </Link>
      ))}
    </div>
  );
}

export function Overview() {
  const { t } = useI18n();
  const sandboxes = useSandboxes();
  const nodes = useNodes();
  const events = useFleetEvents(12);
  const [creating, setCreating] = useState(false);
  const [filter, setFilter] = useState<SandboxState | null>(null);

  const list = sandboxes.data ?? [];
  const counts = new Map<SandboxState, number>();
  for (const sb of list) counts.set(sb.state, (counts.get(sb.state) ?? 0) + 1);

  const running = counts.get("RUNNING") ?? 0;
  const nodesUp = (nodes.data ?? []).filter((n) => n.state === "up").length;
  const capTotal = (nodes.data ?? []).reduce((n, x) => n + x.capacity_mib, 0);
  const capUsed = (nodes.data ?? []).reduce((n, x) => n + x.used_mib, 0);
  const baseSum = (nodes.data ?? []).reduce((n, x) => n + (x.base_mib ?? 0), 0);
  const ceilSum = (nodes.data ?? []).reduce((n, x) => n + (x.ceiling_mib ?? 0), 0);
  const [feedFilter, setFeedFilter] = useState<FeedFilter>("all");
  const feed = useMemo(() => {
    const evs = events.data?.events ?? [];
    if (feedFilter === "all") return evs;
    return evs.filter((ev) =>
      feedFilter === "resources" ? parseResourceEvent(ev) !== null : parseResourceEvent(ev) === null,
    );
  }, [events.data, feedFilter]);

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("Overview", "总览")}
        subtitle={t("Fleet health across every registered node.", "所有已注册节点的舰队健康状况。")}
      />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label={t("Sandboxes", "沙箱")} value={list.length} sub={`${running} ${t("running", "运行中")}`} />
        <Stat label={t("Running", "运行中")} value={running} accent />
        <Stat label={t("Nodes up", "在线节点")} value={`${nodesUp}${nodes.data ? `/${nodes.data.length}` : ""}`} />
        <Stat
          label={t("Memory in use", "内存使用")}
          value={fmtMiB(capUsed)}
          sub={
            ceilSum > 0
              ? `${t("base", "基础")} ${fmtMiB(baseSum)} · ${t("ceiling", "上限")} ${fmtMiB(ceilSum)}`
              : capTotal > 0
                ? `${t("of", "共")} ${fmtMiB(capTotal)}`
                : t("capacity unbounded", "容量无限制")
          }
        />
      </div>

      <Card
        title={t("Fleet", "舰队")}
        actions={
          <div className="flex flex-wrap items-center gap-x-3.5 gap-y-1">
            <button
              onClick={() => setFilter(null)}
              className={`font-mono text-[11px] ${filter === null ? "text-accent" : "text-faint hover:text-muted"}`}
            >
              {t("all", "全部")}
            </button>
            {LEGEND.map((s) => (
              <button
                key={s}
                onClick={() => setFilter(filter === s ? null : s)}
                className={`inline-flex items-center gap-1.5 font-mono text-[11px] transition-colors ${
                  filter === s ? "text-ink" : "text-muted hover:text-ink"
                }`}
              >
                <StatusDot state={s} size={6} />
                {stateLabel(s, t)}
                <span className="tabular-nums text-ink">{counts.get(s) ?? 0}</span>
              </button>
            ))}
          </div>
        }
      >
        {sandboxes.isLoading ? (
          <div className="flex flex-wrap gap-1.5">
            {Array.from({ length: 12 }).map((_, i) => (
              <Skeleton key={i} className="size-6" />
            ))}
          </div>
        ) : (
          <FleetGrid sandboxes={list} filter={filter} onCreate={() => setCreating(true)} />
        )}
      </Card>

      <div className="grid gap-6 lg:grid-cols-3">
        <div className="lg:col-span-2">
          <Card title={t("Nodes", "节点")} actions={<Link to="/nodes" className="text-xs text-accent hover:underline">{t("manage", "管理")} →</Link>}>
            {nodes.data && nodes.data.length > 0 ? (
              <div className="grid gap-3 sm:grid-cols-2">
                {nodes.data.map((n) => (
                  <Link
                    key={n.id}
                    to="/nodes"
                    className="block rounded-md border border-hairline bg-bg p-3 transition-colors hover:border-accent/40"
                  >
                    <div className="mb-2.5 flex items-center justify-between">
                      <Mono className="font-semibold text-ink">{n.id}</Mono>
                      <span className="inline-flex items-center gap-1.5 font-mono text-[11px]">
                        <span
                          className="inline-block size-1.5 rounded-full"
                          style={{ background: n.state === "up" ? "var(--color-ok)" : "var(--color-danger)" }}
                        />
                        <span className={n.state === "up" ? "text-muted" : "text-danger"}>
                          {n.state === "up" ? t("up", "在线") : t("down", "离线")} · {fmtAge(n.last_seen)}
                        </span>
                      </span>
                    </div>
                    <div className="space-y-2.5">
                      <CapacityBar label={t("memory", "内存")} used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
                      <CapacityBar label={t("vcpus", "vCPU")} used={n.used_vcpus} total={n.cpu_cores ?? 0} fmt={(v) => String(v)} />
                      <div className="font-mono text-[11px] text-muted tabular-nums">
                        <span className="text-ink">{n.active_sandboxes}</span> {t("active", "活跃")}
                      </div>
                    </div>
                  </Link>
                ))}
              </div>
            ) : (
              <Empty>{nodes.isLoading ? t("Loading…", "加载中…") : t("No nodes registered.", "暂无已注册节点。")}</Empty>
            )}
          </Card>
        </div>
        <Card
          title={t("Recent activity", "最近活动")}
          pad={false}
          actions={
            <div className="flex gap-2">
              {(
                [
                  ["all", t("all", "全部")],
                  ["lifecycle", t("lifecycle", "生命周期")],
                  ["resources", t("resources", "资源")],
                ] as Array<[FeedFilter, string]>
              ).map(([key, label]) => (
                <button
                  key={key}
                  onClick={() => setFeedFilter(key)}
                  className={`font-mono text-[11px] ${feedFilter === key ? "text-accent" : "text-faint hover:text-muted"}`}
                >
                  {label}
                </button>
              ))}
            </div>
          }
        >
          {feed.length > 0 ? (
            <ul className="divide-y divide-hairline/60">
              {feed.map((ev) => {
                const res = parseResourceEvent(ev);
                const view = res ? describeResourceEvent(res, t) : null;
                const meta = STATE_META[ev.to_state as SandboxState];
                return (
                  <li key={ev.id} className="flex items-center gap-2.5 px-4 py-2">
                    <span
                      aria-hidden
                      className="size-1.5 shrink-0 rounded-full"
                      style={{
                        background: view
                          ? view.tone === "warn"
                            ? "var(--color-warm)"
                            : "var(--color-transit)"
                          : (meta?.color ?? "var(--color-idle)"),
                      }}
                    />
                    <Link
                      to={`/sandboxes/${ev.sandbox_id}`}
                      className="shrink-0 font-mono text-[11px] text-muted hover:text-accent"
                    >
                      {ev.sandbox_id.slice(0, 8)}
                    </Link>
                    <span className="min-w-0 flex-1 truncate text-[12px] text-ink" title={view?.text}>
                      {view ? view.text : stateLabel(ev.to_state as SandboxState, t)}
                    </span>
                    <span className="shrink-0 font-mono text-[10px] text-faint">{fmtAge(ev.at)}</span>
                  </li>
                );
              })}
            </ul>
          ) : (
            <Empty>{events.isLoading ? t("Loading…", "加载中…") : t("No activity yet.", "暂无活动。")}</Empty>
          )}
        </Card>
      </div>

      <CreateSandboxDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}
