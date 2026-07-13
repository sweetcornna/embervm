// Thin async facade over lib/term so the shell (App, Workspace header) can
// talk to TermManager without pulling xterm.js into the entry chunk — the
// heavy module loads only when the Terminal tab (or one of these calls)
// first needs it.

import type { SandboxState } from "../api/types";

export function noteTermState(sandboxId: string, state: SandboxState) {
  void import("./term").then((m) => m.TermManager.noteState(sandboxId, state));
}

export function disposeTermSandbox(sandboxId: string) {
  void import("./term").then((m) => m.TermManager.disposeSandbox(sandboxId));
}

export function disposeAllTerms() {
  void import("./term").then((m) => m.TermManager.disposeAll());
}
