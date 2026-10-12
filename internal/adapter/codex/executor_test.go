package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

type fixtureStore struct {
	task  core.TaskSnapshot
	reads int
}

func (s *fixtureStore) Read(context.Context, string) (core.TaskSnapshot, error) {
	s.reads++
	return s.task, nil
}
func (*fixtureStore) Claim(context.Context, string, core.Claim) error {
	panic("executor must not claim")
}

type fixtureSource struct{ value core.SourceSnapshot }

func (s fixtureSource) Verify(context.Context, string) (core.SourceSnapshot, error) {
	return s.value, nil
}

type fixtureRunner struct {
	t          *testing.T
	name       string
	calls      int
	invocation Invocation
}

func (f *fixtureRunner) Run(ctx context.Context, i Invocation) Process {
	f.calls++
	f.invocation = i
	path, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	p := runProcess(ctx, path, []string{"-test.run=^TestFakeCLIProcess$", "--", "controller-fake-" + f.name}, i.Dir, i.Prompt)
	if len(p.Stdout) > 1024*1024 || len(p.Stderr) > 1024*1024 {
		f.t.Fatal("unbounded process output")
	}
	return p
}

func request(t *testing.T) (Request, time.Time) {
	t.Helper()
	root, err := config.ResolveWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c := core.Contract{TaskID: "task-1", PageID: "page-1", ProjectID: "alpha", Workspace: root, Permission: "source.read", Executor: "executor", Owner: "executor", AuthorityRef: "source-grant", BriefDigest: core.Digest("approved brief")}
	g := &Grant{Reference: "separate-read-only-launch-grant", TaskID: c.TaskID, RunID: "run-1", Workspace: root, SourceIdentity: "fixture-source", BriefDigest: c.BriefDigest, ClaimNonce: "claim-1", LeaseUntil: now.Add(time.Minute)}
	s := &fixtureStore{task: core.TaskSnapshot{TaskID: c.TaskID, PageID: c.PageID, Executor: c.Executor, Owner: c.Owner, Brief: "approved brief", ClaimNonce: g.ClaimNonce, LeaseUntil: g.LeaseUntil, Eligible: true, DependenciesSatisfied: true, OwnershipKnown: true}}
	return Request{Profile: config.Profile{ProjectID: "alpha", WorkspaceRoot: root, TaskAllowlist: []string{c.TaskID}, Permissions: []string{"source.read"}}, Contract: c, RunID: g.RunID, Timeout: 3 * time.Second, Grant: g, Store: s, Source: fixtureSource{core.SourceSnapshot{Identity: g.SourceIdentity, Compatibility: "compatible"}}}, now
}

func TestExecutorFixtures(t *testing.T) {
	for _, tc := range []struct{ name, outcome string }{
		{"pass", "COMPLETED_UNVERIFIED"}, {"soft-denial", "SOFT_DENIED"}, {"malformed-json", "MALFORMED_OUTPUT"}, {"quota", "USAGE_LIMIT"}, {"crash", "CLI_FAILED"}, {"tool-denial", "TOOL_DENIED"}, {"timeout", "TIMEOUT"}, {"cancel", "CANCELLED"}, {"ambiguous", "MALFORMED_OUTPUT"}, {"file-change", "UNAUTHORIZED_ACTION"}, {"unknown-event", "UNSUPPORTED_OUTPUT"}, {"overflow", "OUTPUT_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, now := request(t)
			runner := &fixtureRunner{t: t, name: tc.name}
			e := NewFake(runner)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.name == "timeout" {
				r.Timeout = 150 * time.Millisecond
			}
			if tc.name == "cancel" {
				timer := time.AfterFunc(150*time.Millisecond, cancel)
				defer timer.Stop()
			}
			start := time.Now()
			result := e.Execute(ctx, r, func() time.Time { return now })
			if result.Outcome != tc.outcome || runner.calls != 1 || result.Mode != "fake" || result.PID <= 0 || result.Outcome == "ACCEPTED" || result.Outcome == "DONE" {
				t.Fatalf("result=%+v calls=%d", result, runner.calls)
			}
			if (tc.name == "timeout" || tc.name == "cancel") && time.Since(start) > time.Second {
				t.Fatal("process did not stop promptly")
			}
			want := []string{"exec", "--json", "--sandbox", "read-only", "-c", `approval_policy="never"`, "-C", r.Contract.Workspace, "--ephemeral", "--skip-git-repo-check", "-"}
			actual, _ := json.Marshal(runner.invocation.Args)
			expected, _ := json.Marshal(want)
			if string(actual) != string(expected) {
				t.Fatalf("unsafe argv %s", actual)
			}
			if strings.Contains(strings.Join(runner.invocation.Args, " "), "approved brief") || !strings.HasPrefix(runner.invocation.Prompt, "approved brief\n") {
				t.Fatal("prompt must enter stdin only")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "secret-token") {
				t.Fatal("diagnostic leak")
			}
			if tc.name == "pass" || tc.name == "soft-denial" {
				if result.SessionID != "11111111-1111-4111-8111-111111111111" {
					t.Fatal("missing session identity")
				}
			}
			t.Logf("%s", encoded)
		})
	}
}

