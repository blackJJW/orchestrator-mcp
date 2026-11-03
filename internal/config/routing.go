package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"unicode/utf8"
)

type RoutingPolicy struct {
	SchemaVersion int               `json:"schema_version"`
	ProjectID     string            `json:"project_id"`
	Executors     []RoutingExecutor `json:"executors"`
	Routes        []ExecutionRoute  `json:"routes"`
}

type RoutingExecutor struct {
	ID                 string   `json:"id"`
	SupportedProfiles  []string `json:"supported_profiles"`
	SupportedReasoning []string `json:"supported_reasoning"`
}

func (executor RoutingExecutor) Valid() bool {
	return routingID(executor.ID) &&
		routingChoices(executor.SupportedProfiles, []string{"FAST", "BALANCED", "DEEP", "REVIEW_DEEP"}) &&
		routingChoices(executor.SupportedReasoning, []string{"LOW", "MEDIUM", "HIGH", "EXTRA_HIGH"})
}

type ExecutionRoute struct {
	TaskType          string `json:"task_type"`
	ExecutorID        string `json:"executor_id"`
	CapabilityProfile string `json:"capability_profile"`
	Reasoning         string `json:"reasoning"`
}

var errRoutingPolicy = errors.New("invalid routing policy")

// LoadRoutingPolicy freezes one bounded operator file for the already selected project.
func LoadRoutingPolicy(path, projectID string) (RoutingPolicy, error) {
	var policy RoutingPolicy
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return policy, errRoutingPolicy
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return policy, errRoutingPolicy
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 || !utf8.Valid(data) {
		return policy, errRoutingPolicy
	}
	fields, err := routingObject(data, "schema_version", "project_id", "executors", "routes")
	if err != nil || json.Unmarshal(data, &policy) != nil || policy.SchemaVersion != SchemaVersion ||
		!routingID(policy.ProjectID) || policy.ProjectID != projectID {
		return RoutingPolicy{}, errRoutingPolicy
	}
	for _, kind := range []string{"executors", "routes"} {
		var entries []json.RawMessage
		if json.Unmarshal(fields[kind], &entries) != nil || len(entries) == 0 {
			return RoutingPolicy{}, errRoutingPolicy
		}
		keys := []string{"id", "supported_profiles", "supported_reasoning"}
		if kind == "routes" {
			keys = []string{"task_type", "executor_id", "capability_profile", "reasoning"}
		}
		for _, entry := range entries {
			if _, err := routingObject(entry, keys...); err != nil {
				return RoutingPolicy{}, errRoutingPolicy
			}
		}
	}
	executors := make(map[string]RoutingExecutor)
	for _, executor := range policy.Executors {
		if !executor.Valid() || executors[executor.ID].ID != "" {
			return RoutingPolicy{}, errRoutingPolicy
		}
		executors[executor.ID] = executor
	}
	tasks := make(map[string]bool)
	for _, route := range policy.Routes {
		executor, ok := executors[route.ExecutorID]
		if !routingID(route.TaskType) || route.TaskType == "UNKNOWN" || tasks[route.TaskType] || !ok ||
			!slices.Contains(executor.SupportedProfiles, route.CapabilityProfile) ||
			!slices.Contains(executor.SupportedReasoning, route.Reasoning) {
			return RoutingPolicy{}, errRoutingPolicy
		}
		tasks[route.TaskType] = true
	}
	return policy, nil
}

func routingID(value string) bool {
	return len(value) <= 128 && exactValue(value)
}

func routingChoices(values, allowed []string) bool {
	seen := make(map[string]bool)
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if seen[value] || !slices.Contains(allowed, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

// Token-level keys preserve the existing exact/duplicate-free JSON startup boundary.
func routingObject(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, errRoutingPolicy
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || fields[key] != nil || !slices.Contains(keys, key) {
			return nil, errRoutingPolicy
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return nil, errRoutingPolicy
		}
		fields[key] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') || len(fields) != len(keys) || d.Decode(new(any)) != io.EOF {
		return nil, errRoutingPolicy
	}
	return fields, nil
}
