"""Python SDK for box.

Every sandbox is a microVM with its own kernel. The SDK talks to
``box serve`` over a Unix socket only you can open, and starts it for you
the first time it is needed.

    from sdbox import Sandbox

    with Sandbox("agent") as sb:                 # up on enter, down on exit
        r = sb.exec("python3 -c 'print(6 * 7)'")
        print(r.stdout_text, r.exit_code, r.duration_ms)

    r = Sandbox("agent").run(["pytest", "-q"])  # a fresh microVM, then gone
    r.check()                                   # raise if it failed

No dependencies beyond the standard library.
"""

from __future__ import annotations

import base64
import hashlib
import http.client
import json
import os
import shutil
import socket
import subprocess
import time
from dataclasses import dataclass
from typing import Optional, Sequence, Union

__all__ = [
    "Client",
    "Sandbox",
    "Result",
    "BoxError",
    "NotFound",
    "NotUp",
    "CommandFailed",
    "Change",
]
__version__ = "0.1.0"

Command = Union[str, Sequence[str]]


class BoxError(Exception):
    """The server refused or could not carry out a request."""

    def __init__(self, message: str, code: str = "internal", status: int = 0):
        super().__init__(message)
        self.code = code
        self.status = status


class NotFound(BoxError):
    """No sandbox by that name."""


class NotUp(BoxError):
    """exec was called on a sandbox that is not up."""


class CommandFailed(BoxError):
    """Raised by Result.check() for a command that exited non-zero."""

    def __init__(self, result: "Result"):
        tail = result.stderr_text.strip().splitlines()[-1:] or [""]
        super().__init__(f"exit {result.exit_code}: {tail[0]}", code="command_failed")
        self.result = result


@dataclass
class Result:
    """How a command ended. A non-zero exit is a result, not an exception."""

    exit_code: int
    stdout: bytes
    stderr: bytes
    duration_ms: int
    timed_out: bool = False
    truncated: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0

    @property
    def stdout_text(self) -> str:
        return self.stdout.decode("utf-8", "replace")

    @property
    def stderr_text(self) -> str:
        return self.stderr.decode("utf-8", "replace")

    def check(self) -> "Result":
        """Return self, or raise CommandFailed if the command did not succeed."""
        if not self.ok:
            raise CommandFailed(self)
        return self


def _default_socket() -> str:
    # Mirrors sandbox.SocketPath in the Go server: the box root, or the
    # per-user runtime dir when that path is too long for a Unix socket.
    home = os.environ.get("BOX_HOME") or os.path.join(os.path.expanduser("~"), ".box")
    path = os.path.join(home, "box.sock")
    if len(path.encode()) <= 103:
        return path
    run = os.environ.get("XDG_RUNTIME_DIR")
    if not run:
        raise BoxError(f"socket path {path} is too long for a Unix socket; "
                           "shorten BOX_HOME or set XDG_RUNTIME_DIR", code="bad_socket")
    digest = hashlib.sha256(home.encode()).hexdigest()[:12]
    return os.path.join(run, f"box-{digest}.sock")


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: Optional[float]):
        super().__init__("box", timeout=timeout)
        self._path = path

    def connect(self) -> None:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self._path)
        self.sock = s


def _argv(cmd: Command) -> list:
    # A string is a shell command line, as you would type it; a list is argv,
    # passed as-is with no shell in between.
    if isinstance(cmd, str):
        return ["sh", "-c", cmd]
    argv = list(cmd)
    if not argv or not all(isinstance(a, str) for a in argv):
        raise TypeError("command must be a string or a non-empty list of strings")
    return argv


@dataclass(frozen=True)
class Change:
    """One entry of a fork's diff: kind is "A" added, "M" modified, "D" deleted."""

    kind: str
    path: str


class Client:
    """A connection to the local box server.

    socket:    path to the server socket (default ~/.box/box.sock).
    binary:    the box executable, used to start the server if needed.
    autostart: start ``box serve`` when nothing is listening. It exits by
               itself after ``idle`` without requests.
    """

    def __init__(
        self,
        socket: Optional[str] = None,
        binary: Optional[str] = None,
        autostart: bool = True,
        idle: str = "15m",
    ):
        self.socket = socket or _default_socket()
        self.binary = binary or shutil.which("box") or "box"
        self.autostart = autostart
        self.idle = idle

    # -- transport ---------------------------------------------------------

    def _request(self, method: str, path: str, body: Optional[dict] = None,
                 timeout: Optional[float] = None) -> object:
        payload = json.dumps(body).encode() if body is not None else None
        for attempt in (0, 1):
            conn = _UnixConnection(self.socket, timeout)
            try:
                conn.request(method, path, body=payload,
                             headers={"Content-Type": "application/json"})
                resp = conn.getresponse()
                data = resp.read()
                break
            except (FileNotFoundError, ConnectionRefusedError):
                if attempt or not self.autostart:
                    raise BoxError(
                        f"box server not running at {self.socket}; start it: box serve",
                        code="no_server",
                    )
                self._start_server()
            finally:
                conn.close()
        decoded = json.loads(data) if data else None
        if resp.status >= 400:
            err = decoded or {}
            cls = {"no_sandbox": NotFound, "not_up": NotUp}.get(err.get("code"), BoxError)
            raise cls(err.get("error", f"HTTP {resp.status}"), err.get("code", "internal"), resp.status)
        return decoded

    def _start_server(self) -> None:
        subprocess.Popen(
            [self.binary, "serve", "--socket", self.socket, "--idle", self.idle],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,  # outlives this process and its ^C
        )
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                    s.connect(self.socket)
                return
            except OSError:
                time.sleep(0.02)
        raise BoxError(f"started {self.binary} serve but it never listened on {self.socket}",
                           code="no_server")

    # -- API ---------------------------------------------------------------

    def version(self) -> dict:
        return self._request("GET", "/v1/version")  # type: ignore[return-value]

    def sandboxes(self) -> list:
        """Every defined sandbox, with whether it is up and its warm pool."""
        return self._request("GET", "/v1/sandboxes")  # type: ignore[return-value]

    def sandbox(self, name: str) -> "Sandbox":
        return Sandbox(name, client=self)


