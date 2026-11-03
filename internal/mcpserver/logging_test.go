package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

type toolLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *toolLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *toolLogBuffer) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := b.buf.String()
	b.buf.Reset()
	return text
}

func TestToolCallLogging(t *testing.T) {
	f := newSourceFixture(t)
	routing, err := os.ReadFile("../../testdata/routing/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(t.TempDir(), "routing.json")
	sourceWrite(t, routingPath, []byte(strings.ReplaceAll(string(routing), `"alpha"`, `"fixture-project"`)))
	policy, err := config.LoadRoutingPolicy(routingPath, f.profile.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var logs toolLogBuffer
	handler, err := newHandler(f.profilePath, f.bindingPath, routingPath, &logs)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "logging-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: server.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 3 {
		t.Fatalf("discovery: %+v %v", tools, err)
	}
	if got := logs.take(); got != "" {
		t.Fatalf("discovery must not produce call logs: %s", got)
	}
	private := "Authorization: Bearer inert-token-user-private-input"
	allowed := core.Request{TaskID: "source-check", Workspace: f.root, Permission: "source.read"}
	denied := core.Request{TaskID: private, Workspace: f.root, Permission: "source.read"}
	proof := &sourceProof{Commit: f.binding.SourceSpec.Head, Tree: f.binding.SourceSpec.Tree, SourceSet: f.binding.SourceSpec.SourceSet,
		ReviewBase: f.binding.SourceSpec.ReviewBase, CurrentBase: f.binding.TaskBase, BriefDigest: f.binding.SourceSpec.BriefDigest}
	for _, tc := range []struct {
		name, tool, loggedTool, status, reason string
		args                                   any
		want                                   any
		wantError                              bool
	}{
		{name: "scope-allowed", tool: "check_scope", loggedTool: "check_scope", status: "success", reason: "DRY_RUN_ONLY", args: allowed, want: core.Plan(&f.profile, allowed)},
		{name: "scope-denied", tool: "check_scope", loggedTool: "check_scope", status: "denied", reason: "TASK_NOT_ALLOWLISTED", args: denied, want: core.Plan(&f.profile, denied)},
		{name: "route-allowed", tool: "plan_execution", loggedTool: "plan_execution", status: "success", reason: "ROUTE_PLANNED", args: core.ExecutionRequest{TaskType: "IMPLEMENTATION"}, want: core.PlanExecution(&policy, core.ExecutionRequest{TaskType: "IMPLEMENTATION"})},
		{name: "route-context", tool: "plan_execution", loggedTool: "plan_execution", status: "needs_context", reason: "NEEDS_CONTEXT", args: core.ExecutionRequest{TaskType: private}, want: core.PlanExecution(&policy, core.ExecutionRequest{TaskType: private})},
		{name: "source-verified", tool: "verify_source", loggedTool: "verify_source", status: "success", reason: "SOURCE_VERIFIED", args: sourceInput{TaskID: "source-check"}, want: sourceResult{SourceVerified: true, Reason: "SOURCE_VERIFIED", Proof: proof}},
		{name: "source-denied", tool: "verify_source", loggedTool: "verify_source", status: "denied", reason: "TASK_NOT_ALLOWLISTED", args: sourceInput{TaskID: private}, want: sourceResult{Reason: "TASK_NOT_ALLOWLISTED"}},
		{name: "invalid-input", tool: "check_scope", loggedTool: "check_scope", status: "error", reason: "TOOL_CALL_ERROR", args: map[string]any{"task_id": private, "workspace": f.root, "permission": "source.read", "token": private}, wantError: true},
		{name: "unknown-tool", tool: private, loggedTool: "unknown", status: "error", reason: "TOOL_CALL_ERROR", args: map[string]any{}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if tc.wantError {
				if err == nil && (result == nil || !result.IsError) {
					t.Fatal("SDK error response changed")
				}
			} else {
				if err != nil || result == nil || result.IsError {
					t.Fatalf("call: %+v %v", result, err)
				}
				data, _ := json.Marshal(tc.want)
				var want any
				if err := json.Unmarshal(data, &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result.StructuredContent, want) {
					t.Fatalf("response changed: got %+v want %+v", result.StructuredContent, want)
				}
			}
			text := logs.take()
			var record map[string]any
			if strings.Count(text, "\n") != 1 || json.Unmarshal([]byte(text), &record) != nil || len(record) != 7 {
				t.Fatalf("expected exactly one metadata-only JSON record: %s", text)
			}
			if record["msg"] != "mcp_tool_call" || record["level"] != "INFO" || record["tool"] != tc.loggedTool || record["status"] != tc.status || record["reason"] != tc.reason {
				t.Fatalf("incorrect metadata: %s", text)
			}
			timestamp, ok := record["time"].(string)
			if _, err := time.Parse(time.RFC3339Nano, timestamp); !ok || err != nil || !strings.HasSuffix(timestamp, "Z") {
				t.Fatalf("timestamp must be UTC: %s", text)
			}
			if elapsed, ok := record["duration_ms"].(float64); !ok || elapsed < 0 {
				t.Fatalf("missing duration in milliseconds: %s", text)
			}
			for _, secret := range []string{private, "source-check", f.root, f.binding.SourceSpec.Brief, f.binding.SourceSpec.Receipts[0].Path, f.binding.SourceSpec.Receipts[0].SHA256, proof.Commit, proof.BriefDigest} {
				if strings.Contains(text, secret) {
					t.Fatalf("private input/response leaked: %s", text)
				}
			}
			t.Log(strings.TrimSpace(text))
		})
	}
}
