// Package mcp serves box to AI agents over the Model Context Protocol:
// JSON-RPC 2.0 on stdin and stdout, one message per line. Each tool is a
// request to the same handler `box serve` exposes, called in-process,
// so an agent gets exactly the validation, guards and isolation checks the
// SDKs get, and nothing more.
//
// apply is not offered unless the server is started with it allowed: a fork
// exists so that a human reviews an agent's work before it reaches real
// data, and an agent that can apply its own fork skips that review.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// maxText bounds what one tool result hands back to the model.
const maxText = 64 << 10

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server is one MCP session.
type Server struct {
	api         http.Handler
	version     string
	allowApply  bool
	tools       []tool
	toolsByName map[string]tool
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(s *Server, args map[string]any) (string, bool)
}

// New returns a server answering through api, the local API's handler.
func New(api http.Handler, version string, allowApply bool) *Server {
	s := &Server{api: api, version: version, allowApply: allowApply, toolsByName: map[string]tool{}}
	for _, t := range allTools() {
		if t.Name == "apply" && !allowApply {
			continue
		}
		s.tools = append(s.tools, t)
		s.toolsByName[t.Name] = t
	}
	return s
}

// Serve handles messages from r until it closes, answering on w.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification: nothing to answer
		}
		res, rerr := s.handle(req)
		out := response{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr}
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "box", "version": s.version},
			"instructions": "box runs commands in disposable microVM sandboxes, each with its own kernel. " +
				"Sandboxes are created and built by a human with the box CLI; list them with list_sandboxes. " +
				"`run` uses a fresh VM per command; `up` then `exec` keeps one VM and its state between commands. " +
				"To try changes safely, `fork` a sandbox and work in the fork; a human reviews `diff` and applies it.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		t, ok := s.toolsByName[p.Name]
		if !ok {
			return nil, &rpcError{-32602, "unknown tool: " + p.Name}
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		text, isErr := t.call(s, p.Arguments)
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// call makes one request to the API in-process and decodes its JSON.
func (s *Server) call(method, path string, body any) (map[string]any, []any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://box"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := &recorder{header: http.Header{}, code: 200}
	s.api.ServeHTTP(rec, req)
	var obj map[string]any
	var arr []any
	if err := json.Unmarshal(rec.body.Bytes(), &obj); err != nil {
		if err := json.Unmarshal(rec.body.Bytes(), &arr); err != nil {
			return nil, nil, fmt.Errorf("unreadable response: %s", strings.TrimSpace(rec.body.String()))
		}
	}
	if rec.code >= 400 {
		msg, _ := obj["error"].(string)
		if msg == "" {
			msg = http.StatusText(rec.code)
		}
		return nil, nil, fmt.Errorf("%s", msg)
	}
	return obj, arr, nil
}

type recorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func str(args map[string]any, k string) string {
	v, _ := args[k].(string)
	return v
}

func num(args map[string]any, k string) int {
	if v, ok := args[k].(float64); ok {
		return int(v)
	}
	return 0
}

func clip(b []byte) string {
	if len(b) > maxText {
		return string(b[:maxText]) + fmt.Sprintf("\n[... %d more bytes not shown]", len(b)-maxText)
	}
	return string(b)
}

// commandResult renders an exec/run response for the model. A non-zero exit
// is a result, not a tool failure, so it is not marked as an error.
func commandResult(r map[string]any) string {
	dec := func(k string) []byte {
		s, _ := r[k].(string)
		b, _ := base64.StdEncoding.DecodeString(s)
		return b
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit code %v in %v ms", r["exit_code"], r["duration_ms"])
	if t, _ := r["timed_out"].(bool); t {
		b.WriteString(" (timed out)")
	}
	if out := dec("stdout"); len(out) > 0 {
		b.WriteString("\n\nstdout:\n" + clip(out))
	}
	if errb := dec("stderr"); len(errb) > 0 {
		b.WriteString("\n\nstderr:\n" + clip(errb))
	}
	return b.String()
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var (
	pSandbox = map[string]any{"type": "string", "description": "sandbox name, from list_sandboxes"}
	pCommand = map[string]any{"type": "string", "description": "a shell command line, run with sh -c"}
	pTimeout = map[string]any{"type": "integer", "description": "seconds; the command exits 124 when it is hit"}
)

func allTools() []tool {
	cmd := func(verb string) func(*Server, map[string]any) (string, bool) {
		return func(s *Server, a map[string]any) (string, bool) {
			body := map[string]any{"argv": []string{"sh", "-c", str(a, "command")}}
			if t := num(a, "timeout_seconds"); t > 0 {
				body["timeout_seconds"] = t
			}
			if in := str(a, "stdin"); in != "" {
				body["stdin"] = base64.StdEncoding.EncodeToString([]byte(in))
			}
			r, _, err := s.call("POST", "/v1/sandboxes/"+str(a, "sandbox")+"/"+verb, body)
			if err != nil {
				return err.Error(), true
			}
			return commandResult(r), false
		}
	}
	simple := func(method, verb, done string) func(*Server, map[string]any) (string, bool) {
		return func(s *Server, a map[string]any) (string, bool) {
			r, _, err := s.call(method, "/v1/sandboxes/"+str(a, "sandbox")+"/"+verb, nil)
			if err != nil {
				return err.Error(), true
			}
			if n, ok := r["changes"]; ok {
				return fmt.Sprintf("%s: %v changes", done, n), false
			}
			return done, false
		}
	}
	return []tool{
		{
			Name:        "list_sandboxes",
			Description: "List the sandboxes you can use, whether each is up, and its warm pool.",
			InputSchema: schema(map[string]any{}),
			call: func(s *Server, _ map[string]any) (string, bool) {
				_, arr, err := s.call("GET", "/v1/sandboxes", nil)
				if err != nil {
					return err.Error(), true
				}
				b, _ := json.MarshalIndent(arr, "", "  ")
				return string(b), false
			},
		},
		{
			Name: "run",
			Description: "Run a command in a fresh microVM that is destroyed afterwards. Nothing outside /data " +
				"survives. Use this for one-off commands.",
			InputSchema: schema(map[string]any{"sandbox": pSandbox, "command": pCommand, "timeout_seconds": pTimeout}, "sandbox", "command"),
			call:        cmd("run"),
		},
		{
			Name:        "up",
			Description: "Boot a sandbox's microVM and keep it running, so exec calls share state.",
			InputSchema: schema(map[string]any{"sandbox": pSandbox}, "sandbox"),
			call:        simple("POST", "up", "up"),
		},
		{
			Name: "exec",
			Description: "Run a command in a sandbox's running microVM (see up). Files, installed packages " +
				"and background processes carry over between calls.",
			InputSchema: schema(map[string]any{"sandbox": pSandbox, "command": pCommand, "timeout_seconds": pTimeout,
				"stdin": map[string]any{"type": "string", "description": "text sent to the command's stdin"}}, "sandbox", "command"),
			call: cmd("exec"),
		},
		{
			Name:        "down",
			Description: "Stop a sandbox's running microVM. Everything outside /data is discarded.",
			InputSchema: schema(map[string]any{"sandbox": pSandbox}, "sandbox"),
			call:        simple("POST", "down", "down"),
		},
		{
			Name:        "write_file",
			Description: "Write a text file inside a running sandbox (see up).",
			InputSchema: schema(map[string]any{"sandbox": pSandbox,
				"path":    map[string]any{"type": "string", "description": "absolute path in the guest, e.g. /data/task.py"},
				"content": map[string]any{"type": "string"}}, "sandbox", "path", "content"),
			call: func(s *Server, a map[string]any) (string, bool) {
				body := map[string]any{
					"argv":  []string{"sh", "-c", `umask 077; cat > "$1"`, "sh", str(a, "path")},
					"stdin": base64.StdEncoding.EncodeToString([]byte(str(a, "content"))),
				}
				r, _, err := s.call("POST", "/v1/sandboxes/"+str(a, "sandbox")+"/exec", body)
				if err != nil {
					return err.Error(), true
				}
				if code, _ := r["exit_code"].(float64); code != 0 {
					return commandResult(r), true
				}
				return "written " + str(a, "path"), false
			},
		},
		{
			Name:        "read_file",
			Description: "Read a file from inside a running sandbox (see up).",
			InputSchema: schema(map[string]any{"sandbox": pSandbox,
				"path": map[string]any{"type": "string", "description": "absolute path in the guest"}}, "sandbox", "path"),
			call: func(s *Server, a map[string]any) (string, bool) {
				r, _, err := s.call("POST", "/v1/sandboxes/"+str(a, "sandbox")+"/exec",
					map[string]any{"argv": []string{"cat", "--", str(a, "path")}})
				if err != nil {
					return err.Error(), true
				}
				if code, _ := r["exit_code"].(float64); code != 0 {
					return commandResult(r), true
				}
				out, _ := base64.StdEncoding.DecodeString(r["stdout"].(string))
				return clip(out), false
			},
		},
		{
			Name: "fork",
			Description: "Branch a sandbox: a new sandbox with the same image whose /data overlays the parent's. " +
				"Work in the fork; the parent is untouched until a human applies it.",
			InputSchema: schema(map[string]any{"sandbox": pSandbox,
				"name": map[string]any{"type": "string", "description": "name for the fork"}}, "sandbox", "name"),
			call: func(s *Server, a map[string]any) (string, bool) {
				_, _, err := s.call("POST", "/v1/sandboxes/"+str(a, "sandbox")+"/fork", map[string]any{"as": str(a, "name")})
				if err != nil {
					return err.Error(), true
				}
				return "forked " + str(a, "sandbox") + " to " + str(a, "name"), false
			},
		},
		{
			Name:        "diff",
			Description: "List a fork's changes to /data: A added, M modified, D deleted.",
			InputSchema: schema(map[string]any{"sandbox": map[string]any{"type": "string", "description": "the fork"}}, "sandbox"),
			call: func(s *Server, a map[string]any) (string, bool) {
				r, _, err := s.call("GET", "/v1/sandboxes/"+str(a, "sandbox")+"/diff", nil)
				if err != nil {
					return err.Error(), true
				}
				changes, _ := r["changes"].([]any)
				if len(changes) == 0 {
					return "no changes", false
				}
				var b strings.Builder
				for _, c := range changes {
					m, _ := c.(map[string]any)
					fmt.Fprintf(&b, "%v %v\n", m["kind"], m["path"])
				}
				return b.String(), false
			},
		},
		{
			Name:        "discard",
			Description: "Throw away a fork's changes to /data.",
			InputSchema: schema(map[string]any{"sandbox": map[string]any{"type": "string", "description": "the fork"}}, "sandbox"),
			call:        simple("POST", "discard", "discarded"),
		},
		{
			Name:        "apply",
			Description: "Merge a fork's changes into its parent's /data. Refused while either is up.",
			InputSchema: schema(map[string]any{"sandbox": map[string]any{"type": "string", "description": "the fork"}}, "sandbox"),
			call:        simple("POST", "apply", "applied"),
		},
	}
}
