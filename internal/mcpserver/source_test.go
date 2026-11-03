package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

type sourceFixture struct {
	root, profilePath, bindingPath string
	profile                        config.Profile
	binding                        sourceBinding
}

func sourceGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@users.noreply.github.com", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@users.noreply.github.com", "GIT_TERMINAL_PROMPT=0"}
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v %s", args, err, data)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func sourceWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func sourceHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Same inert S2 Git/receipt fixture pattern as intake/source_test.go; never operational evidence.
func newSourceFixture(t *testing.T) sourceFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, ".local/template")
	if err := os.MkdirAll(template, 0700); err != nil {
		t.Fatal(err)
	}
	sourceGit(t, root, "init", "-b", "main", "--template="+template)
	inputs := []string{".gitignore", "Makefile", "app.go", "go.mod"}
	for path, text := range map[string]string{".gitignore": ".local/\n", "Makefile": "check:\n\ttrue\n", "app.go": "package fixture\nconst Value = 1\n", "go.mod": "module fixture\ngo 1.27\n"} {
		sourceWrite(t, filepath.Join(root, path), []byte(text))
	}
	sourceGit(t, root, append([]string{"add", "--"}, inputs...)...)
	sourceGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture baseline")
	base := sourceGit(t, root, "rev-parse", "HEAD")
	sourceGit(t, root, "switch", "-c", "candidate")
	sourceWrite(t, filepath.Join(root, "app.go"), []byte("package fixture\nconst Value = 2\n"))
	sourceGit(t, root, "add", "--", "app.go")
	sourceGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture candidate")
	actual := map[string]string{}
	for _, path := range inputs {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		actual[path] = sourceHash(data)
	}
	encoded, _ := json.Marshal(actual)
	spec := config.SourceSpec{Workspace: root, ProjectID: "fixture-project", CandidateRef: "refs/heads/candidate", BaseRef: "refs/heads/main", Head: sourceGit(t, root, "rev-parse", "HEAD"), Tree: sourceGit(t, root, "rev-parse", "HEAD^{tree}"), SourceSet: sourceHash(encoded), ReviewBase: base, Brief: "Approved S2 fixture brief; private text.", Inputs: inputs, CompatibilityInputs: inputs}
	spec.BriefDigest = core.Digest(spec.Brief)
	for _, kind := range []string{"canonical", "review"} {
		result, coverage := "PASS", "make check"
		if kind == "review" {
			result, coverage = "ACCEPTED", "independent-review"
		}
		artifact := ".local/evidence/" + kind + ".log"
		data := []byte("Inert fixture projection: " + kind + "; not real validation provenance.\n")
		sourceWrite(t, filepath.Join(root, artifact), data)
		receipt := config.SourceReceipt{Version: 1, Kind: kind, Result: result, ProjectID: spec.ProjectID, Base: base, Binding: "commit", Head: spec.Head, Tree: spec.Tree, SourceSet: spec.SourceSet, BriefDigest: spec.BriefDigest, Inputs: actual, Coverage: []string{coverage}, Artifacts: []config.EvidencePin{{Path: artifact, SHA256: sourceHash(data)}}}
		encoded, _ := json.Marshal(receipt)
		path := ".local/evidence/" + kind + ".json"
		sourceWrite(t, filepath.Join(root, path), encoded)
		spec.Receipts = append(spec.Receipts, config.EvidencePin{Path: path, SHA256: sourceHash(encoded)})
	}
	f := sourceFixture{root: root, profilePath: filepath.Join(t.TempDir(), "profile.json"), bindingPath: filepath.Join(t.TempDir(), "source.json"),
		profile: config.Profile{SchemaVersion: 1, ProjectID: spec.ProjectID, WorkspaceRoot: root, TaskAllowlist: []string{"source-check", "allowed-other"}, Permissions: []string{"source.read"}},
		binding: sourceBinding{Contract: core.Contract{TaskID: "source-check", PageID: "fixture-page", ProjectID: spec.ProjectID, Workspace: root, Permission: "source.read", Executor: "fixture", Owner: "fixture", AuthorityRef: "inert-fixture-only", BriefDigest: spec.BriefDigest, FrozenSource: spec.SourceSet}, SourceSpec: spec, TaskBase: base}}
	encoded, _ = json.Marshal(f.profile)
	sourceWrite(t, f.profilePath, encoded)
	encoded, _ = json.Marshal(f.binding)
	sourceWrite(t, f.bindingPath, encoded)
	return f
}

