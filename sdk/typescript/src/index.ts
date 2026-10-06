/**
 * TypeScript SDK for box: microVM sandboxes that start in milliseconds.
 *
 * Every sandbox is a microVM with its own kernel. The SDK talks to
 * `box serve` over a Unix socket only you can open, and starts it the
 * first time it is needed. Node 18+, no dependencies.
 *
 *   import { Sandbox } from "box-sdk";
 *
 *   const sb = new Sandbox("agent");
 *   await sb.up();                                   // boot once
 *   const r = await sb.exec(["python3", "-c", "print(6 * 7)"]);
 *   console.log(r.stdoutText, r.exitCode, r.durationMs);
 *   await sb.down();
 *
 *   const fresh = await new Sandbox("agent").run("pytest -q"); // a fresh VM
 */

import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { createConnection } from "node:net";
import { homedir } from "node:os";
import { join } from "node:path";
import { request as httpRequest } from "node:http";
import { execFileSync } from "node:child_process";

/** A shell command line (run through `sh -c`) or an argv list (no shell). */
export type Command = string | readonly string[];

/** The server refused or could not carry out a request. */
export class BoxError extends Error {
  constructor(
    message: string,
    /** Machine-readable: no_sandbox, not_up, bad_name, bad_request, no_server, internal. */
    public readonly code: string = "internal",
    public readonly status: number = 0,
  ) {
    super(message);
    this.name = "BoxError";
  }
}

/** No sandbox by that name. */
export class NotFound extends BoxError {
  constructor(message: string, status = 404) {
    super(message, "no_sandbox", status);
    this.name = "NotFound";
  }
}

/** exec was called on a sandbox that is not up. */
export class NotUp extends BoxError {
  constructor(message: string, status = 409) {
    super(message, "not_up", status);
    this.name = "NotUp";
  }
}

/** Thrown by Result.check() for a command that exited non-zero. */
export class CommandFailed extends BoxError {
  constructor(public readonly result: Result) {
    const tail = result.stderrText.trim().split("\n").pop() ?? "";
    super(`exit ${result.exitCode}: ${tail}`, "command_failed");
    this.name = "CommandFailed";
  }
}

/** How a command ended. A non-zero exit is a Result, not an exception. */
export class Result {
  constructor(
    public readonly exitCode: number,
    public readonly stdout: Buffer,
    public readonly stderr: Buffer,
    public readonly durationMs: number,
    public readonly timedOut: boolean = false,
    public readonly truncated: boolean = false,
  ) {}

  get ok(): boolean {
    return this.exitCode === 0;
  }
  get stdoutText(): string {
    return this.stdout.toString("utf8");
  }
  get stderrText(): string {
    return this.stderr.toString("utf8");
  }
  /** Returns this, or throws CommandFailed if the command did not succeed. */
  check(): Result {
    if (!this.ok) throw new CommandFailed(this);
    return this;
  }
}

/** One entry of a fork's diff. */
export interface Change {
  kind: "A" | "M" | "D";
  path: string;
}

export interface SandboxInfo {
  name: string;
  up: boolean;
  warm: number;
  warm_target: number;
  network: string;
  base: string;
  error?: string;
}

export interface ClientOptions {
  /** Path to the server socket. Default: the same path the CLI uses. */
  socket?: string;
  /** The box executable, used to start the server. Default: "box" on PATH. */
  binary?: string;
  /** Start `box serve` when nothing is listening. Default true. */
  autostart?: boolean;
  /** How long an auto-started server lingers unused. Default "15m". */
  idle?: string;
}

/**
 * Mirrors sandbox.SocketPath in the Go server: the box root, or the
 * per-user runtime dir when that path is too long for a Unix socket. Never a
 * shared directory, where another user could listen first.
 */
export function defaultSocket(): string {
  const home = process.env.BOX_HOME || join(homedir(), ".box");
  const path = join(home, "box.sock");
  if (Buffer.byteLength(path) <= 103) return path;
  const run = process.env.XDG_RUNTIME_DIR;
  if (!run) {
    throw new BoxError(
      `socket path ${path} is too long for a Unix socket; shorten BOX_HOME or set XDG_RUNTIME_DIR`,
      "bad_socket",
    );
  }
  const digest = createHash("sha256").update(home).digest("hex").slice(0, 12);
  return join(run, `box-${digest}.sock`);
}

