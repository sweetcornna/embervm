// Guest file tree: lazy directory loads via GET /files?op=list, ARIA tree
// semantics with arrow-key navigation (Up/Down walk visible rows, Right
// expands, Left collapses). Directories sort first server-side.

import { useRef, useState } from "react";
import { useDirectory } from "../api/hooks";
import type { DirEntry } from "../api/types";
import { joinPath } from "../lib/files";
import {
  IconChevronDown,
  IconChevronRight,
  IconFile,
  IconFolder,
  IconFolderOpen,
} from "./icons";
import { Skeleton } from "./ui";

export function FileTree(props: {
  sandboxId: string;
  selected?: string;
  onOpenFile: (path: string) => void;
}) {
  const rootRef = useRef<HTMLDivElement>(null);

  // Up/Down move between the currently visible rows.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const rows = [...(rootRef.current?.querySelectorAll<HTMLElement>("[role=treeitem]") ?? [])];
    const i = rows.indexOf(document.activeElement as HTMLElement);
    const next = rows[i + (e.key === "ArrowDown" ? 1 : -1)];
    if (next) {
      next.focus();
      e.preventDefault();
    }
  };

  return (
    <div
      ref={rootRef}
      role="tree"
      aria-label="Guest files"
      onKeyDown={onKeyDown}
      className="h-full overflow-auto py-1 font-mono text-[12.5px]"
    >
      <DirNode
        sandboxId={props.sandboxId}
        path="/"
        depth={0}
        selected={props.selected}
        onOpenFile={props.onOpenFile}
        initiallyOpen
      />
    </div>
  );
}

function rowCls(active: boolean) {
  return `flex w-full items-center gap-1.5 truncate rounded px-2 py-[3px] text-left outline-none focus-visible:ring-1 focus-visible:ring-accent ${
    active ? "bg-accent-weak text-accent" : "text-muted hover:bg-raised hover:text-ink"
  }`;
}

function DirNode(props: {
  sandboxId: string;
  path: string;
  depth: number;
  selected?: string;
  onOpenFile: (path: string) => void;
  name?: string;
  initiallyOpen?: boolean;
}) {
  const [open, setOpen] = useState(props.initiallyOpen ?? false);
  const { data, isLoading, error } = useDirectory(props.sandboxId, props.path, open);
  const entries = data?.entries ?? [];
  const indent = { paddingLeft: `${props.depth * 14 + 8}px` };

  return (
    <div role={props.name ? "none" : undefined}>
      {props.name && (
        <button
          role="treeitem"
          aria-expanded={open}
          style={indent}
          className={rowCls(false)}
          onClick={() => setOpen(!open)}
          onKeyDown={(e) => {
            if (e.key === "ArrowRight" && !open) {
              setOpen(true);
              e.stopPropagation();
              e.preventDefault();
            }
            if (e.key === "ArrowLeft" && open) {
              setOpen(false);
              e.stopPropagation();
              e.preventDefault();
            }
          }}
        >
          <span className="shrink-0 text-faint">
            {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
          </span>
          <span className="shrink-0 text-warm">
            {open ? <IconFolderOpen size={14} /> : <IconFolder size={14} />}
          </span>
          <span className="truncate">{props.name}</span>
        </button>
      )}
      {open && (
        <div role="group">
          {isLoading && (
            <div style={{ paddingLeft: `${(props.depth + 1) * 14 + 8}px` }} className="space-y-1 py-1">
              <Skeleton className="h-3.5 w-28" />
              <Skeleton className="h-3.5 w-20" />
            </div>
          )}
          {error != null && (
            <div
              style={{ paddingLeft: `${(props.depth + 1) * 14 + 8}px` }}
              className="py-1 pr-2 text-[11px] text-danger"
            >
              {(error as Error).message}
            </div>
          )}
          {entries.map((e) => (
            <Entry
              key={e.name}
              entry={e}
              sandboxId={props.sandboxId}
              parent={props.path}
              depth={props.depth + 1}
              selected={props.selected}
              onOpenFile={props.onOpenFile}
            />
          ))}
          {data?.truncated && (
            <div
              style={{ paddingLeft: `${(props.depth + 1) * 14 + 8}px` }}
              className="py-1 text-[11px] text-faint"
            >
              … listing truncated at 10 000 entries
            </div>
          )}
          {data && entries.length === 0 && (
            <div
              style={{ paddingLeft: `${(props.depth + 1) * 14 + 8}px` }}
              className="py-1 text-[11px] text-faint"
            >
              empty
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function Entry(props: {
  entry: DirEntry;
  sandboxId: string;
  parent: string;
  depth: number;
  selected?: string;
  onOpenFile: (path: string) => void;
}) {
  const { entry } = props;
  const path = joinPath(props.parent, entry.name);
  if (entry.is_dir) {
    return (
      <DirNode
        sandboxId={props.sandboxId}
        path={path}
        name={entry.name + (entry.symlink ? " →" : "")}
        depth={props.depth}
        selected={props.selected}
        onOpenFile={props.onOpenFile}
      />
    );
  }
  return (
    <button
      role="treeitem"
      aria-selected={props.selected === path}
      style={{ paddingLeft: `${props.depth * 14 + 8 + 18}px` }}
      className={rowCls(props.selected === path)}
      onClick={() => props.onOpenFile(path)}
      title={entry.symlink ? `→ ${entry.symlink}` : undefined}
    >
      <span className="shrink-0 text-faint">
        <IconFile size={14} />
      </span>
      <span className="truncate">{entry.name}</span>
    </button>
  );
}
