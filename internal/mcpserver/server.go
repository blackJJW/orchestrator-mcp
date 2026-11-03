// Package mcpserver exposes only the existing dry-run scope decision over MCP.
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// NewHandler loads the operator's one profile before accepting any client input.
func NewHandler(profilePath string) (http.Handler, error) {
	profile, err := config.Load(profilePath)
	if err != nil {
		return nil, err
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
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 64 << 10,
	})
	mux := http.NewServeMux()
	protection := http.NewCrossOriginProtection()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
