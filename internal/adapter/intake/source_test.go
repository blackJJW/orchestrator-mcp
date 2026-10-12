package intake_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

type sourceFixture struct {
	root     string
	spec     config.SourceSpec
	receipts []config.SourceReceipt
}

func fixtureGit(t *testing.T, root string, args ...string) string {
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

func writeFixture(t *testing.T, root, path, data string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func digestFixture(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fixture(t *testing.T) *sourceFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, ".local/template")
	if err := os.MkdirAll(template, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, root, "init", "-b", "main", "--template="+template)
	inputs := []string{".gitignore", "Makefile", "README.md", "app.go", "go.mod"}
	for path, text := range map[string]string{".gitignore": ".local/\n", "Makefile": "check:\n\ttrue\n", "README.md": "Inert local verifier fixture.\n", "app.go": "package fixture\nconst Value = 1\n", "go.mod": "module fixture\ngo 1.27\n"} {
		writeFixture(t, root, path, text)
	}
	fixtureGit(t, root, append([]string{"add", "--"}, inputs...)...)
	fixtureGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture baseline")
	base := fixtureGit(t, root, "rev-parse", "HEAD")
	fixtureGit(t, root, "switch", "-c", "candidate")
	writeFixture(t, root, "app.go", "package fixture\nconst Value = 2\n")
	fixtureGit(t, root, "add", "--", "app.go")
	fixtureGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture candidate")
	actual := make(map[string]string)
	for _, path := range inputs {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		actual[path] = digestFixture(data)
	}
	encoded, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	f := &sourceFixture{root: root, spec: config.SourceSpec{Workspace: root, ProjectID: "fixture-project", CandidateRef: "refs/heads/candidate", BaseRef: "refs/heads/main", Head: fixtureGit(t, root, "rev-parse", "HEAD"), Tree: fixtureGit(t, root, "rev-parse", "HEAD^{tree}"), SourceSet: digestFixture(encoded), ReviewBase: base, Brief: "Approved S2 fixture brief.", BriefDigest: core.Digest("Approved S2 fixture brief."), Inputs: inputs, CompatibilityInputs: []string{".gitignore", "Makefile", "app.go", "go.mod"}}}
	for _, kind := range []string{"canonical", "review"} {
		result, coverage := "PASS", "make check"
		if kind == "review" {
			result, coverage = "ACCEPTED", "independent-review"
		}
		path := ".local/evidence/" + kind + ".log"
		// This is explicit fixture evidence, never a claim that fixture Makefile checks ran.
		data := "Inert fixture projection: " + kind + " evidence bytes.\n"
		writeFixture(t, root, path, data)
		r := config.SourceReceipt{Version: 1, Kind: kind, Result: result, ProjectID: f.spec.ProjectID, Base: base, Binding: "commit", Head: f.spec.Head, Tree: f.spec.Tree, SourceSet: f.spec.SourceSet, BriefDigest: f.spec.BriefDigest, Inputs: actual, Coverage: []string{coverage}, Artifacts: []config.EvidencePin{{Path: path, SHA256: digestFixture([]byte(data))}}}
		f.receipts = append(f.receipts, r)
		f.spec.Receipts = append(f.spec.Receipts, config.EvidencePin{Path: ".local/evidence/" + kind + ".json"})
		f.saveReceipt(t, len(f.receipts)-1)
	}
	return f
}

func (f *sourceFixture) saveReceipt(t *testing.T, index int) {
	t.Helper()
	data, err := json.Marshal(f.receipts[index])
	if err != nil {
		t.Fatal(err)
	}
	f.saveReceiptBytes(t, index, data)
}

func (f *sourceFixture) saveReceiptBytes(t *testing.T, index int, data []byte) {
	t.Helper()
	writeFixture(t, f.root, f.spec.Receipts[index].Path, string(data))
	f.spec.Receipts[index].SHA256 = digestFixture(data)
}

type fixtureStore struct {
	task          core.TaskSnapshot
	reads, claims int
	afterClaim    func()
}

func (s *fixtureStore) Read(context.Context, string) (core.TaskSnapshot, error) {
	s.reads++
	return s.task, nil
}

func (s *fixtureStore) Claim(_ context.Context, _ string, claim core.Claim) error {
	s.claims++
	s.task.Owner, s.task.ClaimNonce, s.task.LeaseUntil = claim.Owner, claim.Nonce, claim.LeaseUntil
	if s.afterClaim != nil {
		s.afterClaim()
	}
	return nil
}

type observedSource struct {
	intake.Source
	calls int
}

type fixtureConsumer struct {
	expectedSource, expectedBrief string
	calls                         int
	delivered                     intake.Result
}

func (consumer *fixtureConsumer) consume(result intake.Result) error {
	consumer.calls++
	consumer.delivered = result
	if !result.Admitted || result.ExecutionStarted || result.SourceIdentity != consumer.expectedSource || result.BriefDigest != consumer.expectedBrief {
		return errors.New("unexpected fixture delivery payload")
	}
	return nil
}

func (s *observedSource) Verify(ctx context.Context, base string) (core.SourceSnapshot, error) {
	s.calls++
	return s.Source.Verify(ctx, base)
}

func fixtureAdmission(t *testing.T, f *sourceFixture, taskBase string, afterClaim func()) (intake.Result, *fixtureStore, int, int) {
	t.Helper()
	p := config.Profile{ProjectID: f.spec.ProjectID, WorkspaceRoot: f.spec.Workspace, TaskAllowlist: []string{"fixture-task"}, Permissions: []string{"source.read"}}
	c := core.Contract{TaskID: "fixture-task", PageID: "fixture-page", ProjectID: f.spec.ProjectID, Workspace: f.spec.Workspace, Permission: "source.read", Executor: "fixture", Owner: "fixture", AuthorityRef: "explicit-inert-fixture-authority", BriefDigest: core.Digest("Approved S2 fixture brief."), FrozenSource: f.spec.SourceSet}
	s := &fixtureStore{task: core.TaskSnapshot{TaskID: c.TaskID, PageID: c.PageID, Executor: c.Executor, Owner: "Unclaimed", Base: taskBase, Brief: "Approved S2 fixture brief.", Eligible: true, DependenciesSatisfied: true, OwnershipKnown: true}, afterClaim: afterClaim}
	v, err := intake.NewGitSource(p, c, f.spec)
	if err != nil {
		return intake.Result{Reason: "SOURCE_UNPROVEN"}, s, 0, 0
	}
	observed := &observedSource{Source: v}
	r := intake.Admit(context.Background(), p, c, s, observed, func() time.Time { return time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC) })
	consumer := fixtureConsumer{expectedSource: f.spec.SourceSet, expectedBrief: c.BriefDigest}
	if r.Admitted {
		if err := consumer.consume(r); err != nil {
			t.Fatal(err)
		}
		if consumer.delivered != r {
			t.Fatal("consumer did not record actual admission result")
		}
	}
	return r, s, observed.calls, consumer.calls
}

