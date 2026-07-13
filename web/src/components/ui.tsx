import type { ReactNode } from "react";
import { Component, useEffect, useRef } from "react";

/* ── Error boundary ──────────────────────────────────────────────────────
   A render throw must degrade to a readable panel, never a white screen.
   Keyed by route so navigating away clears a caught error. */
export class ErrorBoundary extends Component<
  { children: ReactNode; onReset?: () => void },
  { error: Error | null }
> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="grid min-h-64 place-items-center p-8">
        <div className="max-w-md text-center">
          <div className="mb-2 font-mono text-xs uppercase tracking-widest text-danger">
            render error
          </div>
          <p className="mb-4 text-sm text-muted">
            This view hit an unexpected error. The rest of the console is unaffected.
          </p>
          <pre className="mb-4 overflow-x-auto rounded border border-hairline bg-surface p-3 text-left font-mono text-xs text-faint">
            {this.state.error.message}
          </pre>
          <Button
            onClick={() => {
              this.setState({ error: null });
              this.props.onReset?.();
            }}
          >
            Retry
          </Button>
        </div>
      </div>
    );
  }
}

/* ── Page scaffold ───────────────────────────────────────────────────── */
export function PageHeader(props: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="mb-6 flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold tracking-tight text-ink">{props.title}</h1>
        {props.subtitle && <p className="mt-1 text-[13px] text-muted">{props.subtitle}</p>}
      </div>
      {props.actions && <div className="flex items-center gap-2">{props.actions}</div>}
    </header>
  );
}

export function Card(props: { title?: ReactNode; children: ReactNode; actions?: ReactNode; pad?: boolean }) {
  const pad = props.pad ?? true;
  return (
    <section className="overflow-hidden rounded-[var(--radius)] border border-border bg-surface">
      {(props.title || props.actions) && (
        <header className="flex items-center justify-between gap-3 border-b border-hairline px-4 py-3">
          <h2 className="text-[13px] font-semibold tracking-tight text-ink">{props.title}</h2>
          {props.actions}
        </header>
      )}
      <div className={pad ? "p-4" : ""}>{props.children}</div>
    </section>
  );
}

/* ── Stat tile (KPI row) ─────────────────────────────────────────────── */
export function Stat(props: { label: ReactNode; value: ReactNode; sub?: ReactNode; accent?: boolean }) {
  return (
    <div className="rounded-[var(--radius)] border border-border bg-surface px-4 py-3">
      <div className="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">{props.label}</div>
      <div
        className={`mt-1 text-2xl font-semibold tabular-nums tracking-tight ${
          props.accent ? "text-accent" : "text-ink"
        }`}
      >
        {props.value}
      </div>
      {props.sub && <div className="mt-0.5 text-xs text-muted">{props.sub}</div>}
    </div>
  );
}

/* ── Button ──────────────────────────────────────────────────────────── */
export function Button(props: {
  children: ReactNode;
  onClick?: () => void;
  kind?: "primary" | "default" | "danger" | "ghost";
  size?: "sm" | "md";
  disabled?: boolean;
  busy?: boolean;
  type?: "button" | "submit";
  title?: string;
}) {
  const kind = props.kind ?? "default";
  const size = props.size ?? "md";
  const sizing = size === "sm" ? "px-2.5 py-1 text-xs" : "px-3 py-1.5 text-[13px]";
  const look = {
    primary: "bg-accent text-[#1a1206] font-semibold hover:bg-accent-hover",
    default: "border border-border bg-raised/40 text-ink hover:bg-raised hover:border-[#31394a]",
    danger: "border border-danger/40 text-danger hover:bg-danger/10",
    ghost: "text-muted hover:bg-raised hover:text-ink",
  }[kind];
  return (
    <button
      type={props.type ?? "button"}
      className={`inline-flex items-center gap-1.5 rounded-md font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40 ${sizing} ${look}`}
      onClick={props.onClick}
      disabled={props.disabled || props.busy}
      title={props.title}
    >
      {props.busy && <Spinner />}
      {props.children}
    </button>
  );
}

