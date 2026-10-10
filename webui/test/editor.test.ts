// bun test, from webui/: the editor's macro completion and the K-V warning
// on a model file's cache type.
import { CompletionContext } from "@codemirror/autocomplete";
import { EditorState, type TransactionSpec } from "@codemirror/state";
import { expect, test } from "bun:test";
import { parseDocument, isScalar } from "yaml";
import { macros, type Context } from "../src/lib/editor";
import { kvDiagnostic } from "../src/lib/kv";

const ctx = (runtimes: string[]): Context => ({
  schema: () => null,
  sources: () => ({ place: [], device: [], model: [], lane: [], gguf: [] }),
  problem: () => null,
  runtimes: () => runtimes,
  kvKernels: () => null,
});

// The completion at ‸, and the text after accepting the option the typed
// prefix matches, through the option's own apply.
function at(text: string, runtimes = ["llama-server", "ik-llama"]) {
  const pos = text.indexOf("‸");
  let state = EditorState.create({ doc: text.replace("‸", "") });
  const r = macros(ctx(runtimes))(new CompletionContext(state, pos, false));
  if (!r) return null;
  const pick = r.options.find((o) => o.label.startsWith(state.sliceDoc(r.from, pos)))!;
  const view = { get state() { return state; }, dispatch: (spec: TransactionSpec) => (state = state.update(spec).state) };
  (pick.apply as Function)(view, pick, r.from, pos);
  return { labels: r.options.map((o) => o.label), accepted: state.doc.toString(), cursor: state.selection.main.head };
}

test("${ offers the runtimes, then llama-swap's macros", () => {
  expect(at("cmd: |\n  ${‸")?.labels).toEqual(["${llama-server}", "${ik-llama}", "${PORT}", "${MODEL_ID}"]);
  expect(at("cmd: |\n  ${ik‸")?.accepted).toBe("cmd: |\n  ${ik-llama}");
  // closeBrackets' brace is taken in, not doubled.
  const closed = at("cmd: |\n  ${‸}\n");
  expect(closed?.accepted).toBe("cmd: |\n  ${llama-server}\n");
  expect(closed?.cursor).toBe("cmd: |\n  ${llama-server}".length);
  expect(at("cmd: |\n  $‸")).toBeNull();
  expect(at("cmd: |\n  ${‸", [])).toBeNull();
});

test("the K-V warning sits on the cache type flag in cmd", () => {
  const kernels = { "llama-server": ["q8_0-q8_0", "f16-f16"] };
  const file = "# was --cache-type-k q4_0 once\ncmd: |\n  ${llama-server}\n    --port ${PORT}\n    --cache-type-k q4_0\n    --cache-type-v q4_0\n";
  const node = parseDocument(file).get("cmd", true);
  if (!isScalar(node) || !node.range) throw new Error("no cmd");
  const d = kvDiagnostic(String(node.value), file.slice(node.range[0], node.range[1]), node.range[0], kernels);
  expect(d && file.slice(d.from, d.to)).toBe("--cache-type-k q4_0");
  expect(d?.from).toBeGreaterThan(file.indexOf("cmd:"));
  expect(d?.message).toStartWith("llama-server has no FlashAttention kernel for q4_0-q4_0");

  const ok = "cmd: ${llama-server} --cache-type-k q8_0 --cache-type-v q8_0";
  expect(kvDiagnostic("${llama-server} --cache-type-k q8_0 --cache-type-v q8_0", ok.slice(5), 5, kernels)).toBeNull();
  // An unset pair is f16-f16; a runtime not listed is not checked.
  expect(kvDiagnostic("${llama-server} -ctv q4_0", "${llama-server} -ctv q4_0", 5, kernels)).toEqual({
    from: 21, to: 30, message: expect.stringContaining("f16-q4_0"),
  });
  expect(kvDiagnostic("${vllm} --cache-type-k q4_0", "x", 0, kernels)).toBeNull();
  // Neither flag: the runtime's line, not a block scalar's |.
  const block = "cmd: |\n  ${llama-server}\n    --port 1\n";
  const b = parseDocument(block).get("cmd", true);
  if (!isScalar(b) || !b.range) throw new Error("no cmd");
  const r = kvDiagnostic(String(b.value), block.slice(b.range[0], b.range[1]), b.range[0], { "llama-server": ["q8_0-q8_0"] });
  expect(r && block.slice(r.from, r.to)).toBe("${llama-server}");
});
