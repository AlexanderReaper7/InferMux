// bun test, from webui/: the editor's reading of the YAML at the cursor.
import { describe as group, expect, test } from "bun:test";
import { complete, cursor, hover, type Schema, type Sources } from "../src/lib/yamlschema";

// ‸ marks the cursor.
function at(text: string) {
  const i = text.indexOf("‸");
  const before = text.slice(0, i).split("\n");
  const lines = (text.slice(0, i) + text.slice(i + 1)).split("\n");
  return { lines, n: before.length - 1, ch: before[before.length - 1].length };
}

function cur(text: string) {
  const { lines, n, ch } = at(text);
  return cursor(lines, n, ch);
}

group("cursor", () => {
  test("a key at the top", () => {
    expect(cur("hosts:\n  a: {}\nbal‸")).toEqual({ path: [], mode: "key", item: false, prefix: "bal", present: ["hosts"] });
  });
  test("a key two levels down, with its siblings", () => {
    expect(cur("hosts:\n  strix:\n    url: x\n    ‸\n    devices: {}\nbalance: {}")).toMatchObject({
      path: ["hosts", "strix"],
      mode: "key",
      present: ["url", "devices"],
    });
  });
  test("a value", () => {
    expect(cur("hosts:\n  a:\n    devices:\n      gpu:\n        kind: g‸")).toMatchObject({
      path: ["hosts", "a", "devices", "gpu", "kind"],
      mode: "value",
      prefix: "g",
    });
  });
  test("a list item, indented or at its key's column", () => {
    expect(cur("balance:\n  qwen:\n    - here/gpu\n    - ‸")).toMatchObject({ path: ["balance", "qwen", 0], mode: "key", item: true });
    expect(cur("balance:\n  qwen:\n  - ‸")).toMatchObject({ path: ["balance", "qwen", 0], item: true });
  });
  test("a key in a list item's mapping", () => {
    expect(cur("balance:\n  qwen:\n    - place: a/gpu\n      max‸")).toMatchObject({
      path: ["balance", "qwen", 0],
      mode: "key",
      item: false,
      present: ["place"],
    });
    expect(cur("balance:\n  qwen:\n    - place: a/gpu\n      flags:\n        --c‸")).toMatchObject({ path: ["balance", "qwen", 0, "flags"] });
    expect(cur("balance:\n  qwen:\n    - flags:\n        --c‸")).toMatchObject({ path: ["balance", "qwen", 0, "flags"] });
  });
  test("an item of a flow list", () => {
    expect(cur("balance:\n  qwen: [a/gpu, b/‸")).toMatchObject({ path: ["balance", "qwen", 0], mode: "value", prefix: "b/" });
    expect(cur("balance:\n  qwen: [‸")).toMatchObject({ path: ["balance", "qwen", 0], prefix: "" });
  });
  test("nothing in a comment or a block of text", () => {
    expect(cur("# bal‸")).toBeNull();
    expect(cur("cmd: |\n  ${llama-server}\n  --mo‸")).toBeNull();
    expect(cur("cmd: |\n  ${llama-server}\nfi‸")).toMatchObject({ path: [], present: ["cmd"] });
  });
  test("a file path key", () => {
    expect(cur("files:\n  /srv/models/a.gguf: {sha256: x}\n  /srv/‸")).toMatchObject({ path: ["files"], prefix: "/srv/" });
  });
});

const schema: Schema = {
  type: "object",
  additionalProperties: false,
  properties: {
    hosts: {
      type: "object",
      description: "The computers.",
      additionalProperties: {
        type: "object",
        properties: {
          url: { type: "string", description: "The address." },
          devices: {
            type: "object",
            additionalProperties: { type: "object", properties: { kind: { type: "string", enum: ["gpu", "cpu"], description: "gpu or cpu." } } },
          },
        },
      },
    },
    groups: { type: "object", additionalProperties: { type: "array", items: { type: "string", "x-complete": "model" } } },
    balance: {
      type: "object",
      "x-complete-keys": "lane",
      additionalProperties: {
        type: "array",
        items: {
          description: "A place.",
          oneOf: [
            { type: "string", "x-complete": "place" },
            {
              type: "object",
              properties: { place: { type: "string", "x-complete": "place" }, only_if_idle: { type: "boolean", description: "Idle only." } },
            },
          ],
        },
      },
    },
    timeouts: { $ref: "#/definitions/timeouts" },
  },
  definitions: { timeouts: { type: "object", description: "Timeouts.", properties: { load: { type: "integer" } } } },
};

const src: Sources = { place: ["a/gpu", "a/cpu"], device: ["gpu", "cpu"], model: ["qwen", "embed"], lane: ["qwen", "embed", "small"], gguf: [] };

function labels(text: string) {
  const c = cur(text);
  return c ? complete(schema, c, src).map((o) => o.label) : null;
}

group("complete", () => {
  test("the keys a mapping does not have yet", () => {
    expect(labels("hosts: {}\n‸")).toEqual(["groups", "balance", "timeouts"]);
  });
  test("an enum", () => {
    expect(labels("hosts:\n  a:\n    devices:\n      gpu:\n        kind: ‸")).toEqual(["gpu", "cpu"]);
  });
  test("a lane's keys are the models and groups not placed yet", () => {
    expect(labels("balance:\n  qwen: [a/gpu]\n  ‸")).toEqual(["embed", "small"]);
  });
  test("a list item is a place or a place's first key", () => {
    expect(labels("balance:\n  qwen:\n    - ‸")).toEqual(["a/gpu", "a/cpu", "place", "only_if_idle"]);
    expect(labels("balance:\n  qwen: [‸")).toEqual(["a/gpu", "a/cpu"]);
    expect(labels("balance:\n  qwen:\n    - place: a/gpu\n      only_if_idle: ‸")).toEqual(["true", "false"]);
  });
  test("a group's members are models", () => {
    expect(labels("groups:\n  embedders: [‸")).toEqual(["qwen", "embed"]);
  });
  test("through a $ref", () => {
    expect(labels("timeouts:\n  ‸")).toEqual(["load"]);
  });
});

group("hover", () => {
  function text(src: string) {
    const { lines, n, ch } = at(src);
    return hover(schema, lines, n, ch)?.text ?? null;
  }
  test("a key's description, with its enum", () => {
    expect(text("hosts:\n  a:\n    devices:\n      gpu:\n        ki‸nd: gpu")).toBe("gpu or cpu.\n\nOne of: gpu, cpu.");
    expect(text("ho‸sts: {}")).toBe("The computers.");
  });
  test("a key inside a place's mapping", () => {
    expect(text("balance:\n  qwen:\n    - place: a/gpu\n      only_if‸_idle: true")).toBe("Idle only.");
  });
  test("not on a value", () => {
    expect(text("hosts:\n  a:\n    url: htt‸p://a")).toBeNull();
  });
});
