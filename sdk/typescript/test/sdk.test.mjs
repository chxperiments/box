import { test } from "node:test";
import assert from "node:assert/strict";
import { defaultSocket, Client, Sandbox, NotFound, CommandFailed, Result } from "../dist/index.js";

test("defaultSocket follows BOX_HOME and falls back for long paths", () => {
  process.env.BOX_HOME = "/tmp/short";
  assert.equal(defaultSocket(), "/tmp/short/box.sock");
  process.env.BOX_HOME = "/" + "x".repeat(120);
  process.env.XDG_RUNTIME_DIR = "/run/user/1000";
  assert.match(defaultSocket(), /^\/run\/user\/1000\/box-[0-9a-f]{12}\.sock$/);
  delete process.env.XDG_RUNTIME_DIR;
  assert.throws(() => defaultSocket(), /too long/);
  delete process.env.BOX_HOME;
});

test("Result.check throws CommandFailed with the last stderr line", () => {
  const r = new Result(3, Buffer.from(""), Buffer.from("warn\nboom\n"), 5);
  assert.equal(r.ok, false);
  assert.throws(() => r.check(), (e) => e instanceof CommandFailed && e.message === "exit 3: boom");
  assert.equal(new Result(0, Buffer.from("hi"), Buffer.from(""), 1).check().stdoutText, "hi");
});

// The rest needs a built sandbox and KVM: BOX_E2E=<sandbox name>.
const e2e = process.env.BOX_E2E;
test("end to end against a real sandbox", { skip: !e2e && "set BOX_E2E=<sandbox>" }, async () => {
  const c = new Client();
  await assert.rejects(c.sandbox("no-such-sandbox").run("true"), NotFound);
  const sb = c.sandbox(e2e);
  await sb.withUp(async (sb) => {
    const r = await sb.exec(["sh", "-c", "echo out; echo err >&2; exit 3"]);
    assert.deepEqual([r.exitCode, r.stdoutText, r.stderrText], [3, "out\n", "err\n"]);
    assert.equal((await sb.exec("wc -c", { stdin: Buffer.from([0, 1, 255]) })).stdoutText.trim(), "3");
    await sb.writeFile("/data/ts.txt", "hi $HOME 'q'\n");
    assert.equal((await sb.readFile("/data/ts.txt")).toString(), "hi $HOME 'q'\n");
    const t = await sb.exec("sleep 5", { timeout: 1 });
    assert.deepEqual([t.exitCode, t.timedOut], [124, true]);
    const t0 = performance.now();
    for (let i = 0; i < 5; i++) await sb.exec("true");
    console.log(`  exec via SDK: ${((performance.now() - t0) / 5).toFixed(1)} ms avg`);
  });
  const list = await c.sandboxes();
  assert.equal(list.find((s) => s.name === e2e)?.up, false);
  const r = await sb.run("cat /data/ts.txt");
  assert.equal(r.check().stdoutText, "hi $HOME 'q'\n");
});
