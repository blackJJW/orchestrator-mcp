package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRoutingPolicies(t *testing.T) {
	for _, project := range []string{"alpha", "beta"} {
		policy, err := LoadRoutingPolicy("../../testdata/routing/"+project+".json", project)
		if err != nil || policy.ProjectID != project || len(policy.Routes) != 1 || len(policy.Executors) != 1 {
			t.Fatalf("%s: %+v %v", project, policy, err)
		}
	}
}

func TestRoutingPolicyRejects(t *testing.T) {
	data, err := os.ReadFile("../../testdata/routing/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(data)
	var policy RoutingPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	duplicateExecutor := policy
	duplicateExecutor.Executors = append(duplicateExecutor.Executors, policy.Executors[0])
	duplicateTask := policy
	duplicateTask.Routes = append(duplicateTask.Routes, policy.Routes[0])
	duplicateExecutors, _ := json.Marshal(duplicateExecutor)
	duplicateTasks, _ := json.Marshal(duplicateTask)
	for name, invalid := range map[string]string{
		"duplicate-executor":    string(duplicateExecutors),
		"duplicate-task":        string(duplicateTasks),
		"long-task":             strings.Replace(valid, `"IMPLEMENTATION"`, `"`+strings.Repeat("x", 129)+`"`, 1),
		"schema":                strings.Replace(valid, `"schema_version": 1`, `"schema_version": 2`, 1),
		"foreign-project":       strings.Replace(valid, `"project_id": "alpha"`, `"project_id": "beta"`, 1),
		"duplicate":             strings.Replace(valid, `"project_id": "alpha"`, `"project_id":"alpha","project_id":"alpha"`, 1),
		"case-alias":            strings.Replace(valid, `"project_id"`, `"Project_ID"`, 1),
		"nested-alias":          strings.Replace(valid, `"executor_id"`, `"Executor_ID"`, 1),
		"nested-duplicate":      strings.Replace(valid, `"executor_id": "implementation_primary"`, `"executor_id":"implementation_primary","executor_id":"foreign"`, 1),
		"extra":                 strings.Replace(valid, `"task_type": "IMPLEMENTATION"`, `"task_type":"IMPLEMENTATION","execute":true`, 1),
		"null":                  strings.Replace(valid, `"task_type": "IMPLEMENTATION"`, `"task_type":null`, 1),
		"missing":               strings.Replace(valid, `"task_type": "IMPLEMENTATION",`, "", 1),
		"unknown-type":          strings.Replace(valid, `"IMPLEMENTATION"`, `"UNKNOWN"`, 1),
		"wildcard":              strings.Replace(valid, `"IMPLEMENTATION"`, `"*"`, 1),
		"unknown-executor":      strings.Replace(valid, `"executor_id": "implementation_primary"`, `"executor_id":"foreign"`, 1),
		"unknown-profile":       strings.ReplaceAll(valid, `"BALANCED"`, `"MAGIC"`),
		"profile-unsupported":   strings.Replace(valid, `"capability_profile": "BALANCED"`, `"capability_profile":"DEEP"`, 1),
		"reasoning-unsupported": strings.Replace(valid, `"reasoning": "HIGH"`, `"reasoning":"LOW"`, 1),
		"empty-choices":         strings.Replace(valid, `["HIGH"]`, `[]`, 1),
		"duplicate-choices":     strings.Replace(valid, `["HIGH"]`, `["HIGH","HIGH"]`, 1),
		"wrong-type":            strings.Replace(valid, `["HIGH"]`, `"HIGH"`, 1),
		"trailing":              valid + `{}`,
		"oversized":             valid + strings.Repeat(" ", 64*1024),
	} {
		t.Run(name, func(t *testing.T) {
			if invalid == valid {
				t.Fatal("invalid fixture was not changed")
			}
			path := filepath.Join(t.TempDir(), "routing.json")
			if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRoutingPolicy(path, "alpha"); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestRoutingPolicyFileBoundary(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	target, err := filepath.Abs("../../testdata/routing/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "absent"), fifo, link} {
		start := time.Now()
		if _, err := LoadRoutingPolicy(path, "alpha"); err == nil || time.Since(start) > time.Second {
			t.Fatalf("non-regular/missing policy accepted or blocked: %s %v", path, err)
		}
	}
}
