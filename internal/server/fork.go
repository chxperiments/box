package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"box/internal/sandbox"
)

// The fork endpoints run box's own fork, diff, apply and discard
// commands rather than re-implementing them: those carry every guard (no
// apply while up, pools drained, strict sandboxes handled under podman
// unshare, symlinks never followed), and one implementation cannot drift
// from the other.
//
//	POST /v1/sandboxes/{name}/fork     {"as": "<fork>"}
//	GET  /v1/sandboxes/{name}/diff     -> {"changes": [{"kind": "A|M|D", "path": "..."}]}
//	POST /v1/sandboxes/{name}/apply    -> {"changes": n}
//	POST /v1/sandboxes/{name}/discard  -> {"changes": n}

type change struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// cli runs `box args...` and returns stdout, or an error carrying stderr.
func cli(args ...string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(self, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errb.String()), "box: "))
		if msg == "" {
			msg = err.Error()
		}
		return "", &cliError{msg}
	}
	return out.String(), nil
}

type cliError struct{ msg string }

func (e *cliError) Error() string { return e.msg }

func failCLI(w http.ResponseWriter, err error) {
	code, status := "bad_request", http.StatusConflict
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "no sandbox"):
		code, status = "no_sandbox", http.StatusNotFound
	case strings.Contains(msg, "not a fork"):
		code, status = "not_fork", http.StatusBadRequest
	case strings.Contains(msg, " is up;"):
		code = "is_up"
	}
	reply(w, status, apiError{Error: msg, Code: code})
}

func parseDiff(out string) []change {
	cs := []change{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if k, p, ok := strings.Cut(l, " "); ok && len(k) == 1 {
			cs = append(cs, change{Kind: k, Path: p})
		}
	}
	return cs
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	name, err := spec(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		As string `json:"as"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		reply(w, http.StatusBadRequest, apiError{Error: "bad request body: " + err.Error(), Code: "bad_request"})
		return
	}
	if err := sandbox.ValidName(req.As); err != nil {
		fail(w, nameErr{err})
		return
	}
	if _, err := cli("fork", name, req.As); err != nil {
		failCLI(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]string{"fork": req.As, "parent": name})
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	name, err := spec(r)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := cli("diff", name)
	if err != nil {
		failCLI(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"changes": parseDiff(out)})
}

// handleMerge is apply or discard: both confirm with -y, since the API
// caller has already decided, and report how many changes they handled.
func (s *server) handleMerge(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := spec(r)
		if err != nil {
			fail(w, err)
			return
		}
		before, err := cli("diff", name)
		if err != nil {
			failCLI(w, err)
			return
		}
		n := len(parseDiff(before))
		if n > 0 {
			if _, err := cli(verb, "-y", name); err != nil {
				failCLI(w, err)
				return
			}
		}
		reply(w, http.StatusOK, map[string]int{"changes": n})
	}
}
