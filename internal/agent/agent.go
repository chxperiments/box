// Package agent is the channel into a running sandbox. krun has no exec -- a
// microVM has its own kernel, so there is no host-side namespace to step into
// -- so a sandbox brought up with `box up` runs this agent as its main
// process instead, and every `box exec` is a request to it. That turns a
// command from "boot a VM" into "open a socket", which is where the speed
// comes from.
//
// The wire format is a stream of frames: one type byte, a big-endian uint32
// length, then the payload. A session is
//
//	client  hello{token, argv, ...}
//	agent   ready{kernel}
//	client  go                       (only once the kernel is acceptable)
//	both    stdin / stdout / stderr frames
//	agent   exit{code}
//
// The client checks the kernel before the command starts, so a sandbox that is
// somehow not a microVM is refused before anything runs, the same promise
// `box run` makes.
package agent

import (
	"bufio"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Port is where the agent listens inside the guest. The host reaches it only
// through a port published on 127.0.0.1, never on an external interface.
const Port = 7700

// TokenEnv carries the per-VM secret to the agent. Anyone on the host can
// connect to a loopback port, so the token is what makes the agent answer
// only to the box that started it.
const TokenEnv = "BOX_AGENT_TOKEN"

// ExitTimeout matches timeout(1), as `box run` does.
const ExitTimeout = 124

const (
	fHello    = 'H'
	fReady    = 'R'
	fGo       = 'G'
	fStdin    = 'I'
	fStdinEOF = 'i'
	fStdout   = 'O'
	fStderr   = 'E'
	fExit     = 'X'
)

// maxFrame bounds what a peer can make the other side allocate.
const maxFrame = 1 << 20

// Request is what the client asks the agent to run. An empty Argv is a ping:
// the agent answers ready and exits 0, which is how `up` knows it is live.
type Request struct {
	Token          string   `json:"token"`
	Argv           []string `json:"argv"`
	Env            []string `json:"env,omitempty"`
	Dir            string   `json:"dir,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	// NewToken, from an authenticated client, replaces the token for every
	// later connection. A VM restored from a snapshot starts with the token
	// baked into that snapshot, shared by every copy; the first thing the
	// host does is rotate it to one this copy alone knows.
	NewToken string `json:"new_token,omitempty"`
}

// tokenBox holds the current token; rotation swaps it.
type tokenBox struct{ v atomic.Value }

func newTokenBox(t string) *tokenBox { b := &tokenBox{}; b.v.Store(t); return b }
func (b *tokenBox) get() string      { return b.v.Load().(string) }
func (b *tokenBox) set(t string)     { b.v.Store(t) }

type ready struct {
	Kernel string `json:"kernel"`
}

// Result is how a command ended.
type Result struct {
	Kernel   string `json:"-"`
	Code     int    `json:"code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Err      string `json:"err,omitempty"`
}

// conn serialises frame writes: stdout and stderr are copied concurrently.
type conn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

func newConn(c net.Conn) *conn { return &conn{c: c, r: bufio.NewReader(c)} }

func (c *conn) write(t byte, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := c.c.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.c.Write(p)
	return err
}

func (c *conn) writeJSON(t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(t, b)
}

func (c *conn) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds limit", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// frameWriter turns a stream into frames of one type.
type frameWriter struct {
	c *conn
	t byte
}

func (w frameWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += maxFrame {
		end := min(off+maxFrame, len(p))
		if err := w.c.write(w.t, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// Serve runs the agent: it listens on Port and runs whatever an authenticated
// client asks, until the VM is torn down. It never returns on success.
func Serve() error {
	token := os.Getenv(TokenEnv)
	if token == "" {
		return errors.New("no " + TokenEnv + "; the agent is started by `box up`")
	}
	// Commands inherit the agent's environment -- the image's ENV lines --
	// but never the secret that admits a client.
	os.Unsetenv(TokenEnv)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", Port))
	if err != nil {
		return err
	}
	kernel, _ := KernelRelease()
	return serveOn(ln, newTokenBox(token), kernel)
}

// serveOn is Serve on a given listener, so tests can drive the protocol
// without a VM.
func serveOn(ln net.Listener, token *tokenBox, kernel string) error {
	// Anyone on the host can reach the published port, so connections that
	// have not yet shown the token are capped: a local process flooding the
	// agent is turned away at the door instead of starving the owner.
	unauth := make(chan struct{}, maxUnauthenticated)
	for {
		nc, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return err
		}
		if err != nil {
			time.Sleep(10 * time.Millisecond) // e.g. out of fds; do not spin
			continue
		}
		select {
		case unauth <- struct{}{}:
		default:
			nc.Close()
			continue
		}
		go handle(newConn(nc), token, kernel, func() { <-unauth })
	}
}

// maxUnauthenticated bounds handshakes in flight, and helloTimeout how long
// one may take. A real client sends its hello immediately.
const (
	maxUnauthenticated = 16
	helloTimeout       = 3 * time.Second
)

func handle(c *conn, token *tokenBox, kernel string, authed func()) {
	defer c.c.Close()
	var once sync.Once
	release := func() { once.Do(authed) }
	defer release()

	// A peer that connects and says nothing must not hold a slot for long.
	c.c.SetReadDeadline(time.Now().Add(helloTimeout))
	t, p, err := c.read()
	if err != nil || t != fHello {
		return
	}
	var req Request
	if json.Unmarshal(p, &req) != nil {
		return
	}
	// Answer a wrong token with silence, not an error to probe against.
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(token.get())) != 1 {
		return
	}
	if req.NewToken != "" {
		token.set(req.NewToken)
	}
	release()
	if c.writeJSON(fReady, ready{Kernel: kernel}) != nil {
		return
	}
	if len(req.Argv) == 0 {
		c.writeJSON(fExit, Result{Code: 0})
		return
	}
	if t, _, err := c.read(); err != nil || t != fGo {
		return // the client refused this kernel, or went away
	}
	c.c.SetReadDeadline(time.Time{})
	c.writeJSON(fExit, run(c, req))
}

// run executes one request. The command gets its own process group, so a
// timeout or a vanished client kills everything it started, not just the top.
func run(c *conn, req Request) Result {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Dir = req.Dir
	cmd.Stdout = frameWriter{c, fStdout}
	cmd.Stderr = frameWriter{c, fStderr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A background job that keeps stdout open must not hold the session open
	// after the command itself has exited.
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{Code: 127, Err: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return Result{Code: 127, Err: err.Error()}
	}
	killGroup := func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	var timedOut bool
	var mu sync.Mutex
	if req.TimeoutSeconds > 0 {
		t := time.AfterFunc(time.Duration(req.TimeoutSeconds)*time.Second, func() {
			mu.Lock()
			timedOut = true
			mu.Unlock()
			killGroup()
		})
		defer t.Stop()
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		defer stdin.Close()
		for {
			t, p, err := c.read()
			if err != nil {
				// The client is gone: nobody is left to receive the output or
				// the exit code, so do not leave the command running.
				select {
				case <-done:
				default:
					killGroup()
				}
				return
			}
			switch t {
			case fStdin:
				stdin.Write(p)
			case fStdinEOF:
				stdin.Close()
			}
		}
	}()

	err = cmd.Wait()
	mu.Lock()
	defer mu.Unlock()
	if timedOut {
		return Result{Code: ExitTimeout, TimedOut: true}
	}
	return Result{Code: exitStatus(cmd, err)}
}

// exitStatus reports a shell-style status: a command killed by a signal exits
// 128+signal, as it would under sh.
func exitStatus(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState == nil {
		return 127
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	if err != nil {
		return 1
	}
	return 0
}

// Exec runs req through the agent at addr. check sees the guest kernel before
// the command starts and can refuse it. stdin is forwarded when non-nil;
// otherwise the command sees an immediately closed stdin, as under
// `podman run` without -i.
func Exec(addr string, req Request, check func(kernel string) error,
	stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	nc, err := dial(addr)
	if err != nil {
		return Result{}, err
	}
	c := newConn(nc)
	defer nc.Close()

	if err := c.writeJSON(fHello, req); err != nil {
		return Result{}, err
	}
	t, p, err := c.read()
	if err != nil {
		// The agent closes without a word on a bad token.
		return Result{}, fmt.Errorf("agent did not accept the session: %w", err)
	}
	if t != fReady {
		return Result{}, fmt.Errorf("agent protocol error: expected ready, got %q", t)
	}
	var r ready
	if err := json.Unmarshal(p, &r); err != nil {
		return Result{}, err
	}
	if check != nil {
		if err := check(r.Kernel); err != nil {
			return Result{Kernel: r.Kernel}, err
		}
	}
	if len(req.Argv) > 0 {
		if err := c.write(fGo, nil); err != nil {
			return Result{}, err
		}
		go func() {
			if stdin != nil {
				io.Copy(frameWriter{c, fStdin}, stdin)
			}
			c.write(fStdinEOF, nil)
		}()
	}

	for {
		t, p, err := c.read()
		if err != nil {
			return Result{Kernel: r.Kernel}, fmt.Errorf("lost the sandbox mid-command: %w", err)
		}
		switch t {
		case fStdout:
			stdout.Write(p)
		case fStderr:
			stderr.Write(p)
		case fExit:
			var res Result
			if err := json.Unmarshal(p, &res); err != nil {
				return Result{Kernel: r.Kernel}, err
			}
			res.Kernel = r.Kernel
			if res.Err != "" {
				fmt.Fprintf(stderr, "box: %s\n", res.Err)
			}
			return res, nil
		}
	}
}
