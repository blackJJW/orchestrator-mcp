package core

import (
	"slices"

	"orchestrator-mcp/internal/config"
)

type Request struct {
	TaskID     string `json:"task_id"`
	Workspace  string `json:"workspace"`
	Permission string `json:"permission"`
}

type Decision struct {
	SchemaVersion    int     `json:"schema_version"`
	Mode             string  `json:"mode"`
	ProjectID        string  `json:"project_id,omitempty"`
	Request          Request `json:"request"`
	ScopeAllowed     bool    `json:"scope_allowed"`
	ExecutionStarted bool    `json:"execution_started"`
	Reason           string  `json:"reason"`
}

// Plan consumes a validated profile and host-normalized workspace; it never grants or executes authority.
func Plan(profile *config.Profile, request Request) Decision {
	decision := Decision{SchemaVersion: config.SchemaVersion, Mode: "dry-run", Request: request, Reason: "NO_PROFILE"}
	if profile == nil {
		return decision
	}
	decision.ProjectID = profile.ProjectID
	switch {
	case request.TaskID == "" || request.Workspace == "" || request.Permission == "":
		decision.Reason = "REQUEST_REQUIRED"
	case !slices.Contains(profile.TaskAllowlist, request.TaskID):
		decision.Reason = "TASK_NOT_ALLOWLISTED"
	case request.Workspace != profile.WorkspaceRoot:
		decision.Reason = "WORKSPACE_SCOPE_MISMATCH"
	case !slices.Contains(profile.Permissions, request.Permission):
		decision.Reason = "PERMISSION_NOT_GRANTED"
	default:
		decision.ScopeAllowed = true
		decision.Reason = "DRY_RUN_ONLY"
	}
	return decision
}
