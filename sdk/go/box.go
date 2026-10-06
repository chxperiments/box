// Package box is the Go client for box: microVM sandboxes that start
// in milliseconds. It talks to `box serve` over a Unix socket only you can
// open, and starts it for you the first time it is needed.
//
//	c := box.New()
//	sb := c.Sandbox("agent")
//	if err := sb.Up(ctx); err != nil { ... }
//	defer sb.Down(ctx)
//	r, err := sb.Exec(ctx, box.Sh("python3 -c 'print(6*7)'"))
//	fmt.Println(string(r.Stdout), r.ExitCode, r.Duration)
//
// Only the standard library is used.
package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Error is a request the server refused or could not carry out. Code is
// machine-readable: "no_sandbox", "not_up", "bad_name", "bad_request",
// "no_server" or "internal".
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return "box: " + e.Message }

// IsNotFound reports whether err is a request for a sandbox that does not exist.
func IsNotFound(err error) bool { return hasCode(err, "no_sandbox") }

// IsNotUp reports whether err is an Exec on a sandbox that is not up.
func IsNotUp(err error) bool { return hasCode(err, "not_up") }

func hasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Result is how a command ended. A non-zero exit is a Result, not an error.
type Result struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Duration  time.Duration
	TimedOut  bool
	Truncated bool
}

// OK reports whether the command exited 0.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Command is what to run: an argv, with optional stdin and a timeout that
// overrides the Boxfile's timeout_seconds.
type Command struct {
	Argv    []string
	Stdin   []byte
	Timeout time.Duration
}

// Cmd runs argv as-is, with no shell in between.
func Cmd(argv ...string) Command { return Command{Argv: argv} }

// Sh runs a shell command line, as you would type it.
func Sh(line string) Command { return Command{Argv: []string{"sh", "-c", line}} }

// Client is a connection to the local box server.
type Client struct {
	// Socket is the server's socket. New sets the default.
	Socket string
	// Binary is the box executable, used to start the server.
	Binary string
	// Autostart starts `box serve` when nothing is listening.
	Autostart bool
	// Idle is how long an autostarted server lingers unused.
	Idle time.Duration

	http *http.Client
}

// New returns a client for the default socket, which starts the server on
// demand.
func New() *Client {
	bin, err := exec.LookPath("box")
	if err != nil {
		bin = "box"
	}
	sock, _ := DefaultSocket()
	c := &Client{Socket: sock, Binary: bin, Autostart: true, Idle: 15 * time.Minute}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
		},
	}}
	return c
}

// DefaultSocket mirrors the server's own choice: the box root, or the
// per-user runtime directory when that path is too long for a Unix socket.
// It never uses a shared directory, where another user could listen first.
func DefaultSocket() (string, error) {
	home := os.Getenv("BOX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".box")
	}
	p := filepath.Join(home, "box.sock")
	if len(p) <= 103 {
		return p, nil
	}
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		return "", fmt.Errorf("socket path %s is too long; shorten BOX_HOME or set XDG_RUNTIME_DIR", p)
	}
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(run, "box-"+hex.EncodeToString(sum[:6])+".sock"), nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, "http://box"+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt == 0 && c.Autostart && noServer(err) {
				if err := c.start(ctx); err != nil {
					return err
				}
				continue
			}
			if noServer(err) {
				return &Error{Code: "no_server", Message: "server not running at " + c.Socket + "; start it: box serve"}
			}
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			var e struct{ Error, Code string }
			json.NewDecoder(resp.Body).Decode(&e)
			if e.Code == "" {
				e.Code, e.Error = "internal", resp.Status
			}
			return &Error{Code: e.Code, Message: e.Error, Status: resp.StatusCode}
		}
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
}

