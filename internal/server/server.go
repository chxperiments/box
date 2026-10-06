// Package server is the local API the SDKs speak: HTTP with JSON bodies, over
// a Unix socket only the owner can open. It does what the CLI does -- the same
// runtime calls, the same isolation checks -- but returns output as data
// rather than printing it, and saves an SDK the cost of starting the CLI per
// call.
//
//	GET  /v1/version
//	GET  /v1/sandboxes
//	POST /v1/sandboxes/{name}/up
//	POST /v1/sandboxes/{name}/down
//	POST /v1/sandboxes/{name}/exec   {"argv": [...], "stdin": b64, "timeout_seconds": n}
//	POST /v1/sandboxes/{name}/run    the same, in a fresh microVM
//	POST /v1/sandboxes/{name}/fork, GET .../diff, POST .../apply, .../discard  (fork.go)
//
// exec and run answer {"exit_code", "stdout", "stderr", "duration_ms",
// "timed_out", "truncated"}; stdout and stderr are base64, so binary output
// survives. A command that exits non-zero is a successful request.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"box/internal/runtime"
	"box/internal/sandbox"
)

// MaxOutput caps what one command's stdout or stderr may buffer. Past it the
// output is dropped, not the command: the response says so with truncated.
const MaxOutput = 16 << 20

// maxRequest bounds a request body, which carries stdin.
const maxRequest = 64 << 20

type server struct {
	version  string
	inflight atomic.Int64
	mu       sync.Mutex
	last     time.Time
}

// Serve listens on sock until interrupted or, when idle is non-zero, until no
// request has arrived for that long. The SDKs start it with an idle limit, so
// a server nobody uses does not linger.
func Serve(sock, version string, idle time.Duration) error {
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return fmt.Errorf("already serving on %s", sock)
	}
	os.Remove(sock) // stale: left by a server that did not exit cleanly
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	// The socket is the whole access control: whoever can connect can run
	// commands in every sandbox. Create it owner-only from the first moment
	// rather than chmod it after, which would leave a window.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", sock)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	defer os.Remove(sock)

	s := &server{version: version, last: time.Now()}
	hs := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self := exeIdentity()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
			case <-tick.C:
				if s.inflight.Load() > 0 {
					continue
				}
				// An upgraded box must not be shadowed by the old one still
				// serving: once idle, step aside and let the next SDK call
				// start the new binary.
				replaced := self != "" && exeIdentity() != self
				if !replaced && (idle == 0 || time.Since(s.lastActive()) < idle) {
					continue
				}
			}
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			hs.Shutdown(sctx)
			cancel()
			return
		}
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// exeIdentity fingerprints this process's executable on disk, or "" if it
// cannot be read. A rebuild or upgrade replaces the file and changes it.
func exeIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "gone" // deleted or replaced mid-rename: not this binary
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = uint64(st.Ino)
	}
	return fmt.Sprintf("%d:%d:%d", ino, fi.Size(), fi.ModTime().UnixNano())
}

func (s *server) lastActive() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// track counts a request as activity for the idle limit, for as long as it runs.
func (s *server) track(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.inflight.Add(1)
		defer func() {
			s.mu.Lock()
			s.last = time.Now()
			s.mu.Unlock()
			s.inflight.Add(-1)
		}()
		h(w, r)
	}
}

// NewHandler is the API without a listener, for callers that serve it some
// other way: the MCP server calls it in-process.
func NewHandler(version string) http.Handler {
	return (&server{version: version, last: time.Now()}).routes()
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.track(s.handleVersion))
	mux.HandleFunc("GET /v1/sandboxes", s.track(s.handleList))
	mux.HandleFunc("POST /v1/sandboxes/{name}/up", s.track(s.handleUp))
	mux.HandleFunc("POST /v1/sandboxes/{name}/down", s.track(s.handleDown))
	mux.HandleFunc("POST /v1/sandboxes/{name}/exec", s.track(s.handleCommand(false)))
	mux.HandleFunc("POST /v1/sandboxes/{name}/run", s.track(s.handleCommand(true)))
	mux.HandleFunc("POST /v1/sandboxes/{name}/fork", s.track(s.handleFork))
	mux.HandleFunc("GET /v1/sandboxes/{name}/diff", s.track(s.handleDiff))
	mux.HandleFunc("POST /v1/sandboxes/{name}/apply", s.track(s.handleMerge("apply")))
	mux.HandleFunc("POST /v1/sandboxes/{name}/discard", s.track(s.handleMerge("discard")))
	return mux
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, runtime.ErrNoSandbox):
		status, code = http.StatusNotFound, "no_sandbox"
	case errors.Is(err, runtime.ErrNotUp):
		status, code = http.StatusConflict, "not_up"
	case errors.As(err, new(nameErr)):
		status, code = http.StatusBadRequest, "bad_name"
	}
	reply(w, status, apiError{Error: err.Error(), Code: code})
}

