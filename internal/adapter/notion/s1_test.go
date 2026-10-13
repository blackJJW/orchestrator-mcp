package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

var s1Now = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

type s1Fixture struct {
	root      string
	spec      config.SourceSpec
	profile   config.Profile
	contract  core.Contract
	source    *intake.GitSource
	binding   ClaimBinding
	authority *s1Authority
}

func s1Git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@users.noreply.github.com", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@users.noreply.github.com", "GIT_TERMINAL_PROMPT=0"}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local fixture git %v: %v %s", args, err, b)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func s1Write(t *testing.T, root, path string, b []byte) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func newS1(t *testing.T) *s1Fixture {
	t.Helper()
	root, err := config.ResolveWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, ".local/template")
	if err := os.MkdirAll(template, 0700); err != nil {
		t.Fatal(err)
	}
	s1Git(t, root, "init", "-b", "main", "--template="+template)
	inputs := []string{".gitignore", "app.go"}
	bytes := map[string]string{".gitignore": ".local/\n", "app.go": "package fixture\nconst Value=1\n"}
	hashes := map[string]string{}
	for p, b := range bytes {
		s1Write(t, root, p, []byte(b))
		hashes[p] = core.Digest(b)
	}
	s1Git(t, root, "add", "--", ".gitignore", "app.go")
	s1Git(t, root, "-c", "core.hooksPath=/dev/null", "commit", "-m", "inert local S1 fixture")
	head := s1Git(t, root, "rev-parse", "HEAD")
	tree := s1Git(t, root, "rev-parse", "HEAD^{tree}")
	src, _ := json.Marshal(hashes)
	spec := config.SourceSpec{Workspace: root, ProjectID: "fixture-project", CandidateRef: "refs/heads/main", BaseRef: "refs/heads/main", Head: head, Tree: tree, SourceSet: core.Digest(string(src)), ReviewBase: head, Brief: approvedBrief, BriefDigest: core.Digest(approvedBrief), Inputs: inputs, CompatibilityInputs: inputs}
	for _, kind := range []string{"canonical", "review"} {
		result, coverage := "PASS", "make check"
		if kind == "review" {
			result, coverage = "ACCEPTED", "independent-review"
		}
		raw := "Explicit inert fixture projection; not a claim that a canonical or review run occurred."
		logPath := ".local/" + kind + ".log"
		s1Write(t, root, logPath, []byte(raw))
		receipt := config.SourceReceipt{Version: 1, Kind: kind, Result: result, ProjectID: spec.ProjectID, Base: head, Binding: "commit", Head: head, Tree: tree, SourceSet: spec.SourceSet, BriefDigest: spec.BriefDigest, Inputs: hashes, Coverage: []string{coverage}, Artifacts: []config.EvidencePin{{Path: logPath, SHA256: core.Digest(raw)}}}
		b, _ := json.Marshal(receipt)
		path := ".local/" + kind + ".json"
		s1Write(t, root, path, b)
		spec.Receipts = append(spec.Receipts, config.EvidencePin{Path: path, SHA256: core.Digest(string(b))})
	}
	contract := core.Contract{TaskID: "MTT-572", PageID: pageID, ProjectID: spec.ProjectID, Workspace: root, Permission: "source.read", Executor: "Codex", Owner: "Codex", AuthorityRef: "fixture-source-authority", BriefDigest: spec.BriefDigest, FrozenSource: spec.SourceSet}
	profile := config.Profile{SchemaVersion: 1, ProjectID: spec.ProjectID, WorkspaceRoot: root, TaskAllowlist: []string{contract.TaskID}, Permissions: []string{contract.Permission}}
	source, err := intake.NewGitSource(profile, contract, spec)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := source.Check(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := BindClaim(profile, contract, "fixture-run", "fixture-access", proof)
	if err != nil {
		t.Fatal(err)
	}
	f := &s1Fixture{root: root, spec: spec, profile: profile, contract: contract, source: source, binding: binding}
	f.authority = &s1Authority{state: ClaimState{Binding: binding, Owner: "Unclaimed"}, brief: approvedBrief, base: head}
	return f
}

