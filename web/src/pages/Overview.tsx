import { Link } from "react-router-dom";
import { fmtAge, fmtMiB } from "../api/client";
import { useNodes, useSandboxes } from "../api/hooks";
import type { Sandbox, SandboxState } from "../api/types";
import { EmberDot, STATE_HEAT } from "../components/ember";
import { Card, Empty, Mono } from "../components/ui";

/** State groups in thermal order — the legend reads hot to ash. */
const LEGEND: SandboxState[] = ["RUNNING", "PAUSED_HOT", "PAUSED_WARM", "ARCHIVED_COLD", "RECYCLED", "FAILED"];

function FleetGrid(props: { sandboxes: Sandbox[] }) {
  if (props.sandboxes.length === 0)
    return <Empty>No sandboxes yet — create one from the Sandboxes page.</Empty>;
  return (
    <div className="flex flex-wrap gap-2.5">
      {props.sandboxes.map((sb) => (
        <Link
          key={sb.id}
          to={`/sandboxes/${sb.id}`}
          title={`${sb.id.slice(0, 8)} · ${sb.state.toLowerCase()} · ${fmtMiB(sb.memory_mib)}`}
          className="grid size-7 place-items-center rounded border border-transparent hover:border-border hover:bg-raised"
        >
          <EmberDot state={sb.state} size={12} />
        </Link>
      ))}
    </div>
  );
}

function NodeBar(props: { label: string; used: number; total: number; fmt: (n: number) => string }) {
  const boundless = props.total <= 0;
  const pct = boundless ? 0 : Math.min(100, (props.used / props.total) * 100);
  return (
    <div>
      <div className="flex justify-between font-mono text-[11px] text-muted">
        <span>{props.label}</span>
        <span>
          <span className="text-ink">{props.fmt(props.used)}</span>
          {boundless ? " · unlimited" : ` / ${props.fmt(props.total)}`}
        </span>
      </div>
      <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-raised">
        {!boundless && (
          <div
            className="h-full rounded-full bg-ember transition-[width] duration-500"
            style={{ width: `${Math.max(pct > 0 ? 2 : 0, pct)}%` }}
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

  return (
    <div className="mx-auto max-w-5xl space-y-5">
      <header>
        <h1 className="font-display text-2xl font-bold tracking-wide">Fleet</h1>
        <p className="mt-1 text-sm text-muted">
          Every ember is a sandbox; its color is where it sits on the heat curve.
        </p>
      </header>

      <Card
        title="Heat map"
        actions={
          <div className="flex flex-wrap items-center gap-3">
            {LEGEND.map((s) => (
              <span key={s} className="inline-flex items-center gap-1.5 font-mono text-[11px] text-muted">
                <EmberDot state={s} size={7} />
                {STATE_HEAT[s].label}
                <span className="text-ink">{counts.get(s) ?? 0}</span>
              </span>
            ))}
          </div>
        }
      >
        {sandboxes.isLoading ? <Empty>Loading…</Empty> : <FleetGrid sandboxes={list} />}
      </Card>

      <Card title="Nodes">
        {nodes.data && nodes.data.length > 0 ? (
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {nodes.data.map((n) => (
              <div key={n.id} className="rounded border border-hairline bg-bg p-3">
                <div className="mb-2 flex items-center justify-between">
                  <Mono className="font-semibold text-ink">{n.id}</Mono>
                  <span
                    className={`font-mono text-[11px] ${n.state === "up" ? "text-ember" : "text-alarm"}`}
                  >
                    {n.state} · {fmtAge(n.last_seen)}
                  </span>
                </div>
                <div className="space-y-2.5">
                  <NodeBar label="memory" used={n.used_mib} total={n.capacity_mib} fmt={fmtMiB} />
                  <NodeBar
                    label="vcpus"
                    used={n.used_vcpus}
                    total={n.cpu_cores ?? 0}
                    fmt={(v) => String(v)}
                  />
                  <div className="font-mono text-[11px] text-muted">
                    <span className="text-ink">{n.active_sandboxes}</span> active sandboxes
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