func malformedSourceHead(t *testing.T, f *sourceFixture, value string) {
	t.Helper()
	f.receipts[0].Binding, f.receipts[0].Head = "source", ""
	data, err := json.Marshal(f.receipts[0])
	if err != nil {
		t.Fatal(err)
	}
	f.saveReceiptBytes(t, 0, []byte(strings.Replace(string(data), `"head":""`, `"head":`+value, 1)))
}

func (f *sourceFixture) refreshCandidate(t *testing.T) {
	t.Helper()
	f.spec.Head = fixtureGit(t, f.root, "rev-parse", "HEAD")
	f.spec.Tree = fixtureGit(t, f.root, "rev-parse", "HEAD^{tree}")
	inputs := make(map[string]string)
	for _, path := range f.spec.Inputs {
		data, err := os.ReadFile(filepath.Join(f.root, path))
		if err != nil {
			t.Fatal(err)
		}
		inputs[path] = digestFixture(data)
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	f.spec.SourceSet = digestFixture(encoded)
	for i := range f.receipts {
		f.receipts[i].Head, f.receipts[i].Tree, f.receipts[i].SourceSet, f.receipts[i].Inputs = f.spec.Head, f.spec.Tree, f.spec.SourceSet, inputs
		f.saveReceipt(t, i)
	}
}

func TestGitSourceNegative(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *sourceFixture)
	}{
		{"wrong_commit", func(t *testing.T, f *sourceFixture) { f.spec.Head = strings.Repeat("0", 40) }},
		{"wrong_tree", func(t *testing.T, f *sourceFixture) { f.spec.Tree = strings.Repeat("0", 40) }},
		{"wrong_source_set", func(t *testing.T, f *sourceFixture) { f.spec.SourceSet = strings.Repeat("0", 64) }},
		{"brief_digest", func(t *testing.T, f *sourceFixture) { f.spec.BriefDigest = strings.Repeat("0", 64) }},
		{"brief_changed", func(t *testing.T, f *sourceFixture) { f.spec.Brief += " injected instruction" }},
		{"receipt_bytes_tampered", func(t *testing.T, f *sourceFixture) { writeFixture(t, f.root, f.spec.Receipts[0].Path, "PASS") }},
		{"raw_artifact_tampered", func(t *testing.T, f *sourceFixture) { writeFixture(t, f.root, f.receipts[0].Artifacts[0].Path, "PASS") }},
		{"missing_receipt", func(t *testing.T, f *sourceFixture) { f.spec.Receipts = f.spec.Receipts[:1] }},
		{"missing_raw_artifact", func(t *testing.T, f *sourceFixture) {
			if err := os.Remove(filepath.Join(f.root, f.receipts[0].Artifacts[0].Path)); err != nil {
				t.Fatal(err)
			}
		}},
		{"failed_receipt", func(t *testing.T, f *sourceFixture) { f.receipts[0].Result = "FAIL"; f.saveReceipt(t, 0) }},
		{"wrong_receipt_base", func(t *testing.T, f *sourceFixture) { f.receipts[0].Base = f.spec.Head; f.saveReceipt(t, 0) }},
		{"wrong_receipt_head", func(t *testing.T, f *sourceFixture) { f.receipts[0].Head = f.spec.ReviewBase; f.saveReceipt(t, 0) }},
		{"wrong_receipt_inputs", func(t *testing.T, f *sourceFixture) {
			f.receipts[0].Inputs["app.go"] = strings.Repeat("0", 64)
			f.saveReceipt(t, 0)
		}},
		{"missing_coverage", func(t *testing.T, f *sourceFixture) {
			f.receipts[0].Coverage = []string{"echo PASS"}
			f.saveReceipt(t, 0)
		}},
		{"missing_review", func(t *testing.T, f *sourceFixture) { f.receipts[1].Kind = "canonical"; f.saveReceipt(t, 1) }},
		{"moved_candidate_branch", func(t *testing.T, f *sourceFixture) {
			fixtureGit(t, f.root, "update-ref", f.spec.CandidateRef, f.spec.ReviewBase)
		}},
		{"stale_checkout_head", func(t *testing.T, f *sourceFixture) { fixtureGit(t, f.root, "switch", "main") }},
		{"missing_git_object", func(t *testing.T, f *sourceFixture) { f.spec.ReviewBase = strings.Repeat("f", 40) }},
		{"wrong_workspace", func(t *testing.T, f *sourceFixture) { f.spec.Workspace = t.TempDir() }},
		{"source_modified", func(t *testing.T, f *sourceFixture) {
			writeFixture(t, f.root, "app.go", "package fixture\nconst Value = 99\n")
		}},
		{"untracked_input", func(t *testing.T, f *sourceFixture) { writeFixture(t, f.root, "extra.go", "package fixture\n") }},
		{"deleted_input", func(t *testing.T, f *sourceFixture) {
			if err := os.Remove(filepath.Join(f.root, "app.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"path_escape", func(t *testing.T, f *sourceFixture) { f.spec.Inputs[0] = "../outside" }},
		{"source_symlink", func(t *testing.T, f *sourceFixture) {
			data, err := os.ReadFile(filepath.Join(f.root, "app.go"))
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside.go")
			if err = os.WriteFile(outside, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(f.root, "app.go")); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(outside, filepath.Join(f.root, "app.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"artifact_symlink", func(t *testing.T, f *sourceFixture) {
			p := f.receipts[0].Artifacts[0].Path
			data, err := os.ReadFile(filepath.Join(f.root, p))
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "evidence")
			if err = os.WriteFile(outside, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(f.root, p)); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(outside, filepath.Join(f.root, p)); err != nil {
				t.Fatal(err)
			}
		}},
		{"argv_injection", func(t *testing.T, f *sourceFixture) { f.spec.CandidateRef = "--exec=unexpected" }},
		{"config_effect", func(t *testing.T, f *sourceFixture) {
			fixtureGit(t, f.root, "config", "core.fsmonitor", "unexpected-command")
		}},
		{"incompatible_main_advance", func(t *testing.T, f *sourceFixture) {
			fixtureGit(t, f.root, "switch", "main")
			writeFixture(t, f.root, "app.go", "package fixture\nconst Value = 99\n")
			fixtureGit(t, f.root, "add", "--", "app.go")
			fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture incompatible code advancement")
			fixtureGit(t, f.root, "switch", "candidate")
		}},
		{"unknown_added_main_input", func(t *testing.T, f *sourceFixture) {
			fixtureGit(t, f.root, "switch", "main")
			writeFixture(t, f.root, "unexpected.go", "package fixture\nconst Unknown = 1\n")
			fixtureGit(t, f.root, "add", "--", "unexpected.go")
			fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture unknown added input")
			fixtureGit(t, f.root, "switch", "candidate")
		}},
		{"removed_main_input", func(t *testing.T, f *sourceFixture) {
			fixtureGit(t, f.root, "switch", "main")
			fixtureGit(t, f.root, "rm", "--", "app.go")
			fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture removed input")
			fixtureGit(t, f.root, "switch", "candidate")
		}},
		{"compatibility_closure_missing", func(t *testing.T, f *sourceFixture) { f.spec.CompatibilityInputs = []string{"README.md"} }},
		{"duplicate_receipt_key", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = append([]byte(`{"result":"FAIL",`), data[1:]...)
			f.saveReceiptBytes(t, 0, data)
		}},
		{"case_alias_receipt", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"result":`, `"Result":`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"nested_duplicate_key", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"path":`, `"path":".local/unknown","path":`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"nested_case_alias", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"sha256":`, `"SHA256":`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"null_required_field", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"result":"PASS"`, `"result":null`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"missing_required_field", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"head":"`+f.spec.Head+`",`, "", 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"source_head_null", func(t *testing.T, f *sourceFixture) { malformedSourceHead(t, f, "null") }},
		{"source_head_number", func(t *testing.T, f *sourceFixture) { malformedSourceHead(t, f, "1") }},
		{"source_head_array", func(t *testing.T, f *sourceFixture) { malformedSourceHead(t, f, "[]") }},
		{"source_head_object", func(t *testing.T, f *sourceFixture) { malformedSourceHead(t, f, "{}") }},
		{"coverage_null_element", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"coverage":["make check"]`, `"coverage":["make check",null]`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
		{"nested_null_path", func(t *testing.T, f *sourceFixture) {
			data, _ := json.Marshal(f.receipts[0])
			data = []byte(strings.Replace(string(data), `"path":"`+f.receipts[0].Artifacts[0].Path+`"`, `"path":null`, 1))
			f.saveReceiptBytes(t, 0, data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t)
			tc.edit(t, f)
			proof, err := config.VerifySource(context.Background(), f.spec, f.spec.ReviewBase)
			if err == nil || proof.SourceSet != "" || (!errors.Is(err, config.ErrSource) && !errors.Is(err, config.ErrCompatibility)) {
				t.Fatalf("malformed proof accepted: %+v %v", proof, err)
			}
			r, store, calls, consumer := fixtureAdmission(t, f, f.spec.ReviewBase, nil)
			if r.Admitted || r.ExecutionStarted || store.claims != 0 || consumer != 0 {
				t.Fatalf("unsafe admission=%+v claims=%d consumer=%d", r, store.claims, consumer)
			}
			t.Logf("deny=%v admission=%s verification_calls=%d claim=0 consumer=0", err, r.Reason, calls)
		})
	}
}

func TestGitSourcePositive(t *testing.T) {
	for _, name := range []string{"exact_candidate", "same_candidate_receipt_reuse", "compatible_main_advance", "source_bound_precommit_mapping", "unchanged_checkpoint_commit", "accepted_candidate_main", "accepted_candidate_descendant", "new_candidate_input_compatible_advance"} {
		t.Run(name, func(t *testing.T) {
			f := fixture(t)
			taskBase := f.spec.ReviewBase
			switch name {
			case "accepted_candidate_main":
				fixtureGit(t, f.root, "update-ref", f.spec.BaseRef, f.spec.Head)
				taskBase = f.spec.Head
			case "accepted_candidate_descendant":
				fixtureGit(t, f.root, "update-ref", f.spec.BaseRef, f.spec.Head)
				fixtureGit(t, f.root, "switch", "main")
				writeFixture(t, f.root, "README.md", "Approved documentation change after candidate adoption.\n")
				fixtureGit(t, f.root, "add", "--", "README.md")
				fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture compatible adopted descendant")
				taskBase = fixtureGit(t, f.root, "rev-parse", "HEAD")
				fixtureGit(t, f.root, "switch", "candidate")
			case "new_candidate_input_compatible_advance":
				writeFixture(t, f.root, "new.go", "package fixture\nconst NewInput = 1\n")
				f.spec.Inputs = append(f.spec.Inputs, "new.go")
				f.spec.CompatibilityInputs = append(f.spec.CompatibilityInputs, "new.go")
				fixtureGit(t, f.root, "add", "--", "new.go")
				fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture approved new candidate input")
				f.refreshCandidate(t)
				fixtureGit(t, f.root, "switch", "main")
				writeFixture(t, f.root, "README.md", "Approved base documentation advancement.\n")
				fixtureGit(t, f.root, "add", "--", "README.md")
				fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture matching absence on bases")
				taskBase = fixtureGit(t, f.root, "rev-parse", "HEAD")
				fixtureGit(t, f.root, "switch", "candidate")
			case "compatible_main_advance":
				fixtureGit(t, f.root, "switch", "main")
				writeFixture(t, f.root, "README.md", "Documentation-only approved compatibility change.\n")
				fixtureGit(t, f.root, "add", "--", "README.md")
				fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "fixture compatible main advancement")
				taskBase = fixtureGit(t, f.root, "rev-parse", "HEAD")
				fixtureGit(t, f.root, "switch", "candidate")
			case "source_bound_precommit_mapping":
				for i := range f.receipts {
					f.receipts[i].Binding, f.receipts[i].Head = "source", ""
					f.saveReceipt(t, i)
				}
			case "unchanged_checkpoint_commit":
				fixtureGit(t, f.root, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "fixture unchanged checkpoint")
				f.spec.Head = fixtureGit(t, f.root, "rev-parse", "HEAD")
			}
			for iteration := 0; iteration < 2; iteration++ {
				proof, err := config.VerifySource(context.Background(), f.spec, taskBase)
				if err != nil || proof.Commit != f.spec.Head || proof.Tree != f.spec.Tree || proof.SourceSet != f.spec.SourceSet || proof.BriefDigest != f.spec.BriefDigest || len(proof.Receipts) != 2 {
					t.Fatalf("positive proof %+v: %v", proof, err)
				}
				r, store, calls, consumer := fixtureAdmission(t, f, taskBase, nil)
				if !r.Admitted || r.ExecutionStarted || store.reads != 2 || store.claims != 1 || calls != 2 || consumer != 1 || r.SourceIdentity != f.spec.SourceSet {
					t.Fatalf("actual path=%+v reads=%d claims=%d source=%d consumer=%d", r, store.reads, store.claims, calls, consumer)
				}
			}
			t.Log("actual Git/source→Admission→fake consumer; each pass read=2 verification=2 claim=1 consumer=1; execution_started=false")
		})
	}
}

func TestGitSourceClaimDrift(t *testing.T) {
	f := fixture(t)
	r, s, calls, consumer := fixtureAdmission(t, f, f.spec.ReviewBase, func() { writeFixture(t, f.root, "app.go", "package fixture\nconst Value=99\n") })
	if r.Reason != "SOURCE_CHANGED_DURING_CLAIM" || r.Admitted || s.claims != 1 || calls != 2 || consumer != 0 {
		t.Fatalf("drift=%+v claims=%d source=%d consumer=%d", r, s.claims, calls, consumer)
	}
	t.Log("first proof valid; fixture claim1; post-claim drift denied; consumer0")
}

func TestGitSourceConcurrentReadOnly(t *testing.T) {
	f := fixture(t)
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Go(func() { _, err := config.VerifySource(context.Background(), f.spec, f.spec.ReviewBase); errors <- err })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Log("4 concurrent verifications; read-only, no claim or dispatch")
}

func TestGitSourceTrustedContract(t *testing.T) {
	f := fixture(t)
	p := config.Profile{ProjectID: f.spec.ProjectID, WorkspaceRoot: f.root}
	c := core.Contract{ProjectID: f.spec.ProjectID, Workspace: f.root, BriefDigest: f.spec.BriefDigest, FrozenSource: f.spec.SourceSet}
	for _, name := range []string{"project", "workspace", "brief", "source"} {
		t.Run(name, func(t *testing.T) {
			bad := c
			switch name {
			case "project":
				bad.ProjectID = "other"
			case "workspace":
				bad.Workspace += "/child"
			case "brief":
				bad.BriefDigest = strings.Repeat("0", 64)
			case "source":
				bad.FrozenSource = strings.Repeat("0", 64)
			}
			if _, err := intake.NewGitSource(p, bad, f.spec); err == nil {
				t.Fatal("contract mismatch accepted")
			}
		})
	}
	v, err := intake.NewGitSource(p, c, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	original := slices.Clone(f.spec.Inputs)
	f.spec.Inputs[0] = "../outside"
	f.spec.CompatibilityInputs[0] = "../outside"
	f.spec.Receipts[0].SHA256 = strings.Repeat("0", 64)
	if _, err = v.Verify(context.Background(), f.spec.ReviewBase); err != nil {
		t.Fatalf("caller mutation affected pinned copy: %v inputs=%v", err, original)
	}
	var absent *intake.GitSource
	if _, err = absent.Verify(context.Background(), f.spec.ReviewBase); err == nil {
		t.Fatal("nil verifier accepted")
	}
}