// This mutex-backed authority exists only in tests, not in the production adapter.
type s1Authority struct {
	mu                sync.Mutex
	state             ClaimState
	brief, base, mode string
	writes            int
	httpCalls         atomic.Int64
}

func (a *s1Authority) transport(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	a.httpCalls.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	var p any
	if strings.HasSuffix(req.URL.Path, "/markdown") {
		p = map[string]any{"object": "page_markdown", "id": pageID, "markdown": a.brief, "truncated": false, "unknown_block_ids": []string{}}
	} else {
		m := fixture()
		props := m["properties"].(map[string]any)
		props["Depends On"] = map[string]any{"type": "relation", "relation": []any{}, "has_more": false}
		props["Owner"] = map[string]any{"type": "select", "select": map[string]any{"name": a.state.Owner}}
		props["Git Base SHA"] = map[string]any{"type": "rich_text", "rich_text": []any{map[string]any{"plain_text": a.base}}}
		if a.state.Owner != "Unclaimed" {
			props["Lease Until"] = map[string]any{"type": "date", "date": map[string]any{"start": a.state.LeaseUntil.Format(time.RFC3339Nano)}}
		}
		if a.state.Nonce != "" {
			props["Checkpoint"] = map[string]any{"type": "rich_text", "rich_text": []any{map[string]any{"plain_text": "controller-claim:" + a.state.Nonce}}}
		}
		if a.mode == "schema" {
			m["archived"] = nil
		}
		if a.mode == "dependency_escape" {
			props["Depends On"] = map[string]any{"type": "relation", "relation": []any{map[string]any{"id": depID}}, "has_more": false}
		}
		p = m
	}
	status := 200
	if a.mode == "unauthorized" {
		status = 401
	}
	if a.mode == "forbidden" {
		status = 403
	}
	if a.mode == "not_found" {
		status = 404
	}
	b, _ := json.Marshal(p)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}, nil
}

func (f *s1Fixture) access(t *testing.T) *AccessReader {
	t.Helper()
	r, err := NewReader("fake-token-never-live", sourceID, MTTFields(), roundTrip(f.authority.transport))
	if err != nil {
		t.Fatal(err)
	}
	scope := AccessScope{ProjectID: f.profile.ProjectID, Workspace: f.root, DataSource: sourceID, AccessRef: "fixture-access", TaskPages: map[string]string{f.contract.TaskID: pageID}, Pages: []string{pageID}, Actions: []string{"read"}}
	scoped, err := NewAccessReader(f.profile, f.contract, scope, "fixture-access", r)
	if err != nil {
		t.Fatal(err)
	}
	return scoped
}

type s1Store struct {
	*AccessReader
	fixture        *s1Fixture
	expectedNonce  string
	expectedFence  uint64
	confirmedFence uint64
	attempts       int
	beforeCAS      func()
	afterCAS       func()
}

func (s *s1Store) Read(ctx context.Context, id string) (core.TaskSnapshot, error) {
	task, err := s.AccessReader.Read(ctx, id)
	if err != nil {
		return task, err
	}
	if s.confirmedFence != 0 {
		a := s.fixture.authority
		a.mu.Lock()
		state := a.state
		a.mu.Unlock()
		if state.Fence != s.confirmedFence || state.Binding != s.fixture.binding {
			return core.TaskSnapshot{}, intake.ErrClaim
		}
	}
	return task, nil
}

func (s *s1Store) Claim(ctx context.Context, id string, c core.Claim) error {
	s.attempts++
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f := s.fixture
	proof, err := f.source.Check(ctx, f.spec.ReviewBase)
	if err != nil {
		return err
	}
	a := f.authority
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if id != f.binding.PageID || a.mode == "unsupported" {
		a.mu.Unlock()
		return intake.ErrClaim
	}
	next, err := ClaimTransition(f.binding, a.state, s.expectedNonce, s.expectedFence, c, proof, s1Now)
	if err != nil {
		a.mu.Unlock()
		return err
	}
	if a.mode == "partial" {
		a.state.Owner = c.Owner
		a.state.LeaseUntil = c.LeaseUntil
		a.writes++
		a.mu.Unlock()
		return ErrClaimUnknown
	}
	a.state = next
	a.writes++
	mode := a.mode
	s.confirmedFence = next.Fence
	a.mu.Unlock()
	if s.afterCAS != nil {
		s.afterCAS()
	}
	if mode == "timeout_after" || mode == "lost_reply" {
		return ErrClaimUnknown
	}
	return ctx.Err()
}

