// ⌘K command palette (cmdk): navigate, jump to a sandbox, create, and act on
// the sandbox in the current workspace. Also hosts the `g`-nav wiring and
// the `?` shortcut-help overlay so all global keyboard UX lives here.

import { Command } from "cmdk";
import { useEffect, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { useSandboxes, useSandboxAction, verbs } from "../api/hooks";
import { STATE_META, stateLabel } from "./status";
import { CreateSandboxDialog } from "./createSandbox";
import { Dialog, KBD } from "./ui";
import { useI18n } from "../lib/i18n";
import { installShortcuts, onShortcut } from "../lib/shortcuts";
import { toast, toastError } from "../lib/toast";
import type { SandboxState } from "../api/types";

export function CommandPalette() {
  const { t } = useI18n();
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
    onError: toastError(t("Pause failed", "暂停失败")),
  });
  const resume = useSandboxAction(() => verbs.resume(currentId!), {
    sandboxId: currentId,
    optimistic: () => ({ state: "RESUMING" as SandboxState }),
    onError: toastError(t("Resume failed", "恢复失败")),
  });
  const snapshot = useSandboxAction(() => verbs.snapshot(currentId!, "console"), {
    onSuccess: () => toast.success(t("Snapshot taken", "已快照")),
    onError: toastError(t("Snapshot failed", "快照失败")),
  });

  return (
    <>
      <Command.Dialog
        open={open}
        onOpenChange={setOpen}
        label={t("Command palette", "命令面板")}
        className="fixed inset-0 z-[70] grid place-items-start justify-center bg-black/50 pt-[12vh] backdrop-blur-[1px]"
        // cmdk renders a wrapper; the real dialog is inside
      >
        <div className="enter-down w-[min(36rem,92vw)] overflow-hidden rounded-xl border border-border bg-raised shadow-[var(--shadow-overlay)]">
          <Command.Input
            placeholder={t("Search sandboxes, run a command…", "搜索沙箱、执行命令…")}
            className="w-full border-b border-hairline bg-transparent px-4 py-3.5 text-[14px] text-ink outline-none placeholder:text-faint"
          />
          <Command.List className="max-h-80 overflow-y-auto p-1.5">
            <Command.Empty className="px-3 py-6 text-center text-[13px] text-faint">
              {t("No matches.", "无匹配。")}
            </Command.Empty>

            <Command.Group heading={t("Navigate", "导航")} className="cmdk-group">
              <Item onSelect={() => run(() => nav("/"))}>{t("Overview", "总览")}</Item>
              <Item onSelect={() => run(() => nav("/sandboxes"))}>{t("Sandboxes", "沙箱")}</Item>
              <Item onSelect={() => run(() => nav("/nodes"))}>{t("Nodes", "节点")}</Item>
              <Item onSelect={() => run(() => nav("/templates"))}>{t("Templates", "模板")}</Item>
              <Item onSelect={() => run(() => nav("/storage"))}>{t("Storage", "存储")}</Item>
            </Command.Group>

            <Command.Group heading={t("Actions", "操作")} className="cmdk-group">
              <Item onSelect={() => run(() => setCreating(true))}>{t("Create sandbox…", "创建沙箱…")}</Item>
              {currentId && (
                <>
                  <Item onSelect={() => run(() => pause.mutate())}>{t("Pause this sandbox", "暂停此沙箱")}</Item>
                  <Item onSelect={() => run(() => resume.mutate())}>{t("Resume this sandbox", "恢复此沙箱")}</Item>
                  <Item onSelect={() => run(() => snapshot.mutate())}>{t("Snapshot this sandbox", "为此沙箱快照")}</Item>
                  <Item onSelect={() => run(() => nav(`/sandboxes/${currentId}/terminal`))}>
                    {t("Open terminal", "打开终端")}
                  </Item>
                </>
              )}
              <Item onSelect={() => run(() => setHelpOpen(true))}>{t("Keyboard shortcuts", "键盘快捷键")}</Item>
            </Command.Group>

            {(sandboxes.data ?? []).length > 0 && (
              <Command.Group heading={t("Sandboxes", "沙箱")} className="cmdk-group">
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
                      <span className="text-faint">{stateLabel(sb.state, t)}</span>
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
  const { t } = useI18n();
  const rows: Array<[React.ReactNode, string]> = [
    [<KBD key="k">⌘K</KBD>, t("Command palette", "命令面板")],
    [
      <span key="g" className="flex gap-1">
        <KBD>g</KBD> <KBD>o/s/n/t/g</KBD>
      </span>,
      t("Go to Overview / Sandboxes / Nodes / Templates / storaGe", "跳转到 总览 / 沙箱 / 节点 / 模板 / 存储(storaGe)"),
    ],
    [<KBD key="s">⌘S</KBD>, t("Save file (in the editor)", "保存文件（在编辑器中）")],
    [<KBD key="e">⌘↵</KBD>, t("Run (in a command field)", "运行（在命令输入框中）")],
    [<KBD key="q">?</KBD>, t("This help", "本帮助")],
  ];
  return (
    <Dialog title={t("Keyboard shortcuts", "键盘快捷键")} open={props.open} onClose={props.onClose}>
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
