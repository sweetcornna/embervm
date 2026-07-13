// Files tab: guest file tree | CodeMirror editor, split-pane. Guards: binary
// sniff, size ceilings, unsaved-changes confirm. Mutations (upload / new
// file) go through PUT /files; mkdir/rm stay in the terminal for now.

import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import type { Sandbox } from "../../api/types";
import {
  IconDownload,
  IconFile,
  IconPlus,
  IconRefresh,
  IconUpload,
} from "../../components/icons";
import { SplitPane } from "../../components/split";
import { FileTree } from "../../components/tree";
import { Tip } from "../../components/tooltip";
import {
  Button,
  ConfirmDialog,
  Dialog,
  Empty,
  IconButton,
  Mono,
  Spinner,
  inputCls,
} from "../../components/ui";
import {
  EDIT_MAX_BYTES,
  EDIT_WARN_BYTES,
  baseName,
  downloadGuestFile,
  joinPath,
  languageOf,
  looksBinary,
  normalizePath,
  readGuestFile,
  writeGuestFile,
} from "../../lib/files";
import { useI18n } from "../../lib/i18n";
import { toast } from "../../lib/toast";
import { Editor } from "./Editor";

interface OpenFile {
  path: string;
  text: string;
  size: number;
  binary: boolean;
  tooLarge: boolean;
  readOnly: boolean;
  generation: number; // bump to force an editor remount (revert/reload)
}

