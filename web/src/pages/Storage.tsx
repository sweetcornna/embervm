import { Link } from "react-router-dom";
import { fmtBytes } from "../api/client";
import { useStorageReport } from "../api/hooks";
import { Empty, Mono } from "../components/ui";

const TIER_COLOR: Record<string, string> = {
  hot: "var(--color-ember)",
  warm: "var(--color-rust)",
  cold: "var(--color-cold)",
  recycled: "var(--color-ash)",
  none: "var(--color-faint)",
};

export function Storage() {
  const { data, isLoading } = useStorageReport();
  const list = data ?? [];
  const totalLogical = list.reduce((n, r) => n + r.logical_bytes, 0);
  const totalStored = list.reduce((n, r) => n + r.stored_bytes, 0);

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <header>
        <h1 className="font-display text-2xl font-bold tracking-wide">Storage</h1>
        <p className="mt-1 text-sm text-muted">
          What each sandbox's snapshots actually cost after zero-skip, lz4, and dedup.
        </p>
      </header>

      <div className="flex gap-8 rounded-md border border-border bg-surface px-5 py-4">
        <div>
          <div className="font-mono text-[11px] uppercase tracking-wider text-muted">logical</div>
          <div className="font-display text-xl font-bold">{fmtBytes(totalLogical)}</div>
        </div>
        <div>
          <div className="font-mono text-[11px] uppercase tracking-wider text-muted">stored</div>
          <div className="font-display text-xl font-bold text-ember">{fmtBytes(totalStored)}</div>
        </div>
        <div>
          <div className="font-mono text-[11px] uppercase tracking-wider text-muted">paying for</div>
          <div className="font-display text-xl font-bold">
            {totalLogical > 0 ? `${((totalStored / totalLogical) * 100).toFixed(1)}%` : "—"}
          </div>
        </div>
      </div>

      <div className="overflow-x-auto rounded-md border border-border bg-surface">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-hairline text-left font-mono text-[11px] uppercase tracking-wider text-muted">
              <th className="px-4 py-2.5 font-medium">Sandbox</th>
              <th className="px-4 py-2.5 font-medium">Tier</th>
              <th className="px-4 py-2.5 font-medium">Logical</th>
              <th className="px-4 py-2.5 font-medium">Stored</th>
              <th className="px-4 py-2.5 font-medium">Ratio</th>
              <th className="px-4 py-2.5 font-medium">Layers</th>
            </tr>
          </thead>
          <tbody>
            {list.map((r) => (
              <tr key={r.sandbox_id} className="border-b border-hairline last:border-0 hover:bg-raised/50">
                <td className="px-4 py-2.5">
                  <Link to={`/sandboxes/${r.sandbox_id}`} className="hover:text-ember">
                    <Mono>{r.sandbox_id.slice(0, 8)}</Mono>
                  </Link>
                </td>
                <td className="px-4 py-2.5">
                  <span className="font-mono text-xs" style={{ color: TIER_COLOR[r.tier] }}>
                    {r.tier}
                  </span>
                </td>
                <td className="px-4 py-2.5">
                  <Mono>{fmtBytes(r.logical_bytes)}</Mono>
                </td>
                <td className="px-4 py-2.5">
                  <Mono>{fmtBytes(r.stored_bytes)}</Mono>
                </td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">
                    {r.logical_bytes > 0 ? `${(r.stored_ratio * 100).toFixed(1)}%` : "—"}
                  </Mono>
                </td>
                <td className="px-4 py-2.5">
                  <Mono className="text-muted">{r.layers}</Mono>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {!isLoading && list.length === 0 && <Empty>Nothing stored yet — pause a sandbox to see its footprint.</Empty>}
        {isLoading && <Empty>Loading…</Empty>}
      </div>
    </div>
  );
}
