package codex

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func s3Fixture(t *testing.T) (FixturePreflight, NativeObservation, time.Time) {
	t.Helper()
	r, now := request(t)
	r.Contract.PageID = "11111111-1111-4111-8111-111111111111"
	r.Store.(*fixtureStore).task.PageID = r.Contract.PageID
	r.Contract.FrozenSource = core.Digest("s3-fixture-source")
	r.Grant.SourceIdentity = r.Contract.FrozenSource
	r.Source = fixtureSource{core.SourceSnapshot{Identity: r.Grant.SourceIdentity, Compatibility: "compatible"}}
	proof := config.SourceProof{Commit: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40), SourceSet: r.Contract.FrozenSource,
		ReviewBase: strings.Repeat("3", 40), CurrentBase: strings.Repeat("3", 40), BriefDigest: r.Contract.BriefDigest,
		Receipts: []config.EvidencePin{{Path: ".local/canonical.json", SHA256: core.Digest("canonical")}, {Path: ".local/review.json", SHA256: core.Digest("review")}}}
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, "fixture-access-reference", proof)
	if err != nil {
		t.Fatal(err)
	}
	state := notion.ClaimState{Binding: binding, Owner: binding.Owner, Nonce: r.Grant.ClaimNonce, Fence: 1, LeaseUntil: r.Grant.LeaseUntil, Consumed: true}
	path := filepath.Join(r.Contract.Workspace, "codex")
	data := []byte("\xcf\xfa\xed\xfeS3 inert native-header fixture; never executable code")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, name := range []string{"HOME", "CODEX_HOME", "TMPDIR"} {
		p := filepath.Join(r.Contract.Workspace, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		env[name] = p
	}
	native := NativeIdentity{Path: path, SHA256: core.Digest(string(data)), Version: "0.161.0"}
	plan := FixturePreflight{Request: r, Claim: state, Source: proof, Native: native, Environment: env}
	return plan, NativeObservation{Native: native, Workspace: r.Contract.Workspace, Environment: maps.Clone(env), Isolated: true, ReadOnlySandbox: true, NetworkDenied: true}, now
}

func TestS3Preflight(t *testing.T) {
	for _, name := range []string{"native-replaced", "native-alias", "native-outside", "wrong-hash", "wrong-version", "cwd-drift", "cwd-unresolved", "source-drift", "brief-drift", "receipt-drift", "run-drift", "owner-drift", "nonce-drift", "no-fence", "unconsumed", "expired-lease", "lease-drift", "no-grant", "timeout", "inherited-environment", "auth-environment", "environment-drift", "config-present", "environment-escape", "sandbox-unknown", "network-unknown"} {
		t.Run(name, func(t *testing.T) {
			p, o, now := s3Fixture(t)
			switch name {
			case "native-replaced":
				if err := os.WriteFile(p.Native.Path, []byte("\xcf\xfa\xed\xfechanged"), 0700); err != nil {
					t.Fatal(err)
				}
			case "native-alias":
				alias := filepath.Join(p.Request.Contract.Workspace, "alias")
				if err := os.Symlink(p.Native.Path, alias); err != nil {
					t.Fatal(err)
				}
				p.Native.Path, o.Native.Path = alias, alias
			case "native-outside":
				p.Native.Path, o.Native.Path = filepath.Join(t.TempDir(), "codex"), filepath.Join(t.TempDir(), "codex")
			case "wrong-hash":
				o.Native.SHA256 = core.Digest("wrong")
			case "wrong-version":
				o.Native.Version = "0.162.0"
			case "cwd-drift":
				o.Workspace = t.TempDir()
			case "cwd-unresolved":
				root := p.Request.Contract.Workspace
				unresolved := root + "/../" + filepath.Base(root)
				p.Request.Contract.Workspace, p.Request.Profile.WorkspaceRoot = unresolved, unresolved
				p.Claim.Binding.Workspace, p.Request.Grant.Workspace, o.Workspace = unresolved, unresolved, unresolved
			case "source-drift":
				p.Source.SourceSet = core.Digest("drift")
			case "brief-drift":
				p.Source.BriefDigest = core.Digest("drift")
			case "receipt-drift":
				p.Source.Receipts[0].SHA256 = core.Digest("drift")
			case "run-drift":
				p.Request.RunID = "another-run"
			case "owner-drift":
				p.Claim.Owner = "other-owner"
			case "nonce-drift":
				p.Request.Grant.ClaimNonce = "other-nonce"
			case "no-fence":
				p.Claim.Fence = 0
			case "unconsumed":
				p.Claim.Consumed = false
			case "expired-lease":
				p.Claim.LeaseUntil = now.Add(-time.Second)
			case "lease-drift":
				p.Request.Grant.LeaseUntil = now.Add(2 * time.Minute)
			case "no-grant":
				p.Request.Grant = nil
			case "timeout":
				p.Request.Timeout = 0
			case "inherited-environment":
				o.Isolated = false
			case "auth-environment":
				p.Environment["OPENAI_API_KEY"] = "inert-fixture-value"
				o.Environment = maps.Clone(p.Environment)
			case "environment-drift":
				o.Environment["HOME"] = t.TempDir()
			case "config-present":
				if err := os.WriteFile(filepath.Join(p.Environment["CODEX_HOME"], "config.toml"), []byte("fixture config"), 0600); err != nil {
					t.Fatal(err)
				}
			case "environment-escape":
				p.Environment["HOME"] = t.TempDir()
				o.Environment = maps.Clone(p.Environment)
			case "sandbox-unknown":
				o.ReadOnlySandbox = false
			case "network-unknown":
				o.NetworkDenied = false
			}
			deadline, err := CheckFixturePreflight(p, o, now)
			calls := 0
			runner := functionRunner(func(context.Context, Invocation) Process { calls++; return completedProcess() })
			if err == nil {
				NewFake(runner).Execute(context.Background(), p.Request, time.Now)
			}
			if err != ErrPreflight || !deadline.IsZero() || calls != 0 || p.Request.Store.(*fixtureStore).reads != 0 {
				t.Fatalf("deadline=%v err=%v calls=%d", deadline, err, calls)
			}
			t.Logf("%s=%v fake_delivery0 provider_jobs0", name, err)
		})
	}
	t.Run("positive-consumer-and-deadline", func(t *testing.T) {
		p, o, now := s3Fixture(t)
		deadline, err := CheckFixturePreflight(p, o, now)
		if err != nil || !deadline.Equal(now.Add(p.Request.Timeout)) {
			t.Fatalf("deadline=%v err=%v", deadline, err)
		}
		p.Request.Timeout = 5 * time.Minute
		deadline, err = CheckFixturePreflight(p, o, now)
		if err != nil || !deadline.Equal(p.Claim.LeaseUntil) {
			t.Fatalf("lease cap=%v err=%v", deadline, err)
		}
		calls := 0
		runner := functionRunner(func(_ context.Context, i Invocation) Process {
			calls++
			if i.Dir != p.Request.Contract.Workspace || !strings.HasPrefix(i.Prompt, "approved brief\n") {
				t.Fatal("consumer input lost")
			}
			return completedProcess()
		})
		result := NewFake(runner).Execute(context.Background(), p.Request, time.Now)
		if calls != 1 || result.Mode != "fake" || result.Outcome != "COMPLETED_UNVERIFIED" || result.PID != 0 {
			t.Fatalf("result=%+v calls=%d", result, calls)
		}
		NewFake(runner).Execute(context.Background(), p.Request, time.Now)
		if calls != 1 {
			t.Fatal("fixture replay delivered")
		}
		t.Log("fake consumer1/replay0; inert header read only, native/provider execution0")
	})
}

