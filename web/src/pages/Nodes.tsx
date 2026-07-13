// Fleet node view: capacity cards + a detail drawer listing each node's
// sandboxes. Node membership is static config (no per-node REST endpoint),
// so the drawer joins /nodes with /sandboxes client-side.

import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useNodes, useSandboxes } from "../api/hooks";
import type { NodeView, Sandbox } from "../api/types";
import { StateBadge } from "../components/status";
import {
  CapacityBar,
  Drawer,
  Empty,
  Mono,
  PageHeader,
  Skeleton,
} from "../components/ui";
import { useI18n } from "../lib/i18n";

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
        <CapacityBar label={t("memory", "内存")} used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
        <CapacityBar label={t("vcpus", "vCPU")} used={n.used_vcpus} total={n.cpu_cores ?? 0} fmt={(v) => String(v)} />
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
        <CapacityBar label={t("memory", "内存")} used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
        <CapacityBar label={t("vcpus", "vCPU")} used={n.used_vcpus} total={n.cpu_cores ?? 0} fmt={(v) => String(v)} />
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
                  <Mono className="text-[11px] text-muted tabular-nums">{fmtMiB(sb.memory_mib)}</Mono>
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
