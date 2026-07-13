// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { installShortcuts, onShortcut } from "./shortcuts";

function press(key: string, opts: KeyboardEventInit = {}, target?: EventTarget) {
  const ev = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...opts });
  (target ?? window).dispatchEvent(ev);
  return ev;
}

describe("shortcuts", () => {
  let uninstall: () => void;
  afterEach(() => uninstall?.());

  it("⌘K opens the palette and is preventDefaulted", () => {
    uninstall = installShortcuts();
    const fn = vi.fn();
    onShortcut("palette:open", fn);
    const ev = press("k", { metaKey: true });
    expect(fn).toHaveBeenCalledOnce();
    expect(ev.defaultPrevented).toBe(true);
  });

  it("g-then-o navigates to overview; the sequence expires", () => {
    vi.useFakeTimers();
    uninstall = installShortcuts();
    const fn = vi.fn();
    onShortcut("go:overview", fn);

    press("g");
    press("o");
    expect(fn).toHaveBeenCalledOnce();

    // g alone, then wait past the window: the next key is not a go-target.
    press("g");
    vi.advanceTimersByTime(1500);
    press("o");
    expect(fn).toHaveBeenCalledOnce(); // still 1
    vi.useRealTimers();
  });

  it("? opens help", () => {
    uninstall = installShortcuts();
    const fn = vi.fn();
    onShortcut("help:open", fn);
    press("?");
    expect(fn).toHaveBeenCalledOnce();
  });

  it("is suppressed while typing in an input", () => {
    uninstall = installShortcuts();
    const fn = vi.fn();
    onShortcut("go:sandboxes", fn);
    const input = document.createElement("input");
    document.body.appendChild(input);
    press("g", {}, input);
    press("s", {}, input);
    expect(fn).not.toHaveBeenCalled();
    input.remove();
  });

  it("⌘K still fires from within an input (global open)", () => {
    uninstall = installShortcuts();
    const fn = vi.fn();
    onShortcut("palette:open", fn);
    const input = document.createElement("input");
    document.body.appendChild(input);
    press("k", { metaKey: true }, input);
    expect(fn).toHaveBeenCalledOnce();
    input.remove();
  });

  it("unsubscribing stops delivery", () => {
    uninstall = installShortcuts();
    const fn = vi.fn();
    const off = onShortcut("help:open", fn);
    off();
    press("?");
    expect(fn).not.toHaveBeenCalled();
  });
});
