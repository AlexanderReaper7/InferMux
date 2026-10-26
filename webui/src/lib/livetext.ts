// What /warden/live's raw bodies say, for the Active tab (0031): a reply's
// text, reasoning and tool calls put back together from its stream, and a
// request's messages. Anything not recognised is shown raw.

// One entry on the board, as the daemon sends it.
export interface LiveEntry {
  id: number;
  time: string;
  model: string;
  client: string;
  path: string;
  request_type: string;
  request: string;
  request_bytes: number;
  request_truncated?: boolean;
  request_kept: boolean;
  status: number;
  response_type: string;
  response: string;
  response_bytes: number;
  response_truncated?: boolean;
  response_kept: boolean;
  done: boolean;
  duration_ms?: number;
  // The bodies were cleared; the sizes, times and status stay.
  wiped?: boolean;
}

export interface Reply {
  reasoning: string;
  text: string;
  tools: { name: string; arguments: string }[];
  // Whether anything was recognised; if not, the page shows the raw body.
  parsed: boolean;
}

interface Delta {
  content?: unknown;
  reasoning_content?: unknown;
  reasoning?: unknown;
  tool_calls?: { index?: number; function?: { name?: string; arguments?: string } }[];
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

// add folds one chat message, delta or chunk into the reply.
function add(r: Reply, d: Delta, toolBase: number) {
  r.text += str(d.content);
  r.reasoning += str(d.reasoning_content) || str(d.reasoning);
  for (const [i, call] of (d.tool_calls ?? []).entries()) {
    const at = toolBase + (call.index ?? i);
    r.tools[at] ??= { name: "", arguments: "" };
    r.tools[at].name += call.function?.name ?? "";
    r.tools[at].arguments += call.function?.arguments ?? "";
  }
}

// event folds one JSON object, a stream's event or a whole reply, into r.
function event(r: Reply, e: any): boolean {
  if (!e || typeof e !== "object") return false;
  if (Array.isArray(e.choices)) {
    for (const c of e.choices) {
      if (c.delta) add(r, c.delta, 0);
      else if (c.message) add(r, c.message, 0);
      r.text += str(c.text);
    }
    return true;
  }
  // The Responses API.
  if (typeof e.type === "string") {
    if (e.type === "response.output_text.delta") r.text += str(e.delta);
    else if (e.type.startsWith("response.reasoning") && e.type.endsWith(".delta")) r.reasoning += str(e.delta);
    else if (e.type === "response.output_item.added" && e.item?.type === "function_call")
      r.tools.push({ name: str(e.item.name), arguments: "" });
    else if (e.type === "response.function_call_arguments.delta" && r.tools.length)
      r.tools[r.tools.length - 1].arguments += str(e.delta);
    // Anthropic's /v1/messages.
    else if (e.type === "content_block_delta") {
      r.text += str(e.delta?.text);
      r.reasoning += str(e.delta?.thinking);
      if (e.delta?.type === "input_json_delta" && r.tools.length) r.tools[r.tools.length - 1].arguments += str(e.delta.partial_json);
    } else if (e.type === "content_block_start" && e.content_block?.type === "tool_use")
      r.tools.push({ name: str(e.content_block.name), arguments: "" });
    return true;
  }
  // llama-server's own /completion.
  if (typeof e.content === "string") {
    r.text += e.content;
    return true;
  }
  return false;
}

// reply reads a response body, a stream or a whole JSON reply.
export function reply(body: string, contentType: string): Reply {
  const r: Reply = { reasoning: "", text: "", tools: [], parsed: false };
  if (contentType.startsWith("text/event-stream")) {
    for (const line of body.split("\n")) {
      const data = line.startsWith("data:") ? line.slice(5).trim() : "";
      if (!data.startsWith("{")) continue;
      try {
        if (event(r, JSON.parse(data))) r.parsed = true;
      } catch {
        // The last line of a stream still arriving.
      }
    }
    return r;
  }
  try {
    r.parsed = event(r, JSON.parse(body));
  } catch {
    // Not JSON: shown raw.
  }
  return r;
}

export interface Message {
  role: string;
  text: string;
}

// part is one piece of a message's content as text; an image or audio is
// named, not shown.
function part(p: any): string {
  if (typeof p === "string") return p;
  if (!p || typeof p !== "object") return "";
  if (typeof p.text === "string") return p.text;
  if (p.type === "tool_result") return content(p.content);
  return `[${p.type ?? "part"}]`;
}

function content(c: unknown): string {
  if (typeof c === "string") return c;
  if (Array.isArray(c)) return c.map(part).join("\n");
  return "";
}

export interface Prompt {
  messages: Message[];
  // Everything else in the request, the tools' definitions included.
  rest: Record<string, unknown>;
}

// prompt reads a chat, Responses or Messages request's conversation, or a
// completion's prompt. null when the body is not a JSON object.
export function prompt(body: string): Prompt | null {
  let req: any;
  try {
    req = JSON.parse(body);
  } catch {
    return null;
  }
  if (!req || typeof req !== "object" || Array.isArray(req)) return null;
  const { messages, input, prompt: text, system, instructions, ...rest } = req;
  const out: Message[] = [];
  if (typeof system === "string" || Array.isArray(system)) out.push({ role: "system", text: content(system) });
  if (typeof instructions === "string") out.push({ role: "instructions", text: instructions });
  for (const m of Array.isArray(messages) ? messages : []) {
    let text = content(m?.content);
    for (const call of m?.tool_calls ?? []) text += `${text ? "\n" : ""}→ ${call.function?.name}(${call.function?.arguments ?? ""})`;
    out.push({ role: str(m?.role) || "?", text });
  }
  if (typeof input === "string") out.push({ role: "user", text: input });
  else if (Array.isArray(input))
    for (const m of input) {
      if (m?.type === "function_call") out.push({ role: "tool call", text: `${m.name}(${m.arguments ?? ""})` });
      else if (m?.type === "function_call_output") out.push({ role: "tool", text: str(m.output) });
      else out.push({ role: str(m?.role) || str(m?.type) || "?", text: content(m?.content) });
    }
  if (typeof text === "string") out.push({ role: "prompt", text });
  else if (Array.isArray(text)) out.push(...text.map((t) => ({ role: "prompt", text: String(t) })));
  return { messages: out, rest };
}
