package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

const bridgeBrief = `Executor role: Codex — Read-only Verification
Mutation authority: NONE
## Objective
Inert stage B fixture
## Git base & relevant context
Task-owned local objects only
## Required change / investigation
Read only
## Preserve / invariants
Default deny
## Out of scope / must not
No provider or real Notion
## Authorization delta
Fixture only
## Validation classification
N/A
## Acceptance criteria
Fake consumer observed
## Stop / block conditions
Unknown
## Expected result / handoff
Unverified evidence`

type bridgeHTTP struct {
	mu                                          sync.Mutex
	page, block, dataSource, base, owner, brief string
	lease                                       any
	reads, gets, writes                         int
	slot                                        string
	blockFlags                                  string
	lost, unknown, conflict                     bool
	afterRead                                   func(int)
}

func (f *bridgeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Scheme != "https" || r.URL.Host != "api.notion.com" || r.Header.Get("Authorization") != "Bearer fixture-only-token" || r.Header.Get("Notion-Version") != "2026-03-11" {
		return nil, errors.New("fixture access violation")
	}
	var value any
	switch r.URL.Path {
	case "/v1/pages/" + f.page:
		if r.Method != http.MethodGet {
			return nil, errors.New("Notion claim attempted")
		}
		f.reads++
		if f.afterRead != nil {
			f.afterRead(f.reads)
		}
		sel := func(s string) any { return map[string]any{"type": "select", "select": map[string]string{"name": s}} }
		value = map[string]any{"object": "page", "id": f.page, "archived": false, "in_trash": false,
			"parent": map[string]string{"data_source_id": f.dataSource}, "properties": map[string]any{
				"Task ID": map[string]any{"type": "unique_id", "unique_id": map[string]any{"prefix": "MTT", "number": 574}},
				"Status":  sel("READY_FOR_IMPLEMENTATION"), "Assigned To": sel("Codex"), "Owner": sel(f.owner),
				"Lease Until":  map[string]any{"type": "date", "date": f.lease},
				"Git Base SHA": map[string]any{"type": "rich_text", "rich_text": []any{map[string]string{"plain_text": f.base}}},
				"Checkpoint":   map[string]any{"type": "rich_text", "rich_text": []any{}},
				"Depends On":   map[string]any{"type": "relation", "relation": []any{}, "has_more": false}}}
	case "/v1/pages/" + f.page + "/markdown":
		value = map[string]any{"object": "page_markdown", "id": f.page, "markdown": f.brief, "truncated": false, "unknown_block_ids": []string{}, "warnings": []any{}}
	case "/v1/blocks/" + f.block:
		if r.Method == http.MethodPatch {
			f.writes++
			var body struct {
				Paragraph struct {
					Rich []struct {
						Text struct {
							Content string `json:"content"`
						} `json:"text"`
					} `json:"rich_text"`
				} `json:"paragraph"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Paragraph.Rich) != 1 {
				return nil, errors.New("invalid fixed replace")
			}
			f.slot = body.Paragraph.Rich[0].Text.Content
			if f.lost {
				return nil, errors.New("response lost after application")
			}
		} else if r.Method == http.MethodGet {
			f.gets++
			if f.unknown && f.writes > 0 {
				return nil, errors.New("readback unavailable")
			}
		} else {
			return nil, errors.New("unexpected method")
		}
		text := []any{}
		if f.conflict {
			text = []any{map[string]string{"plain_text": "foreign writer"}}
		} else if f.slot != "" {
			text = []any{map[string]string{"plain_text": f.slot}}
		}
		value = map[string]any{"object": "block", "id": f.block, "type": "paragraph", "parent": map[string]string{"type": "page_id", "page_id": f.page}, "paragraph": map[string]any{"rich_text": text}, "future_extension": map[string]bool{"supported": true}}
	default:
		return nil, errors.New("path escape")
	}
	b, _ := json.Marshal(value)
	if strings.HasPrefix(r.URL.Path, "/v1/blocks/") {
		b = append([]byte("{"+f.blockFlags), b[1:]...)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}, nil
}

func bridgeGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@users.noreply.github.com", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@users.noreply.github.com", "GIT_TERMINAL_PROMPT=0"}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git: %v %s", err, b)
	}
	return strings.TrimSpace(string(b))
}

func bridgeFixture(t *testing.T) (BridgePlan, *bridgeHTTP, *int) {
	t.Helper()
	p, o, _ := s3Fixture(t)
	r := &p.Request
	r.Profile.SchemaVersion = 1
	r.Contract.TaskID = "MTT-574"
	r.Profile.TaskAllowlist = []string{r.Contract.TaskID}
	r.Contract.Owner, r.Contract.Executor = "Codex", "Codex"
	r.Contract.BriefDigest = core.Digest(bridgeBrief)
	r.Grant.TaskID, r.Grant.BriefDigest = r.Contract.TaskID, r.Contract.BriefDigest
	r.Grant.LeaseUntil = time.Now().Add(2 * time.Minute)
	r.Timeout = 30 * time.Second
	root := r.Contract.Workspace
	if err := os.MkdirAll(filepath.Join(root, ".local/template"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, old := range p.Environment {
		newpath := filepath.Join(root, ".local", name)
		if err := os.Rename(old, newpath); err != nil {
			t.Fatal(err)
		}
		p.Environment[name] = newpath
	}
	newnative := filepath.Join(root, ".local/codex")
	if err := os.Rename(p.Native.Path, newnative); err != nil {
		t.Fatal(err)
	}
	p.Native.Path = newnative
	o.Native = p.Native
	o.Environment = maps.Clone(p.Environment)
	bridgeGit(t, root, "init", "-b", "main", "--template="+filepath.Join(root, ".local/template"))
	inputs := map[string]string{".gitignore": ".local/\n", "app.go": "package fixture\nconst Value=1\n"}
	hashes := map[string]string{}
	for path, data := range inputs {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		hashes[path] = core.Digest(data)
	}
	bridgeGit(t, root, "add", "--", ".gitignore", "app.go")
	bridgeGit(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "inert bridge fixture")
	head := bridgeGit(t, root, "rev-parse", "HEAD")
	tree := bridgeGit(t, root, "rev-parse", "HEAD^{tree}")
	encoded, _ := json.Marshal(hashes)
	spec := config.SourceSpec{Workspace: root, ProjectID: r.Profile.ProjectID, CandidateRef: "refs/heads/main", BaseRef: "refs/heads/main", Head: head, Tree: tree, SourceSet: core.Digest(string(encoded)), ReviewBase: head, Brief: bridgeBrief, BriefDigest: r.Contract.BriefDigest, Inputs: []string{".gitignore", "app.go"}, CompatibilityInputs: []string{".gitignore", "app.go"}}
	for _, kind := range []string{"canonical", "review"} {
		result, coverage := "PASS", "make check"
		if kind == "review" {
			result, coverage = "ACCEPTED", "independent-review"
		}
		raw := []byte("Inert fixture projection; no canonical/review provenance claim.")
		log := ".local/" + kind + ".log"
		if err := os.WriteFile(filepath.Join(root, log), raw, 0600); err != nil {
			t.Fatal(err)
		}
		receipt := config.SourceReceipt{Version: 1, Kind: kind, Result: result, ProjectID: spec.ProjectID, Base: head, Binding: "commit", Head: head, Tree: tree, SourceSet: spec.SourceSet, BriefDigest: spec.BriefDigest, Inputs: hashes, Coverage: []string{coverage}, Artifacts: []config.EvidencePin{{Path: log, SHA256: core.Digest(string(raw))}}}
		data, _ := json.Marshal(receipt)
		path := ".local/" + kind + ".json"
		if err := os.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
			t.Fatal(err)
		}
		spec.Receipts = append(spec.Receipts, config.EvidencePin{Path: path, SHA256: core.Digest(string(data))})
	}
	r.Contract.FrozenSource = spec.SourceSet
	r.Grant.SourceIdentity = spec.SourceSet
	source, err := intake.NewGitSource(r.Profile, r.Contract, spec)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := source.Check(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	p.Source = proof
	access := "fixture-stage-b-read"
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, access, proof)
	if err != nil {
		t.Fatal(err)
	}
	p.Claim = notion.ClaimState{Binding: binding, Owner: binding.Owner, Nonce: r.Grant.ClaimNonce, Fence: 1, LeaseUntil: r.Grant.LeaseUntil, Consumed: true}
	f := &bridgeHTTP{page: r.Contract.PageID, block: "33333333-3333-4333-8333-333333333333", dataSource: "22222222-2222-4222-8222-222222222222", base: head, owner: "Unclaimed", brief: bridgeBrief, blockFlags: `"in_trash":false,"archived":false,`}
	reader, err := notion.NewReader("fixture-only-token", f.dataSource, notion.MTTFields(), f)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := notion.NewAccessReader(r.Profile, r.Contract, notion.AccessScope{ProjectID: r.Profile.ProjectID, Workspace: root, DataSource: f.dataSource, AccessRef: access, TaskPages: map[string]string{r.Contract.TaskID: f.page}, Pages: []string{f.page}, Actions: []string{"read"}}, access, reader)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := notion.NewResultDelivery(notion.ResultTarget{Reference: "fixture-result-grant", Project: r.Profile.ProjectID, Workspace: root, Task: r.Contract.TaskID, Page: f.page, Block: f.block}, "fixture-only-token", f)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	runner := functionRunner(func(ctx context.Context, i Invocation) Process {
		*calls++
		if i.Dir != root || len(i.Environment) != 4 || i.Environment[0] != "PATH=/usr/bin:/bin" || !strings.HasPrefix(i.Prompt, bridgeBrief+"\n") {
			t.Error("consumer binding/environment lost")
		}
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		return runProcessEnvironment(ctx, self, []string{"-test.run=^TestBEnvironmentHelper$", "BENV"}, i.Dir, i.Prompt, i.Environment)
	})
	cleanup := func(_ context.Context, result Result, state notion.ClaimState) (FixtureCleanup, error) {
		h, err := notion.NewResultHandoff(state, []byte("fixture cleanup"))
		if err != nil {
			return FixtureCleanup{}, err
		}
		return FixtureCleanup{Identity: FixtureProcessIdentity{TaskID: r.Contract.TaskID, RunID: r.RunID, ClaimKey: h.Key, BirthToken: "fixture-birth", ExecutableSHA: p.Native.SHA256, PID: result.PID, PGID: result.PID}, Complete: true}, nil
	}
	return BridgePlan{DevelopmentRef: "explicit-stage-b-development", AccessRef: access, ControllerRoot: t.TempDir(), BirthToken: "fixture-birth", Preflight: p, Observation: o, Reader: scoped, Source: source, Executor: NewFake(runner), Delivery: delivery, Cleanup: cleanup}, f, calls
}

func TestBEnvironmentHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "BENV" {
		return
	}
	if os.Getenv("FORBIDDEN_PARENT") != "" || os.Getenv("HOME") == "" || os.Getenv("CODEX_HOME") == "" || os.Getenv("TMPDIR") == "" {
		os.Exit(3)
	}
	fmt.Print(string(completedProcess().Stdout))
	os.Exit(0)
}

func TestBBridgePositiveRecovery(t *testing.T) {
	t.Setenv("FORBIDDEN_PARENT", "inert-parent-value")
	p, f, calls := bridgeFixture(t)
	f.lost = true
	b, err := NewFixtureBridge(p)
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.Run(context.Background())
	if err != nil || result.Outcome != "COMPLETED_UNVERIFIED" || *calls != 1 || f.writes != 1 {
		t.Fatalf("result=%+v err=%v calls=%d writes=%d", result, err, *calls, f.writes)
	}
	if _, err := os.Stat(filepath.Join(p.Preflight.Request.Contract.Workspace, journal.Namespace)); !os.IsNotExist(err) {
		t.Fatal("journal wrote into source workspace")
	}
	b.Close()
	b, err = NewFixtureBridge(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Run(context.Background())
	if err != nil || *calls != 1 || f.writes != 1 {
		t.Fatalf("recovery repeated execution/write=%v %d/%d", err, *calls, f.writes)
	}
	t.Log("actual Git+scoped Reader+local durable authority+sealed fake test process1+supported PATCH1+lost response exact readback; provider0")
}

func TestBBridgeFailClosed(t *testing.T) {
	for _, name := range []string{"no-development", "no-grant", "native-mode", "access-mismatch", "foreign-owner", "live-lease", "source-drift", "post-intent-brief", "post-intent-native", "sandbox", "environment", "nonce", "intent-write", "cleanup", "conflicting-result", "unknown-readback"} {
		t.Run(name, func(t *testing.T) {
			p, f, calls := bridgeFixture(t)
			switch name {
			case "no-development":
				p.DevelopmentRef = ""
			case "no-grant":
				p.Preflight.Request.Grant = nil
			case "native-mode":
				var err error
				p.Executor, err = NewNative(p.Preflight.Native.Path, p.Preflight.Native.SHA256)
				if err != nil {
					t.Fatal(err)
				}
			case "access-mismatch":
				p.AccessRef = "different"
			case "foreign-owner":
				f.owner = "Human"
				f.lease = map[string]string{"start": time.Now().Add(-time.Minute).Format(time.RFC3339)}
			case "live-lease":
				f.lease = map[string]string{"start": time.Now().Add(time.Minute).Format(time.RFC3339)}
			case "source-drift":
				os.WriteFile(filepath.Join(p.Preflight.Request.Contract.Workspace, "app.go"), []byte("changed source"), 0600)
			case "post-intent-brief":
				f.afterRead = func(n int) {
					if n == 2 {
						f.brief = bridgeBrief + "\nchanged"
					}
				}
			case "post-intent-native":
				f.afterRead = func(n int) {
					if n == 2 {
						os.WriteFile(p.Preflight.Native.Path, []byte("changed native"), 0700)
					}
				}
			case "sandbox":
				p.Observation.ReadOnlySandbox = false
			case "environment":
				p.Preflight.Environment["EXTRA"] = "inert"
				p.Observation.Environment = maps.Clone(p.Preflight.Environment)
			case "nonce":
				p.Preflight.Request.Grant.ClaimNonce = "bad\nnonce"
			case "cleanup":
				old := p.Cleanup
				p.Cleanup = func(ctx context.Context, r Result, s notion.ClaimState) (FixtureCleanup, error) {
					v, e := old(ctx, r, s)
					v.Identity.BirthToken = "reused"
					return v, e
				}
			case "conflicting-result":
				f.conflict = true
			case "unknown-readback":
				f.unknown = true
			}
			b, err := NewFixtureBridge(p)
			if b != nil {
				defer b.Close()
				if name == "intent-write" {
					if e := os.Mkdir(filepath.Join(p.ControllerRoot, journal.Namespace, "journal.next"), 0700); e != nil {
						t.Fatal(e)
					}
				}
				_, err = b.Run(context.Background())
			}
			if err == nil {
				t.Fatal("malformed/unqualified bridge accepted")
			}
			post := name == "cleanup" || name == "conflicting-result" || name == "unknown-readback"
			if (!post && *calls != 0) || (post && *calls != 1) {
				t.Fatalf("calls=%d error=%v", *calls, err)
			}
			if name == "unknown-readback" {
				b.Close()
				f.unknown = false
				b, err = NewFixtureBridge(p)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				if _, err = b.Run(context.Background()); err != nil || *calls != 1 || f.writes != 1 {
					t.Fatalf("readback-only recovery=%v calls%d/write%d", err, *calls, f.writes)
				}
			}
			t.Logf("%s denied; fake calls%d/provider0", name, *calls)
		})
	}
}

func TestBBridgeCompetitionAndStorage(t *testing.T) {
	p, _, calls := bridgeFixture(t)
	b, err := NewFixtureBridge(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if other, err := NewFixtureBridge(p); err != journal.ErrBusy {
		if other != nil {
			other.Close()
		}
		t.Fatalf("competing controller=%v", err)
	}
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := b.Run(context.Background())
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if *calls != 1 {
		t.Fatalf("duplicate consumer=%d", *calls)
	}
	for _, path := range []string{p.Preflight.Request.Contract.Workspace, filepath.Join(p.Preflight.Request.Contract.Workspace, ".local")} {
		if j, err := journal.OpenControllerFixture(p.Preflight.Request.Profile, path); err == nil {
			j.Close()
			t.Fatal("source/storage overlap accepted")
		}
	}
	t.Log("single cooperating namespace/competing controller0 duplicate fake0; source/storage overlap deny")
}

func TestBResultBeforeDeliveryRecovery(t *testing.T) {
	p, f, calls := bridgeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	old := p.Cleanup
	p.Cleanup = func(c context.Context, r Result, s notion.ClaimState) (FixtureCleanup, error) {
		v, e := old(c, r, s)
		cancel()
		return v, e
	}
	b, err := NewFixtureBridge(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Run(ctx); err == nil || *calls != 1 || f.writes != 0 {
		t.Fatalf("pre-delivery crash=%v calls%d writes%d", err, *calls, f.writes)
	}
	b.Close()
	p.Cleanup = old
	b, err = NewFixtureBridge(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err = b.Run(context.Background()); err != nil || *calls != 1 || f.writes != 1 {
		t.Fatalf("saved-result recovery=%v calls%d writes%d", err, *calls, f.writes)
	}
	t.Log("RESULT_DURABLE before attempted write resumes only saved result; fake rerun0")
}

func TestBDeliveryAuthorityRecovery(t *testing.T) {
	for _, stage := range []string{"RESULT_DURABLE", "SYNC_UNKNOWN"} {
		for _, field := range []string{"reference", "block"} {
			t.Run(stage+"/"+field, func(t *testing.T) {
				p, f, calls := bridgeFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				old := p.Cleanup
				if stage == "RESULT_DURABLE" {
					p.Cleanup = func(c context.Context, r Result, s notion.ClaimState) (FixtureCleanup, error) {
						v, e := old(c, r, s)
						cancel()
						return v, e
					}
				} else {
					f.unknown = true
				}
				b, err := NewFixtureBridge(p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = b.Run(ctx); err == nil || *calls != 1 {
					t.Fatalf("saved-result precondition=%v calls%d", err, *calls)
				}
				b.Close()
				p.Cleanup = old
				f.unknown = false
				gets, writes := f.gets, f.writes
				c := p.Preflight.Request.Contract
				target := notion.ResultTarget{Reference: "fixture-result-grant", Project: c.ProjectID, Workspace: c.Workspace, Task: c.TaskID, Page: c.PageID, Block: f.block}
				if field == "reference" {
					target.Reference = "changed-authority"
				} else {
					target.Block = "44444444-4444-4444-8444-444444444444"
					f.block = target.Block
				}
				p.Delivery, err = notion.NewResultDelivery(target, "fixture-only-token", f)
				if err != nil {
					t.Fatal(err)
				}
				b, err = NewFixtureBridge(p)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				if _, err = b.Run(context.Background()); err != journal.ErrIdentity || *calls != 1 || f.gets != gets || f.writes != writes {
					t.Fatalf("authority drift accepted: %v calls%d GET%d PATCH%d", err, *calls, f.gets-gets, f.writes-writes)
				}
				t.Log("changed durable target denied before GET/PATCH; fake rerun0")
			})
		}
	}
}

func TestBDeliveryPinAndContract(t *testing.T) {
	p, f, calls := bridgeFixture(t)
	c := p.Preflight.Request.Contract
	target := notion.ResultTarget{Reference: "fixture-result-grant", Project: c.ProjectID, Workspace: c.Workspace, Task: c.TaskID, Page: c.PageID, Block: f.block}
	for _, field := range []string{"reference", "project", "workspace", "task", "page", "block"} {
		changed := target
		switch field {
		case "reference":
			changed.Reference = "changed"
		case "project":
			changed.Project = "other"
		case "workspace":
			changed.Workspace = t.TempDir()
		case "task":
			changed.Task = "MTT-575"
		case "page":
			changed.Page = "44444444-4444-4444-8444-444444444444"
		case "block":
			changed.Block = "44444444-4444-4444-8444-444444444444"
		}
		delivery, err := notion.NewResultDelivery(changed, "fixture-only-token", f)
		if err != nil || delivery.BindingDigest() == p.Delivery.BindingDigest() {
			t.Fatalf("%s omitted from pin: %v", field, err)
		}
		if field != "reference" && field != "block" {
			q := p
			q.Delivery = delivery
			if bridge, err := NewFixtureBridge(q); err == nil {
				bridge.Close()
				t.Fatalf("%s contract mismatch admitted", field)
			}
		}
	}
	j, err := journal.OpenControllerFixture(p.Preflight.Request.Profile, p.ControllerRoot)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.Identity{Contract: c, Claim: p.Preflight.Claim, Proof: p.Preflight.Source, Deadline: p.Preflight.Claim.LeaseUntil}
	for _, pin := range []string{"", "unknown"} {
		id.DeliveryTargetDigest = pin
		if _, err := j.ClaimIntentFixture(id, time.Now()); err != journal.ErrIdentity {
			t.Fatalf("missing/unknown pin consumed: %v", err)
		}
	}
	id.DeliveryTargetDigest = p.Delivery.BindingDigest()
	id, err = j.ClaimIntentFixture(id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, err = journal.OpenControllerFixture(p.Preflight.Request.Profile, p.ControllerRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, pin := range []string{"", core.Digest("different wire operation")} {
		q := id
		q.DeliveryTargetDigest = pin
		if _, _, err := j.FindFixture(q, time.Now()); err != journal.ErrIdentity {
			t.Fatalf("changed pin reopened: %v", err)
		}
		if _, err := j.RecoverFixture(q, time.Now()); err != journal.ErrIdentity {
			t.Fatalf("changed pin recovered: %v", err)
		}
	}
	if *calls != 0 || f.gets != 0 || f.writes != 0 {
		t.Fatal("pin check reached consumer/writer")
	}
	t.Log("all public target fields pinned; missing/unknown/operation drift denied; fake/GET/PATCH0")
}

func TestBResultBlockSchema(t *testing.T) {
	p, f, _ := bridgeFixture(t)
	payload := []byte("inert readback payload")
	handoff, err := notion.NewResultHandoff(p.Preflight.Claim, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Delivery.Replace(context.Background(), handoff, payload); err != nil {
		t.Fatal(err)
	}
	saved := f.slot
	f.writes = 0
	cases := []struct {
		name, flags string
		valid       bool
	}{
		{"false", `"in_trash":false,"archived":false,`, true},
		{"deprecated-omitted", `"in_trash":false,`, true},
		{"missing", `"archived":false,`, false},
		{"null", `"in_trash":null,`, false},
		{"true", `"in_trash":true,`, false},
		{"number", `"in_trash":0,`, false},
		{"string", `"in_trash":"false",`, false},
		{"array", `"in_trash":[],`, false},
		{"object", `"in_trash":{},`, false},
		{"alias", `"In_Trash":false,`, false},
		{"collision", `"in_trash":false,"In_Trash":true,`, false},
		{"duplicate", `"in_trash":true,"in_trash":false,`, false},
		{"archived-null", `"in_trash":false,"archived":null,`, false},
		{"archived-true", `"in_trash":false,"archived":true,`, false},
		{"archived-number", `"in_trash":false,"archived":0,`, false},
		{"archived-string", `"in_trash":false,"archived":"false",`, false},
		{"archived-array", `"in_trash":false,"archived":[],`, false},
		{"archived-object", `"in_trash":false,"archived":{},`, false},
		{"archived-alias", `"in_trash":false,"Archived":false,`, false},
		{"archived-collision", `"in_trash":false,"archived":false,"Archived":true,`, false},
		{"archived-duplicate", `"in_trash":false,"archived":true,"archived":false,`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.blockFlags = tc.flags
			for _, slot := range []string{"", saved} {
				f.slot = slot
				matching, empty, err := p.Delivery.Observe(context.Background(), handoff, payload)
				if tc.valid {
					if err != nil || matching != (slot != "") || empty != (slot == "") {
						t.Fatalf("valid schema rejected: %v %t/%t", err, matching, empty)
					}
				} else if err == nil || matching || empty {
					t.Fatalf("malformed/deleted schema accepted: %v %t/%t", err, matching, empty)
				}
			}
		})
	}
	if f.writes != 0 {
		t.Fatal("readback performed a write")
	}
}

func TestBDeletedResultBlocksDelivery(t *testing.T) {
	for _, flags := range []string{`"in_trash":true,"archived":false,`, `"in_trash":false,"archived":true,`} {
		p, f, calls := bridgeFixture(t)
		f.blockFlags = flags
		b, err := NewFixtureBridge(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.Run(context.Background())
		b.Close()
		if err != journal.ErrUnknown || *calls != 1 || f.writes != 0 {
			t.Fatalf("deleted target promoted: %v calls%d writes%d", err, *calls, f.writes)
		}
		b, err = NewFixtureBridge(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.Run(context.Background())
		b.Close()
		if err != journal.ErrUnknown || *calls != 1 || f.writes != 0 {
			t.Fatal("deleted target recovery retried provider/write")
		}
	}
	t.Log("deleted readback remains UNKNOWN; PATCH0/confirmed0/provider rerun0")
}
