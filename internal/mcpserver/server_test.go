package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	workspace := t.TempDir()
	profile := filepath.Join(t.TempDir(), "profile.json")
	data, _ := json.Marshal(config.Profile{SchemaVersion: 1, ProjectID: "alpha", WorkspaceRoot: workspace,
		TaskAllowlist: []string{"scope-check"}, Permissions: []string{"source.read"}})
	if err := os.WriteFile(profile, data, 0600); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(profile, "")
	if err != nil {
		t.Fatal(err)
	}
	// No request can reload or replace the operator-selected startup profile.
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, workspace
}

func TestMCPHTTPDiscoveryAndCalls(t *testing.T) {
	s, workspace := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "scope-integration", Version: "1"}, nil)
	session, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.URL + "/mcp", HTTPClient: s.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult(); got.ServerInfo.Name != "orchestrator-mcp" || got.ProtocolVersion == "" {
		t.Fatalf("initialize: %+v", got)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "check_scope" {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	annotation := tools.Tools[0].Annotations
	if annotation == nil || !annotation.ReadOnlyHint || annotation.OpenWorldHint == nil || *annotation.OpenWorldHint {
		t.Fatalf("read-only tool annotations: %+v", annotation)
	}
	for _, tc := range []struct {
		name, task, root, permission, reason string
		allowed                              bool
	}{
		{"allowed", "scope-check", workspace, "source.read", "DRY_RUN_ONLY", true},
		{"task", "unapproved", workspace, "source.read", "TASK_NOT_ALLOWLISTED", false},
		{"project", "scope-check", filepath.Join(workspace, "foreign-project"), "source.read", "WORKSPACE_SCOPE_MISMATCH", false},
		{"execution", "scope-check", workspace, "task.launch", "PERMISSION_NOT_GRANTED", false},
		{"empty", "scope-check", workspace, "", "REQUEST_REQUIRED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := core.Request{TaskID: tc.task, Workspace: tc.root, Permission: tc.permission}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "check_scope", Arguments: request})
			if err != nil || res.IsError {
				t.Fatalf("call: %+v %v", res, err)
			}
			data, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var got core.Decision
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != 1 || got.Mode != "dry-run" || got.ProjectID != "alpha" || got.Request != request || got.ExecutionStarted || got.ScopeAllowed != tc.allowed || got.Reason != tc.reason {
				t.Fatalf("decision: %s", data)
			}
			t.Log(string(data))
		})
	}
	for _, tc := range []struct {
		name string
		args any
	}{
		{"missing", map[string]any{"task_id": "scope-check"}},
		{"type", map[string]any{"task_id": 42, "workspace": workspace, "permission": "source.read"}},
		{"profile", map[string]any{"task_id": "scope-check", "workspace": workspace, "permission": "source.read", "profile": "arbitrary.json"}},
		{"project_id", map[string]any{"task_id": "scope-check", "workspace": workspace, "permission": "source.read", "project_id": "foreign"}},
		{"execute", map[string]any{"task_id": "scope-check", "workspace": workspace, "permission": "source.read", "execute": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "check_scope", Arguments: tc.args})
			if err == nil && (res == nil || !res.IsError) {
				t.Fatalf("invalid input accepted: %+v", res)
			}
		})
	}
	for _, name := range []string{"execute", "verify_source"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("unknown/unconfigured tool accepted: %+v", res)
		}
	}
	t.Logf("initialize/discovery/calls completed at %s/mcp", s.URL)
}

func TestHTTPBoundaries(t *testing.T) {
	s, _ := testServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"boundary-test","version":"1"}}}`
	for _, tc := range []struct {
		name, method, path, host, origin, fetch string
		status                                  int
	}{
		{"native", "POST", "/mcp", "", "", "", 200},
		{"same-origin", "POST", "/mcp", "", s.URL, "", 200},
		{"host", "POST", "/mcp", "attacker.invalid", "", "", 403},
		{"host-suffix", "POST", "/mcp", "127.0.0.1.attacker.invalid", "", "", 403},
		{"origin", "POST", "/mcp", "", "https://attacker.invalid", "", 403},
		{"null-origin", "POST", "/mcp", "", "null", "", 403},
		{"fetch-metadata", "POST", "/mcp", "", "", "cross-site", 403},
		{"explicit-origin", "POST", "/mcp", "", "https://attacker.invalid", "same-origin", 403},
		{"get-origin", "GET", "/mcp", "", "https://attacker.invalid", "", 403},
		{"other-endpoint", "POST", "/execute", "", "", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, s.URL+tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetch != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			res, err := s.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", res.StatusCode, tc.status)
			}
		})
	}
	for _, length := range []int64{int64(64<<10 + 1), -1} {
		req, _ := http.NewRequest("POST", s.URL+"/mcp", strings.NewReader(strings.Repeat(" ", 64<<10+1)))
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body length=%d status=%d", length, res.StatusCode)
		}
	}
}

func TestInvalidStartupProfile(t *testing.T) {
	if h, err := NewHandler("", ""); err == nil || h != nil {
		t.Fatal("missing profile accepted")
	}
}

func TestHostProtectionBehindContainerNAT(t *testing.T) {
	server, _ := testServer(t)
	local := &net.TCPAddr{IP: net.ParseIP("172.18.0.2"), Port: 8080}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"nat-test","version":"1"}}}`
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "attacker.invalid", "127.0.0.1.attacker.invalid:8080", "172.18.0.2:8080", "0.0.0.0:8080"} {
		t.Run(host, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://"+host+"/mcp", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			w := httptest.NewRecorder()
			server.Config.Handler.ServeHTTP(w, req)
			want := http.StatusForbidden
			if host == "127.0.0.1:8080" || host == "localhost:8080" || host == "[::1]:8080" {
				want = http.StatusOK
			}
			if w.Code != want || req.Context().Value(http.LocalAddrContextKey) != local {
				t.Fatalf("NAT Host protection: %s status=%d want=%d", host, w.Code, want)
			}
		})
	}
}