export function Spinner() {
  return (
    <span
      aria-hidden
      className="spin inline-block size-3 rounded-full border-[1.5px] border-current border-t-transparent"
    />
  );
}

/* ── Form primitives ─────────────────────────────────────────────────── */
export function Field(props: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="block">
      <div className="mb-1.5 text-xs font-medium text-muted">{props.label}</div>
      {props.children}
      {props.hint && <div className="mt-1 text-xs text-faint">{props.hint}</div>}
    </label>
  );
}

export const inputCls =
  "w-full rounded-md border border-border bg-bg px-2.5 py-1.5 text-[13px] text-ink placeholder:text-faint focus:border-accent focus:outline-none";

export function Toggle(props: { checked: boolean; onChange: (v: boolean) => void; label: ReactNode }) {
  return (
    <label className="flex cursor-pointer items-center gap-2.5 text-[13px] text-ink">
      <button
        type="button"
        role="switch"
        aria-checked={props.checked}
        onClick={() => props.onChange(!props.checked)}
        className={`relative h-[18px] w-8 shrink-0 rounded-full transition-colors ${
          props.checked ? "bg-accent" : "bg-overlay"
        }`}
      >
        <span
          className={`absolute top-0.5 size-3.5 rounded-full bg-white transition-transform ${
            props.checked ? "translate-x-[15px]" : "translate-x-0.5"
          }`}
        />
      </button>
      <span>{props.label}</span>
    </label>
  );
}

/* ── Dialog ──────────────────────────────────────────────────────────── */
export function Dialog(props: { title: string; open: boolean; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (props.open && !d.open) d.showModal();
    if (!props.open && d.open) d.close();
  }, [props.open]);
  return (
    <dialog
      ref={ref}
      onClose={props.onClose}
      onClick={(e) => {
        if (e.target === ref.current) props.onClose();
      }}
      className="m-auto w-[min(32rem,calc(100vw-2rem))] rounded-lg border border-border bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/70 backdrop:backdrop-blur-sm"
    >
      <div className="flex items-center justify-between border-b border-hairline px-5 py-3.5">
        <h2 className="text-sm font-semibold tracking-tight">{props.title}</h2>
        <button
          onClick={props.onClose}
          aria-label="Close"
          className="rounded px-1.5 py-0.5 text-muted hover:bg-raised hover:text-ink"
        >
          ✕
        </button>
      </div>
      <div className="p-5">{props.children}</div>
    </dialog>
  );
}

/* ── Feedback ────────────────────────────────────────────────────────── */
export function ErrorNote(props: { error: unknown }) {
  if (!props.error) return null;
  const msg = props.error instanceof Error ? props.error.message : String(props.error);
  return (
    <p
      role="alert"
      className="flex items-start gap-2 rounded-md border border-danger/35 bg-danger/8 px-3 py-2 text-[13px] text-[#f3a6a2]"
    >
      <span aria-hidden className="mt-0.5 text-danger">
        ⚠
      </span>
      <span className="min-w-0 break-words">{msg}</span>
    </p>
  );
}

export function Empty(props: { children: ReactNode }) {
  return <div className="px-4 py-10 text-center text-[13px] text-faint">{props.children}</div>;
}

export function Mono(props: { children: ReactNode; className?: string }) {
  return <span className={`font-mono text-[12.5px] ${props.className ?? ""}`}>{props.children}</span>;
}

/* ── Table shell ─────────────────────────────────────────────────────── */
export function Table(props: { head: ReactNode[]; children: ReactNode }) {
  return (
    <div className="overflow-x-auto rounded-[var(--radius)] border border-border bg-surface">
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr className="border-b border-hairline text-left">
            {props.head.map((h, i) => (
              <th
                key={i}
                className="px-4 py-2.5 font-mono text-[10px] font-medium uppercase tracking-[0.12em] text-faint"
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{props.children}</tbody>
      </table>
    </div>
  );
}