class Sandbox:
    """One sandbox, by name. Create and build it first with the CLI:

        box new agent --from tiny-python && box build agent

    As a context manager it brings the sandbox up on entry and down on exit,
    unless it was already up, in which case it is left as it was found.
    """

    def __init__(self, name: str, client: Optional[Client] = None):
        self.name = name
        self.client = client or Client()
        self._downs_on_exit = False

    def __repr__(self) -> str:
        return f"Sandbox({self.name!r})"

    def _path(self, verb: str) -> str:
        return f"/v1/sandboxes/{self.name}/{verb}"

    def up(self) -> dict:
        """Boot the microVM once and keep it running for exec."""
        return self.client._request("POST", self._path("up"))  # type: ignore[return-value]

    def down(self) -> None:
        """Stop the running microVM. Everything outside /data goes with it."""
        self.client._request("POST", self._path("down"))

    def _command(self, verb: str, cmd: Command, stdin: Union[bytes, str, None],
                 timeout: Optional[int]) -> Result:
        body: dict = {"argv": _argv(cmd)}
        if stdin is not None:
            raw = stdin.encode() if isinstance(stdin, str) else stdin
            body["stdin"] = base64.b64encode(raw).decode()
        if timeout:
            body["timeout_seconds"] = int(timeout)
        r = self.client._request("POST", self._path(verb), body)
        assert isinstance(r, dict)
        return Result(
            exit_code=r["exit_code"],
            stdout=base64.b64decode(r["stdout"]),
            stderr=base64.b64decode(r["stderr"]),
            duration_ms=r["duration_ms"],
            timed_out=r.get("timed_out", False),
            truncated=r.get("truncated", False),
        )

    def exec(self, cmd: Command, stdin: Union[bytes, str, None] = None,
             timeout: Optional[int] = None) -> Result:
        """Run in the running microVM (see up). State carries between calls.

        cmd is a shell string or an argv list. timeout, in seconds, overrides
        the Boxfile's timeout_seconds; a timed-out command exits 124.
        """
        return self._command("exec", cmd, stdin, timeout)

    def run(self, cmd: Command, timeout: Optional[int] = None) -> Result:
        """Run in a fresh microVM that is destroyed afterwards. With ``warm:``
        in the Boxfile it comes from the pool and starts in milliseconds."""
        return self._command("run", cmd, None, timeout)

    def write_file(self, path: str, data: Union[bytes, str], mode: str = "0644") -> None:
        """Write a file inside the running microVM.

        The write happens in the guest, not on the host, so a path or symlink
        the sandbox controls can never redirect it onto a host file.
        """
        if not (len(mode) in (3, 4) and all(c in "01234567" for c in mode)):
            raise ValueError(f"mode must be octal like '0644', got {mode!r}")
        script = 'umask 077; cat > "$1" && chmod "$2" "$1"'
        self.exec(["sh", "-c", script, "sh", path, mode], stdin=data).check()

    # -- forks ------------------------------------------------------------

    def fork(self, name: str) -> "Sandbox":
        """Branch this sandbox: a new sandbox with the same image and a /data
        that overlays this one's. Running the fork never touches this
        sandbox; diff shows its changes, apply merges them back."""
        self.client._request("POST", self._path("fork"), {"as": name})
        return Sandbox(name, client=self.client)

    def diff(self) -> list:
        """A fork's changes to /data: [Change(kind, path)], kind A/M/D."""
        r = self.client._request("GET", self._path("diff"))
        return [Change(c["kind"], c["path"]) for c in r["changes"]]  # type: ignore[index]

    def apply(self) -> int:
        """Merge a fork's changes into its parent's /data. Returns how many
        changes were applied. Refused while either side is up."""
        return self.client._request("POST", self._path("apply"))["changes"]  # type: ignore[index]

    def discard(self) -> int:
        """Throw a fork's changes away. Returns how many were discarded."""
        return self.client._request("POST", self._path("discard"))["changes"]  # type: ignore[index]

    def read_file(self, path: str) -> bytes:
        """Read a file from inside the running microVM."""
        return self.exec(["cat", "--", path]).check().stdout

    def __enter__(self) -> "Sandbox":
        r = self.up()
        self._downs_on_exit = not r.get("already", False)
        return self

    def __exit__(self, *exc) -> None:
        if self._downs_on_exit:
            self.down()
