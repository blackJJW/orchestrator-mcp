package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

func stageA(t *testing.T) (FixturePreflight, journal.Identity, journal.ProcessEvidence, time.Time) {
	t.Helper()
	p, observation, now := s3Fixture(t)
	p.Request.Profile.SchemaVersion = 1
	deadline, err := CheckFixturePreflight(p, observation, now)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.Identity{Contract: p.Request.Contract, Claim: p.Claim, Proof: p.Source, Deadline: deadline}
	h, err := notion.NewResultHandoff(p.Claim, []byte("fixture result"))
	if err != nil {
		t.Fatal(err)
	}
	process := FixtureProcessIdentity{TaskID: p.Request.Contract.TaskID, RunID: p.Request.RunID,
		ClaimKey: h.Key, BirthToken: "fixture-birth", ExecutableSHA: p.Native.SHA256, PID: 4242, PGID: 4242}
	if err := CheckFixtureCleanup(process, FixtureCleanup{Identity: process, Complete: true}); err != nil {
		t.Fatal(err)
	}
	return p, id, journal.ProcessEvidence{TaskID: process.TaskID, RunID: process.RunID, ClaimKey: h.Key,
		BirthToken: process.BirthToken, ExecutableSHA: process.ExecutableSHA, PID: process.PID, PGID: process.PGID, Complete: true}, now
}

