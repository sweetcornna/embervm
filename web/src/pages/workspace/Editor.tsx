// CodeMirror 6 wrapper. The view lives for the mounted file (keyed by path
// in FilesTab); doc edits report dirtiness upward and Mod-S saves.

import { css } from "@codemirror/lang-css";
import { html } from "@codemirror/lang-html";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";
import { markdown } from "@codemirror/lang-markdown";
import { python } from "@codemirror/lang-python";
import { yaml } from "@codemirror/lang-yaml";
import { StreamLanguage } from "@codemirror/language";
import { dockerFile } from "@codemirror/legacy-modes/mode/dockerfile";
import { go } from "@codemirror/legacy-modes/mode/go";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { toml } from "@codemirror/legacy-modes/mode/toml";
import type { Extension } from "@codemirror/state";
import { EditorState } from "@codemirror/state";
import { keymap } from "@codemirror/view";
import { EditorView, basicSetup } from "codemirror";
import { useEffect, useRef } from "react";
import { emberTheme } from "../../lib/cmTheme";

function languageExt(id: string): Extension {
  switch (id) {
    case "javascript":
      return javascript({ typescript: true, jsx: true });
    case "python":
      return python();
    case "json":
      return json();
    case "markdown":
      return markdown();
    case "html":
      return html();
    case "css":
      return css();
    case "yaml":
      return yaml();
    case "shell":
      return StreamLanguage.define(shell);
    case "go":
      return StreamLanguage.define(go);
    case "toml":
      return StreamLanguage.define(toml);
    case "dockerfile":
      return StreamLanguage.define(dockerFile);
    default:
      return [];
  }
}

export function Editor(props: {
  initialValue: string;
  language: string;
  readOnly?: boolean;
  onDirty: (dirty: boolean) => void;
  onChange?: (text: string) => void;
  onSave: (text: string) => void;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  // Latest callbacks without re-creating the view.
  const cbRef = useRef({ onDirty: props.onDirty, onChange: props.onChange, onSave: props.onSave });
  cbRef.current = { onDirty: props.onDirty, onChange: props.onChange, onSave: props.onSave };

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const initial = props.initialValue;
    const view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: initial,
        extensions: [
          basicSetup,
          emberTheme,
          languageExt(props.language),
          EditorState.readOnly.of(props.readOnly ?? false),
          EditorView.lineWrapping,
          keymap.of([
            {
              key: "Mod-s",
              preventDefault: true,
              run: (v) => {
                cbRef.current.onSave(v.state.doc.toString());
                return true;
              },
            },
          ]),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) {
              const text = u.state.doc.toString();
              cbRef.current.onDirty(text !== initial);
              cbRef.current.onChange?.(text);
            }
          }),
        ],
      }),
    });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
    // The view is intentionally created once per file: FilesTab keys this
    // component by path+generation, so a revert/reload remounts it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return <div ref={hostRef} className="h-full min-h-0 overflow-hidden" />;
}
