// Package mcpserver exposes read-only scope, optional S2 verification and routing over MCP.
package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// NewHandler loads the operator's one profile before accepting any client input.
func NewHandler(profilePath, sourceSpecPath, routingPolicyPath string) (http.Handler, error) {
	return newHandler(profilePath, sourceSpecPath, routingPolicyPath, os.Stderr)
}

func newHandler(profilePath, sourceSpecPath, routingPolicyPath string, logOutput io.Writer) (http.Handler, error) {
	profile, err := config.Load(profilePath)
	if err != nil {
		return nil, err
	}
	var policy *config.RoutingPolicy
	if routingPolicyPath != "" {
		loaded, err := config.LoadRoutingPolicy(routingPolicyPath, profile.ProjectID)
		if err != nil {
			return nil, err
		}
		policy = &loaded
	}
	var binding *sourceBinding
	if sourceSpecPath != "" {
		binding, err = loadSourceBinding(profile, sourceSpecPath)
		if err != nil {
			return nil, err
		}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "orchestrator-mcp", Version: "0.1.0"}, nil)
	closedWorld := false
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_scope",
		Description: "Check task/workspace/permission against the operator's fixed profile. Dry-run only; scope_allowed never grants execution, Claim or authority.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(_ context.Context, _ *mcp.CallToolRequest, input core.Request) (*mcp.CallToolResult, core.Decision, error) {
		// Compare the canonical profile root directly; never resolve arbitrary client paths.
		return nil, core.Plan(&profile, input), nil
	})
	if policy != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "plan_execution",
			Description: "Suggest an executor/capability/reasoning route for an explicit task_type using fixed project policy. Missing or unmapped type needs context. Manual dispatch only; never grants authority, Grant, Claim or execution.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
		}, func(_ context.Context, _ *mcp.CallToolRequest, input core.ExecutionRequest) (*mcp.CallToolResult, core.RoutingDecision, error) {
			return nil, core.PlanExecution(policy, input), nil
		})
	}
	if binding != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "verify_source",
			Description: "Verify the operator's fixed local Git/source/brief/receipt binding for one task. Read-only; proof grants no Claim, execution or future immutability.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input sourceInput) (*mcp.CallToolResult, sourceResult, error) {
			return nil, verifySource(ctx, profile, *binding, input.TaskID), nil
		})
	}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				attr.Value = slog.TimeValue(attr.Value.Time().UTC())
			}
			return attr
		},
	}))
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			started := time.Now()
			result, err := next(ctx, method, req)
			tool, status, reason := "unknown", "error", "TOOL_CALL_ERROR"
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				switch params.Name {
				case "check_scope", "plan_execution", "verify_source":
					tool = params.Name
				}
			}
			if call, ok := result.(*mcp.CallToolResult); err == nil && ok && !call.IsError {
				var decision struct {
					Reason string `json:"reason"`
				}
				if raw, ok := call.StructuredContent.(json.RawMessage); ok && json.Unmarshal(raw, &decision) == nil {
					// Only fixed result codes are loggable; never copy input or error text.
					switch decision.Reason {
					case "DRY_RUN_ONLY", "ROUTE_PLANNED", "SOURCE_VERIFIED":
						status, reason = "success", decision.Reason
					case "NEEDS_CONTEXT":
						status, reason = "needs_context", decision.Reason
					case "NO_PROFILE", "REQUEST_REQUIRED", "TASK_NOT_ALLOWLISTED", "WORKSPACE_SCOPE_MISMATCH", "PERMISSION_NOT_GRANTED",
						"NO_ROUTING_POLICY", "EXECUTOR_UNSUPPORTED", "SOURCE_UNPROVEN", "SOURCE_INCOMPATIBLE":
						status, reason = "denied", decision.Reason
					}
				}
			}
			logger.InfoContext(ctx, "mcp_tool_call", "tool", tool, "status", status, "reason", reason,
				"duration_ms", time.Since(started).Seconds()*1000)
			return result, err
		}
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 64 << 10,
	})
	mux := http.NewServeMux()
	protection := http.NewCrossOriginProtection()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Docker NAT is not a loopback destination; retain the SDK's loopback Host policy there too.
		if !loopbackHost(r.Host) {
			http.Error(w, "invalid Host", http.StatusForbidden)
			return
		}
		// MCP validates explicit Origin on safe methods too, independently of fetch metadata.
		check := *r
		check.Method = http.MethodPost
		if r.Header.Get("Origin") != "" {
			check.Header = r.Header.Clone()
			check.Header.Del("Sec-Fetch-Site")
		}
		if err := protection.Check(&check); err != nil {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		transport.ServeHTTP(w, r)
	}))
	return mux, nil
}

func loopbackHost(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return host == "localhost" || (err == nil && ip.IsLoopback())
}