function toArgv(cmd: Command): string[] {
  if (typeof cmd === "string") return ["sh", "-c", cmd];
  const argv = [...cmd];
  if (argv.length === 0 || !argv.every((a) => typeof a === "string")) {
    throw new TypeError("command must be a string or a non-empty list of strings");
  }
  return argv;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** A connection to the local box server. */
export class Client {
  readonly socket: string;
  readonly binary: string;
  readonly autostart: boolean;
  readonly idle: string;

  constructor(opts: ClientOptions = {}) {
    this.socket = opts.socket ?? defaultSocket();
    this.binary = opts.binary ?? "box";
    this.autostart = opts.autostart ?? true;
    this.idle = opts.idle ?? "15m";
  }

  // -- transport -----------------------------------------------------------

  private rawRequest(method: string, path: string, body?: string): Promise<{ status: number; data: Buffer }> {
    return new Promise((resolve, reject) => {
      const req = httpRequest(
        {
          socketPath: this.socket,
          method,
          path,
          headers: { "Content-Type": "application/json", Host: "box" },
        },
        (res) => {
          const chunks: Buffer[] = [];
          res.on("data", (c: Buffer) => chunks.push(c));
          res.on("end", () => resolve({ status: res.statusCode ?? 0, data: Buffer.concat(chunks) }));
          res.on("error", reject);
        },
      );
      req.on("error", reject);
      if (body !== undefined) req.write(body);
      req.end();
    });
  }

  /** @internal */
  async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    let res: { status: number; data: Buffer } | undefined;
    for (let attempt = 0; attempt < 2 && !res; attempt++) {
      try {
        res = await this.rawRequest(method, path, payload);
      } catch (err) {
        const code = (err as NodeJS.ErrnoException).code;
        if ((code === "ENOENT" || code === "ECONNREFUSED") && attempt === 0 && this.autostart) {
          await this.startServer();
          continue;
        }
        if (code === "ENOENT" || code === "ECONNREFUSED") {
          throw new BoxError(
            `box server not running at ${this.socket}; start it: box serve`,
            "no_server",
          );
        }
        throw err;
      }
    }
    const { status, data } = res!;
    const decoded = data.length ? JSON.parse(data.toString("utf8")) : null;
    if (status >= 400) {
      const e = decoded ?? {};
      const message = e.error ?? `HTTP ${status}`;
      switch (e.code) {
        case "no_sandbox":
          throw new NotFound(message, status);
        case "not_up":
          throw new NotUp(message, status);
        default:
          throw new BoxError(message, e.code ?? "internal", status);
      }
    }
    return decoded as T;
  }

  private async startServer(): Promise<void> {
    const child = spawn(this.binary, ["serve", "--socket", this.socket, "--idle", this.idle], {
      detached: true, // outlives this process and its ^C
      stdio: "ignore",
    });
    child.on("error", () => {}); // reported below, as a failure to connect
    child.unref();
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      if (await this.canConnect()) return;
      await sleep(20);
    }
    throw new BoxError(
      `started ${this.binary} serve but it never listened on ${this.socket}`,
      "no_server",
    );
  }

  private canConnect(): Promise<boolean> {
    return new Promise((resolve) => {
      const s = createConnection(this.socket);
      s.once("connect", () => {
        s.end();
        resolve(true);
      });
      s.once("error", () => resolve(false));
    });
  }

  // -- API -----------------------------------------------------------------

  version(): Promise<{ version: string; api: string }> {
    return this.request("GET", "/v1/version");
  }

  /** Every defined sandbox, with whether it is up and its warm pool. */
  sandboxes(): Promise<SandboxInfo[]> {
    return this.request("GET", "/v1/sandboxes");
  }

  sandbox(name: string): Sandbox {
    return new Sandbox(name, this);
  }
}

export interface ExecOptions {
  /** Forwarded to the command's stdin. */
  stdin?: Buffer | string;
  /** Seconds; overrides the Boxfile's timeout_seconds. A timed-out command exits 124. */
  timeout?: number;
}

/**
 * One sandbox, by name. Create and build it first with the CLI:
 *
 *   box new agent --from tiny-python && box build agent
 */
export class Sandbox {
  readonly client: Client;