type s1Source struct {
	source *intake.GitSource
	calls  int
}

func (s *s1Source) Verify(ctx context.Context, base string) (core.SourceSnapshot, error) {
	s.calls++
	return s.source.Verify(ctx, base)
}

type s1Consumer struct {
	calls     int
	delivered intake.Result
}

func (c *s1Consumer) deliver(result intake.Result) {
	c.calls++
	c.delivered = result
}

func s1Consume(ctx context.Context, f *s1Fixture, s intake.Store, source *s1Source, c *s1Consumer) intake.Result {
	result := intake.Admit(ctx, f.profile, f.contract, s, source, func() time.Time { return s1Now })
	if result.Admitted {
		c.deliver(result)
	}
	return result
}

func TestS1Access(t *testing.T) {
	for _, name := range []string{"no_reference", "wrong_reference", "wrong_workspace", "wrong_project", "wrong_task", "wrong_page", "wrong_data_source", "write_action", "duplicate_pages", "missing_pages", "dependency_escape", "schema", "unauthorized", "forbidden", "not_found", "production_claim"} {
		t.Run(name, func(t *testing.T) {
			f := newS1(t)
			scope := AccessScope{ProjectID: f.profile.ProjectID, Workspace: f.root, DataSource: sourceID, AccessRef: "fixture-access", TaskPages: map[string]string{f.contract.TaskID: pageID}, Pages: []string{pageID}, Actions: []string{"read"}}
			caller := "fixture-access"
			switch name {
			case "no_reference":
				scope.AccessRef = ""
			case "wrong_reference":
				caller = "unapproved"
			case "wrong_workspace":
				scope.Workspace = t.TempDir()
			case "wrong_project":
				scope.ProjectID = "other"
			case "wrong_task":
				scope.TaskPages = map[string]string{"MTT-999": pageID}
			case "wrong_page":
				scope.TaskPages[f.contract.TaskID] = depID
			case "wrong_data_source":
				scope.DataSource = depID
			case "write_action":
				scope.Actions = append(scope.Actions, "claim")
			case "duplicate_pages":
				scope.Pages = append(scope.Pages, pageID)
			case "missing_pages":
				scope.Pages = nil
			}
			r, err := NewReader("fake-token-never-live", sourceID, MTTFields(), roundTrip(f.authority.transport))
			if err != nil {
				t.Fatal(err)
			}
			s, err := NewAccessReader(f.profile, f.contract, scope, caller, r)
			if err != nil {
				if !errors.Is(err, intake.ErrAccess) || f.authority.httpCalls.Load() != 0 {
					t.Fatal(err)
				}
				t.Log("access denied before GET/claim/consumer")
				return
			}
			f.authority.mode = name
			src, consumer := &s1Source{source: f.source}, &s1Consumer{}
			result := s1Consume(context.Background(), f, s, src, consumer)
			if result.Admitted || consumer.calls != 0 || f.authority.writes != 0 {
				t.Fatalf("denial dispatched: %+v", result)
			}
			if name == "production_claim" && (result.Reason != "CLAIM_WRITE_UNAUTHORIZED" || src.calls != 1) {
				t.Fatal(result)
			}
			if name != "production_claim" && src.calls != 0 {
				t.Fatalf("invalid reader reached source: %s", name)
			}
			t.Logf("reason=%s source=%d claim_writes=0 consumer=0", result.Reason, src.calls)
		})
	}
	t.Run("no_authentication", func(t *testing.T) {
		if _, err := NewReader("", sourceID, MTTFields(), nil); !errors.Is(err, intake.ErrAccess) {
			t.Fatal(err)
		}
	})
	t.Run("scope_copy", func(t *testing.T) {
		f := newS1(t)
		r, _ := NewReader("fake-token-never-live", sourceID, MTTFields(), roundTrip(f.authority.transport))
		scope := AccessScope{ProjectID: f.profile.ProjectID, Workspace: f.root, DataSource: sourceID, AccessRef: "fixture-access", TaskPages: map[string]string{f.contract.TaskID: pageID}, Pages: []string{pageID}, Actions: []string{"read"}}
		s, err := NewAccessReader(f.profile, f.contract, scope, "fixture-access", r)
		if err != nil {
			t.Fatal(err)
		}
		scope.Pages[0] = depID
		scope.TaskPages[f.contract.TaskID] = depID
		scope.Actions[0] = "claim"
		if _, err := s.Read(context.Background(), pageID); err != nil {
			t.Fatal(err)
		}
		before := f.authority.httpCalls.Load()
		if _, err := s.Read(context.Background(), depID); !errors.Is(err, intake.ErrAccess) || f.authority.httpCalls.Load() != before {
			t.Fatal("scope widened")
		}
	})
}

