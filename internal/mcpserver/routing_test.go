package mcpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func TestPlanExecutionMCP(t *testing.T) {
	for _, project := range []string{"alpha", "beta"} {
		t.Run(project, func(t *testing.T) {
			profilePath := "../../testdata/projects/" + project + ".json"
			policyPath := filepath.Join(t.TempDir(), "routing.json")
			data, err := os.ReadFile("../../testdata/routing/" + project + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(policyPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			handler, err := NewHandler(profilePath, "", policyPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(policyPath); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "routing-integration", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: server.Client()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 2 {
				t.Fatalf("discovery: %+v %v", tools, err)
			}
			found := false
			for _, tool := range tools.Tools {
				if tool.Name == "plan_execution" {
					found = true
					if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
						t.Fatal("routing annotations must remain read-only and closed-world")
					}
				}
			}
			if !found {
				t.Fatal("plan_execution missing")
			}
			for _, task := range []string{"IMPLEMENTATION", "UNKNOWN", "unmapped", ""} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plan_execution", Arguments: map[string]any{"task_type": task}})
				if err != nil || result.IsError {
					t.Fatalf("call: %+v %v", result, err)
				}
				data, _ := json.Marshal(result.StructuredContent)
				var got core.RoutingDecision
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				policy, err := config.LoadRoutingPolicy("../../testdata/routing/"+project+".json", project)
				if err != nil {
					t.Fatal(err)
				}
				want := core.PlanExecution(&policy, core.ExecutionRequest{TaskType: task})
				if got != want {
					t.Fatalf("decision: %s want %+v", data, want)
				}
				t.Logf("SDK MCP %s task_type=%q: %s", project, task, data)
			}
			missing, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plan_execution", Arguments: map[string]any{}})
			if err != nil || missing.IsError {
				t.Fatalf("missing type must reach NEEDS_CONTEXT: %+v %v", missing, err)
			}
			data, _ = json.Marshal(missing.StructuredContent)
			var decision core.RoutingDecision
			if json.Unmarshal(data, &decision) != nil || decision.Reason != "NEEDS_CONTEXT" || decision.RoutingAllowed {
				t.Fatalf("missing context: %s", data)
			}
			for _, args := range []any{
				map[string]any{"task_type": 7},
				map[string]any{"Task_Type": "IMPLEMENTATION"},
				map[string]any{"task_type": "IMPLEMENTATION", "executor_id": "foreign"},
				map[string]any{"task_type": "IMPLEMENTATION", "policy": "other.json"},
				map[string]any{"task_type": "IMPLEMENTATION", "project_id": "foreign"},
				map[string]any{"task_type": "IMPLEMENTATION", "execute": true},
			} {
				got, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "plan_execution", Arguments: args})
				if err == nil && (got == nil || !got.IsError) {
					t.Fatalf("client policy/authority override accepted: %+v", got)
				}
			}
		})
	}
}

func TestInvalidRoutingPolicyStopsHandler(t *testing.T) {
	if h, err := NewHandler("../../testdata/projects/alpha.json", "", "../../testdata/routing/beta.json"); err == nil || h != nil {
		t.Fatal("cross-project routing policy accepted")
	}
}