func TestVerifySourceMCP(t *testing.T) {
	f := newSourceFixture(t)
	policy, err := os.ReadFile("../../testdata/routing/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	routingPath := filepath.Join(t.TempDir(), "routing.json")
	sourceWrite(t, routingPath, []byte(strings.ReplaceAll(string(policy), `"alpha"`, `"fixture-project"`)))
	h, err := NewHandler(f.profilePath, f.bindingPath, routingPath)
	if err != nil {
		t.Fatal(err)
	}
	// Both host files are startup-only inputs, not mutable client-selected policy.
	os.Remove(f.profilePath)
	os.Remove(f.bindingPath)
	s := httptest.NewServer(h)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "s2-mcp-integration", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.URL + "/mcp", HTTPClient: s.Client()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 3 {
		t.Fatalf("discovery: %+v %v", tools, err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("annotations: %+v", tool)
		}
	}
	if !names["check_scope"] || !names["verify_source"] || !names["plan_execution"] {
		t.Fatal(names)
	}
	request := core.Request{TaskID: "source-check", Workspace: f.root, Permission: "source.read"}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "check_scope", Arguments: request})
	if err != nil || res.IsError {
		t.Fatalf("check_scope: %+v %v", res, err)
	}
	data, _ := json.Marshal(res.StructuredContent)
	var decision core.Decision
	if json.Unmarshal(data, &decision) != nil || decision != core.Plan(&f.profile, request) {
		t.Fatalf("check_scope changed: %s", data)
	}
	call := func(task, reason string) sourceResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "verify_source", Arguments: sourceInput{TaskID: task}})
		if err != nil || res.IsError {
			t.Fatalf("call: %+v %v", res, err)
		}
		data, _ := json.Marshal(res.StructuredContent)
		var got sourceResult
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &got) != nil || json.Unmarshal(data, &fields) != nil || len(fields) != 4 || got.Reason != reason || got.ExecutionStarted || got.SourceVerified != (reason == "SOURCE_VERIFIED") || (got.Proof != nil) != got.SourceVerified {
			t.Fatalf("decision: %s want %s", data, reason)
		}
		if got.Proof != nil {
			var proof map[string]any
			json.Unmarshal(fields["proof"], &proof)
			if len(proof) != 6 || got.Proof.Commit != f.binding.SourceSpec.Head || got.Proof.Tree != f.binding.SourceSpec.Tree || got.Proof.SourceSet != f.binding.SourceSpec.SourceSet || got.Proof.ReviewBase != f.binding.TaskBase || got.Proof.CurrentBase != f.binding.TaskBase || got.Proof.BriefDigest != f.binding.Contract.BriefDigest {
				t.Fatalf("proof: %s", data)
			}
		} else if string(fields["proof"]) != "null" {
			t.Fatalf("proof must be null: %s", data)
		}
		for _, private := range []string{f.root, f.binding.SourceSpec.Brief, f.binding.SourceSpec.Receipts[0].Path, f.binding.SourceSpec.Receipts[0].SHA256} {
			if strings.Contains(string(data), private) {
				t.Fatalf("private input disclosed: %s", data)
			}
		}
		t.Logf("actual MCP %s: %s", task, data)
		return got
	}
	call("source-check", "SOURCE_VERIFIED")
	call("unapproved", "TASK_NOT_ALLOWLISTED")
	call("allowed-other", "SOURCE_UNPROVEN")
	call("", "REQUEST_REQUIRED")
	for _, args := range []any{map[string]any{}, map[string]any{"task_id": 1}, map[string]any{"task_id": nil}, map[string]any{"Task_ID": "source-check"}} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "verify_source", Arguments: args})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("invalid MCP arguments accepted: %+v", res)
		}
	}
	for _, field := range []string{"workspace", "profile", "path", "source_ref", "digest", "receipt", "task_base", "permission"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "verify_source", Arguments: map[string]any{"task_id": "source-check", field: "client-selected"}})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("client override accepted: %s %+v", field, res)
		}
	}
	for _, path := range []string{"app.go", ".local/evidence/canonical.log", ".local/evidence/review.json"} {
		full := filepath.Join(f.root, path)
		original, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		sourceWrite(t, full, []byte("private tampered bytes"))
		call("source-check", "SOURCE_UNPROVEN")
		sourceWrite(t, full, original)
	}
	if err := os.Rename(filepath.Join(f.root, ".git"), filepath.Join(f.root, ".local/saved-git")); err != nil {
		t.Fatal(err)
	}
	sourceWrite(t, filepath.Join(f.root, ".git"), []byte("gitdir: unsupported-linked-worktree\n"))
	call("source-check", "SOURCE_UNPROVEN")
	if err := os.Remove(filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.root, ".local/saved-git"), filepath.Join(f.root, ".git")); err != nil {
		t.Fatal(err)
	}
	sourceGit(t, f.root, "switch", "main")
	sourceWrite(t, filepath.Join(f.root, "app.go"), []byte("package fixture\nconst Value = 3\n"))
	sourceGit(t, f.root, "add", "app.go")
	sourceGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "incompatible fixture base")
	sourceGit(t, f.root, "switch", "candidate")
	call("source-check", "SOURCE_INCOMPATIBLE")
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if got := verifySource(canceled, f.profile, f.binding, "source-check"); got.SourceVerified || got.Proof != nil || got.ExecutionStarted {
		t.Fatalf("canceled verification accepted: %+v", got)
	}
}