func TestUnauthorizedInvocationFixtures(t *testing.T) {
	for _, name := range []string{"no-grant", "wrong-task", "wrong-run", "wrong-source", "wrong-brief", "lease-drift", "expired-lease", "wrong-workspace", "argument-injection", "write-permission", "brief-drift", "dependency-drift", "same-agent-other-claim"} {
		t.Run(name, func(t *testing.T) {
			r, now := request(t)
			store := r.Store.(*fixtureStore)
			switch name {
			case "no-grant":
				r.Grant = nil
			case "wrong-task":
				r.Grant.TaskID = "other"
			case "wrong-run":
				r.Grant.RunID = "other"
			case "wrong-source":
				r.Grant.SourceIdentity = "different"
			case "wrong-brief":
				r.Grant.BriefDigest = "different"
			case "lease-drift":
				store.task.LeaseUntil = now.Add(2 * time.Minute)
			case "expired-lease":
				store.task.LeaseUntil = now.Add(-time.Minute)
				r.Grant.LeaseUntil = store.task.LeaseUntil
			case "wrong-workspace":
				r.Contract.Workspace = filepath.Dir(r.Contract.Workspace)
				r.Grant.Workspace = r.Contract.Workspace
			case "argument-injection":
				r.RunID = "--yolo\nsecret-token"
				r.Grant.RunID = r.RunID
			case "write-permission":
				r.Contract.Permission = "source.write"
				r.Profile.Permissions = append(r.Profile.Permissions, "source.write")
			case "brief-drift":
				store.task.Brief = "launch secrets"
			case "dependency-drift":
				store.task.DependenciesSatisfied = false
			case "same-agent-other-claim":
				store.task.ClaimNonce = "other-run"
			}
			runner := &fixtureRunner{t: t, name: "pass"}
			result := NewFake(runner).Execute(context.Background(), r, func() time.Time { return now })
			if runner.calls != 0 || result.PID != 0 || result.Outcome == "COMPLETED_UNVERIFIED" {
				t.Fatalf("unauthorized spawn %+v", result)
			}
			t.Logf("outcome=%s fake_spawns=%d", result.Outcome, runner.calls)
		})
	}
}

func TestNativePinAndCwd(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "fifo")
	if _, err := NewNative(fifo, "unknown"); err == nil {
		t.Fatal("missing executable allowed")
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewNative(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong executable pin allowed")
	}
	script := filepath.Join(root, "codex")
	scriptBytes := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(script, scriptBytes, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNative(script, core.Digest(string(scriptBytes))); err == nil {
		t.Fatal("script launcher accepted as native identity")
	}
	p := runProcess(context.Background(), path, nil, root+"/../"+filepath.Base(root), "")
	if p.PID != 0 || p.Err == nil {
		t.Fatal("unresolved cwd allowed")
	}
}

func TestAuthorizedNativeIdentityPreflight(t *testing.T) {
	path, sha := os.Getenv("ORCHESTRATOR_TEST_NATIVE"), os.Getenv("ORCHESTRATOR_TEST_NATIVE_SHA")
	if path == "" {
		t.Skip("native identity supplied only for authorized read-only preflight")
	}
	e, err := NewNative(path, sha)
	if err != nil || e.mode != "native" {
		t.Fatalf("native identity rejected: %v", err)
	}
	r, now := request(t)
	r.Grant = nil
	result := e.Execute(context.Background(), r, func() time.Time { return now })
	if result.PID != 0 || result.Outcome != "LAUNCH_UNAUTHORIZED" {
		t.Fatalf("preflight launched a job: %+v", result)
	}
	t.Logf("exact native identity accepted; without launch grant outcome=%s provider_jobs=0", result.Outcome)
}

// A test binary is the only spawned fixture, with no official provider/auth/model execution.
func TestFakeCLIProcess(t *testing.T) {
	name := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "controller-fake-") {
			name = strings.TrimPrefix(arg, "controller-fake-")
		}
	}
	if name == "" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if name == "timeout" || name == "cancel" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if name == "crash" {
		fmt.Fprintln(os.Stderr, "secret-token crash diagnostic")
		os.Exit(7)
	}
	if name == "quota" {
		fmt.Fprintln(os.Stderr, "usage limit reached secret-token")
		os.Exit(1)
	}
	if name == "overflow" {
		fmt.Print(strings.Repeat("x", 2*1024*1024))
		os.Exit(0)
	}
	if name == "malformed-json" {
		fmt.Println("{malformed secret-token")
		os.Exit(0)
	}
	fmt.Println(`{"type":"thread.started","thread_id":"11111111-1111-4111-8111-111111111111"}`)
	fmt.Println(`{"type":"turn.started"}`)
	if name == "unknown-event" {
		fmt.Println(`{"type":"future.event"}`)
	}
	if name == "tool-denial" {
		fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","status":"failed","exit_code":1,"aggregated_output":"permission denied secret-token"}}`)
	}
	if name == "file-change" {
		fmt.Println(`{"type":"item.completed","item":{"type":"file_change","status":"completed"}}`)
	}
	final := `{"status":"completed","summary":"secret-token fixture text"}`
	if name == "soft-denial" {
		final = `{"status":"blocked","summary":"denied"}`
	}
	if name == "ambiguous" {
		final = `{"status":"blocked","status":"completed","summary":"ambiguous"}`
	}
	event := map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": final}}
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	os.Exit(0)
}
