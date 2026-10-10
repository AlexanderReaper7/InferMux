// Completion and hover for the one config's YAML, from the JSON Schema that
// infermux-ui serves (internal/placement/schema.go). The path to the cursor
// is read from the indentation rather than parsed, so it works on a draft
// that does not parse: the line being typed is usually what breaks it.
//
// The schema marks what the draft fills in: "x-complete-keys" names what a
// mapping's keys are, "x-complete" what a string is.

export type Schema = Record<string, any>;

// A key, or 0 for a list item.
export type Step = string | 0;

export type Kind = "place" | "device" | "model" | "lane" | "gguf";
export type Sources = Record<Kind, string[]>;

export interface Cursor {
  path: Step[];
  mode: "key" | "value";
  // A list item being typed after "- ": a value, or the first key of a mapping.
  item: boolean;
  prefix: string;
  // The keys the mapping already has.
  present: string[];
}

export interface Option {
  label: string;
  type: "property" | "enum" | "variable";
  info?: string;
  apply?: string;
}

interface Line {
  indent: number;
  dashes: number;
  keyCol: number;
  key: string | null;
  // "key:" with nothing after it, so the lines under it are its value.
  open: boolean;
  // "key: |" or ">", so the lines under it are text.
  block: boolean;
}

const lineRe = /^( *)((?:-(?: +|$))*)(.*)$/;
const keyRe = /^("[^"]*"|'[^']*'|[^\s#'"[\]{},][^#]*?):(?:\s+(.*))?$/;

function unquote(key: string) {
  return /^(["']).*\1$/.test(key) ? key.slice(1, -1) : key;
}

function info(text: string): Line | null {
  if (/^\s*(#.*)?$/.test(text)) return null;
  const m = lineRe.exec(text)!;
  const kv = keyRe.exec(m[3]);
  const value = (kv?.[2] ?? "").replace(/(^|\s)#.*$/, "").trim();
  return {
    indent: m[1].length,
    dashes: (m[2].match(/-/g) ?? []).length,
    keyCol: m[1].length + m[2].length,
    key: kv ? unquote(kv[1]) : null,
    open: kv !== null && value === "",
    block: kv !== null && /^[|>][-+0-9]*$/.test(value),
  };
}

// parents is the path to the mapping or list that holds a line at col,
// looking up from line n; null inside a block of text or a broken nesting.
// forDash: the line is a list item, which may sit at its key's own column.
function parents(lines: string[], n: number, col: number, forDash: boolean): Step[] | null {
  for (let i = n - 1; i >= 0; i--) {
    const l = info(lines[i]);
    if (!l) continue;
    if (l.dashes === 0) {
      if (l.indent < col || (forDash && l.indent === col && l.open)) {
        if (l.key === null || l.block) return null;
        const up = parents(lines, i, l.indent, false);
        return up && [...up, l.key];
      }
      continue;
    }
    if (col <= l.indent) continue;
    const up = parents(lines, i, l.indent, true);
    if (!up) return null;
    const item: Step[] = [...up, ...Array<Step>(l.dashes).fill(0)];
    if (col > l.keyCol && l.key !== null) return l.block ? null : [...item, l.key];
    return item;
  }
  return [];
}

// siblings are the keys of the mapping a key at col on line n belongs to.
function siblings(lines: string[], n: number, col: number): string[] {
  const keys: string[] = [];
  const at = (l: Line) => {
    if (l.keyCol === col && l.key !== null) keys.push(l.key);
  };
  for (let i = n - 1; i >= 0; i--) {
    const l = info(lines[i]);
    if (!l || l.indent > col) continue;
    if (l.indent < col) {
      if (l.dashes) at(l);
      break;
    }
    at(l);
  }
  for (let i = n + 1; i < lines.length; i++) {
    const l = info(lines[i]);
    if (!l || l.indent > col) continue;
    if (l.indent < col) break;
    at(l);
  }
  return keys;
}

function base(lines: string[], n: number, indent: number, dashes: number, keyCol: number): Step[] | null {
  if (!dashes) return parents(lines, n, keyCol, false);
  const up = parents(lines, n, indent, true);
  return up && [...up, ...Array<Step>(dashes).fill(0)];
}

// cursor is what is being typed at line n, column ch; null where nothing
// can be offered, such as a comment or a block of text.
export function cursor(lines: string[], n: number, ch: number): Cursor | null {
  const m = lineRe.exec(lines[n].slice(0, ch))!;
  const indent = m[1].length;
  const dashes = (m[2].match(/-/g) ?? []).length;
  const keyCol = indent + m[2].length;
  const rest = m[3];
  if (/(^|\s)#/.test(rest)) return null;
  const path = base(lines, n, indent, dashes, keyCol);
  if (!path) return null;
  const kv = keyRe.exec(rest);
  if (kv && kv[2] !== undefined) {
    const key = unquote(kv[1]);
    const value = kv[2];
    if (/^[|>]/.test(value)) return null;
    const flow = /^\[(?:[^[\]{}]*,)?\s*([^,[\]{}]*)$/.exec(value);
    if (flow) return { path: [...path, key, 0], mode: "value", item: true, prefix: flow[1], present: [] };
    if (/^[[{]/.test(value)) return null;
    return { path: [...path, key], mode: "value", item: false, prefix: value, present: [] };
  }
  if (rest.includes(":")) return null;
  return { path, mode: "key", item: dashes > 0, prefix: rest, present: siblings(lines, n, keyCol) };
}

function deref(root: Schema, s: Schema): Schema {
  const ref = s.$ref as string | undefined;
  if (!ref?.startsWith("#/")) return s;
  let node: any = root;
  for (const part of ref.slice(2).split("/")) node = node?.[part];
  return node ?? {};
}

// expand follows $ref and opens oneOf and anyOf into their alternatives.
function expand(root: Schema, list: Schema[]): Schema[] {
  const out: Schema[] = [];
  for (const raw of list) {
    const s = deref(root, raw);
    const alts = s.oneOf ?? s.anyOf;
    if (alts) out.push(...expand(root, alts));
    else out.push(s);
  }
  return out;
}

// at is the schemas a path can be, before the last one is expanded, so a
// field's own description is still there.
function at(root: Schema, path: Step[]): Schema[] {
  let cur: Schema[] = [root];
  for (const step of path) {
    const next: Schema[] = [];
    for (const s of expand(root, cur)) {
      if (step === 0) {
        if (s.items && typeof s.items === "object") next.push(s.items);
      } else if (s.properties?.[step]) next.push(s.properties[step]);
      else if (s.additionalProperties && typeof s.additionalProperties === "object") next.push(s.additionalProperties);
    }
    cur = next;
  }
  return cur;
}

export function describe(root: Schema, s: Schema): string {
  const d = deref(root, s);
  const parts = [s.description ?? d.description ?? ""];
  if (d.enum) parts.push(`One of: ${d.enum.join(", ")}.`);
  if (d.default !== undefined) parts.push(`Default: ${JSON.stringify(d.default)}.`);
  return parts.filter(Boolean).join("\n\n");
}

export function complete(root: Schema, c: Cursor, src: Sources): Option[] {
  const raw = at(root, c.path);
  const out: Option[] = [];
  const add = (o: Option) => {
    if (!out.some((p) => p.label === o.label)) out.push(o);
  };
  for (const s of expand(root, raw)) {
    if (c.mode === "value" || c.item) {
      for (const v of s.enum ?? []) add({ label: String(v), type: "enum" });
      if (s.type === "boolean") ["true", "false"].forEach((v) => add({ label: v, type: "enum" }));
      for (const v of src[s["x-complete"] as Kind] ?? []) add({ label: v, type: "variable" });
    }
    if (c.mode === "key" && (s.type === undefined || s.type === "object")) {
      for (const [k, p] of Object.entries<Schema>(s.properties ?? {})) {
        if (!c.present.includes(k)) add({ label: k, type: "property", info: describe(root, p), apply: k + ": " });
      }
      for (const k of src[s["x-complete-keys"] as Kind] ?? []) {
        if (!c.present.includes(k)) add({ label: k, type: "variable", apply: k + ": " });
      }
    }
  }
  return out;
}

// hover is the description of the key under column ch of line n.
export function hover(root: Schema, lines: string[], n: number, ch: number): { from: number; to: number; text: string } | null {
  const m = lineRe.exec(lines[n])!;
  const kv = keyRe.exec(m[3]);
  const indent = m[1].length;
  const keyCol = indent + m[2].length;
  if (!kv || ch < keyCol || ch > keyCol + kv[1].length) return null;
  const path = base(lines, n, indent, (m[2].match(/-/g) ?? []).length, keyCol);
  if (!path) return null;
  const text = at(root, [...path, unquote(kv[1])])
    .map((s) => describe(root, s))
    .find(Boolean);
  return text ? { from: keyCol, to: keyCol + kv[1].length, text } : null;
}
