package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orchestrator-mcp/internal/core"
)

func TestCLI(t *testing.T) {
	alpha := "../../testdata/projects/alpha.json"
	beta := "../../testdata/projects/beta.json"
	alphaRoot := "../../testdata/workspaces/alpha"
	betaRoot := "../../testdata/workspaces/beta"
	for _, test := range []struct {
		name    string
		args    []string
		code    int
		reason  string
		allowed bool
	}{
		{"no config", nil, 0, "NO_PROFILE", false},
		{"no implicit authority", []string{"-task", "bootstrap-check", "-workspace", alphaRoot, "-permission", "source.read"}, 0, "NO_PROFILE", false},
		{"profile alone", []string{"-profile", alpha}, 0, "REQUEST_REQUIRED", false},
		{"alpha", []string{"-profile", alpha, "-task", "bootstrap-check", "-workspace", alphaRoot, "-permission", "source.write"}, 0, "DRY_RUN_ONLY", true},
		{"beta", []string{"-profile", beta, "-task", "bootstrap-check", "-workspace", betaRoot, "-permission", "source.read"}, 0, "DRY_RUN_ONLY", true},
		{"cross project", []string{"-profile", alpha, "-task", "bootstrap-check", "-workspace", betaRoot, "-permission", "source.read"}, 0, "WORKSPACE_SCOPE_MISMATCH", false},
		{"deny permission", []string{"-profile", beta, "-task", "bootstrap-check", "-workspace", betaRoot, "-permission", "source.write"}, 0, "PERMISSION_NOT_GRANTED", false},
		{"missing profile", []string{"-profile", "missing.json"}, 1, "", false},
		{"missing workspace", []string{"-profile", alpha, "-workspace", "missing"}, 1, "", false},
		{"unknown option", []string{"-launch"}, 2, "", false},
		{"positional", []string{"launch"}, 2, "", false},
		{"help", []string{"-help"}, 0, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != test.code {
				t.Fatalf("exit %d, want %d: %s", code, test.code, stderr.String())
			}
			if test.reason == "" {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatal("usage/errors must not output a success decision")
				}
				return
			}
			var decision core.Decision
			if err := json.Unmarshal(stdout.Bytes(), &decision); err != nil {
				t.Fatal(err)
			}
			if stderr.Len() != 0 || decision.Reason != test.reason || decision.ScopeAllowed != test.allowed || decision.ExecutionStarted {
				t.Fatalf("unexpected result: %+v, stderr=%s", decision, stderr.String())
			}
		})
	}
}

func TestSymlinkEscapeDenied(t *testing.T) {
	link := filepath.Join(t.TempDir(), "outside")
	target, err := filepath.Abs("../../testdata/workspaces/beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-profile", "../../testdata/projects/alpha.json", "-task", "bootstrap-check", "-workspace", link, "-permission", "source.read"}, &stdout, &stderr)
	var decision core.Decision
	if err := json.Unmarshal(stdout.Bytes(), &decision); err != nil {
		t.Fatal(err)
	}
	if code != 0 || decision.ScopeAllowed || decision.ExecutionStarted || decision.Reason != "WORKSPACE_SCOPE_MISMATCH" {
		t.Fatalf("symlink escape: %+v, exit=%d, stderr=%s", decision, code, stderr.String())
	}
}
