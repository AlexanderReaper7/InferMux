// bun test, from webui/: the add-model commands' edits to the draft.
import { expect, test } from "bun:test";
import { parse } from "yaml";
import { addToLane, fromModel, newModel, sources } from "../src/lib/draft";

const files = {
  "infermux.yaml": "hosts:\n  strix:\n    devices:\n      gpu: {kind: gpu}\n      cpu: {kind: cpu}\n  zbox:\n    devices:\n      gpu: {kind: gpu}\ngroups:\n  embedders: [embed]\n",
  "models/qwen.yaml":
    "cmd: |\n  ${llama-server}\n    --port ${PORT}\n    --model /srv/models/qwen.gguf\n    --mmproj /srv/models/mmproj.gguf\n    --ctx-size 65536\n",
  "models/embed.yaml": "cmd: ${ik-llama} --port ${PORT} -m /srv/models/e.gguf --embeddings\n",
};

test("sources", () => {
  expect(sources(files, ["/a.gguf"])).toEqual({
    place: ["strix/gpu", "strix/cpu", "zbox/gpu"],
    device: ["gpu", "cpu"],
    model: ["embed", "qwen"],
    lane: ["embed", "qwen", "embedders"],
    gguf: ["/a.gguf"],
  });
  expect(sources({ ...files, "infermux.yaml": "hosts: [" }, [])).toBeNull();
});

test("a command from the same runtime's model", () => {
  expect(fromModel(files, "llama-server", "/srv/models/new.gguf")).toEqual({
    cmd: "${llama-server}\n  --port ${PORT}\n  --model /srv/models/new.gguf\n  --ctx-size 65536\n",
    from: "qwen",
  });
  expect(fromModel(files, "ik-llama", "/n.gguf")?.cmd).toBe("${ik-llama} --port ${PORT} -m /n.gguf --embeddings");
  expect(fromModel(files, "vllm", "/n.gguf")).toBeNull();
});

test("a new model's file loads with its files", () => {
  const text = newModel(files, "llama-server", "/srv/models/new.gguf", "a".repeat(64), "org/repo/new.gguf");
  expect(text.startsWith("# The command is qwen's")).toBe(true);
  expect(parse(text)).toEqual({
    cmd: "${llama-server}\n  --port ${PORT}\n  --model /srv/models/new.gguf\n  --ctx-size 65536\n",
    files: { "/srv/models/new.gguf": { sha256: "a".repeat(64), hf: "org/repo/new.gguf" } },
  });
  expect(parse(newModel(files, "vllm", "/n.gguf", "b".repeat(64), null)).files).toEqual({ "/n.gguf": { sha256: "b".repeat(64) } });
});

test("a model added to a lane", () => {
  const main = "hosts: {}\nbalance:\n    qwen: [strix/gpu]\n\n# the failover lane\nfailover:\n  qwen: [zbox/gpu]\n";
  expect(addToLane(main, "balance", "new", "zbox/gpu")).toBe(
    "hosts: {}\nbalance:\n    qwen: [strix/gpu]\n    new: [zbox/gpu]\n\n# the failover lane\nfailover:\n  qwen: [zbox/gpu]\n",
  );
  expect(addToLane(main, "failover", "new", "strix/cpu")).toBe(main + "  new: [strix/cpu]\n");
  expect(addToLane("hosts: {}\nbalance: {}\n", "balance", "new", "a/gpu")).toBe("hosts: {}\nbalance:\n  new: [a/gpu]\n");
  expect(addToLane("hosts: {}", "failover", "new", "a/cpu")).toBe("hosts: {}\nfailover:\n  new: [a/cpu]\n");
  expect(parse(addToLane(main, "balance", "new", "zbox/gpu")).balance).toEqual({ qwen: ["strix/gpu"], new: ["zbox/gpu"] });
});
