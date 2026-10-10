// The one config's draft: what the editor completes from it, and the edits
// the add-model commands make to it (0022, 9).
import { parse, stringify } from "yaml";
import type { Sources } from "./yamlschema";

export const mainFile = "infermux.yaml";

export function modelFile(name: string) {
  return `models/${name}.yaml`;
}

export function modelNames(files: Record<string, string>): string[] {
  return Object.keys(files)
    .filter((f) => /^models\/[^/]+\.yaml$/.test(f))
    .map((f) => f.slice("models/".length, -".yaml".length))
    .sort();
}

function load(text: string | undefined): any {
  try {
    return parse(text ?? "") ?? {};
  } catch {
    return null;
  }
}

// sources are what the draft offers the editor; null while infermux.yaml
// does not parse, so the caller keeps the last ones.
export function sources(files: Record<string, string>, ggufs: string[]): Sources | null {
  const main = load(files[mainFile]);
  if (main === null || typeof main !== "object") return null;
  const place: string[] = [];
  const device = new Set<string>();
  for (const [host, h] of Object.entries<any>(main.hosts ?? {})) {
    for (const d of Object.keys(h?.devices ?? {})) {
      place.push(`${host}/${d}`);
      device.add(d);
    }
  }
  const model = modelNames(files);
  const groups = Object.keys(main.groups ?? {});
  return { place, device: [...device], model, lane: [...model, ...groups.filter((g) => !model.includes(g))], gguf: ggufs };
}

// fromModel is cmd taken from another model on the same runtime: its model
// path replaced, its other .gguf files (mmproj, draft) left out. null when
// no model's cmd starts with the runtime.
export function fromModel(files: Record<string, string>, runtime: string, path: string): { cmd: string; from: string } | null {
  for (const name of modelNames(files)) {
    const cmd = load(files[modelFile(name)])?.cmd;
    if (typeof cmd !== "string" || !cmd.trimStart().startsWith("${" + runtime + "}")) continue;
    const out = cmd.replace(/(\s+)(--?[\w-]+)(\s+)(\S+\.gguf)(?=\s|$)/g, (_, sp, flag, sp2) =>
      flag === "--model" || flag === "-m" ? `${sp}${flag}${sp2}${path}` : "",
    );
    return { cmd: out, from: name };
  }
  return null;
}

// newModel is a model file for one GGUF.
export function newModel(files: Record<string, string>, runtime: string, path: string, sha256: string, hf: string | null): string {
  const like = fromModel(files, runtime, path);
  const cmd = like?.cmd ?? `\${${runtime}}\n  --port \${PORT}\n  --model ${path}\n`;
  const file: Record<string, string> = { sha256 };
  if (hf) file.hf = hf;
  const head = like ? `# The command is ${like.from}'s, without its other .gguf files. Check its flags.\n` : "";
  return head + stringify({ cmd: cmd.endsWith("\n") ? cmd : cmd + "\n", files: { [path]: file } });
}

// addToLane appends `name: [place]` to the lane's block of infermux.yaml,
// leaving the rest of the text as it is.
export function addToLane(text: string, lane: "balance" | "failover", name: string, place: string): string {
  const entry = `${name}: [${place}]`;
  const lines = text.split("\n");
  const start = lines.findIndex((l) => new RegExp(`^${lane}:\\s*(\\{\\s*\\})?\\s*(#.*)?$`).test(l));
  if (start < 0) return `${text.replace(/\n*$/, "\n")}${lane}:\n  ${entry}\n`;
  if (/\{\s*\}/.test(lines[start])) {
    lines.splice(start, 1, `${lane}:`, `  ${entry}`);
    return lines.join("\n");
  }
  let last = start;
  let indent = "  ";
  for (let i = start + 1; i < lines.length; i++) {
    const l = lines[i];
    if (l.trim() === "") continue;
    if (!/^\s/.test(l)) {
      if (l.startsWith("#")) continue;
      break;
    }
    if (last === start) indent = /^\s*/.exec(l)![0];
    last = i;
  }
  lines.splice(last + 1, 0, indent + entry);
  return lines.join("\n");
}
