import { Link } from "react-router-dom";
import { fmtBytes } from "../api/client";
import { useStorageReport } from "../api/hooks";
import { Empty, Mono, PageHeader, Stat, Table } from "../components/ui";

const TIER_COLOR: Record<string, string> = {
  hot: "var(--color-hot)",
  warm: "var(--color-warm)",
  cold: "var(--color-cold)",
  recycled: "var(--color-idle)",
  none: "var(--color-faint)",
};

export function Storage() {
  const { data, isLoading } = useStorageReport();
  // The endpoint returns an object with pre-summed totals, not an array.
  const sandboxes = data?.sandboxes ?? [];
  const logical = data?.total_logical_bytes ?? 0;
  const stored = data?.total_stored_bytes ?? 0;

  return (
    <div className="space-y-5">
      <PageHeader
        title="Storage"
        subtitle="What each sandbox's snapshots cost after zero-skip, lz4, and dedup."
      />

      <div className="grid grid-cols-3 gap-3">
        <Stat label="Logical" value={fmtBytes(logical)} />
        <Stat label="Stored" value={fmtBytes(stored)} accent />
        <Stat
          label="Paying for"
          value={logical > 0 ? `${((stored / logical) * 100).toFixed(1)}%` : "—"}
          sub="stored ÷ logical"
        />
      </div>

      <Table head={["Sandbox", "Tier", "Logical", "Stored", "Ratio", "Layers"]}>
        {sandboxes.map((r) => (
          <tr key={r.sandbox_id} className="border-b border-hairline last:border-0 hover:bg-raised/40">
            <td className="px-4 py-2.5">
              <Link to={`/sandboxes/${r.sandbox_id}`} className="hover:text-accent">
                <Mono>{r.sandbox_id.slice(0, 8)}</Mono>
              </Link>
            </td>
            <td className="px-4 py-2.5">
              <span
                className="inline-flex items-center gap-1.5 font-mono text-xs"
                style={{ color: TIER_COLOR[r.tier] ?? "var(--color-faint)" }}
              >
                <span
                  className="inline-block size-1.5 rounded-full"
                  style={{ background: TIER_COLOR[r.tier] ?? "var(--color-faint)" }}
                />
                {r.tier}
              </span>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="tabular-nums">{fmtBytes(r.logical_bytes)}</Mono>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="tabular-nums">{fmtBytes(r.stored_bytes)}</Mono>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted tabular-nums">
                {r.logical_bytes > 0 ? `${(r.stored_ratio * 100).toFixed(1)}%` : "—"}
              </Mono>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted tabular-nums">{r.layers}</Mono>
            </td>
          </tr>
        ))}
      </Table>
      {!isLoading && sandboxes.length === 0 && (
        <Empty>Nothing stored yet — pause a sandbox to see its footprint.</Empty>
      )}
      {isLoading && <Empty>Loading…</Empty>}
    </div>
  );
}
