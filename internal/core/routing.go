package core

import (
	"slices"

	"orchestrator-mcp/internal/config"
)

type ExecutionRequest struct {
	TaskType string `json:"task_type,omitempty"`
}

type RoutingDecision struct {
	ProjectID              string `json:"project_id,omitempty"`
	RoutingAllowed         bool   `json:"routing_allowed"`
	Reason                 string `json:"reason"`
	ExecutorID             string `json:"executor_id,omitempty"`
	CapabilityProfile      string `json:"capability_profile,omitempty"`
	Reasoning              string `json:"reasoning,omitempty"`
	ManualDispatchRequired bool   `json:"manual_dispatch_required"`
	AuthorityGranted       bool   `json:"authority_granted"`
	ExecutionStarted       bool   `json:"execution_started"`
}

// PlanExecution consumes validated, project-owned policy; a route is never a Grant or Claim.
func PlanExecution(policy *config.RoutingPolicy, request ExecutionRequest) RoutingDecision {
	decision := RoutingDecision{Reason: "NO_ROUTING_POLICY", ManualDispatchRequired: true}
	if policy == nil {
		return decision
	}
	decision.ProjectID = policy.ProjectID
	decision.Reason = "NEEDS_CONTEXT"
	if request.TaskType == "" || request.TaskType == "UNKNOWN" {
		return decision
	}
	for _, route := range policy.Routes {
		if route.TaskType != request.TaskType {
			continue
		}
		decision.Reason = "EXECUTOR_UNSUPPORTED"
		for _, executor := range policy.Executors {
			if executor.ID != route.ExecutorID || !executor.Valid() || !slices.Contains(executor.SupportedProfiles, route.CapabilityProfile) ||
				!slices.Contains(executor.SupportedReasoning, route.Reasoning) {
				continue
			}
			decision.RoutingAllowed, decision.Reason = true, "ROUTE_PLANNED"
			decision.ExecutorID, decision.CapabilityProfile, decision.Reasoning = route.ExecutorID, route.CapabilityProfile, route.Reasoning
			return decision
		}
		return decision
	}
	return decision
}