func TestS3Cleanup(t *testing.T) {
	p, _, _ := s3Fixture(t)
	handoff, err := notion.NewResultHandoff(p.Claim, []byte("fixture-result"))
	if err != nil {
		t.Fatal(err)
	}
	expected := FixtureProcessIdentity{TaskID: p.Claim.Binding.TaskID, RunID: p.Claim.Binding.RunID, ClaimKey: handoff.Key, BirthToken: "fixture-birth-1", ExecutableSHA: p.Native.SHA256, PID: 42, PGID: 42}
	for _, name := range []string{"pid-only", "pid-reused", "wrong-run", "wrong-task", "wrong-claim-fence", "wrong-executable", "wrong-group", "incomplete", "unknown", "remaining-descendant"} {
		t.Run(name, func(t *testing.T) {
			want := expected
			o := FixtureCleanup{Identity: expected, Complete: true}
			switch name {
			case "pid-only":
				want.BirthToken, o.Identity.BirthToken = "", ""
			case "pid-reused":
				o.Identity.BirthToken = "fixture-birth-2"
			case "wrong-run":
				o.Identity.RunID = "other-run"
			case "wrong-task":
				o.Identity.TaskID = "other-task"
			case "wrong-claim-fence":
				o.Identity.ClaimKey = core.Digest("other-fence")
			case "wrong-executable":
				o.Identity.ExecutableSHA = core.Digest("other-native")
			case "wrong-group":
				o.Identity.PGID = 99
			case "incomplete":
				o.Complete = false
			case "unknown":
				o.Unknown = true
			case "remaining-descendant":
				o.Remaining = []FixtureProcessIdentity{expected}
			}
			if CheckFixtureCleanup(want, o) != ErrCleanup {
				t.Fatal("unproven cleanup accepted")
			}
		})
	}
	if err := CheckFixtureCleanup(expected, FixtureCleanup{Identity: expected, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := CheckFixtureCleanup(FixtureProcessIdentity{}, FixtureCleanup{Complete: true}); err != ErrCleanup {
		t.Fatal("empty identity accepted")
	}
	t.Log("cleanup fixture readback only; process lookup/signal/recovery0")
}
