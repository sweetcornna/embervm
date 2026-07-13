import { useState } from "react";
import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useFleetEvents, useNodes, useSandboxes } from "../api/hooks";
import type { Sandbox, SandboxState } from "../api/types";
import { CreateSandboxDialog } from "../components/createSandbox";
import { STATE_META, StatusDot } from "../components/status";
import { Button, CapacityBar, Card, Empty, Mono, PageHeader, Skeleton, Stat } from "../components/ui";

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
  const shown = props.filter ? props.sandboxes.filter((s) => s.state === props.filter) : props.sandboxes;
  if (props.sandboxes.length === 0)
    return (
      <div className="flex flex-col items-center gap-3 py-8 text-center">
        <p className="text-[13px] text-faint">No sandboxes yet.</p>
        <Button kind="primary" onClick={props.onCreate}>
          New sandbox
        </Button>
      </div>
    );
  if (shown.length === 0)
    return <Empty>No {props.filter ? STATE_META[props.filter].label : ""} sandboxes.</Empty>;
  return (
    <div className="flex flex-wrap gap-1.5">
      {shown.map((sb) => (
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

export function Overview() {
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

  return (
    <div className="space-y-6">
      <PageHeader title="Overview" subtitle="Fleet health across every registered node." />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="Sandboxes" value={list.length} sub={`${running} running`} />
        <Stat label="Running" value={running} accent />
        <Stat label="Nodes up" value={`${nodesUp}${nodes.data ? `/${nodes.data.length}` : ""}`} />
        <Stat
          label="Memory in use"
          value={fmtMiB(capUsed)}
          sub={capTotal > 0 ? `of ${fmtMiB(capTotal)}` : "capacity unbounded"}
        />
      </div>

      <Card
        title="Fleet"
        actions={
          <div className="flex flex-wrap items-center gap-x-3.5 gap-y-1">
            <button
              onClick={() => setFilter(null)}
              className={`font-mono text-[11px] ${filter === null ? "text-accent" : "text-faint hover:text-muted"}`}
            >
              all
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
                {STATE_META[s].label}
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
          <Card title="Nodes" actions={<Link to="/nodes" className="text-xs text-accent hover:underline">manage →</Link>}>
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
                          {n.state} · {fmtAge(n.last_seen)}
                        </span>
                      </span>
                    </div>
                    <div className="space-y-2.5">
                      <CapacityBar label="memory" used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
                      <CapacityBar label="vcpus" used={n.used_vcpus} total={n.cpu_cores ?? 0} fmt={(v) => String(v)} />
                      <div className="font-mono text-[11px] text-muted tabular-nums">
                        <span className="text-ink">{n.active_sandboxes}</span> active
                      </div>
                    </div>
                  </Link>
                ))}
              </div>
            ) : (
              <Empty>{nodes.isLoading ? "Loading…" : "No nodes registered."}</Empty>
            )}
          </Card>
        </div>
        <Card title="Recent activity" pad={false}>
          {events.data?.events && events.data.events.length > 0 ? (
            <ul className="divide-y divide-hairline/60">
              {events.data.events.map((ev) => {
                const meta = STATE_META[ev.to_state as SandboxState];
                return (
                  <li key={ev.id} className="flex items-center gap-2.5 px-4 py-2">
                    <span
                      aria-hidden
                      className="size-1.5 shrink-0 rounded-full"
                      style={{ background: meta?.color ?? "var(--color-idle)" }}
                    />
                    <Link
                      to={`/sandboxes/${ev.sandbox_id}`}
                      className="shrink-0 font-mono text-[11px] text-muted hover:text-accent"
                    >
                      {ev.sandbox_id.slice(0, 8)}
                    </Link>
                    <span className="min-w-0 flex-1 truncate text-[12px] text-ink">
                      {meta?.label ?? ev.to_state}
                    </span>
                    <span className="shrink-0 font-mono text-[10px] text-faint">{fmtAge(ev.at)}</span>
                  </li>
                );
              })}
            </ul>
          ) : (
            <Empty>{events.isLoading ? "Loading…" : "No activity yet."}</Empty>
          )}
        </Card>
      </div>

      <CreateSandboxDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}