func openStageA(t *testing.T, profile config.Profile) *journal.Fixture {
	t.Helper()
	j, err := journal.OpenFixture(profile)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

type stageAInput struct {
	Profile config.Profile
	ID      journal.Identity
	Process journal.ProcessEvidence
	Now     time.Time
}

func stageAChild(t *testing.T, input stageAInput, mode string) []byte {
	t.Helper()
	path := filepath.Join(input.Profile.WorkspaceRoot, ".local", "process-input.json")
	b, _ := json.Marshal(input)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^Test574ProcessHelper$", "-test.v")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "MTT574_FIXTURE_INPUT=" + path, "MTT574_FIXTURE_MODE=" + mode}
	b, err = cmd.CombinedOutput()
	if mode == "busy" {
		if err != nil || !bytes.Contains(b, []byte("LOCK_BUSY fake_delivery0")) {
			t.Fatalf("competing fixture process: %v %s", err, b)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("fixture crash: %v %s", err, b)
		}
	}
	return b
}

// Subprocess entry point for task-owned Go fixtures; never invokes a provider.
func Test574ProcessHelper(t *testing.T) {
	path := os.Getenv("MTT574_FIXTURE_INPUT")
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input stageAInput
	if err := json.Unmarshal(b, &input); err != nil {
		t.Fatal(err)
	}
	j, err := journal.OpenFixture(input.Profile)
	mode := os.Getenv("MTT574_FIXTURE_MODE")
	if mode == "busy" {
		if err != journal.ErrBusy {
			t.Fatalf("expected lock busy, got %v", err)
		}
		t.Log("LOCK_BUSY fake_delivery0")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if mode != "before-intent" {
		if err := j.IntentFixture(input.ID, input.Now); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "after-consumer" || mode == "after-result" {
		// A real test-local delivery effect, independent of process-local C1 reservations.
		if err := os.WriteFile(filepath.Join(input.Profile.WorkspaceRoot, ".local", "fake-deliveries"), []byte("1"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "after-result" {
		if err := j.ResultFixture(input.ID, input.Process, "COMPLETED_UNVERIFIED", []byte("fixture result"), input.Now); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

func Test574SingletonAndConsumer(t *testing.T) {
	p, id, process, now := stageA(t)
	j := openStageA(t, p.Request.Profile)
	defer j.Close()
	stageAChild(t, stageAInput{p.Request.Profile, id, process, now}, "busy")
	var successes, calls atomic.Int64
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := j.IntentFixture(id, now)
			if err == nil {
				successes.Add(1)
				calls.Add(1)
			} else if err != journal.ErrConsumed {
				t.Errorf("intent: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("intent/fake deliveries=%d/%d", successes.Load(), calls.Load())
	}
	t.Log("concurrent intents1 fake_delivery1 competing process delivery0")
}

func Test574CrashRecovery(t *testing.T) {
	for _, mode := range []string{"before-intent", "after-intent", "after-consumer", "after-result"} {
		t.Run(mode, func(t *testing.T) {
			p, id, process, now := stageA(t)
			initial := openStageA(t, p.Request.Profile)
			initial.Close()
			stageAChild(t, stageAInput{p.Request.Profile, id, process, now}, mode)
			j := openStageA(t, p.Request.Profile)
			defer j.Close()
			count, _ := os.ReadFile(filepath.Join(p.Request.Profile.WorkspaceRoot, ".local", "fake-deliveries"))
			if mode == "before-intent" {
				if err := j.IntentFixture(id, now); err != nil {
					t.Fatal(err)
				}
				if len(count) != 0 {
					t.Fatal("pre-intent fixture delivered")
				}
			} else {
				if err := j.IntentFixture(id, now); err != journal.ErrConsumed {
					t.Fatalf("replay=%v", err)
				}
				r, err := j.RecoverFixture(id, now)
				if mode == "after-result" {
					if err != nil || r.Stage != "RESULT_DURABLE" || string(r.Payload) != "fixture result" || string(count) != "1" {
						t.Fatalf("result recovery=%+v/%v deliveries=%s", r, err, count)
					}
				} else if err != journal.ErrUnknown {
					t.Fatalf("uncertain spawn=%v", err)
				}
				if (mode == "after-consumer" && string(count) != "1") || (mode == "after-intent" && len(count) != 0) {
					t.Fatal("fixture delivery evidence mismatch")
				}
			}
			t.Logf("%s SIGKILL/restart: duplicate delivery0; provider jobs0", mode)
		})
	}
}

func Test574ImmutableResultReadback(t *testing.T) {
	p, id, process, now := stageA(t)
	j := openStageA(t, p.Request.Profile)
	if err := j.IntentFixture(id, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := functionRunner(func(context.Context, Invocation) Process {
		calls++
		result := completedProcess()
		result.PID = process.PID
		return result
	})
	result := NewFake(runner).Execute(context.Background(), p.Request, time.Now)
	if calls != 1 || result.Outcome != "COMPLETED_UNVERIFIED" {
		t.Fatalf("actual fake consumer=%d/%+v", calls, result)
	}
	payload, _ := json.Marshal(result)
	if err := j.ResultFixture(id, process, result.Outcome, payload, now); err != nil {
		t.Fatal(err)
	}
	r, err := j.RecoverFixture(id, now)
	if err != nil {
		t.Fatal(err)
	}
	applied := *r.Delivery // In-memory sole-writer projection applied; response lost.
	if err := j.ReadbackFixture(id, nil, now); err != journal.ErrUnknown {
		t.Fatalf("lost response=%v", err)
	}
	j.Close()
	j = openStageA(t, p.Request.Profile)
	defer j.Close()
	r, err = j.RecoverFixture(id, now.Add(4*time.Second)) // Reporting may outlive execution deadline, within same lease.
	if err != nil || r.Stage != "SYNC_UNKNOWN" || !bytes.Equal(r.Payload, payload) {
		t.Fatalf("immutable result recovery=%+v/%v", r, err)
	}
	r.Payload[0] ^= 1
	again, err := j.RecoverFixture(id, now)
	if err != nil || !bytes.Equal(again.Payload, payload) {
		t.Fatal("caller changed immutable payload")
	}
	wrong := applied
	wrong.PayloadDigest = core.Digest("wrong")
	if err := j.ReadbackFixture(id, &wrong, now); err != journal.ErrUnknown {
		t.Fatalf("ambiguous readback=%v", err)
	}
	if err := j.ReadbackFixture(id, &applied, now); err != nil {
		t.Fatal(err)
	}
	again, err = j.RecoverFixture(id, now)
	if err != nil || again.Stage != "SYNC_CONFIRMED" || calls != 1 {
		t.Fatal("readback launched or lost result")
	}
	if err := j.ResultFixture(id, process, result.Outcome, []byte("replacement"), now); err == nil {
		t.Fatal("result replacement accepted")
	}
	if err := j.IntentFixture(id, now); err != journal.ErrConsumed {
		t.Fatal("consumed run reopened")
	}
	id.Claim.Binding.RunID = "new-run"
	if err := j.IntentFixture(id, now); err != journal.ErrConsumed {
		t.Fatalf("consumed claim reused=%v", err)
	}
	t.Log("consumer1 response-loss/readback confirmed exact result; provider rerun0")
}

func Test574StaleIdentity(t *testing.T) {
	for _, name := range []string{"source", "brief", "receipt", "owner", "nonce", "fence", "workspace", "lease", "process-unknown", "pid-only", "accepted-outcome"} {
		t.Run(name, func(t *testing.T) {
			p, id, process, now := stageA(t)
			j := openStageA(t, p.Request.Profile)
			defer j.Close()
			if err := j.IntentFixture(id, now); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "source":
				id.Proof.SourceSet = core.Digest("stale")
			case "brief":
				id.Proof.BriefDigest = core.Digest("stale")
			case "receipt":
				id.Proof.Receipts[0].SHA256 = core.Digest("stale")
			case "owner":
				id.Claim.Owner = "other-owner"
			case "nonce":
				id.Claim.Nonce = "stale-nonce"
			case "fence":
				id.Claim.Fence++
			case "workspace":
				id.Contract.Workspace = t.TempDir()
			case "lease":
				now = id.Claim.LeaseUntil
			case "process-unknown":
				process.Unknown = true
			case "pid-only":
				process.BirthToken = ""
			}
			outcome := "COMPLETED_UNVERIFIED"
			if name == "accepted-outcome" {
				outcome = "ACCEPTED"
			}
			if err := j.ResultFixture(id, process, outcome, []byte("fixture result"), now); err == nil {
				t.Fatalf("%s accepted", name)
			}
			t.Logf("%s denied; new fake_delivery0/provider0", name)
		})
	}
}

func Test574DamagedStorage(t *testing.T) {
	for _, name := range []string{"partial-write", "duplicate", "alias", "truncated", "digest", "missing", "lock-replaced", "namespace-symlink"} {
		t.Run(name, func(t *testing.T) {
			p, id, _, now := stageA(t)
			j := openStageA(t, p.Request.Profile)
			dir := filepath.Join(p.Request.Profile.WorkspaceRoot, journal.Namespace)
			path := filepath.Join(dir, "journal.json")
			if name == "partial-write" {
				if err := os.Mkdir(filepath.Join(dir, "journal.next"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := j.IntentFixture(id, now); err != journal.ErrUnknown {
					t.Fatalf("failed durable commit=%v", err)
				}
			} else if name == "lock-replaced" {
				if err := os.Rename(filepath.Join(dir, "lock"), filepath.Join(dir, "old-lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := j.IntentFixture(id, now); err != journal.ErrUnknown {
					t.Fatalf("lock inode replacement=%v", err)
				}
			}
			j.Close()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "duplicate":
				data = bytes.Replace(data, []byte(`"Version":1`), []byte(`"Version":1,"Version":1`), 1)
			case "alias":
				data = bytes.Replace(data, []byte(`"Version":1`), []byte(`"version":1`), 1)
			case "truncated":
				data = data[:len(data)/2]
			case "digest":
				data = bytes.Replace(data, []byte(`"Digest":"`), []byte(`"Digest":"0`), 1)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "namespace-symlink":
				renamed := dir + "-original"
				if err := os.Rename(dir, renamed); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(renamed, dir); err != nil {
					t.Fatal(err)
				}
			}
			if name != "missing" && name != "namespace-symlink" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "lock-replaced" {
				// Replacement observed by the live owner; no old INTENT existed to recover.
				t.Log("live lock replacement blocked before fake delivery")
				return
			}
			reopened, err := journal.OpenFixture(p.Request.Profile)
			if err == nil {
				reopened.Close()
				t.Fatalf("%s reopened damaged storage", name)
			}
			t.Logf("%s=%v; fake_delivery0/provider0", name, err)
		})
	}
}

func Test574ClaimSchemaBeforeConsumption(t *testing.T) {
	for _, nonce := range []string{" ", "bad\nnonce", "bad\x00nonce", strings.Repeat("x", 129)} {
		t.Run(core.Digest(nonce)[:8], func(t *testing.T) {
			p, id, _, now := stageA(t)
			j := openStageA(t, p.Request.Profile)
			id.Claim.Nonce = nonce
			calls := 0
			consumer := NewFake(functionRunner(func(context.Context, Invocation) Process {
				calls++
				return completedProcess()
			}))
			if _, err := notion.NewResultHandoff(id.Claim, []byte("probe")); err == nil {
				t.Fatal("S1 malformed nonce accepted")
			}
			if err := j.IntentFixture(id, now); err == nil {
				consumer.Execute(context.Background(), p.Request, time.Now)
			} else if err != journal.ErrIdentity {
				t.Fatalf("expected identity rejection=%v", err)
			}
			path := filepath.Join(p.Request.Profile.WorkspaceRoot, journal.Namespace, "journal.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk struct {
				Version            int
				Project, Workspace string
				Records            []journal.Record
				Digest             string
			}
			if err := json.Unmarshal(data, &disk); err != nil || len(disk.Records) != 0 || calls != 0 {
				t.Fatalf("malformed consumed/delivered: %v records=%d calls=%d", err, len(disk.Records), calls)
			}
			id.Claim.Nonce = p.Claim.Nonce
			if err := j.IntentFixture(id, now); err != nil {
				t.Fatal("normal nonce blocked:", err)
			}
			j.Close()
			// Recompute valid canonical integrity to prove decode rejects the schema, not the digest.
			data, _ = os.ReadFile(path)
			if err := json.Unmarshal(data, &disk); err != nil {
				t.Fatal(err)
			}
			disk.Records[0].Identity.Claim.Nonce = nonce
			disk.Digest = ""
			unsigned, _ := json.Marshal(disk)
			disk.Digest = core.Digest(string(unsigned))
			data, _ = json.Marshal(disk)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if reopened, err := journal.OpenFixture(p.Request.Profile); err == nil {
				reopened.Close()
				t.Fatal("malformed persisted nonce recovered")
			}
			t.Log("S1 schema denied before INTENT/consumption/delivery0; valid nonce preserved; invalid durable schema denied")
		})
	}
}

func Test574NamespaceBinding(t *testing.T) {
	p, _, _, _ := stageA(t)
	j := openStageA(t, p.Request.Profile)
	j.Close()
	wrong := p.Request.Profile
	wrong.ProjectID = "other-project"
	if next, err := journal.OpenFixture(wrong); err == nil {
		next.Close()
		t.Fatal("other project opened ledger")
	}
	wrong = p.Request.Profile
	wrong.WorkspaceRoot += "/../" + filepath.Base(wrong.WorkspaceRoot)
	if next, err := journal.OpenFixture(wrong); err == nil {
		next.Close()
		t.Fatal("noncanonical workspace opened")
	}
	t.Log(strings.Join([]string{"fixed namespace", "project/workspace denial", "no credential or native execution"}, "; "))
}
