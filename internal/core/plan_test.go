package core

import (
	"testing"

	"orchestrator-mcp/internal/config"
)

func TestDefaultDeny(t *testing.T) {
	profile := &config.Profile{ProjectID: "alpha", WorkspaceRoot: "/projects/alpha", TaskAllowlist: []string{"task"}, Permissions: []string{"read"}}
	request := Request{TaskID: "task", Workspace: "/projects/alpha", Permission: "read"}
	for _, test := range []struct {
		name    string
		profile *config.Profile
		request Request
		reason  string
	}{
		{"no profile", nil, request, "NO_PROFILE"},
		{"missing request", profile, Request{}, "REQUEST_REQUIRED"},
		{"missing task", profile, Request{Workspace: request.Workspace, Permission: "read"}, "REQUEST_REQUIRED"},
		{"missing workspace", profile, Request{TaskID: "task", Permission: "read"}, "REQUEST_REQUIRED"},
		{"missing permission", profile, Request{TaskID: "task", Workspace: request.Workspace}, "REQUEST_REQUIRED"},
		{"unknown task", profile, Request{TaskID: "other", Workspace: request.Workspace, Permission: "read"}, "TASK_NOT_ALLOWLISTED"},
		{"task case", profile, Request{TaskID: "TASK", Workspace: request.Workspace, Permission: "read"}, "TASK_NOT_ALLOWLISTED"},
		{"empty allowlist", &config.Profile{ProjectID: "alpha"}, request, "TASK_NOT_ALLOWLISTED"},
		{"other project", profile, Request{TaskID: "task", Workspace: "/projects/beta", Permission: "read"}, "WORKSPACE_SCOPE_MISMATCH"},
		{"prefix sibling", profile, Request{TaskID: "task", Workspace: "/projects/alpha-copy", Permission: "read"}, "WORKSPACE_SCOPE_MISMATCH"},
		{"nested workspace", profile, Request{TaskID: "task", Workspace: "/projects/alpha/child", Permission: "read"}, "WORKSPACE_SCOPE_MISMATCH"},
		{"unknown permission", profile, Request{TaskID: "task", Workspace: request.Workspace, Permission: "write"}, "PERMISSION_NOT_GRANTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := Plan(test.profile, test.request)
			if decision.ScopeAllowed || decision.ExecutionStarted || decision.Mode != "dry-run" || decision.Reason != test.reason {
				t.Fatalf("unexpected decision: %+v", decision)
			}
		})
	}
}

func TestTwoProjectIsolation(t *testing.T) {
	alpha, err := config.Load("../../testdata/projects/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := config.Load("../../testdata/projects/beta.json")
	if err != nil {
		t.Fatal(err)
	}
	request := Request{TaskID: "bootstrap-check", Workspace: alpha.WorkspaceRoot, Permission: "source.write"}
	decision := Plan(&alpha, request)
	if !decision.ScopeAllowed || decision.ExecutionStarted || decision.Reason != "DRY_RUN_ONLY" {
		t.Fatalf("matching scope: %+v", decision)
	}
	if decision != Plan(&alpha, request) {
		t.Fatal("identical input changed the decision")
	}
	if got := Plan(&beta, request); got.ScopeAllowed || got.Reason != "WORKSPACE_SCOPE_MISMATCH" {
		t.Fatalf("cross-project leak: %+v", got)
	}
	request.Workspace = beta.WorkspaceRoot
	if got := Plan(&beta, request); got.ScopeAllowed || got.Reason != "PERMISSION_NOT_GRANTED" {
		t.Fatalf("permission leak: %+v", got)
	}
	request.Permission = "source.read"
	if got := Plan(&beta, request); !got.ScopeAllowed || got.ExecutionStarted {
		t.Fatalf("beta matching scope: %+v", got)
	}
}