func TestSourceBindingRejects(t *testing.T) {
	f := newSourceFixture(t)
	data, err := os.ReadFile(f.bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	base := string(data)
	inputs, _ := json.Marshal(f.binding.SourceSpec.Inputs)
	cases := map[string]string{
		"root-null": "null", "root-array": "[]", "trailing": base + " {}", "unknown": strings.TrimSuffix(base, "}") + `,"unknown":true}`,
		"top-case":           strings.Replace(base, `"task_base":`, `"Task_Base":`, 1),
		"top-duplicate":      strings.Replace(base, `"task_base":`, `"task_base":"ignored","task_base":`, 1),
		"contract-case":      strings.Replace(base, `"TaskID":`, `"taskid":`, 1),
		"contract-duplicate": strings.Replace(base, `"TaskID":`, `"TaskID":"ignored","TaskID":`, 1),
		"contract-null":      strings.Replace(base, `"PageID":"fixture-page"`, `"PageID":null`, 1),
		"contract-number":    strings.Replace(base, `"PageID":"fixture-page"`, `"PageID":1`, 1),
		"contract-missing":   strings.Replace(base, `"PageID":"fixture-page",`, "", 1),
		"empty":              strings.Replace(base, `"FrozenSource":"`+f.binding.Contract.FrozenSource+`"`, `"FrozenSource":""`, 1),
		"permission":         strings.Replace(base, `"Permission":"source.read"`, `"Permission":"source.write"`, 1),
		"task":               strings.Replace(base, `"TaskID":"source-check"`, `"TaskID":"unapproved"`, 1),
		"project":            strings.Replace(base, `"ProjectID":"fixture-project"`, `"ProjectID":"foreign-project"`, 1),
		"workspace":          strings.ReplaceAll(base, `"Workspace":"`+f.root+`"`, `"Workspace":"/not/approved"`),
		"brief":              strings.Replace(base, f.binding.SourceSpec.Brief, "changed brief", 1),
		"spec-case":          strings.Replace(base, `"Head":`, `"head":`, 1),
		"spec-duplicate":     strings.Replace(base, `"Brief":`, `"Brief":"ignored","Brief":`, 1),
		"spec-null":          strings.Replace(base, `"Brief":"`+f.binding.SourceSpec.Brief+`"`, `"Brief":null`, 1),
		"inputs-null":        strings.Replace(base, `"Inputs":`+string(inputs), `"Inputs":null`, 1),
		"input-null-item":    strings.Replace(base, `"Inputs":[`, `"Inputs":[null,`, 1),
		"pin-case":           strings.Replace(base, `"path":`, `"Path":`, 1),
		"pin-duplicate":      strings.Replace(base, `"sha256":`, `"sha256":"ignored","sha256":`, 1),
		"pin-null":           strings.Replace(base, `"sha256":"`+f.binding.SourceSpec.Receipts[0].SHA256+`"`, `"sha256":null`, 1),
		"bad-pin":            strings.Replace(base, f.binding.SourceSpec.Receipts[0].SHA256, strings.Repeat("0", 64), 1),
		"invalid-utf8":       strings.Replace(base, "private text", string([]byte{0xff}), 1),
		"oversize":           strings.Repeat(" ", 64*1024+1),
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			sourceWrite(t, f.bindingPath, []byte(invalid))
			if h, err := NewHandler(f.profilePath, f.bindingPath, ""); err != config.ErrSource || h != nil {
				t.Fatalf("invalid binding accepted or leaked error: %v", err)
			}
		})
	}
	sourceWrite(t, f.bindingPath, data)
	f.profile.Permissions = []string{"source.write"}
	encoded, _ := json.Marshal(f.profile)
	sourceWrite(t, f.profilePath, encoded)
	if h, err := NewHandler(f.profilePath, f.bindingPath, ""); err != config.ErrSource || h != nil {
		t.Fatal("source.read not required")
	}
	f.profile.Permissions = []string{"source.read"}
	encoded, _ = json.Marshal(f.profile)
	sourceWrite(t, f.profilePath, encoded)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(f.bindingPath, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link, t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		start := time.Now()
		if h, err := NewHandler(f.profilePath, path, ""); err != config.ErrSource || h != nil || time.Since(start) > time.Second {
			t.Fatalf("nonregular source config accepted/blocked: %v", err)
		}
	}
}
