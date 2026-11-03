package core

import (
	"encoding/json"
	"testing"

	"orchestrator-mcp/internal/config"
)

func TestPlanExecutionProjectsAndDeterminism(t *testing.T) {
	for _, tc := range []struct{ project, executor, profile, reasoning string }{
		{"alpha", "implementation_primary", "BALANCED", "HIGH"},
		{"beta", "builder_pool", "FAST", "LOW"},
	} {
		t.Run(tc.project, func(t *testing.T) {
			policy, err := config.LoadRoutingPolicy("../../testdata/routing/"+tc.project+".json", tc.project)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(policy)
			input := ExecutionRequest{TaskType: "IMPLEMENTATION"}
			got := PlanExecution(&policy, input)
			again := PlanExecution(&policy, input)
			first, _ := json.Marshal(got)
			second, _ := json.Marshal(again)
			after, _ := json.Marshal(policy)
			if string(first) != string(second) || string(before) != string(after) || !got.RoutingAllowed || got.Reason != "ROUTE_PLANNED" ||
				got.ProjectID != tc.project || got.ExecutorID != tc.executor || got.CapabilityProfile != tc.profile || got.Reasoning != tc.reasoning ||
				!got.ManualDispatchRequired || got.AuthorityGranted || got.ExecutionStarted {
				t.Fatalf("routing/determinism/authority: %s", first)
			}
			t.Log(string(first))
		})
	}
}

func TestPlanExecutionDenials(t *testing.T) {
	policy, err := config.LoadRoutingPolicy("../../testdata/routing/alpha.json", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	check := func(got RoutingDecision, reason string) {
		t.Helper()
		if got.RoutingAllowed || got.Reason != reason || got.ExecutorID != "" || got.CapabilityProfile != "" || got.Reasoning != "" ||
			!got.ManualDispatchRequired || got.ExecutionStarted || got.AuthorityGranted {
			t.Fatalf("unsafe denial: %+v", got)
		}
	}
	check(PlanExecution(nil, ExecutionRequest{TaskType: "IMPLEMENTATION"}), "NO_ROUTING_POLICY")
	for _, task := range []string{"", "UNKNOWN", "unmapped", " IMPLEMENTATION", "implementation"} {
		check(PlanExecution(&policy, ExecutionRequest{TaskType: task}), "NEEDS_CONTEXT")
	}
	for _, route := range []config.ExecutionRoute{
		{TaskType: "IMPLEMENTATION", ExecutorID: "foreign", CapabilityProfile: "BALANCED", Reasoning: "HIGH"},
		{TaskType: "IMPLEMENTATION", ExecutorID: "implementation_primary", CapabilityProfile: "DEEP", Reasoning: "HIGH"},
		{TaskType: "IMPLEMENTATION", ExecutorID: "implementation_primary", CapabilityProfile: "BALANCED", Reasoning: "LOW"},
	} {
		invalid := policy
		invalid.Routes = []config.ExecutionRoute{route}
		check(PlanExecution(&invalid, ExecutionRequest{TaskType: "IMPLEMENTATION"}), "EXECUTOR_UNSUPPORTED")
	}
}

func TestPlanExecutionRejectsUnsupportedCatalogVocabulary(t *testing.T) {
	for _, tc := range []struct{ profile, reasoning string }{{"MAGIC", "HIGH"}, {"BALANCED", "EXTREME"}} {
		policy := config.RoutingPolicy{SchemaVersion: 1, ProjectID: "inert",
			Executors: []config.RoutingExecutor{{ID: "worker", SupportedProfiles: []string{tc.profile}, SupportedReasoning: []string{tc.reasoning}}},
			Routes:    []config.ExecutionRoute{{TaskType: "IMPLEMENTATION", ExecutorID: "worker", CapabilityProfile: tc.profile, Reasoning: tc.reasoning}}}
		got := PlanExecution(&policy, ExecutionRequest{TaskType: "IMPLEMENTATION"})
		if got.RoutingAllowed || got.Reason != "EXECUTOR_UNSUPPORTED" || got.ExecutorID != "" || got.CapabilityProfile != "" || got.Reasoning != "" ||
			!got.ManualDispatchRequired || got.ExecutionStarted || got.AuthorityGranted {
			t.Fatalf("unsupported vocabulary routed: %+v", got)
		}
	}
}
