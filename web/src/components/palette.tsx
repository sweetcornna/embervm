// ⌘K command palette (cmdk): navigate, jump to a sandbox, create, and act on
// the sandbox in the current workspace. Also hosts the `g`-nav wiring and
// the `?` shortcut-help overlay so all global keyboard UX lives here.

import { Command } from "cmdk";
import { useEffect, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { useSandboxes, useSandboxAction, verbs } from "../api/hooks";
import { STATE_META } from "./status";
import { CreateSandboxDialog } from "./createSandbox";
import { Dialog, KBD } from "./ui";
import { installShortcuts, onShortcut } from "../lib/shortcuts";
import { toast, toastError } from "../lib/toast";
import type { SandboxState } from "../api/types";

export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const nav = useNavigate();
  const loc = useLocation();
  const params = useParams();
  const sandboxes = useSandboxes();
  const currentId = params.id;

  useEffect(() => installShortcuts(), []);
  useEffect(() => {
    const subs = [
      onShortcut("palette:open", () => setOpen((v) => !v)),
      onShortcut("help:open", () => setHelpOpen(true)),
      onShortcut("go:overview", () => nav("/")),
      onShortcut("go:sandboxes", () => nav("/sandboxes")),
      onShortcut("go:nodes", () => nav("/nodes")),
      onShortcut("go:templates", () => nav("/templates")),
      onShortcut("go:storage", () => nav("/storage")),
    ];
    return () => subs.forEach((off) => off());
  }, [nav]);

  // Close on navigation.
  useEffect(() => setOpen(false), [loc.pathname]);

  const run = (fn: () => void) => {
    setOpen(false);
    fn();
  };

  const pause = useSandboxAction(() => verbs.pause(currentId!), {
    sandboxId: currentId,
    optimistic: () => ({ state: "PAUSING" as SandboxState }),
    onError: toastError("Pause failed"),
  });
  const resume = useSandboxAction(() => verbs.resume(currentId!), {
    sandboxId: currentId,
    optimistic: () => ({ state: "RESUMING" as SandboxState }),
    onError: toastError("Resume failed"),
  });
  const snapshot = useSandboxAction(() => verbs.snapshot(currentId!, "console"), {
    onSuccess: () => toast.success("Snapshot taken"),
    onError: toastError("Snapshot failed"),
  });

  return (
    <>
      <Command.Dialog
        open={open}
        onOpenChange={setOpen}
        label="Command palette"
        className="fixed inset-0 z-[70] grid place-items-start justify-center bg-black/50 pt-[12vh] backdrop-blur-[1px]"
        // cmdk renders a wrapper; the real dialog is inside
      >
        <div className="enter-down w-[min(36rem,92vw)] overflow-hidden rounded-xl border border-border bg-raised shadow-[var(--shadow-overlay)]">
          <Command.Input
            placeholder="Search sandboxes, run a command…"
            className="w-full border-b border-hairline bg-transparent px-4 py-3.5 text-[14px] text-ink outline-none placeholder:text-faint"
          />
          <Command.List className="max-h-80 overflow-y-auto p-1.5">
            <Command.Empty className="px-3 py-6 text-center text-[13px] text-faint">
              No matches.
            </Command.Empty>

            <Command.Group heading="Navigate" className="cmdk-group">
              <Item onSelect={() => run(() => nav("/"))}>Overview</Item>
              <Item onSelect={() => run(() => nav("/sandboxes"))}>Sandboxes</Item>
              <Item onSelect={() => run(() => nav("/nodes"))}>Nodes</Item>
              <Item onSelect={() => run(() => nav("/templates"))}>Templates</Item>
              <Item onSelect={() => run(() => nav("/storage"))}>Storage</Item>
            </Command.Group>

            <Command.Group heading="Actions" className="cmdk-group">
              <Item onSelect={() => run(() => setCreating(true))}>Create sandbox…</Item>
              {currentId && (
                <>
                  <Item onSelect={() => run(() => pause.mutate())}>Pause this sandbox</Item>
                  <Item onSelect={() => run(() => resume.mutate())}>Resume this sandbox</Item>
                  <Item onSelect={() => run(() => snapshot.mutate())}>Snapshot this sandbox</Item>
                  <Item onSelect={() => run(() => nav(`/sandboxes/${currentId}/terminal`))}>
                    Open terminal
                  </Item>
                </>
              )}
              <Item onSelect={() => run(() => setHelpOpen(true))}>Keyboard shortcuts</Item>
            </Command.Group>

            {(sandboxes.data ?? []).length > 0 && (
              <Command.Group heading="Sandboxes" className="cmdk-group">
                {(sandboxes.data ?? []).slice(0, 50).map((sb) => (
                  <Item
                    key={sb.id}
                    value={`sandbox ${sb.id} ${sb.template_id} ${sb.state}`}
                    onSelect={() => run(() => nav(`/sandboxes/${sb.id}`))}
                  >
                    <span className="flex items-center gap-2">
                      <span
                        aria-hidden
                        className="size-1.5 rounded-full"
                        style={{ background: STATE_META[sb.state]?.color ?? "var(--color-idle)" }}
                      />
                      <span className="font-mono">{sb.id.slice(0, 8)}</span>
                      <span className="text-faint">{STATE_META[sb.state]?.label ?? sb.state}</span>
                    </span>
                  </Item>
                ))}
              </Command.Group>
            )}
          </Command.List>
        </div>
      </Command.Dialog>

      <CreateSandboxDialog open={creating} onClose={() => setCreating(false)} />
      <HelpDialog open={helpOpen} onClose={() => setHelpOpen(false)} />
    </>
  );
}

function Item(props: { children: React.ReactNode; onSelect: () => void; value?: string }) {
  return (
    <Command.Item
      value={props.value}
      onSelect={props.onSelect}
      className="flex cursor-default items-center justify-between rounded-md px-3 py-2 text-[13px] text-muted data-[selected=true]:bg-overlay data-[selected=true]:text-ink"
    >
      {props.children}
    </Command.Item>
  );
}

function HelpDialog(props: { open: boolean; onClose: () => void }) {
  const rows: Array<[React.ReactNode, string]> = [
    [<KBD key="k">⌘K</KBD>, "Command palette"],
    [
      <span key="g" className="flex gap-1">
        <KBD>g</KBD> <KBD>o/s/n/t/g</KBD>
      </span>,
      "Go to Overview / Sandboxes / Nodes / Templates / storaGe",
    ],
    [<KBD key="s">⌘S</KBD>, "Save file (in the editor)"],
    [<KBD key="e">⌘↵</KBD>, "Run (in a command field)"],
    [<KBD key="q">?</KBD>, "This help"],
  ];
  return (
    <Dialog title="Keyboard shortcuts" open={props.open} onClose={props.onClose}>
      <dl className="space-y-2.5">
        {rows.map(([keys, desc], i) => (
          <div key={i} className="flex items-center justify-between gap-4">
            <dt className="shrink-0">{keys}</dt>
            <dd className="text-right text-[13px] text-muted">{desc}</dd>
          </div>
        ))}
      </dl>
    </Dialog>
  );
}
