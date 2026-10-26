// bun test, from webui/: the Active tab's reading of /warden/live's bodies.
import { expect, test } from "bun:test";
import { prompt, reply } from "../src/lib/livetext";

const sse = (...events: unknown[]) => events.map((e) => `data: ${typeof e === "string" ? e : JSON.stringify(e)}\n\n`).join("");

test("a chat stream's text, reasoning and tool call", () => {
  const body = sse(
    { choices: [{ delta: { role: "assistant" } }] },
    { choices: [{ delta: { reasoning_content: "think" } }] },
    { choices: [{ delta: { content: "Hel" } }] },
    { choices: [{ delta: { content: "lo" } }] },
    { choices: [{ delta: { tool_calls: [{ index: 0, function: { name: "read", arguments: '{"p' } }] } }] },
    { choices: [{ delta: { tool_calls: [{ index: 0, function: { arguments: '":1}' } }] } }] },
    "[DONE]",
  );
  expect(reply(body, "text/event-stream")).toEqual({
    reasoning: "think",
    text: "Hello",
    tools: [{ name: "read", arguments: '{"p":1}' }],
    parsed: true,
  });
});

test("a stream cut mid-line, as it arrives", () => {
  const body = sse({ choices: [{ delta: { content: "a" } }] }) + 'data: {"choices":[{"del';
  expect(reply(body, "text/event-stream").text).toBe("a");
});

test("a whole chat reply", () => {
  const body = JSON.stringify({ choices: [{ message: { content: "done", reasoning_content: "r" } }] });
  expect(reply(body, "application/json")).toMatchObject({ text: "done", reasoning: "r", parsed: true });
});

test("the Responses API's and Anthropic's streams", () => {
  expect(
    reply(
      sse(
        { type: "response.created" },
        { type: "response.reasoning_text.delta", delta: "hm" },
        { type: "response.output_text.delta", delta: "ok" },
        { type: "response.output_item.added", item: { type: "function_call", name: "f" } },
        { type: "response.function_call_arguments.delta", delta: "{}" },
      ),
      "text/event-stream",
    ),
  ).toEqual({ reasoning: "hm", text: "ok", tools: [{ name: "f", arguments: "{}" }], parsed: true });
  expect(
    reply(
      sse(
        { type: "content_block_delta", delta: { type: "thinking_delta", thinking: "t" } },
        { type: "content_block_delta", delta: { type: "text_delta", text: "x" } },
      ),
      "text/event-stream",
    ),
  ).toMatchObject({ reasoning: "t", text: "x" });
});

test("an embedding is not read, so it is shown raw", () => {
  expect(reply(JSON.stringify({ data: [{ embedding: [0.1] }] }), "application/json").parsed).toBe(false);
  expect(reply("not json", "text/plain").parsed).toBe(false);
});

test("a chat request's messages, parts and tool calls", () => {
  const body = JSON.stringify({
    model: "qwen",
    stream: true,
    messages: [
      { role: "system", content: "be brief" },
      { role: "user", content: [{ type: "text", text: "what is this" }, { type: "image_url", image_url: { url: "data:..." } }] },
      { role: "assistant", content: null, tool_calls: [{ function: { name: "look", arguments: "{}" } }] },
      { role: "tool", content: "a cat" },
    ],
  });
  expect(prompt(body)).toEqual({
    messages: [
      { role: "system", text: "be brief" },
      { role: "user", text: "what is this\n[image_url]" },
      { role: "assistant", text: "→ look({})" },
      { role: "tool", text: "a cat" },
    ],
    rest: { model: "qwen", stream: true },
  });
});

test("a Responses request, a completion and a body that is not JSON", () => {
  expect(prompt(JSON.stringify({ instructions: "i", input: "hi" }))?.messages).toEqual([
    { role: "instructions", text: "i" },
    { role: "user", text: "hi" },
  ]);
  expect(prompt(JSON.stringify({ prompt: "once upon" }))?.messages).toEqual([{ role: "prompt", text: "once upon" }]);
  expect(prompt("--boundary")).toBeNull();
  expect(prompt("[1,2]")).toBeNull();
});
