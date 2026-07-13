// Two-pane horizontal split with a draggable, keyboard-operable separator.
// Width persists per storageKey so the file browser remembers its layout.

import type { ReactNode } from "react";
import { useCallback, useRef, useState } from "react";

export function SplitPane(props: {
  left: ReactNode;
  right: ReactNode;
  storageKey: string;
  defaultLeft?: number; // px
  minLeft?: number;
  minRight?: number;
}) {
  const { minLeft = 200, minRight = 380, defaultLeft = 280 } = props;
  const key = `embervm.split.${props.storageKey}`;
  const rootRef = useRef<HTMLDivElement>(null);
  const [leftW, setLeftW] = useState<number>(() => {
    const saved = Number(localStorage.getItem(key));
    return saved >= minLeft ? saved : defaultLeft;
  });

  const clamp = useCallback(
    (w: number) => {
      const total = rootRef.current?.clientWidth ?? 1200;
      return Math.min(Math.max(w, minLeft), Math.max(minLeft, total - minRight));
    },
    [minLeft, minRight],
  );

  const commit = (w: number) => {
    const v = clamp(w);
    setLeftW(v);
    localStorage.setItem(key, String(Math.round(v)));
  };

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    const startX = e.clientX;
    const startW = leftW;
    const el = e.currentTarget;
    el.setPointerCapture(e.pointerId);
    const move = (ev: PointerEvent) => commit(startW + (ev.clientX - startX));
    const up = () => {
      el.removeEventListener("pointermove", move);
      el.removeEventListener("pointerup", up);
    };
    el.addEventListener("pointermove", move);
    el.addEventListener("pointerup", up);
  };

  return (
    <div ref={rootRef} className="flex h-full min-h-0 w-full">
      <div style={{ width: leftW }} className="min-h-0 shrink-0 overflow-hidden">
        {props.left}
      </div>
      <div
        role="separator"
        aria-orientation="vertical"
        aria-valuenow={Math.round(leftW)}
        aria-valuemin={minLeft}
        aria-label="Resize panels"
        tabIndex={0}
        onPointerDown={onPointerDown}
        onKeyDown={(e) => {
          if (e.key === "ArrowLeft") commit(leftW - 16);
          if (e.key === "ArrowRight") commit(leftW + 16);
        }}
        className="group relative w-1 shrink-0 cursor-col-resize"
      >
        <div className="absolute inset-y-0 left-0 w-px bg-hairline transition-colors group-hover:bg-accent group-focus-visible:bg-accent" />
      </div>
      <div className="min-h-0 min-w-0 flex-1">{props.right}</div>
    </div>
  );
}