// nameErr marks a request refused for its sandbox name.
type nameErr struct{ err error }

func (e nameErr) Error() string { return e.err.Error() }

// spec validates the name in the path before anything touches the disk.
func spec(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if err := sandbox.ValidName(name); err != nil {
		return "", nameErr{err}
	}
	return name, nil
}

func (s *server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]string{"version": s.version, "api": "v1"})
}

type sandboxInfo struct {
	Name       string `json:"name"`
	Up         bool   `json:"up"`
	Warm       int    `json:"warm"`
	WarmTarget int    `json:"warm_target"`
	Network    string `json:"network"`
	Base       string `json:"base"`
	Error      string `json:"error,omitempty"`
}

func (s *server) handleList(w http.ResponseWriter, _ *http.Request) {
	names, _ := sandbox.List()
	out := []sandboxInfo{}
	for _, n := range names {
		info := sandboxInfo{Name: n, Up: sandbox.IsUp(n), Warm: runtime.Warm(n)}
		if sp, err := runtime.LoadSpec(n); err != nil {
			info.Error = err.Error()
		} else {
			info.WarmTarget, info.Network, info.Base = sp.Warm, sp.Network, sp.Base
		}
		out = append(out, info)
	}
	reply(w, http.StatusOK, out)
}

func (s *server) handleUp(w http.ResponseWriter, r *http.Request) {
	name, err := spec(r)
	if err != nil {
		fail(w, err)
		return
	}
	sp, err := runtime.LoadSpec(name)
	if err != nil {
		fail(w, err)
		return
	}
	if sandbox.IsUp(name) {
		reply(w, http.StatusOK, map[string]any{"up": true, "already": true})
		return
	}
	took, err := runtime.UpChecked(name, sp, nil)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"up": true, "ready_ms": took.Milliseconds()})
}

func (s *server) handleDown(w http.ResponseWriter, r *http.Request) {
	name, err := spec(r)
	if err != nil {
		fail(w, err)
		return
	}
	if !sandbox.Exists(name) {
		fail(w, fmt.Errorf("%w %q", runtime.ErrNoSandbox, name))
		return
	}
	if err := runtime.Down(name); err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"up": false})
}

type commandRequest struct {
	Argv           []string `json:"argv"`
	Stdin          []byte   `json:"stdin,omitempty"` // base64 in JSON
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type commandResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
}

// capped keeps the first MaxOutput bytes and quietly drops the rest: failing
// the write would break the stream to the agent and lose the exit code.
type capped struct {
	b         []byte
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := MaxOutput - len(c.b)
	if room < len(p) {
		c.truncated = true
		if room > 0 {
			c.b = append(c.b, p[:room]...)
		}
		return len(p), nil
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

func (s *server) handleCommand(fresh bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := spec(r)
		if err != nil {
			fail(w, err)
			return
		}
		var req commandRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, apiError{Error: "bad request body: " + err.Error(), Code: "bad_request"})
			return
		}
		if len(req.Argv) == 0 || req.Argv[0] == "" {
			reply(w, http.StatusBadRequest, apiError{Error: "argv is required", Code: "bad_request"})
			return
		}
		sp, err := runtime.LoadSpec(name)
		if err != nil {
			fail(w, err)
			return
		}
		if req.TimeoutSeconds > 0 {
			sp.TimeoutSeconds = req.TimeoutSeconds
		}

		var out, errw capped
		streams := runtime.Streams{Stdout: &out, Stderr: &errw}
		if len(req.Stdin) > 0 {
			streams.Stdin = bytes.NewReader(req.Stdin)
		}
		started := time.Now()
		if fresh {
			err = runtime.RunFresh(name, sp, req.Argv, streams, nil)
		} else {
			err = runtime.Exec(name, sp, req.Argv, streams)
		}
		res := commandResult{
			Stdout:     nonNil(out.b),
			Stderr:     nonNil(errw.b),
			DurationMS: time.Since(started).Milliseconds(),
			Truncated:  out.truncated || errw.truncated,
		}
		switch {
		case err == nil:
		case errors.Is(err, runtime.ErrTimeout):
			res.ExitCode, res.TimedOut = runtime.ExitTimeout, true
		case runtime.ExitCode(err) >= 0:
			res.ExitCode = runtime.ExitCode(err)
		default:
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, res)
	}
}

func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