func noServer(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func (c *Client) start(ctx context.Context) error {
	cmd := exec.Command(c.Binary, "serve", "--socket", c.Socket, "--idle", c.Idle.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive this process
	if err := cmd.Start(); err != nil {
		return &Error{Code: "no_server", Message: "cannot start " + c.Binary + " serve: " + err.Error()}
	}
	cmd.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", c.Socket); err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return &Error{Code: "no_server", Message: "started " + c.Binary + " serve but it never listened on " + c.Socket}
}

// SandboxInfo describes one defined sandbox.
type SandboxInfo struct {
	Name       string `json:"name"`
	Up         bool   `json:"up"`
	Warm       int    `json:"warm"`
	WarmTarget int    `json:"warm_target"`
	Network    string `json:"network"`
	Base       string `json:"base"`
	Error      string `json:"error,omitempty"`
}

// Sandboxes lists every defined sandbox.
func (c *Client) Sandboxes(ctx context.Context) ([]SandboxInfo, error) {
	var out []SandboxInfo
	return out, c.do(ctx, "GET", "/v1/sandboxes", nil, &out)
}

// Sandbox returns a handle on one sandbox, by name. Create and build it
// first with the CLI: box new agent --from tiny-python && box build agent
func (c *Client) Sandbox(name string) *Sandbox { return &Sandbox{Name: name, c: c} }

// Sandbox is one sandbox.
type Sandbox struct {
	Name string
	c    *Client
}

func (s *Sandbox) path(verb string) string { return "/v1/sandboxes/" + s.Name + "/" + verb }

// Up boots the microVM once and keeps it running for Exec. It is not an
// error if it is already up.
func (s *Sandbox) Up(ctx context.Context) error { return s.c.do(ctx, "POST", s.path("up"), nil, nil) }

// Down stops the running microVM; everything outside /data goes with it.
func (s *Sandbox) Down(ctx context.Context) error {
	return s.c.do(ctx, "POST", s.path("down"), nil, nil)
}

// Exec runs cmd in the running microVM. State carries between calls.
func (s *Sandbox) Exec(ctx context.Context, cmd Command) (Result, error) {
	return s.command(ctx, "exec", cmd)
}

// Run runs cmd in a fresh microVM that is destroyed afterwards. With warm:
// in the Boxfile it comes from the pool and starts in milliseconds. Stdin is
// not forwarded to a run.
func (s *Sandbox) Run(ctx context.Context, cmd Command) (Result, error) {
	cmd.Stdin = nil
	return s.command(ctx, "run", cmd)
}

func (s *Sandbox) command(ctx context.Context, verb string, cmd Command) (Result, error) {
	if len(cmd.Argv) == 0 {
		return Result{}, &Error{Code: "bad_request", Message: "empty command"}
	}
	in := struct {
		Argv    []string `json:"argv"`
		Stdin   []byte   `json:"stdin,omitempty"`
		Timeout int      `json:"timeout_seconds,omitempty"`
	}{cmd.Argv, cmd.Stdin, int(cmd.Timeout.Round(time.Second) / time.Second)}
	var out struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     []byte `json:"stdout"`
		Stderr     []byte `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
		TimedOut   bool   `json:"timed_out"`
		Truncated  bool   `json:"truncated"`
	}
	if err := s.c.do(ctx, "POST", s.path(verb), in, &out); err != nil {
		return Result{}, err
	}
	return Result{
		ExitCode:  out.ExitCode,
		Stdout:    out.Stdout,
		Stderr:    out.Stderr,
		Duration:  time.Duration(out.DurationMS) * time.Millisecond,
		TimedOut:  out.TimedOut,
		Truncated: out.Truncated,
	}, nil
}

// Change is one entry of a fork's diff: Kind is "A" added, "M" modified,
// "D" deleted.
type Change struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// Fork branches this sandbox: a new sandbox with the same image and a /data
// that overlays this one's. Running the fork never touches this sandbox.
func (s *Sandbox) Fork(ctx context.Context, name string) (*Sandbox, error) {
	if err := s.c.do(ctx, "POST", s.path("fork"), map[string]string{"as": name}, nil); err != nil {
		return nil, err
	}
	return s.c.Sandbox(name), nil
}

// Diff lists a fork's changes to /data.
func (s *Sandbox) Diff(ctx context.Context) ([]Change, error) {
	var out struct{ Changes []Change }
	return out.Changes, s.c.do(ctx, "GET", s.path("diff"), nil, &out)
}

// Apply merges a fork's changes into its parent and returns how many.
// Refused while either side is up.
func (s *Sandbox) Apply(ctx context.Context) (int, error) {
	var out struct{ Changes int }
	return out.Changes, s.c.do(ctx, "POST", s.path("apply"), nil, &out)
}

// Discard throws a fork's changes away and returns how many.
func (s *Sandbox) Discard(ctx context.Context) (int, error) {
	var out struct{ Changes int }
	return out.Changes, s.c.do(ctx, "POST", s.path("discard"), nil, &out)
}

// WriteFile writes a file inside the running microVM. The write happens in
// the guest, so a path or symlink the sandbox controls can never redirect it
// onto a host file.
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	r, err := s.Exec(ctx, Command{
		Argv:  []string{"sh", "-c", `umask 077; cat > "$1"`, "sh", path},
		Stdin: data,
	})
	if err == nil && !r.OK() {
		err = &Error{Code: "command_failed", Message: fmt.Sprintf("write %s: exit %d: %s", path, r.ExitCode, r.Stderr)}
	}
	return err
}

// ReadFile reads a file from inside the running microVM.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	r, err := s.Exec(ctx, Cmd("cat", "--", path))
	if err == nil && !r.OK() {
		err = &Error{Code: "command_failed", Message: fmt.Sprintf("read %s: exit %d: %s", path, r.ExitCode, r.Stderr)}
	}
	return r.Stdout, err
}
