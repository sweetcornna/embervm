import type { ReactNode } from "react";
import { useEffect, useRef } from "react";

export function Card(props: { title?: ReactNode; children: ReactNode; actions?: ReactNode }) {
  return (
    <section className="rounded-md border border-border bg-surface">
      {(props.title || props.actions) && (
        <header className="flex items-center justify-between gap-3 border-b border-hairline px-4 py-2.5">
          <h2 className="font-display text-sm font-semibold tracking-wide text-ink">{props.title}</h2>
          {props.actions}
        </header>
      )}
      <div className="p-4">{props.children}</div>
    </section>
  );
}

export function Button(props: {
  children: ReactNode;
  onClick?: () => void;
  kind?: "primary" | "quiet" | "danger";
  disabled?: boolean;
  busy?: boolean;
  type?: "button" | "submit";
  title?: string;
}) {
  const kind = props.kind ?? "quiet";
  const base =
    "inline-flex items-center gap-1.5 rounded px-3 py-1.5 text-[13px] font-medium transition-colors disabled:opacity-45 disabled:cursor-not-allowed";
  const look =
    kind === "primary"
      ? "bg-ember text-bg hover:bg-[#ffb264]"
      : kind === "danger"
        ? "border border-alarm/50 text-alarm hover:bg-alarm/10"
        : "border border-border text-ink hover:bg-raised";
  return (
    <button
      type={props.type ?? "button"}
      className={`${base} ${look}`}
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
      className="inline-block size-3 animate-spin rounded-full border-[1.5px] border-current border-t-transparent"
    />
  );
}

export function Field(props: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="block">
      <div className="mb-1 font-mono text-[11px] uppercase tracking-wider text-muted">{props.label}</div>
      {props.children}
      {props.hint && <div className="mt-1 text-xs text-faint">{props.hint}</div>}
    </label>
  );
}

export const inputCls =
  "w-full rounded border border-border bg-bg px-2.5 py-1.5 text-sm text-ink placeholder:text-faint focus:border-ember focus:outline-none";

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
      className="m-auto w-full max-w-lg rounded-lg border border-border bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/60"
    >
      <div className="flex items-center justify-between border-b border-hairline px-5 py-3">
        <h2 className="font-display text-base font-semibold">{props.title}</h2>
        <button
          onClick={props.onClose}
          aria-label="Close"
          className="rounded px-2 py-0.5 text-muted hover:bg-raised hover:text-ink"
        >
          ✕
        </button>
      </div>
      <div className="p-5">{props.children}</div>
    </dialog>
  );
}

export function ErrorNote(props: { error: unknown }) {
  if (!props.error) return null;
  const msg = props.error instanceof Error ? props.error.message : String(props.error);
  return (
    <p role="alert" className="rounded border border-alarm/40 bg-alarm/10 px-3 py-2 text-sm text-[#f1a3a6]">
      {msg}
    </p>
  );
}

export function Empty(props: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-faint">{props.children}</p>;
}

export function Mono(props: { children: ReactNode; className?: string }) {
  return <span className={`font-mono text-[13px] ${props.className ?? ""}`}>{props.children}</span>;
}