func TestS1ClaimFailures(t *testing.T) {
	for _, name := range []string{"owner_lease", "owned_unknown_lease", "stale_nonce", "stale_fence", "fence_overflow", "brief_before", "brief_after", "source_before", "source_after", "receipt_after", "fence_after", "timeout_before", "timeout_claim_before", "timeout_after", "partial", "lost_reply", "unsupported"} {
		t.Run(name, func(t *testing.T) {
			f := newS1(t)
			store := &s1Store{AccessReader: f.access(t), fixture: f}
			source, consumer := &s1Source{source: f.source}, &s1Consumer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "owner_lease":
				f.authority.state.Owner = "Human"
				f.authority.state.LeaseUntil = s1Now.Add(time.Minute)
			case "owned_unknown_lease":
				f.authority.state.Owner = "Human"
			case "stale_nonce":
				store.expectedNonce = "stale"
			case "stale_fence":
				store.expectedFence = 1
			case "fence_overflow":
				f.authority.state.Fence = math.MaxUint64
				store.expectedFence = math.MaxUint64
			case "brief_before":
				f.authority.brief = approvedBrief + " changed"
			case "brief_after":
				store.afterCAS = func() { f.authority.mu.Lock(); f.authority.brief = approvedBrief + " changed"; f.authority.mu.Unlock() }
			case "source_before":
				s1Write(t, f.root, "app.go", []byte("package fixture\nconst Value=2\n"))
			case "source_after":
				store.afterCAS = func() { s1Write(t, f.root, "app.go", []byte("package fixture\nconst Value=2\n")) }
			case "receipt_after":
				store.afterCAS = func() { s1Write(t, f.root, ".local/canonical.log", []byte("tampered")) }
			case "fence_after":
				store.afterCAS = func() { f.authority.mu.Lock(); f.authority.state.Fence++; f.authority.mu.Unlock() }
			case "timeout_before":
				expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				ctx = expired
			case "timeout_claim_before":
				store.beforeCAS = cancel
			case "timeout_after":
				f.authority.mode = name
				store.afterCAS = cancel
			default:
				f.authority.mode = name
			}
			result := s1Consume(ctx, f, store, source, consumer)
			if result.Admitted || result.ExecutionStarted || consumer.calls != 0 {
				t.Fatalf("unsafe delivery: %+v", result)
			}
			if name == "timeout_before" && (f.authority.writes != 0 || source.calls != 0) {
				t.Fatal("cancel reached writer/source")
			}
			if name == "timeout_claim_before" && (f.authority.writes != 0 || source.calls != 1) {
				t.Fatal("cancel crossed atomic writer")
			}
			if name == "owner_lease" && f.authority.writes != 0 {
				t.Fatal("live lease overwritten")
			}
			if (name == "partial" || name == "timeout_after" || name == "lost_reply") && f.authority.writes != 1 {
				t.Fatal("fault did not model possible application")
			}
			if name == "source_after" || name == "receipt_after" {
				if result.Reason != "SOURCE_CHANGED_DURING_CLAIM" {
					t.Fatal(result)
				}
			}
			t.Logf("reason=%s source=%d claim_attempts=%d writes=%d consumer=0", result.Reason, source.calls, store.attempts, f.authority.writes)
		})
	}
}

