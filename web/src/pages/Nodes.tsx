// Fleet node view: capacity cards + a detail drawer listing each node's
// sandboxes. Node membership is static config (no per-node REST endpoint),
// so the drawer joins /nodes with /sandboxes client-side.

import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useNodes, useSandboxes } from "../api/hooks";
import type { NodeView, Sandbox } from "../api/types";
import { AutoscaleBadge, MemGauge, StateBadge } from "../components/status";
import {
  CapacityBar,
  Drawer,
  Empty,
  Mono,
  OversellBar,
  PageHeader,
  Skeleton,
} from "../components/ui";
import { useI18n } from "../lib/i18n";
import type { TFn } from "../lib/i18n";

/** Memory bars: the M7 oversell view when the server reports base/ceiling
    sums, else the plain used/capacity bar (pre-M7 server). */
function NodeMemoryBar(props: { n: NodeView; t: TFn }) {
  const { n, t } = props;
  if (n.ceiling_mib === undefined || n.base_mib === undefined) {
    return <CapacityBar label={t("memory", "内存")} used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />;
  }
  return (
    <OversellBar
      label={t("memory", "内存")}
      base={n.base_mib}
      used={n.used_mib}
      ceiling={n.ceiling_mib}
      capacity={n.capacity_mib}
      budget={n.mem_budget_mib ?? 0}
      fmt={fmtMiB}
      overBudgetTitle={t(
        `If every sandbox grew to its ceiling this node would need ${fmtMiB(n.ceiling_mib)} — grows will defer.`,
        `若所有沙箱都涨到上限，此节点需 ${fmtMiB(n.ceiling_mib)} —— 届时扩容将被推迟。`,
      )}
    />
  );
}

function NodeCPUBar(props: { n: NodeView; t: TFn }) {
  const { n, t } = props;
  if (n.ceiling_vcpus === undefined || n.base_vcpus === undefined) {
    return <CapacityBar label={t("vcpus", "vCPU")} used={n.used_vcpus} total={n.cpu_cores ?? 0} fmt={(v) => String(v)} />;
  }
  return (
    <OversellBar
      label={t("vcpus", "vCPU")}
      base={n.base_vcpus}
      used={n.used_vcpus}
      ceiling={n.ceiling_vcpus}
      capacity={n.cpu_cores ?? 0}
      budget={n.vcpu_budget ?? 0}
      fmt={(v) => String(v)}
      overBudgetTitle={t(
        `If every sandbox grew to its vCPU ceiling this node would owe ${n.ceiling_vcpus} vCPUs — grows will defer.`,
        `若所有沙箱都涨到 vCPU 上限，此节点需 ${n.ceiling_vcpus} vCPU —— 届时扩容将被推迟。`,
      )}
    />
  );
}

export function Nodes() {
  const { t } = useI18n();
  const nodes = useNodes();
  const sandboxes = useSandboxes();
  const [selected, setSelected] = useState<string | null>(null);
  const node = (nodes.data ?? []).find((n) => n.id === selected) ?? null;

  return (
    <div className="space-y-5">
      <PageHeader title={t("Nodes", "节点")} subtitle={t("Capacity and liveness across the cluster.", "集群的容量与存活状态。")} />

      {nodes.isLoading && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-36 w-full" />
          ))}
        </div>
      )}

      {nodes.data && nodes.data.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {nodes.data.map((n) => (
            <NodeCard key={n.id} node={n} onOpen={() => setSelected(n.id)} />
          ))}
        </div>
      )}
      {nodes.data && nodes.data.length === 0 && <Empty>{t("No nodes registered.", "暂无已注册节点。")}</Empty>}

      <Drawer
        title={node ? `${t("Node", "节点")} ${node.id}` : t("Node", "节点")}
        open={node !== null}
        onClose={() => setSelected(null)}
      >
        {node && <NodeDetail node={node} sandboxes={sandboxes.data ?? []} />}
      </Drawer>
    </div>
  );
}

function NodeCard(props: { node: NodeView; onOpen: () => void }) {
  const { t } = useI18n();
  const { node: n } = props;
  return (
    <button
      onClick={props.onOpen}
      className="rounded-md border border-border bg-surface p-3 text-left transition-colors hover:border-accent/50"
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
        <NodeMemoryBar n={n} t={t} />
        <NodeCPUBar n={n} t={t} />
        <div className="font-mono text-[11px] text-muted tabular-nums">
          <span className="text-ink">{n.active_sandboxes}</span> {t("active", "活跃")}
        </div>
      </div>
    </button>
  );
}

function NodeDetail(props: { node: NodeView; sandboxes: Sandbox[] }) {
  const { t } = useI18n();
  const { node: n } = props;
  const here = props.sandboxes.filter((sb) => (sb.node_id || "local") === n.id);
  const meta: Array<[string, string]> = [
    [t("state", "状态"), n.state === "up" ? t("up", "在线") : t("down", "离线")],
    [t("address", "地址"), n.addr || "—"],
    [t("cpu cores", "CPU 核数"), n.cpu_cores ? String(n.cpu_cores) : "—"],
    [t("capacity", "容量"), n.capacity_mib > 0 ? fmtMiB(n.capacity_mib) : t("unlimited", "无限制")],
    [t("used", "已用"), fmtMiB(n.used_mib)],
    [t("last seen", "最后心跳"), `${fmtAge(n.last_seen)} ${t("ago", "前")}`],
  ];
  return (
    <div className="space-y-5">
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2.5">
        {meta.map(([k, v]) => (
          <div key={k}>
            <dt className="font-mono text-[10px] uppercase tracking-[0.12em] text-faint">{k}</dt>
            <dd className="mt-0.5 font-mono text-[13px] text-ink">{v}</dd>
          </div>
        ))}
      </dl>
      <div className="space-y-2.5">
        <NodeMemoryBar n={n} t={t} />
        <NodeCPUBar n={n} t={t} />
      </div>
      <div>
        <h3 className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
          {t("Sandboxes here", "此节点上的沙箱")} ({here.length})
        </h3>
        {here.length === 0 ? (
          <Empty>{t("No sandboxes on this node.", "此节点上暂无沙箱。")}</Empty>
        ) : (
          <ul className="divide-y divide-hairline overflow-hidden rounded-md border border-hairline">
            {here.map((sb) => (
              <li key={sb.id} className="flex items-center justify-between gap-3 px-3 py-2">
                <Link to={`/sandboxes/${sb.id}`} className="hover:text-accent">
                  <Mono className="text-[12px]">{sb.id.slice(0, 8)}</Mono>
                </Link>
                <div className="flex items-center gap-3">
                  {sb.autoscale && <AutoscaleBadge on />}
                  <MemGauge state={sb.state} memoryMiB={sb.memory_mib} baseMiB={sb.base_memory_mib} maxMiB={sb.max_memory_mib} />
                  <StateBadge state={sb.state} />
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