export function FilesTab(props: { sb: Sandbox }) {
  const { sb } = props;
  const qc = useQueryClient();
  const { t } = useI18n();
  const [file, setFile] = useState<OpenFile | null>(null);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saving, setSaving] = useState(false);
  const [pendingOpen, setPendingOpen] = useState<string | null>(null);
  const [newFileOpen, setNewFileOpen] = useState(false);
  const uploadRef = useRef<HTMLInputElement>(null);
  const genRef = useRef(1);
  // The editor owns the buffer; this mirror lets the toolbar Save work.
  const latestTextRef = useRef("");
  const running = sb.state === "RUNNING";

  const refreshTree = () =>
    qc.invalidateQueries({ queryKey: ["sandboxes", sb.id, "dir"] });

  async function openFile(path: string, force = false) {
    if (dirty && !force) {
      setPendingOpen(path);
      return;
    }
    setBusy(true);
    setDirty(false);
    try {
      const bytes = await readGuestFile(sb.id, path);
      const binary = looksBinary(bytes);
      const tooLarge = bytes.length > EDIT_MAX_BYTES;
      const text = binary || tooLarge ? "" : new TextDecoder().decode(bytes);
      latestTextRef.current = text;
      setFile({
        path,
        text,
        size: bytes.length,
        binary,
        tooLarge,
        readOnly: !binary && !tooLarge && bytes.length > EDIT_WARN_BYTES,
        generation: genRef.current++,
      });
    } catch (err) {
      toast.error(`${t("Open", "打开")} ${baseName(path)} ${t("failed", "失败")}`, (err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function save(text: string) {
    if (!file || file.readOnly || file.binary) return;
    setSaving(true);
    try {
      await writeGuestFile(sb.id, file.path, text);
      setFile({ ...file, text, size: new TextEncoder().encode(text).length });
      setDirty(false);
      toast.success(`${t("Saved", "已保存")} ${baseName(file.path)}`);
    } catch (err) {
      toast.error(t("Save failed", "保存失败"), (err as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function createFile(path: string) {
    try {
      await writeGuestFile(sb.id, path, "");
      await refreshTree();
      await openFile(path, true);
    } catch (err) {
      toast.error(t("Create failed", "创建失败"), (err as Error).message);
    }
  }

  async function upload(f: File, dir: string) {
    try {
      await writeGuestFile(sb.id, joinPath(dir, f.name), f);
      await refreshTree();
      toast.success(`${t("Uploaded", "已上传")} ${f.name}`, `${t("to", "至")} ${dir}`);
    } catch (err) {
      toast.error(t("Upload failed", "上传失败"), (err as Error).message);
    }
  }

  if (!running)
    return (
      <Empty>
        <div className="mx-auto max-w-sm space-y-2">
          <IconFile size={22} className="mx-auto text-faint" />
          <p>{t("The file browser needs a running guest.", "文件浏览器需要运行中的 guest。")}</p>
          <p className="text-faint">{t("Resume the sandbox from the header to browse its disk.", "从顶部恢复沙箱即可浏览其磁盘。")}</p>
        </div>
      </Empty>
    );

  const currentDir = file ? file.path.slice(0, file.path.lastIndexOf("/")) || "/" : "/";

  return (
    <>
      <SplitPane
        storageKey="files"
        left={
          <div className="flex h-full min-h-0 flex-col border-r border-hairline">
            <div className="flex items-center justify-between border-b border-hairline px-2 py-1">
              <span className="px-1 font-mono text-[10px] uppercase tracking-[0.14em] text-faint">
                {t("guest disk", "guest 磁盘")}
              </span>
              <div className="flex">
                <Tip content={t("New file", "新建文件")}>
                  <IconButton label={t("New file", "新建文件")} onClick={() => setNewFileOpen(true)}>
                    <IconPlus size={13} />
                  </IconButton>
                </Tip>
                <Tip content={`${t("Upload to", "上传到")} ${currentDir}`}>
                  <IconButton label={t("Upload file", "上传文件")} onClick={() => uploadRef.current?.click()}>
                    <IconUpload size={13} />
                  </IconButton>
                </Tip>
                <Tip content={t("Refresh listing", "刷新列表")}>
                  <IconButton label={t("Refresh", "刷新")} onClick={() => void refreshTree()}>
                    <IconRefresh size={13} />
                  </IconButton>
                </Tip>
              </div>
              <input
                ref={uploadRef}
                type="file"
                className="hidden"
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) void upload(f, currentDir);
                  e.target.value = "";
                }}
              />
            </div>
            <div className="min-h-0 flex-1">
              <FileTree sandboxId={sb.id} selected={file?.path} onOpenFile={(p) => void openFile(p)} />
            </div>
          </div>
        }
        right={
          <div className="flex h-full min-h-0 flex-col">
            {file && (
              <div className="flex items-center gap-2 border-b border-hairline px-3 py-1.5">
                <button
                  className="min-w-0 truncate font-mono text-[12px] text-muted hover:text-ink"
                  title={t("Copy path", "复制路径")}
                  onClick={() => {
                    void navigator.clipboard.writeText(file.path);
                    toast.info(t("Path copied", "已复制路径"));
                  }}
                >
                  {file.path}
                </button>
                {dirty && (
                  <span aria-label={t("Unsaved changes", "未保存的更改")} className="size-1.5 shrink-0 rounded-full bg-accent" />
                )}
                <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">
                  {file.size.toLocaleString()} B
                  {file.readOnly && ` · ${t("read-only (large file)", "只读（大文件）")}`}
                </span>
                <Tip content={t("Download", "下载")}>
                  <IconButton
                    label={t("Download file", "下载文件")}
                    onClick={() => void downloadGuestFile(sb.id, file.path)}
                  >
                    <IconDownload size={13} />
                  </IconButton>
                </Tip>
                <Button
                  size="sm"
                  kind="primary"
                  disabled={!dirty || file.readOnly || file.binary}
                  busy={saving}
                  onClick={() => void save(latestTextRef.current)}
                  title={`${t("Save", "保存")} (⌘S)`}
                >
                  {t("Save", "保存")}
                </Button>
              </div>
            )}
            <div className="relative min-h-0 flex-1">
              {busy && (
                <div className="absolute inset-0 z-10 grid place-items-center bg-bg/60">
                  <Spinner />
                </div>
              )}
              {!file && (
                <Empty>
                  <div className="mx-auto max-w-xs space-y-1.5">
                    <p>{t("Select a file to view or edit it.", "选择一个文件以查看或编辑。")}</p>
                    <p className="text-faint">
                      {t("Saves go straight to the guest disk", "保存直接写入 guest 磁盘")} (<Mono>PUT /files</Mono>).
                    </p>
                  </div>
                </Empty>
              )}
              {file?.binary && (
                <Empty>
                  <div className="space-y-3">
                    <p>
                      <Mono>{baseName(file.path)}</Mono> {t("looks binary", "看起来是二进制文件")} ({file.size.toLocaleString()} B).
                    </p>
                    <Button onClick={() => void downloadGuestFile(sb.id, file.path)}>
                      <IconDownload size={13} /> {t("Download", "下载")}
                    </Button>
                  </div>
                </Empty>
              )}
              {file?.tooLarge && !file.binary && (
                <Empty>
                  <div className="space-y-3">
                    <p>
                      <Mono>{baseName(file.path)}</Mono> {t("is", "为")} {(file.size / 1048576).toFixed(1)} MiB —{" "}
                      {t("too large to edit here", "过大，无法在此编辑")}.
                    </p>
                    <Button onClick={() => void downloadGuestFile(sb.id, file.path)}>
                      <IconDownload size={13} /> {t("Download", "下载")}
                    </Button>
                  </div>
                </Empty>
              )}
              {file && !file.binary && !file.tooLarge && (
                <Editor
                  key={`${file.path}#${file.generation}`}
                  initialValue={file.text}
                  language={languageOf(baseName(file.path))}
                  readOnly={file.readOnly}
                  onDirty={setDirty}
                  onChange={(text) => {
                    latestTextRef.current = text;
                  }}
                  onSave={(text) => void save(text)}
                />
              )}
            </div>
          </div>
        }
      />

      <ConfirmDialog
        open={pendingOpen !== null}
        title={t("Discard unsaved changes?", "放弃未保存的更改？")}
        body={
          <>
            <Mono className="text-ink">{file ? baseName(file.path) : ""}</Mono>{" "}
            {t("has unsaved changes. Opening another file discards them.", "有未保存的更改。打开其他文件将丢弃它们。")}
          </>
        }
        confirmLabel={t("Discard changes", "放弃更改")}
        onConfirm={() => {
          const next = pendingOpen;
          setPendingOpen(null);
          if (next) void openFile(next, true);
        }}
        onClose={() => setPendingOpen(null)}
      />
      <NewFileDialog
        open={newFileOpen}
        dir={currentDir}
        onClose={() => setNewFileOpen(false)}
        onCreate={(p) => {
          setNewFileOpen(false);
          void createFile(p);
        }}
      />
    </>
  );
}

function NewFileDialog(props: {
  open: boolean;
  dir: string;
  onClose: () => void;
  onCreate: (path: string) => void;
}) {
  const { t } = useI18n();
  const [path, setPath] = useState("");
  useEffect(() => {
    if (props.open) setPath(props.dir === "/" ? "/" : props.dir + "/");
  }, [props.open, props.dir]);
  const valid = path.startsWith("/") && !path.endsWith("/") && path.length > 1;
  return (
    <Dialog title={t("New file", "新建文件")} open={props.open} onClose={props.onClose}>
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (valid) props.onCreate(normalizePath(path));
        }}
      >
        <label className="block">
          <div className="mb-1.5 text-xs font-medium text-muted">{t("Absolute path", "绝对路径")}</div>
          <input
            className={`${inputCls} font-mono`}
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="/workspace/main.py"
          />
        </label>
        <div className="flex justify-end gap-2">
          <Button onClick={props.onClose}>{t("Cancel", "取消")}</Button>
          <Button kind="primary" type="submit" disabled={!valid}>
            {t("Create", "创建")}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
