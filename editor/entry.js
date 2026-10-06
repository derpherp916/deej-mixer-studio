// Bundled into ui/editor.js: exposes window.DeejEditor.create(parent, text, onChange)
import { basicSetup } from "codemirror";
import { EditorView, keymap } from "@codemirror/view";
import { EditorState } from "@codemirror/state";
import { indentWithTab } from "@codemirror/commands";
import { cpp } from "@codemirror/lang-cpp";
import { oneDark } from "@codemirror/theme-one-dark";

window.DeejEditor = {
  create(parent, text, onChange) {
    const view = new EditorView({
      parent,
      state: EditorState.create({
        doc: text,
        extensions: [
          basicSetup, cpp(), oneDark, keymap.of([indentWithTab]),
          EditorView.theme({ "&": { height: "100%", fontSize: "13.5px" }, ".cm-scroller": { fontFamily: "Cascadia Code, Consolas, monospace" } }),
          EditorView.updateListener.of(u => { if (u.docChanged && onChange) onChange(); }),
        ],
      }),
    });
    return {
      get: () => view.state.doc.toString(),
      set: t => view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: t } }),
      focus: () => view.focus(),
      gotoLine: n => { const l = view.state.doc.line(Math.max(1, Math.min(n, view.state.doc.lines)));
        view.dispatch({ selection: { anchor: l.from }, scrollIntoView: true }); view.focus(); },
    };
  },
};
