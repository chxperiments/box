//! Rust SDK for box: run code in microVM sandboxes that each have their
//! own kernel.
//!
//! The SDK talks to `box serve` over a Unix socket only you can open,
//! and starts the server the first time it is needed. Calls are blocking.
//!
//! ```no_run
//! use sdbox::{Command, Sandbox};
//!
//! # fn main() -> Result<(), sdbox::Error> {
//! let sb = Sandbox::new("agent")?;
//! sb.up()?;
//! let out = sb.exec(Command::new(["python3", "-c", "print(6*7)"]))?;
//! println!("{} {}", out.stdout_text(), out.exit_code);
//! sb.down()?;
//!
//! // A fork: branch /data, try something, keep it or drop it.
//! let trial = Sandbox::new("agent")?.fork("trial")?;
//! trial.run(Command::sh("pytest -q"))?.check()?;
//! for ch in trial.diff()? {
//!     println!("{} {}", ch.kind, ch.path);
//! }
//! trial.apply()?;
//! # Ok(())
//! # }
//! ```

use std::fmt;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{json, Value};

mod base64;

/// Everything that can go wrong. A command that exits non-zero is not an
/// error: it is an [`Output`] with that exit code (see [`Output::check`]).
#[derive(Debug)]
pub enum Error {
    /// No sandbox by that name.
    NotFound(String),
    /// `exec` on a sandbox that is not up.
    NotUp(String),
    /// Any other refusal from the server, with its machine-readable code:
    /// `bad_name`, `bad_request`, `not_fork`, `is_up`, `internal`.
    Api {
        code: String,
        message: String,
        status: u16,
    },
    /// Raised by [`Output::check`] for a non-zero exit.
    CommandFailed(Box<Output>),
    /// The server is not running and could not be started.
    NoServer(String),
    /// A response that is not what the API sends.
    Protocol(String),
    Io(io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotFound(m) | Error::NotUp(m) | Error::NoServer(m) | Error::Protocol(m) => {
                write!(f, "box: {m}")
            }
            Error::Api { message, .. } => write!(f, "box: {message}"),
            Error::CommandFailed(o) => {
                let tail = o.stderr_text();
                let tail = tail.trim().lines().last().unwrap_or("").to_string();
                write!(f, "box: exit {}: {tail}", o.exit_code)
            }
            Error::Io(e) => write!(f, "box: {e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<io::Error> for Error {
    fn from(e: io::Error) -> Self {
        Error::Io(e)
    }
}

pub type Result<T> = std::result::Result<T, Error>;

/// What to run: an argv (no shell), or a shell command line with [`Command::sh`].
#[derive(Debug, Clone)]
pub struct Command {
    argv: Vec<String>,
    stdin: Option<Vec<u8>>,
    timeout: Option<u32>,
}

impl Command {
    /// Runs `argv` as-is, with no shell in between.
    pub fn new<I, S>(argv: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: Into<String>,
    {
        Command {
            argv: argv.into_iter().map(Into::into).collect(),
            stdin: None,
            timeout: None,
        }
    }

    /// Runs a shell command line, as you would type it.
    pub fn sh(line: &str) -> Self {
        Command::new(["sh", "-c", line])
    }

    /// Bytes for the command's stdin (`exec` only; a `run` gets none).
    pub fn stdin(mut self, data: impl Into<Vec<u8>>) -> Self {
        self.stdin = Some(data.into());
        self
    }

    /// Seconds, overriding the Boxfile's `timeout_seconds`. A timed-out
    /// command exits 124.
    pub fn timeout(mut self, seconds: u32) -> Self {
        self.timeout = Some(seconds);
        self
    }
}

impl From<&str> for Command {
    fn from(line: &str) -> Self {
        Command::sh(line)
    }
}

/// How a command ended.
#[derive(Debug, Clone)]
pub struct Output {
    pub exit_code: i32,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub duration_ms: u64,
    pub timed_out: bool,
    pub truncated: bool,
}

impl Output {
    pub fn ok(&self) -> bool {
        self.exit_code == 0
    }
    pub fn stdout_text(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }
    pub fn stderr_text(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }
    /// Returns self, or `Error::CommandFailed` for a non-zero exit.
    pub fn check(self) -> Result<Output> {
        if self.ok() {
            Ok(self)
        } else {
            Err(Error::CommandFailed(Box::new(self)))
        }
    }
}

/// One entry of a fork's diff: `kind` is "A" added, "M" modified, "D" deleted.
#[derive(Debug, Clone, Deserialize, PartialEq, Eq)]
pub struct Change {
    pub kind: String,
    pub path: String,
}

/// One defined sandbox.
#[derive(Debug, Clone, Deserialize)]
pub struct SandboxInfo {
    pub name: String,
    pub up: bool,
    pub warm: u32,
    pub warm_target: u32,
    #[serde(default)]
    pub network: String,
    #[serde(default)]
    pub base: String,
    #[serde(default)]
    pub error: Option<String>,
}

/// A connection to the local box server.
#[derive(Debug, Clone)]
pub struct Client {
    socket: PathBuf,
    binary: String,
    autostart: bool,
    idle: String,
}

impl Client {
    /// A client for the default socket, starting the server on demand.
    pub fn new() -> Result<Self> {
        Ok(Client {
            socket: default_socket()?,
            binary: "box".into(),
            autostart: true,
            idle: "15m".into(),
        })
    }

    pub fn with_socket(mut self, path: impl Into<PathBuf>) -> Self {
        self.socket = path.into();
        self
    }
    /// The box executable used to start the server.
    pub fn with_binary(mut self, binary: impl Into<String>) -> Self {
        self.binary = binary.into();
        self
    }
    pub fn with_autostart(mut self, on: bool) -> Self {
        self.autostart = on;
        self
    }

    pub fn socket(&self) -> &Path {
        &self.socket
    }

    /// Every defined sandbox, whether it is up, and its warm pool.
    pub fn sandboxes(&self) -> Result<Vec<SandboxInfo>> {
        let v = self.request("GET", "/v1/sandboxes", None)?;
        serde_json::from_value(v).map_err(|e| Error::Protocol(e.to_string()))
    }

    pub fn sandbox(&self, name: &str) -> Sandbox {
        Sandbox {
            name: name.to_string(),
            client: self.clone(),
        }
    }

    fn request(&self, method: &str, path: &str, body: Option<&Value>) -> Result<Value> {
        let payload = body.map(|b| b.to_string()).unwrap_or_default();
        let (status, data) = match http(&self.socket, method, path, &payload) {
            Ok(r) => r,
            Err(e) if self.autostart && no_server(&e) => {
                self.start_server()?;
                http(&self.socket, method, path, &payload)?
            }
            Err(e) if no_server(&e) => {
                return Err(Error::NoServer(format!(
                    "server not running at {}; start it: box serve",
                    self.socket.display()
                )))
            }
            Err(e) => return Err(e.into()),
        };
        let v: Value = if data.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&data)
                .map_err(|e| Error::Protocol(format!("bad JSON from server: {e}")))?
        };
        if status >= 400 {
            let message = v["error"].as_str().unwrap_or("request failed").to_string();
            let code = v["code"].as_str().unwrap_or("internal").to_string();
            return Err(match code.as_str() {
                "no_sandbox" => Error::NotFound(message),
                "not_up" => Error::NotUp(message),
                _ => Error::Api {
                    code,
                    message,
                    status,
                },
            });
        }
        Ok(v)
    }

    fn start_server(&self) -> Result<()> {
        std::process::Command::new(&self.binary)
            .args(["serve", "--socket"])
            .arg(&self.socket)
            .args(["--idle", &self.idle])
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .process_group(0) // outlives this process and its ^C
            .spawn()
            .map_err(|e| Error::NoServer(format!("cannot start {} serve: {e}", self.binary)))?;
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if UnixStream::connect(&self.socket).is_ok() {
                return Ok(());
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        Err(Error::NoServer(format!(
            "started {} serve but it never listened on {}",
            self.binary,
            self.socket.display()
        )))
    }
}

fn no_server(e: &io::Error) -> bool {
    matches!(
        e.kind(),
        io::ErrorKind::NotFound | io::ErrorKind::ConnectionRefused
    )
}

/// One sandbox, by name. Create and build it first with the CLI:
/// `box new agent --from tiny-python && box build agent`.
#[derive(Debug, Clone)]
pub struct Sandbox {
    name: String,
    client: Client,
}

impl Sandbox {
    /// A handle on `name` through a default [`Client`].
    pub fn new(name: &str) -> Result<Self> {
        Ok(Client::new()?.sandbox(name))
    }

    pub fn name(&self) -> &str {
        &self.name
    }

    fn path(&self, verb: &str) -> String {
        format!("/v1/sandboxes/{}/{}", self.name, verb)
    }

    /// Boot the microVM once and keep it running for `exec`. Returns false
    /// if it was already up.
    pub fn up(&self) -> Result<bool> {
        let v = self.client.request("POST", &self.path("up"), None)?;
        Ok(!v["already"].as_bool().unwrap_or(false))
    }

    /// Stop the running microVM. Everything outside /data goes with it.
    pub fn down(&self) -> Result<()> {
        self.client
            .request("POST", &self.path("down"), None)
            .map(|_| ())
    }

    /// Run in the running microVM (see [`Sandbox::up`]). State carries over.
    pub fn exec(&self, cmd: impl Into<Command>) -> Result<Output> {
        self.command("exec", cmd.into())
    }

    /// Run in a fresh microVM that is destroyed afterwards. With `warm:` in
    /// the Boxfile it comes from the pool and starts in milliseconds.
    pub fn run(&self, cmd: impl Into<Command>) -> Result<Output> {
        let mut cmd = cmd.into();
        cmd.stdin = None;
        self.command("run", cmd)
    }

    fn command(&self, verb: &str, cmd: Command) -> Result<Output> {
        if cmd.argv.is_empty() {
            return Err(Error::Api {
                code: "bad_request".into(),
                message: "empty command".into(),
                status: 0,
            });
        }
        let mut body = json!({ "argv": cmd.argv });
        if let Some(s) = &cmd.stdin {
            body["stdin"] = json!(base64::encode(s));
        }
        if let Some(t) = cmd.timeout {
            body["timeout_seconds"] = json!(t);
        }
        let v = self.client.request("POST", &self.path(verb), Some(&body))?;
        let bytes = |k: &str| -> Result<Vec<u8>> {
            base64::decode(v[k].as_str().unwrap_or(""))
                .ok_or_else(|| Error::Protocol(format!("bad base64 in {k}")))
        };
        Ok(Output {
            exit_code: v["exit_code"].as_i64().unwrap_or(-1) as i32,
            stdout: bytes("stdout")?,
            stderr: bytes("stderr")?,
            duration_ms: v["duration_ms"].as_u64().unwrap_or(0),
            timed_out: v["timed_out"].as_bool().unwrap_or(false),
            truncated: v["truncated"].as_bool().unwrap_or(false),
        })
    }

    /// Write a file inside the running microVM. The write happens in the
    /// guest, so a path or symlink the sandbox controls can never redirect
    /// it onto a host file.
    pub fn write_file(&self, path: &str, data: impl Into<Vec<u8>>) -> Result<()> {
        let cmd = Command::new(["sh", "-c", "umask 077; cat > \"$1\"", "sh", path]).stdin(data);
        self.exec(cmd)?.check().map(|_| ())
    }

    /// Read a file from inside the running microVM.
    pub fn read_file(&self, path: &str) -> Result<Vec<u8>> {
        Ok(self
            .exec(Command::new(["cat", "--", path]))?
            .check()?
            .stdout)
    }

    /// Branch this sandbox: a new sandbox with the same image whose /data
    /// overlays this one's. Running the fork never touches this sandbox.
    pub fn fork(&self, name: &str) -> Result<Sandbox> {
        self.client
            .request("POST", &self.path("fork"), Some(&json!({ "as": name })))?;
        Ok(self.client.sandbox(name))
    }

    /// A fork's changes to /data.
    pub fn diff(&self) -> Result<Vec<Change>> {
        let v = self.client.request("GET", &self.path("diff"), None)?;
        serde_json::from_value(v["changes"].clone()).map_err(|e| Error::Protocol(e.to_string()))
    }

    /// Merge a fork's changes into its parent; returns how many. Refused
    /// while either side is up.
    pub fn apply(&self) -> Result<u64> {
        Ok(
            self.client.request("POST", &self.path("apply"), None)?["changes"]
                .as_u64()
                .unwrap_or(0),
        )
    }

    /// Throw a fork's changes away; returns how many.
    pub fn discard(&self) -> Result<u64> {
        Ok(
            self.client.request("POST", &self.path("discard"), None)?["changes"]
                .as_u64()
                .unwrap_or(0),
        )
    }
}

/// Mirrors the Go server's choice: the box root, or the per-user
/// runtime directory when that path is too long for a Unix socket. Never a
/// shared directory, where another user could listen first.
pub fn default_socket() -> Result<PathBuf> {
    use sha2::{Digest, Sha256};
    let home = match std::env::var("BOX_HOME") {
        Ok(h) if !h.is_empty() => h,
        _ => {
            let h = std::env::var("HOME").map_err(|_| Error::Protocol("HOME is not set".into()))?;
            format!("{h}/.box")
        }
    };
    let path = PathBuf::from(&home).join("box.sock");
    if path.as_os_str().len() <= 103 {
        return Ok(path);
    }
    let run = std::env::var("XDG_RUNTIME_DIR").map_err(|_| {
        Error::Protocol(format!(
            "socket path {} is too long; shorten BOX_HOME or set XDG_RUNTIME_DIR",
            path.display()
        ))
    })?;
    let digest = Sha256::digest(home.as_bytes());
    let hex: String = digest.iter().take(6).map(|b| format!("{b:02x}")).collect();
    Ok(PathBuf::from(run).join(format!("box-{hex}.sock")))
}

/// One HTTP/1.1 request over the Unix socket. The connection is closed
/// after each response, so the body is everything after the headers,
/// de-chunked if the server chunked it.
fn http(socket: &Path, method: &str, path: &str, body: &str) -> io::Result<(u16, Vec<u8>)> {
    let mut s = UnixStream::connect(socket)?;
    write!(
        s,
        "{method} {path} HTTP/1.1\r\nHost: box\r\nContent-Type: application/json\r\n\
         Content-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )?;
    s.flush()?;
    parse_response(BufReader::new(s))
}

fn parse_response<R: Read>(mut r: BufReader<R>) -> io::Result<(u16, Vec<u8>)> {
    let bad = |m: &str| io::Error::new(io::ErrorKind::InvalidData, m.to_string());
    let mut line = String::new();
    r.read_line(&mut line)?;
    let status: u16 = line
        .split_whitespace()
        .nth(1)
        .and_then(|c| c.parse().ok())
        .ok_or_else(|| bad("bad status line"))?;
    let mut chunked = false;
    let mut length: Option<usize> = None;
    loop {
        line.clear();
        if r.read_line(&mut line)? == 0 {
            return Err(bad("headers cut short"));
        }
        let l = line.trim_end();
        if l.is_empty() {
            break;
        }
        if let Some((k, v)) = l.split_once(':') {
            let (k, v) = (k.trim().to_ascii_lowercase(), v.trim());
            if k == "transfer-encoding" && v.eq_ignore_ascii_case("chunked") {
                chunked = true;
            } else if k == "content-length" {
                length = v.parse().ok();
            }
        }
    }
    let mut body = Vec::new();
    if chunked {
        loop {
            line.clear();
            r.read_line(&mut line)?;
            let size = usize::from_str_radix(line.trim().split(';').next().unwrap_or(""), 16)
                .map_err(|_| bad("bad chunk size"))?;
            if size == 0 {
                break;
            }
            let start = body.len();
            body.resize(start + size, 0);
            r.read_exact(&mut body[start..])?;
            line.clear();
            r.read_line(&mut line)?; // the CRLF after each chunk
        }
    } else if let Some(n) = length {
        body.resize(n, 0);
        r.read_exact(&mut body)?;
    } else {
        r.read_to_end(&mut body)?;
    }
    Ok((status, body))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_content_length_and_chunked_responses() {
        let plain = b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}";
        assert_eq!(
            parse_response(BufReader::new(&plain[..])).unwrap(),
            (200, b"{}".to_vec())
        );
        let chunked = b"HTTP/1.1 404 Not Found\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n";
        assert_eq!(
            parse_response(BufReader::new(&chunked[..])).unwrap(),
            (404, b"abcde".to_vec())
        );
    }

    #[test]
    fn socket_path_follows_box_home_and_falls_back() {
        std::env::set_var("BOX_HOME", "/tmp/short");
        assert_eq!(
            default_socket().unwrap(),
            PathBuf::from("/tmp/short/box.sock")
        );
        std::env::set_var("BOX_HOME", format!("/{}", "x".repeat(120)));
        std::env::set_var("XDG_RUNTIME_DIR", "/run/user/1000");
        let p = default_socket().unwrap().display().to_string();
        assert!(
            p.starts_with("/run/user/1000/box-")
                && p.ends_with(".sock")
                && p.len() == "/run/user/1000/box-".len() + 12 + 5,
            "{p}"
        );
        std::env::remove_var("XDG_RUNTIME_DIR");
        assert!(default_socket().is_err());
        std::env::remove_var("BOX_HOME");
    }

    #[test]
    fn check_reports_the_last_stderr_line() {
        let o = Output {
            exit_code: 3,
            stdout: vec![],
            stderr: b"warn\nboom\n".to_vec(),
            duration_ms: 1,
            timed_out: false,
            truncated: false,
        };
        let e = o.check().unwrap_err();
        assert_eq!(e.to_string(), "box: exit 3: boom");
    }
}
