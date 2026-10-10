// The one config's YAML editor (0022, 9): CodeMirror with the page's black,
// completion and hover from the schema, and two lints: the YAML parser's on
// every change, and the daemons' own loader through /api/config/check.
import {
  autocompletion,
  pickedCompletion,
  startCompletion,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from "@codemirror/autocomplete";
import { indentWithTab } from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import { linter, lintGutter, type Diagnostic } from "@codemirror/lint";
import { EditorState, Prec, type Extension } from "@codemirror/state";
import { oneDark } from "@codemirror/theme-one-dark";
import { EditorView, hoverTooltip, keymap } from "@codemirror/view";
import { basicSetup } from "codemirror";
import { isScalar, parseDocument } from "yaml";
import { kvDiagnostic } from "./kv";
import { complete, cursor, hover, type Schema, type Sources } from "./yamlschema";

// Over One Dark's own background, whatever the order of the extensions.
export const black = Prec.highest(EditorView.theme(
  {
    "&": { backgroundColor: "#0a0a0a", height: "100%" },
    ".cm-gutters": { backgroundColor: "#0a0a0a", borderRight: "1px solid #262626" },
    ".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace" },
    ".cm-tooltip": { backgroundColor: "#171717", border: "1px solid #404040" },
    ".cm-schema-doc": { maxWidth: "42rem", padding: "0.4rem 0.6rem", whiteSpace: "pre-wrap", color: "#d4d4d4" },
  },
  { dark: true },
));

export interface Context {
  schema(): Schema | null;
  sources(): Sources;
  // The loader's problem in this file, when it is about this text.
  problem(text: string): { line: number; message: string } | null;
  // A model file's: the base config's runtime macros, and per runtime the
  // K-V cache pairs it was compiled for (ui.kvKernels).
  runtimes(): string[];
  kvKernels(): Record<string, string[]> | null;
}

function completion(ctx: Context) {
  return (c: CompletionContext): CompletionResult | null => {
    const schema = ctx.schema();
    if (!schema) return null;
    const line = c.state.doc.lineAt(c.pos);
    const cur = cursor(c.state.doc.toJSON(), line.number - 1, c.pos - line.from);
    if (!cur) return null;
    const options = complete(schema, cur, ctx.sources());
    if (!options.length) return null;
    return {
      from: c.pos - cur.prefix.length,
      options: options.map((o) => ({ label: o.label, type: o.type, info: o.info || undefined, apply: o.apply })),
      validFor: /^[^\s:,[\]{}]*$/,
    };
  };
}

// A macro in a model's text: `${` offers the runtimes, first, and llama-swap's
// own. The closing brace closeBrackets typed is replaced, not doubled; it
// stays out of the range CodeMirror filters on.
export function macros(ctx: Context) {
  const apply = (view: EditorView, c: Completion, from: number, to: number) => {
    const end = view.state.sliceDoc(to, to + 1) === "}" ? to + 1 : to;
    view.dispatch({
      changes: { from, to: end, insert: c.label },
      selection: { anchor: from + c.label.length },
      annotations: pickedCompletion.of(c),
      userEvent: "input.complete",
    });
  };
  return (c: CompletionContext): CompletionResult | null => {
    const typed = c.matchBefore(/\$\{[\w.-]*/);
    const runtimes = ctx.runtimes();
    if (!typed || !runtimes.length) return null;
    return {
      from: typed.from,
      options: [
        ...runtimes.map((n) => ({ label: "${" + n + "}", type: "function", boost: 1, apply })),
        ...["PORT", "MODEL_ID"].map((n) => ({ label: "${" + n + "}", type: "variable", apply })),
      ],
      validFor: /^\$\{[\w.-]*$/,
    };
  };
}

// CodeMirror asks the source on every typed character, so a value is
// offered after a typed "key: ", "- " or "[". A completed key leaves "key: "
// without typing, so the values are asked for here.
const opensAfterKey = EditorView.updateListener.of((u) => {
  if (!u.transactions.some((t) => t.isUserEvent("input.complete"))) return;
  const pos = u.state.selection.main.head;
  if (u.state.sliceDoc(Math.max(0, pos - 2), pos) === ": ") startCompletion(u.view);
});

function tooltip(ctx: Context) {
  return hoverTooltip((view, pos) => {
    const schema = ctx.schema();
    if (!schema) return null;
    const line = view.state.doc.lineAt(pos);
    const h = hover(schema, view.state.doc.toJSON(), line.number - 1, pos - line.from);
    if (!h) return null;
    return {
      pos: line.from + h.from,
      end: line.from + h.to,
      above: true,
      create() {
        const dom = document.createElement("div");
        dom.className = "cm-schema-doc";
        dom.textContent = h.text;
        return { dom };
      },
    };
  });
}

function lint(ctx: Context) {
  return linter(
    (view) => {
      const text = view.state.doc.toString();
      const doc = parseDocument(text, { prettyErrors: false });
      const clamp = (n: number) => Math.min(Math.max(n, 0), text.length);
      const out: Diagnostic[] = [
        ...doc.errors.map((e) => ({ from: clamp(e.pos[0]), to: clamp(Math.max(e.pos[1], e.pos[0])), severity: "error" as const, message: e.message })),
        ...doc.warnings.map((e) => ({ from: clamp(e.pos[0]), to: clamp(Math.max(e.pos[1], e.pos[0])), severity: "warning" as const, message: e.message })),
      ];
      const cmd = doc.errors.length ? undefined : doc.get("cmd", true);
      if (isScalar(cmd) && typeof cmd.value === "string" && cmd.range) {
        const kv = kvDiagnostic(cmd.value, text.slice(cmd.range[0], cmd.range[1]), cmd.range[0], ctx.kvKernels());
        if (kv) out.push({ ...kv, severity: "warning" });
      }
      // The loader reports a YAML error too, with a line only.
      const p = doc.errors.length ? null : ctx.problem(text);
      if (p) {
        const line = view.state.doc.line(Math.min(Math.max(p.line, 1), view.state.doc.lines));
        out.push({ from: line.from, to: line.to, severity: "error", message: p.message });
      }
      return out;
    },
    { delay: 250 },
  );
}

export function extensions(ctx: Context, onchange: (text: string) => void): Extension[] {
  return [
    basicSetup,
    keymap.of([indentWithTab]),
    EditorState.tabSize.of(2),
    yaml(),
    oneDark,
    black,
    lintGutter(),
    lint(ctx),
    autocompletion({ override: [completion(ctx), macros(ctx)] }),
    opensAfterKey,
    tooltip(ctx),
    EditorView.updateListener.of((u) => {
      if (u.docChanged) onchange(u.state.doc.toString());
    }),
  ];
}

// The other side of a comparison: highlighted, not editable.
export const readOnly: Extension[] = [basicSetup, yaml(), oneDark, black, EditorState.readOnly.of(true), EditorView.editable.of(false)];
