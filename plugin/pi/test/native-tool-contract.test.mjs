import assert from "node:assert/strict";
import { copyFile, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROOT = dirname(dirname(fileURLToPath(import.meta.url)));

async function installRuntimeStubs(root) {
  const nodeModules = join(root, "node_modules");
  await mkdir(join(nodeModules, "@earendil-works", "pi-tui"), { recursive: true });
  await writeFile(
    join(nodeModules, "@earendil-works", "pi-tui", "package.json"),
    JSON.stringify({ type: "module", exports: "./index.js" }),
  );
  await writeFile(
    join(nodeModules, "@earendil-works", "pi-tui", "index.js"),
    "export class Text { constructor(text) { this.text = text; } }\n",
  );

  await mkdir(join(nodeModules, "typebox"), { recursive: true });
  await writeFile(
    join(nodeModules, "typebox", "package.json"),
    JSON.stringify({ type: "module", exports: "./index.js" }),
  );
  await writeFile(
    join(nodeModules, "typebox", "index.js"),
    `const schema = (kind) => (...args) => ({ kind, args });
export const Type = new Proxy({}, { get: (_target, prop) => schema(String(prop)) });
`,
  );
}

test("registered Pi-native mem_search reports native provider transport failure", async () => {
  const fixtureRoot = await mkdtemp(join(tmpdir(), "recalldock-pi-contract-"));
  const originalFetch = globalThis.fetch;
  const originalUrl = process.env.RECALLDOCK_URL;
  process.env.RECALLDOCK_URL = "http://127.0.0.1:17437";
  globalThis.fetch = async () => {
    throw new Error("connection refused");
  };

  try {
    for (const file of ["index.ts", "compaction-recovery.js", "memory-tool-chrome.js", "private-redaction.js"]) {
      await copyFile(join(ROOT, file), join(fixtureRoot, file));
    }
    await writeFile(join(fixtureRoot, "package.json"), JSON.stringify({ type: "module" }));
    await installRuntimeStubs(fixtureRoot);
    const registeredTools = new Map();
    const pluginUrl = pathToFileURL(join(fixtureRoot, "index.ts"));
    const { default: registerEngram } = await import(pluginUrl.href);
    registerEngram({
      registerTool(tool) {
        registeredTools.set(tool.name, tool);
      },
      on() {},
    });

    const memSearch = registeredTools.get("mem_search");
    assert.ok(memSearch, "mem_search tool should be registered");

    const result = await memSearch.execute(
      "tool-call-1",
      { query: "state markers", project: "gentle-agent-state" },
      undefined,
      undefined,
      {
        cwd: fixtureRoot,
        sessionManager: { getSessionId: () => "test-session" },
        ui: { setStatus() {} },
      },
    );

    assert.equal(result.isError, true);
    assert.match(result.content[0].text, /gentle-recalldock could not reach the Engram HTTP server/);
    assert.match(result.content[0].text, /Pi-native mem_\* tools are registered/);
    assert.match(result.details.error, /native memory provider is not currently responding/);
  } finally {
    globalThis.fetch = originalFetch;
    if (originalUrl === undefined) delete process.env.RECALLDOCK_URL;
    else process.env.RECALLDOCK_URL = originalUrl;
    await rm(fixtureRoot, { recursive: true, force: true });
  }
});
