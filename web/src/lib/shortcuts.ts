// Global keyboard shortcuts: ⌘K palette, `g`-then-key navigation, `?` help.
// A tiny event bus so App can own the listener while any component subscribes.
// Suppressed while typing in an input, textarea, contenteditable, or the
// terminal — those own the keyboard.

type Handler = () => void;

const listeners = new Map<string, Set<Handler>>();

export function onShortcut(event: string, fn: Handler): () => void {
  let set = listeners.get(event);
  if (!set) {
    set = new Set();
    listeners.set(event, set);
  }
  set.add(fn);
  return () => set!.delete(fn);
}

function emit(event: string) {
  listeners.get(event)?.forEach((fn) => fn());
}

function inEditable(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  const tag = el.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    el.isContentEditable ||
    el.closest(".xterm") !== null ||
    el.closest(".cm-editor") !== null
  );
}

// `g` starts a two-key sequence; it expires quickly.
let goArmed = false;
let goTimer: ReturnType<typeof setTimeout> | null = null;

const GO_MAP: Record<string, string> = {
  o: "go:overview",
  s: "go:sandboxes",
  n: "go:nodes",
  t: "go:templates",
  g: "go:storage", // g g → storage (mnemonic: last)
};

export function installShortcuts(): () => void {
  const onKey = (e: KeyboardEvent) => {
    // ⌘K / Ctrl-K opens the palette even from most contexts.
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      emit("palette:open");
      return;
    }
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (inEditable(e.target)) return;

    if (goArmed) {
      goArmed = false;
      if (goTimer) clearTimeout(goTimer);
      const ev = GO_MAP[e.key.toLowerCase()];
      if (ev) {
        e.preventDefault();
        emit(ev);
      }
      return;
    }
    if (e.key === "g") {
      goArmed = true;
      goTimer = setTimeout(() => {
        goArmed = false;
      }, 1200);
      return;
    }
    if (e.key === "?") {
      e.preventDefault();
      emit("help:open");
    }
  };
  window.addEventListener("keydown", onKey);
  return () => window.removeEventListener("keydown", onKey);
}
