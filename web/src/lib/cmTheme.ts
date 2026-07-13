// CodeMirror 6 theme built from the console's design tokens — the editor
// must look native to the slate/ember system, not like a bolted-on one-dark.

import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import type { Extension } from "@codemirror/state";
import { EditorView } from "codemirror";
import { tags as t } from "@lezer/highlight";

const colors = {
  bg: "#0c0e13",
  surface: "#12151c",
  raised: "#191d26",
  border: "#262c39",
  hairline: "#1c212b",
  ink: "#e7eaf0",
  muted: "#9aa3b2",
  faint: "#616b7c",
  accent: "#f5a524",
  ok: "#3fb454",
  cold: "#4a94dd",
  danger: "#e5534b",
  warm: "#b98150",
  magenta: "#b07dd6",
  cyan: "#3fb0b4",
};

const editorChrome = EditorView.theme(
  {
    "&": {
      backgroundColor: colors.bg,
      color: colors.ink,
      fontSize: "13px",
      height: "100%",
    },
    ".cm-content": {
      fontFamily: '"JetBrains Mono Variable", ui-monospace, monospace',
      caretColor: colors.accent,
      padding: "8px 0",
    },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: colors.accent },
    "&.cm-focused": { outline: "none" },
    "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, ::selection":
      { backgroundColor: `${colors.accent}30` },
    ".cm-selectionBackground": { backgroundColor: `${colors.accent}22` },
    ".cm-activeLine": { backgroundColor: `${colors.raised}66` },
    ".cm-gutters": {
      backgroundColor: colors.bg,
      color: colors.faint,
      borderRight: `1px solid ${colors.hairline}`,
    },
    ".cm-activeLineGutter": {
      backgroundColor: `${colors.raised}66`,
      color: colors.muted,
    },
    ".cm-lineNumbers .cm-gutterElement": { padding: "0 8px 0 12px" },
    ".cm-searchMatch": {
      backgroundColor: `${colors.accent}2a`,
      outline: `1px solid ${colors.accent}55`,
    },
    ".cm-matchingBracket": {
      backgroundColor: `${colors.accent}25`,
      outline: `1px solid ${colors.accent}50`,
    },
    ".cm-tooltip": {
      backgroundColor: colors.raised,
      border: `1px solid ${colors.border}`,
      color: colors.ink,
    },
    ".cm-panels": {
      backgroundColor: colors.surface,
      color: colors.ink,
      borderTop: `1px solid ${colors.hairline}`,
    },
  },
  { dark: true },
);

const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.controlKeyword, t.moduleKeyword], color: colors.magenta },
  { tag: [t.string, t.special(t.string)], color: colors.ok },
  { tag: [t.number, t.bool, t.null, t.atom], color: colors.warm },
  { tag: [t.comment, t.blockComment, t.lineComment], color: colors.faint, fontStyle: "italic" },
  { tag: [t.function(t.variableName), t.function(t.propertyName)], color: colors.cold },
  { tag: [t.typeName, t.className, t.namespace], color: colors.cyan },
  { tag: [t.propertyName, t.attributeName], color: colors.ink },
  { tag: [t.variableName, t.definition(t.variableName)], color: colors.ink },
  { tag: [t.operator, t.punctuation, t.bracket], color: colors.muted },
  { tag: [t.meta, t.processingInstruction], color: colors.accent },
  { tag: t.heading, color: colors.accent, fontWeight: "600" },
  { tag: t.link, color: colors.cold, textDecoration: "underline" },
  { tag: t.emphasis, fontStyle: "italic" },
  { tag: t.strong, fontWeight: "600" },
  { tag: t.invalid, color: colors.danger },
]);

export const emberTheme: Extension = [editorChrome, syntaxHighlighting(highlight)];
