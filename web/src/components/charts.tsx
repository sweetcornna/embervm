// Hand-rolled SVG time-series sparkline for the workspace's live gauges.
// Dataviz discipline: one series per chart (the title names it — no legend),
// 2px line + a single low-opacity area fill, recessive hairline grid,
// crosshair + tooltip on hover, values in text tokens (the series color
// carries identity on the mark only), and a generated aria summary.

import { useMemo, useRef, useState } from "react";
import { useI18n } from "../lib/i18n";

export interface SeriesPoint {
  at: number; // epoch ms
  value: number;
}

const W = 300; // viewBox units; the SVG stretches to its container
const H = 64;
const PAD_Y = 6;

function scale(points: SeriesPoint[], yMin?: number, yMax?: number) {
  const t0 = points[0].at;
  const t1 = points[points.length - 1].at;
  const tSpan = Math.max(1, t1 - t0);
  let lo = yMin ?? Math.min(...points.map((p) => p.value));
  let hi = yMax ?? Math.max(...points.map((p) => p.value));
  if (hi - lo < 1e-9) {
    hi = lo + 1;
    lo = Math.max(yMin ?? lo - 1, lo - 1);
  }
  const x = (at: number) => ((at - t0) / tSpan) * W;
  const y = (v: number) =>
    H - PAD_Y - ((v - lo) / (hi - lo)) * (H - 2 * PAD_Y);
  return { x, y, lo, hi };
}

export function Sparkline(props: {
  points: SeriesPoint[];
  label: string; // e.g. "memory used"
  format: (v: number) => string;
  yMin?: number;
  yMax?: number;
  color?: string; // series color; default accent
  trendWords?: [string, string, string]; // falling / steady / rising
  /** Dashed horizontal reference lines (M7: base/ceiling bounds on the
      effective-memory staircase). Values outside the y-domain are skipped. */
  refLines?: { value: number; label: string }[];
}) {
  const { t } = useI18n();
  const color = props.color ?? "var(--color-accent)";
  const svgRef = useRef<SVGSVGElement>(null);
  const [hover, setHover] = useState<SeriesPoint | null>(null);

  const built = useMemo(() => {
    const pts = props.points;
    if (pts.length < 2) return null;
    const { x, y } = scale(pts, props.yMin, props.yMax);
    const line = pts
      .map((p, i) => `${i === 0 ? "M" : "L"}${x(p.at).toFixed(2)},${y(p.value).toFixed(2)}`)
      .join("");
    const area = `${line}L${W},${H - PAD_Y}L0,${H - PAD_Y}Z`;
    return { line, area, x, y };
  }, [props.points, props.yMin, props.yMax]);

  const aria = useMemo(() => {
    const pts = props.points;
    if (pts.length === 0) return `${props.label}: ${t("no data", "无数据")}`;
    const last = pts[pts.length - 1].value;
    const prev = pts[Math.max(0, pts.length - 13)].value; // ~30s back at 2.5s cadence
    const [fall, steady, rise] = props.trendWords ?? [
      t("falling", "下降"),
      t("steady", "平稳"),
      t("rising", "上升"),
    ];
    const trend =
      Math.abs(last - prev) < Math.max(1e-9, Math.abs(prev) * 0.03)
        ? steady
        : last > prev
          ? rise
          : fall;
    return `${props.label}: ${props.format(last)}, ${trend}`;
  }, [props.points, props.label, props.format, props.trendWords, t]);

  if (!built) {
    return (
      <div
        role="img"
        aria-label={aria}
        className="flex h-16 items-center justify-center rounded bg-bg text-[11px] text-faint"
      >
        {t("collecting…", "采集中…")}
      </div>
    );
  }

  const onMove = (e: React.PointerEvent) => {
    const rect = svgRef.current?.getBoundingClientRect();
    if (!rect || props.points.length === 0) return;
    const fx = ((e.clientX - rect.left) / rect.width) * W;
    let best = props.points[0];
    let bestD = Infinity;
    for (const p of props.points) {
      const d = Math.abs(built.x(p.at) - fx);
      if (d < bestD) {
        bestD = d;
        best = p;
      }
    }
    setHover(best);
  };

  return (
    <div className="relative">
      <svg
        ref={svgRef}
        role="img"
        aria-label={aria}
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        className="block h-16 w-full"
        onPointerMove={onMove}
        onPointerLeave={() => setHover(null)}
      >
        {/* recessive grid: quarter lines only */}
        {[0.25, 0.5, 0.75].map((f) => (
          <line
            key={f}
            x1="0"
            x2={W}
            y1={PAD_Y + f * (H - 2 * PAD_Y)}
            y2={PAD_Y + f * (H - 2 * PAD_Y)}
            stroke="var(--color-grid)"
            strokeWidth="1"
            vectorEffect="non-scaling-stroke"
          />
        ))}
        {(props.refLines ?? []).map((r) => {
          const ry = built.y(r.value);
          if (ry < PAD_Y - 0.5 || ry > H - PAD_Y + 0.5) return null;
          return (
            <g key={`${r.label}-${r.value}`}>
              <line
                x1="0"
                x2={W}
                y1={ry}
                y2={ry}
                stroke="var(--color-faint)"
                strokeWidth="1"
                strokeDasharray="4 3"
                vectorEffect="non-scaling-stroke"
              />
              <text
                x={W - 2}
                y={ry - 2}
                textAnchor="end"
                fill="var(--color-faint)"
                style={{ font: "9px var(--font-mono, monospace)" }}
              >
                {r.label}
              </text>
            </g>
          );
        })}
        <path d={built.area} fill={color} opacity="0.08" />
        <path
          d={built.line}
          fill="none"
          stroke={color}
          strokeWidth="2"
          strokeLinejoin="round"
          strokeLinecap="round"
          vectorEffect="non-scaling-stroke"
        />
        {hover && (
          <>
            <line
              x1={built.x(hover.at)}
              x2={built.x(hover.at)}
              y1={PAD_Y}
              y2={H - PAD_Y}
              stroke="var(--color-faint)"
              strokeWidth="1"
              strokeDasharray="2 3"
              vectorEffect="non-scaling-stroke"
            />
            <circle
              cx={built.x(hover.at)}
              cy={built.y(hover.value)}
              r="3"
              fill={color}
              stroke="var(--color-surface)"
              strokeWidth="2"
              vectorEffect="non-scaling-stroke"
            />
          </>
        )}
      </svg>
      {hover && (
        <div
          className="pointer-events-none absolute -top-1 z-10 -translate-x-1/2 -translate-y-full whitespace-nowrap rounded border border-border bg-raised px-2 py-1 font-mono text-[11px] tabular-nums text-ink shadow-[var(--shadow-raised)]"
          style={{ left: `${(built.x(hover.at) / W) * 100}%` }}
        >
          {props.format(hover.value)}
          <span className="ml-1.5 text-faint">
            {new Date(hover.at).toLocaleTimeString()}
          </span>
        </div>
      )}
    </div>
  );
}
