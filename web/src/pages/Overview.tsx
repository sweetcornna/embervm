import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useNodes, useSandboxes } from "../api/hooks";
import type { Sandbox, SandboxState } from "../api/types";
import { STATE_META, StatusDot } from "../components/status";
import { Card, Empty, Mono, PageHeader, Stat } from "../components/ui";

// Distribution legend in thermal order — hot to ash.
const LEGEND: SandboxState[] = [
  "RUNNING",
  "PAUSED_HOT",
  "PAUSED_WARM",
  "ARCHIVED_COLD",
  "RECYCLED",
  "FAILED",
];

function FleetGrid(props: { sandboxes: Sandbox[] }) {
  if (props.sandboxes.length === 0)
    return <Empty>No sandboxes yet — create one from the Sandboxes page.</Empty>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {props.sandboxes.map((sb) => (
        <Link
          key={sb.id}
          to={`/sandboxes/${sb.id}`}
          title={`${sb.id.slice(0, 8)} · ${STATE_META[sb.state]?.label ?? sb.state} · ${fmtMiB(sb.memory_mib)}`}
          className="grid size-6 place-items-center rounded border border-hairline transition-colors hover:border-accent hover:bg-raised"
        >
          <StatusDot state={sb.state} size={10} />
        </Link>
      ))}
    </div>
  );
}

function CapacityBar(props: { label: string; used: number; total: number; fmt: (n: number) => string }) {
  const boundless = props.total <= 0;
  const pct = boundless ? 0 : Math.min(100, (props.used / props.total) * 100);
  const hot = pct >= 85;
  return (
    <div>
      <div className="flex justify-between font-mono text-[11px] text-muted tabular-nums">
        <span>{props.label}</span>
        <span>
          <span className="text-ink">{props.fmt(props.used)}</span>
          {boundless ? <span className="text-faint"> · unlimited</span> : ` / ${props.fmt(props.total)}`}
        </span>
      </div>
      <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-overlay">
        {!boundless && (
          <div
            className="h-full rounded-full transition-[width] duration-500"
            style={{
              width: `${Math.max(pct > 0 ? 3 : 0, pct)}%`,
              background: hot ? "var(--color-danger)" : "var(--color-accent)",
            }}
          />
        )}
      </div>
    </div>
  );
}

export function Overview() {
  const sandboxes = useSandboxes();
  const nodes = useNodes();
  const list = sandboxes.data ?? [];
  const counts = new Map<SandboxState, number>();
  for (const sb of list) counts.set(sb.state, (counts.get(sb.state) ?? 0) + 1);

  const running = counts.get("RUNNING") ?? 0;
  const nodesUp = (nodes.data ?? []).filter((n) => n.state === "up").length;
  const capTotal = (nodes.data ?? []).reduce((n, x) => n + x.capacity_mib, 0);
  const capUsed = (nodes.data ?? []).reduce((n, x) => n + x.used_mib, 0);

  return (
    <div className="space-y-6">
      <PageHeader title="Overview" subtitle="Fleet health across every registered node." />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="Sandboxes" value={list.length} sub={`${running} running`} />
        <Stat label="Running" value={running} accent />
        <Stat
          label="Nodes up"
          value={`${nodesUp}${nodes.data ? `/${nodes.data.length}` : ""}`}
        />
        <Stat
          label="Memory in use"
          value={capTotal > 0 ? fmtMiB(capUsed) : fmtMiB(capUsed)}
          sub={capTotal > 0 ? `of ${fmtMiB(capTotal)}` : "capacity unbounded"}
        />
      </div>

      <Card
        title="Fleet"
        actions={
          <div className="flex flex-wrap items-center gap-x-3.5 gap-y-1">
            {LEGEND.map((s) => (
              <span key={s} className="inline-flex items-center gap-1.5 font-mono text-[11px] text-muted">
                <StatusDot state={s} size={6} />
                {STATE_META[s].label}
                <span className="tabular-nums text-ink">{counts.get(s) ?? 0}</span>
              </span>
            ))}
          </div>
        }
      >
        {sandboxes.isLoading ? <Empty>Loading…</Empty> : <FleetGrid sandboxes={list} />}
      </Card>

      <Card title="Nodes">
        {nodes.data && nodes.data.length > 0 ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {nodes.data.map((n) => (
              <div key={n.id} className="rounded-md border border-hairline bg-bg p-3">
                <div className="mb-2.5 flex items-center justify-between">
                  <Mono className="font-semibold text-ink">{n.id}</Mono>
                  <span className="inline-flex items-center gap-1.5 font-mono text-[11px]">
                    <span
                      className="inline-block size-1.5 rounded-full"
                      style={{ background: n.state === "up" ? "var(--color-ok)" : "var(--color-danger)" }}
                    />
                    <span className={n.state === "up" ? "text-muted" : "text-danger"}>
                      {n.state} · {fmtAge(n.last_seen)}
                    </span>
                  </span>
                </div>
                <div className="space-y-2.5">
                  <CapacityBar label="memory" used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
                  <CapacityBar
                    label="vcpus"
                    used={n.used_vcpus}
                    total={n.cpu_cores ?? 0}
                    fmt={(v) => String(v)}
                  />
                  <div className="font-mono text-[11px] text-muted tabular-nums">
                    <span className="text-ink">{n.active_sandboxes}</span> active
                  </div>
                </div>
              </div>
            ))}
          </div>
        ) : (
          <Empty>{nodes.isLoading ? "Loading…" : "No nodes registered."}</Empty>
        )}
      </Card>
    </div>
  );
}