func TestS1ConcurrentClaim(t *testing.T) {
	f := newS1(t)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	stores := []*s1Store{{AccessReader: f.access(t), fixture: f}, {AccessReader: f.access(t), fixture: f}}
	consumers := []*s1Consumer{{}, {}}
	sources := []*s1Source{{source: f.source}, {source: f.source}}
	results := make([]intake.Result, 2)
	for i := range stores {
		stores[i].beforeCAS = func() { arrived <- struct{}{}; <-release }
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s1Consume(context.Background(), f, stores[i], sources[i], consumers[i])
		}(i)
	}
	<-arrived
	<-arrived
	close(release)
	wg.Wait()
	accepted, calls := 0, 0
	for i, r := range results {
		if r.Admitted {
			accepted++
			if consumers[i].delivered != r || r.SourceIdentity != f.binding.Source || r.BriefDigest != f.binding.BriefDigest || sources[i].calls != 2 {
				t.Fatal("delivery or source proof missing")
			}
		}
		calls += consumers[i].calls
	}
	if accepted != 1 || calls != 1 || f.authority.writes != 1 || f.authority.state.Fence != 1 {
		t.Fatalf("duplicate accepted=%d consumer=%d writes=%d results=%+v", accepted, calls, f.authority.writes, results)
	}
	// The same run is consumed even after its lease expires; no replay or duplicate consumer.
	f.authority.state.LeaseUntil = s1Now.Add(-time.Minute)
	replay := &s1Store{AccessReader: f.access(t), fixture: f, expectedNonce: f.authority.state.Nonce, expectedFence: f.authority.state.Fence}
	c := &s1Consumer{}
	result := s1Consume(context.Background(), f, replay, &s1Source{source: f.source}, c)
	if result.Admitted || c.calls != 0 || f.authority.writes != 1 {
		t.Fatal("replay")
	}
	t.Log("two competing controllers: claim1 delivery1; duplicate/replay0; pre/post actual Git verification observed")
}

func TestS1ResultContract(t *testing.T) {
	f := newS1(t)
	s := &s1Store{AccessReader: f.access(t), fixture: f}
	result := s1Consume(context.Background(), f, s, &s1Source{source: f.source}, &s1Consumer{})
	if !result.Admitted {
		t.Fatal(result)
	}
	h, err := NewResultHandoff(f.authority.state, []byte("immutable fixture result"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfirmResultReadback(h, h); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"payload", "run", "source", "nonce", "fence", "operation", "key", "partial"} {
		t.Run(name, func(t *testing.T) {
			other := h
			switch name {
			case "payload":
				other.PayloadDigest = core.Digest("different")
			case "run":
				other.Binding.RunID = "old-run"
			case "source":
				other.Binding.Source = core.Digest("stale")
			case "nonce":
				other.ClaimNonce = "old-nonce"
			case "fence":
				other.Fence++
			case "operation":
				other.Operation = "blind-append"
			case "key":
				other.Key = core.Digest("wrong")
			case "partial":
				other = ResultHandoff{}
			}
			if !errors.Is(ConfirmResultReadback(h, other), ErrResult) {
				t.Fatal("ambiguous result readback accepted")
			}
		})
	}
	h2, err := NewResultHandoff(f.authority.state, []byte("different result"))
	if err != nil || h2.Key != h.Key || h2.PayloadDigest == h.PayloadDigest {
		t.Fatal("delivery key/payload identity lost")
	}
	if _, err := NewResultHandoff(f.authority.state, nil); !errors.Is(err, ErrResult) {
		t.Fatal("empty payload")
	}
	forged := ClaimState{Consumed: true, Owner: "Codex", Nonce: "forged", Fence: 1}
	if _, err := NewResultHandoff(forged, []byte("payload")); !errors.Is(err, ErrResult) {
		t.Fatal("unbound result accepted")
	}
	t.Log(fmt.Sprintf("same delivery key=%s, payload change conflicts; no writer/retry/journal", h.Key))
}
