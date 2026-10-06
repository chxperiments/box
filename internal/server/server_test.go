package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func start(t *testing.T, idle time.Duration) (*http.Client, string, chan error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BOX_HOME", home)
	sock := filepath.Join(home, "box.sock")
	done := make(chan error, 1)
	go func() { done <- Serve(sock, "test", idle) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	return client, sock, done
}

func TestSocketIsOwnerOnly(t *testing.T) {
	_, sock, _ := start(t, 0)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode %o, want 600", perm)
	}
}

func TestSecondServerRefused(t *testing.T) {
	_, sock, _ := start(t, 0)
	if err := Serve(sock, "test", 0); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("second Serve = %v", err)
	}
}

func TestRoutes(t *testing.T) {
	c, _, _ := start(t, 0)
	cases := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/v1/version", "", 200, ""},
		{"GET", "/v1/sandboxes", "", 200, ""},
		{"POST", "/v1/sandboxes/ghost/exec", `{"argv":["true"]}`, 404, "no_sandbox"},
		{"POST", "/v1/sandboxes/ghost/run", `{"argv":["true"]}`, 404, "no_sandbox"},
		{"POST", "/v1/sandboxes/..hidden/up", "", 400, "bad_name"},
		{"POST", "/v1/sandboxes/ghost/exec", `{"argv":[]}`, 400, "bad_request"},
		{"POST", "/v1/sandboxes/ghost/exec", `not json`, 400, "bad_request"},
		{"GET", "/v1/sandboxes/ghost/exec", "", 405, ""},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, "http://box"+tc.path, strings.NewReader(tc.body))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s %s = %d, want %d (%v)", tc.method, tc.path, resp.StatusCode, tc.status, body)
		}
		if tc.code != "" && body["code"] != tc.code {
			t.Errorf("%s %s code = %v, want %s", tc.method, tc.path, body["code"], tc.code)
		}
	}
}

func TestIdleExitRemovesSocket(t *testing.T) {
	_, sock, done := start(t, time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit when idle")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestCappedKeepsHeadAndFlagsTruncation(t *testing.T) {
	var c capped
	c.Write(make([]byte, MaxOutput-1))
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("write past the cap = %d, %v; must not fail the stream", n, err)
	}
	if len(c.b) != MaxOutput || !c.truncated {
		t.Fatalf("len %d truncated %v", len(c.b), c.truncated)
	}
}
