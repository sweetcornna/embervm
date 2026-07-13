import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { fmtBytes, fmtPct } from "../api/client";
import { useStorageReport } from "../api/hooks";
import type { StorageReport } from "../api/types";
import { Card, Empty, Mono, PageHeader, Skeleton, Stat, Table } from "../components/ui";

const TIER_COLOR: Record<string, string> = {
  hot: "var(--color-hot)",
  warm: "var(--color-warm)",
  cold: "var(--color-cold)",
  recycled: "var(--color-idle)",
  none: "var(--color-faint)",
};
const TIER_ORDER = ["hot", "warm", "cold", "recycled", "none"];

type SortKey = "logical" | "stored" | "ratio";

export function Storage() {
  const { data, isLoading } = useStorageReport();
  const [sort, setSort] = useState<{ key: SortKey; dir: 1 | -1 }>({ key: "stored", dir: -1 });

  const sandboxes = data?.sandboxes ?? [];
  const logical = data?.total_logical_bytes ?? 0;
  const stored = data?.total_stored_bytes ?? 0;

  // Stored bytes per tier — the cost-by-temperature story.
  const byTier = useMemo(() => {
    const m = new Map<string, number>();
    for (const r of sandboxes) m.set(r.tier, (m.get(r.tier) ?? 0) + r.stored_bytes);
    return TIER_ORDER.map((t) => ({ tier: t, bytes: m.get(t) ?? 0 })).filter((x) => x.bytes > 0);
  }, [sandboxes]);
  const tierTotal = byTier.reduce((n, x) => n + x.bytes, 0);

  const rows = useMemo(() => {
    return [...sandboxes].sort((a, b) => {
      const val = (r: StorageReport) =>
        sort.key === "logical" ? r.logical_bytes : sort.key === "stored" ? r.stored_bytes : r.stored_ratio;
      return (val(a) - val(b)) * sort.dir;
    });
  }, [sandboxes, sort]);

  const toggle = (key: SortKey) =>
    setSort((s) => (s.key === key ? { key, dir: s.dir === 1 ? -1 : 1 } : { key, dir: -1 }));

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
          value={logical > 0 ? fmtPct(stored / logical) : "—"}
          sub="stored ÷ logical after dedup"
        />
      </div>

      {tierTotal > 0 && (
        <Card title="Stored bytes by tier">
          <div className="space-y-2">
            <div className="flex h-3 w-full overflow-hidden rounded-full bg-overlay">
              {byTier.map((t, i) => (
                <div
                  key={t.tier}
                  className="h-full"
                  style={{
                    width: `${(t.bytes / tierTotal) * 100}%`,
                    background: TIER_COLOR[t.tier],
                    marginLeft: i === 0 ? 0 : 2,
                  }}
                  title={`${t.tier}: ${fmtBytes(t.bytes)}`}
                />
              ))}
            </div>
            <div className="flex flex-wrap gap-x-4 gap-y-1">
              {byTier.map((t) => (
                <span key={t.tier} className="inline-flex items-center gap-1.5 font-mono text-[11px] text-muted">
                  <span className="inline-block size-1.5 rounded-full" style={{ background: TIER_COLOR[t.tier] }} />
                  {t.tier}
                  <span className="tabular-nums text-ink">{fmtBytes(t.bytes)}</span>
                </span>
              ))}
            </div>
          </div>
        </Card>
      )}

      <Table
        head={[
          "Sandbox",
          "Tier",
          <SortHeader key="l" label="Logical" active={sort.key === "logical"} dir={sort.dir} onClick={() => toggle("logical")} />,
          <SortHeader key="s" label="Stored" active={sort.key === "stored"} dir={sort.dir} onClick={() => toggle("stored")} />,
          <SortHeader key="r" label="Ratio" active={sort.key === "ratio"} dir={sort.dir} onClick={() => toggle("ratio")} />,
          "Layers",
        ]}
      >
        {rows.map((r) => (
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
                {r.logical_bytes > 0 ? fmtPct(r.stored_ratio) : "—"}
              </Mono>
            </td>
            <td className="px-4 py-2.5">
              <Mono className="text-muted tabular-nums">{r.layers}</Mono>
            </td>
          </tr>
        ))}
      </Table>
      {isLoading && <Skeleton className="h-24 w-full" />}
      {!isLoading && sandboxes.length === 0 && (
        <Empty>Nothing stored yet — pause a sandbox to see its footprint.</Empty>
      )}
    </div>
  );
}

function SortHeader(props: { label: string; active: boolean; dir: 1 | -1; onClick: () => void }) {
  return (
    <button
      onClick={props.onClick}
      className={`inline-flex items-center gap-1 ${props.active ? "text-muted" : ""}`}
      aria-sort={props.active ? (props.dir === 1 ? "ascending" : "descending") : "none"}
    >
      {props.label}
      {props.active && <span className="text-[9px]">{props.dir === 1 ? "▲" : "▼"}</span>}
    </button>
  );
}
