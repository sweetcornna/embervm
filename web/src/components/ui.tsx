import type { ReactNode } from "react";
import { Component, useEffect, useRef, useState } from "react";
import { useI18n } from "../lib/i18n";

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
      <ErrorPanel
        message={this.state.error.message}
        onRetry={() => {
          this.setState({ error: null });
          this.props.onReset?.();
        }}
      />
    );
  }
}

// Functional so it can translate (the class boundary above cannot use hooks).
function ErrorPanel(props: { message: string; onRetry: () => void }) {
  const { t } = useI18n();
  return (
    <div className="grid min-h-64 place-items-center p-8">
      <div className="max-w-md text-center">
        <div className="mb-2 font-mono text-xs uppercase tracking-widest text-danger">
          {t("render error", "渲染错误")}
        </div>
        <p className="mb-4 text-sm text-muted">
          {t(
            "This view hit an unexpected error. The rest of the console is unaffected.",
            "此视图遇到意外错误，控制台其余部分不受影响。",
          )}
        </p>
        <pre className="mb-4 overflow-x-auto rounded border border-hairline bg-surface p-3 text-left font-mono text-xs text-faint">
          {props.message}
        </pre>
        <Button onClick={props.onRetry}>{t("Retry", "重试")}</Button>
      </div>
    </div>
  );
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
  const { t } = useI18n();
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
          aria-label={t("Close", "关闭")}
          className="rounded px-1.5 py-0.5 text-muted hover:bg-raised hover:text-ink"
        >
          ✕
        </button>
      </div>
      <div className="p-5">{props.children}</div>
    </dialog>
  );
}

/* ── Confirm dialog ──────────────────────────────────────────────────────
   The one replacement for window.confirm: states the consequence, focuses
   Cancel (destructive default must not be a stray Enter away). */
export function ConfirmDialog(props: {
  open: boolean;
  title: string;
  body: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const cancelRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (props.open) cancelRef.current?.focus();
  }, [props.open]);
  return (
    <Dialog title={props.title} open={props.open} onClose={props.onClose}>
      <div className="space-y-4">
        <div className="text-[13px] leading-relaxed text-muted">{props.body}</div>
        <div className="flex justify-end gap-2">
          <button
            ref={cancelRef}
            type="button"
            onClick={props.onClose}
            className="inline-flex items-center rounded-md border border-border bg-raised/40 px-3 py-1.5 text-[13px] font-medium text-ink hover:bg-raised"
          >
            {t("Cancel", "取消")}
          </button>
          <Button kind="danger" onClick={props.onConfirm} busy={props.busy}>
            {props.confirmLabel}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}

/** Imperative confirm state helper: `const c = useConfirm(); c.ask(fn)`. */
export function useConfirm() {
  const [pending, setPending] = useState<(() => void) | null>(null);
  return {
    open: pending !== null,
    ask: (fn: () => void) => setPending(() => fn),
    confirm: () => {
      pending?.();
      setPending(null);
    },
    close: () => setPending(null),
  };
}

/* ── Drawer (right-hand detail panel) ────────────────────────────────── */
export function Drawer(props: {
  title: ReactNode;
  open: boolean;
  onClose: () => void;
  children: ReactNode;
}) {
  const { t } = useI18n();
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
      className="enter-up fixed inset-y-0 right-0 m-0 ml-auto h-dvh max-h-dvh w-[min(30rem,92vw)] border-l border-border bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/60 backdrop:backdrop-blur-[2px]"
    >
      <div className="flex h-full flex-col">
        <div className="flex items-center justify-between border-b border-hairline px-5 py-3.5">
          <h2 className="min-w-0 truncate text-sm font-semibold tracking-tight">{props.title}</h2>
          <button
            onClick={props.onClose}
            aria-label={t("Close", "关闭")}
            className="rounded px-1.5 py-0.5 text-muted hover:bg-raised hover:text-ink"
          >
            ✕
          </button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-5">{props.children}</div>
      </div>
    </dialog>
  );
}

/* ── Capacity bar (nodes) ────────────────────────────────────────────── */
export function CapacityBar(props: {
  label: string;
  used: number;
  total: number;
  fmt: (n: number) => string;
}) {
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

/* ── Skeleton / KBD / IconButton ─────────────────────────────────────── */
export function Skeleton(props: { className?: string }) {
  return (
    <div
      aria-hidden
      className={`skeleton rounded bg-raised ${props.className ?? "h-4 w-24"}`}
    />
  );
}

export function KBD(props: { children: ReactNode }) {
  return (
    <kbd className="rounded border border-border bg-bg px-1.5 py-0.5 font-mono text-[10px] text-muted">
      {props.children}
    </kbd>
  );
}

export function IconButton(props: {
  children: ReactNode;
  label: string;
  onClick?: () => void;
  disabled?: boolean;
  danger?: boolean;
}) {
  return (
    <button
      type="button"
      aria-label={props.label}
      title={props.label}
      onClick={props.onClick}
      disabled={props.disabled}
      className={`inline-flex size-7 items-center justify-center rounded-md transition-colors disabled:cursor-not-allowed disabled:opacity-40 ${
        props.danger
          ? "text-muted hover:bg-danger/10 hover:text-danger"
          : "text-muted hover:bg-raised hover:text-ink"
      }`}
    >
      {props.children}
    </button>
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
