// Instinct, acting on behalf of Tal Chaim Damri.
// Local SDK interoperability regression, not a model/client-GUI/conformance test.
import test from "node:test";
import assert from "node:assert/strict";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "node:net";
import { once } from "node:events";

const source = resolve(
  fileURLToPath(new URL("../..", import.meta.url)),
  "server",
);
const requestOptions = { timeout: 3000 };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function unusedPort() {
  const s = createServer();
  await new Promise((r, reject) => {
    s.once("error", reject);
    s.listen(0, "127.0.0.1", r);
  });
  const port = s.address().port;
  await new Promise((r, reject) => s.close((e) => (e ? reject(e) : r())));
  return port;
}

async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const exited = once(child, "exit");
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 3000);
  try {
    await exited;
  } finally {
    clearTimeout(timer);
  }
}

function payload(result) {
  assert.notEqual(result.isError, true);
  assert.equal(result.content[0].type, "text");
  return JSON.parse(result.content[0].text);
}

function call(client, name, args) {
  return client.callTool({ name, arguments: args }, undefined, requestOptions);
}

test(
  "real SDK clients exchange synthetic messages with local ACQ",
  { timeout: 90000 },
  async () => {
    const dir = await mkdtemp(join(tmpdir(), "acq-sdk-"));
    const a = new Client(
      { name: "acq-local-sdk-a", version: "1" },
      { capabilities: {} },
    );
    const b = new Client(
      { name: "acq-local-sdk-b", version: "1" },
      { capabilities: {} },
    );
    let child;
    let room;
    try {
      const binary = join(
        dir,
        process.platform === "win32" ? "acq.exe" : "acq",
      );
      const build = spawnSync("go", ["build", "-o", binary, "."], {
        cwd: source,
        encoding: "utf8",
        timeout: 60000,
      });
      assert.ifError(build.error);
      assert.equal(build.status, 0, build.stderr);
      const port = await unusedPort();
      // URL is constructed here, never accepted from an argument or environment.
      const base = `http://127.0.0.1:${port}`;
      const env = {
        PATH: process.env.PATH,
        HOME: dir,
        ...(process.env.SystemRoot
          ? { SystemRoot: process.env.SystemRoot }
          : {}),
        PORT: String(port),
        ROOMS_DIR: join(dir, "rooms"),
        STATS_FILE: join(dir, "stats.json"),
        FEEDBACK_FILE: join(dir, "feedback.json"),
        ANALYTICS_FILE: join(dir, "analytics.json"),
        ACQ_PUBLIC_BASE_URL: base,
      };
      // Runtime gets only temporary storage and non-secret process essentials.
      // No production auth, analytics token, fault injection or session settings.
      child = spawn(binary, [], { cwd: dir, env, stdio: "ignore" });
      let spawnError;
      child.on("error", (e) => {
        spawnError = e;
      });
      let ready = false;
      for (let n = 0; n < 100; n++) {
        if (spawnError) throw spawnError;
        if (child.exitCode !== null)
          throw new Error("local ACQ exited before readiness");
        try {
          if (
            (await fetch(`${base}/stats`, { signal: AbortSignal.timeout(250) }))
              .ok
          ) {
            ready = true;
            break;
          }
        } catch {}
        await sleep(25);
      }
      assert.equal(ready, true, "local ACQ failed to become ready");
      await Promise.all([
        a.connect(
          new StreamableHTTPClientTransport(new URL(`${base}/mcp`)),
          requestOptions,
        ),
        b.connect(
          new StreamableHTTPClientTransport(new URL(`${base}/mcp`)),
          requestOptions,
        ),
      ]);
      for (const client of [a, b]) {
        const tools = await client.listTools(undefined, requestOptions);
        assert.equal(tools.tools.length, 9);
        for (const name of [
          "acq_create_room",
          "acq_join_room",
          "acq_send",
          "acq_recv",
        ])
          assert.ok(tools.tools.some((t) => t.name === name));
      }
      room = payload(
        await call(a, "acq_create_room", {
          topic: "local SDK synthetic regression",
        }),
      );
      const uin = room.roomId;
      const guest = payload(await call(b, "acq_join_room", { uin }));
      assert.ok(room.token !== guest.token, "seat tokens must differ");
      const sent = payload(
        await call(a, "acq_send", {
          uin,
          token: room.token,
          text: "Synthetic question: 2 + 2?",
          cid: "sdk-a",
        }),
      );
      const duplicate = payload(
        await call(a, "acq_send", {
          uin,
          token: room.token,
          text: "Synthetic question: 2 + 2?",
          cid: "sdk-a",
        }),
      );
      assert.equal(duplicate.duplicate, true);
      assert.equal(duplicate.seq, sent.seq);
      const wrong = await call(b, "acq_recv", {
        uin,
        token: "deliberately-wrong-token",
        since: 0,
        peek: true,
      });
      assert.equal(wrong.isError, true);
      assert.match(wrong.content[0].text, /^HTTP 404:/);
      const received = payload(
        await call(b, "acq_recv", {
          uin,
          token: guest.token,
          since: 0,
          peek: true,
        }),
      );
      assert.equal(
        received.messages.filter((m) => m.text === "Synthetic question: 2 + 2?")
          .length,
        1,
      );
      payload(
        await call(b, "acq_send", {
          uin,
          token: guest.token,
          text: "Synthetic answer: 4",
          cid: "sdk-b",
        }),
      );
      const reply = payload(
        await call(a, "acq_recv", {
          uin,
          token: room.token,
          since: 0,
          peek: true,
        }),
      );
      assert.ok(reply.messages.some((m) => m.text === "Synthetic answer: 4"));
      payload(
        await call(a, "acq_send", {
          uin,
          token: room.token,
          text: "Synthetic exercise ends",
          end: true,
          cid: "sdk-end",
        }),
      );
      const late = await call(b, "acq_send", {
        uin,
        token: guest.token,
        text: "Must not commit",
        cid: "sdk-late",
      });
      assert.equal(late.isError, true);
      assert.match(late.content[0].text, /^HTTP 410:/);
      const ended = payload(
        await call(a, "acq_recv", {
          uin,
          token: room.token,
          since: 0,
          self: true,
          peek: true,
        }),
      );
      assert.equal(ended.ended, true);
      assert.ok(!ended.messages.some((m) => m.text === "Must not commit"));
    } finally {
      // No credentials or transcript printed; local room data is removed even on failure.
      await Promise.allSettled([a.close(), b.close()]);
      await stop(child);
      await rm(dir, { recursive: true, force: true });
    }
  },
);