  constructor(
    public readonly name: string,
    client?: Client,
  ) {
    this.client = client ?? new Client();
  }

  private path(verb: string): string {
    return `/v1/sandboxes/${encodeURIComponent(this.name)}/${verb}`;
  }

  /** Boot the microVM once and keep it running for exec. */
  up(): Promise<{ up: boolean; already?: boolean; ready_ms?: number }> {
    return this.client.request("POST", this.path("up"));
  }

  /** Stop the running microVM. Everything outside /data goes with it. */
  async down(): Promise<void> {
    await this.client.request("POST", this.path("down"));
  }

  private async command(verb: string, cmd: Command, opts: ExecOptions): Promise<Result> {
    const body: Record<string, unknown> = { argv: toArgv(cmd) };
    if (opts.stdin !== undefined) {
      body.stdin = Buffer.from(opts.stdin).toString("base64");
    }
    if (opts.timeout) body.timeout_seconds = Math.trunc(opts.timeout);
    const r = await this.client.request<{
      exit_code: number;
      stdout: string;
      stderr: string;
      duration_ms: number;
      timed_out?: boolean;
      truncated?: boolean;
    }>("POST", this.path(verb), body);
    return new Result(
      r.exit_code,
      Buffer.from(r.stdout, "base64"),
      Buffer.from(r.stderr, "base64"),
      r.duration_ms,
      r.timed_out ?? false,
      r.truncated ?? false,
    );
  }

  /** Run in the running microVM (see up). State carries between calls. */
  exec(cmd: Command, opts: ExecOptions = {}): Promise<Result> {
    return this.command("exec", cmd, opts);
  }

  /**
   * Run in a fresh microVM that is destroyed afterwards. With `warm:` in the
   * Boxfile it comes from the pool and starts in milliseconds. Stdin is not
   * forwarded to a run.
   */
  run(cmd: Command, opts: Pick<ExecOptions, "timeout"> = {}): Promise<Result> {
    return this.command("run", cmd, opts);
  }

  /**
   * Write a file inside the running microVM. The write happens in the guest,
   * so a path or symlink the sandbox controls can never redirect it onto a
   * host file.
   */
  async writeFile(path: string, data: Buffer | string, mode = "0644"): Promise<void> {
    if (!/^[0-7]{3,4}$/.test(mode)) throw new RangeError(`mode must be octal like '0644', got ${mode}`);
    (await this.exec(["sh", "-c", 'umask 077; cat > "$1" && chmod "$2" "$1"', "sh", path, mode], { stdin: data })).check();
  }

  /**
   * Branch this sandbox: a new sandbox with the same image and a /data that
   * overlays this one's. Running the fork never touches this sandbox; diff
   * shows its changes, apply merges them back.
   */
  async fork(name: string): Promise<Sandbox> {
    await this.client.request("POST", this.path("fork"), { as: name });
    return new Sandbox(name, this.client);
  }

  /** A fork's changes to /data: kind "A" added, "M" modified, "D" deleted. */
  async diff(): Promise<Change[]> {
    return (await this.client.request<{ changes: Change[] }>("GET", this.path("diff"))).changes;
  }

  /** Merge a fork's changes into its parent. Resolves to how many. Refused while either side is up. */
  async apply(): Promise<number> {
    return (await this.client.request<{ changes: number }>("POST", this.path("apply"))).changes;
  }

  /** Throw a fork's changes away. Resolves to how many. */
  async discard(): Promise<number> {
    return (await this.client.request<{ changes: number }>("POST", this.path("discard"))).changes;
  }

  /** Read a file from inside the running microVM. */
  async readFile(path: string): Promise<Buffer> {
    return (await this.exec(["cat", "--", path])).check().stdout;
  }

  /** Runs fn with the sandbox up, and brings it down afterwards unless it was already up. */
  async withUp<T>(fn: (sb: Sandbox) => Promise<T>): Promise<T> {
    const r = await this.up();
    try {
      return await fn(this);
    } finally {
      if (!r.already) await this.down();
    }
  }
}

/** The installed box's version, or null if it is not on PATH. */
export function cliVersion(binary = "box"): string | null {
  try {
    return execFileSync(binary, ["--version"], { encoding: "utf8" }).trim();
  } catch {
    return null;
  }
}
