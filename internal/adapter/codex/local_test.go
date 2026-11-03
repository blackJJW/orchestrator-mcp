package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/core"
)

// Reuse accepted Git, sealed-environment and inert test-process fixtures; no provider is called.
func localFixture(t *testing.T) (BridgePlan, core.TaskSnapshot, NativeActivation, *bridgeHTTP, *int) {
	t.Helper()
	p, a, f, calls := activationFixture(t)
	r := p.Preflight.Request
	task := core.TaskSnapshot{TaskID: r.Contract.TaskID, PageID: r.Contract.PageID, Executor: r.Contract.Executor,
		Owner: "Unclaimed", Base: f.base, Brief: bridgeBrief, Eligible: true, DependenciesSatisfied: true, OwnershipKnown: true}
	p.Reader, p.Delivery = nil, nil
	qualify := a.Qualify
	a.Qualify = func(ctx context.Context, launch NativeLaunch) (NativeQualification, error) {
		if launch.LocalTask == nil {
			t.Error("local task missing from qualification binding")
		}
		q, err := qualify(ctx, launch)
		q.ResultDelivery = false
		return q, err
	}
	return p, task, a, f, calls
}

func inertLocalBridge(t *testing.T, p BridgePlan, task core.TaskSnapshot, a NativeActivation) (*NativeBridge, error) {
	t.Helper()
	b, err := NewLocalNativeBridge(p, task, a)
	if err == nil {
		b.bridge.plan.Executor.runner = p.Executor.runner
	}
	return b, err
}

func TestLocalRunDurableReadAndReopen(t *testing.T) {
	p, task, a, f, calls := localFixture(t)
	b, err := inertLocalBridge(t, p, task, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { b.Close() }()
	if other, err := NewLocalNativeBridge(p, task, a); err != journal.ErrBusy || other != nil {
		t.Fatalf("competing local controller: %v", err)
	}
	if _, err := b.ReadResult(context.Background()); err != journal.ErrUnknown {
		t.Fatalf("result before execution: %v", err)
	}
	result, err := b.Run(context.Background())
	if err != nil || result.Outcome != "COMPLETED_UNVERIFIED" || result.summary != "fixture" || *calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, *calls)
	}
	saved, err := b.ReadResult(context.Background())
	var actual LocalResult
	expectedPayload, _ := json.Marshal(LocalResult{Result: result, Summary: "fixture"})
	if err != nil || saved.Stage != "RESULT_DURABLE" || json.Unmarshal(saved.Payload, &actual) != nil || actual.Summary != "fixture" || !bytes.Equal(saved.Payload, expectedPayload) ||
		saved.Delivery == nil || core.Digest(string(saved.Payload)) != saved.Delivery.PayloadDigest {
		t.Fatalf("saved local result=%+v err=%v", saved, err)
	}
	payload := bytes.Clone(saved.Payload)
	saved.Payload[0] = '!'
	if _, err := b.Run(context.Background()); err != journal.ErrConsumed || *calls != 1 {
		t.Fatalf("repeat Run: %v calls=%d", err, *calls)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = inertLocalBridge(t, p, task, a)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = b.ReadResult(context.Background())
	if err != nil || !bytes.Equal(saved.Payload, payload) || saved.Stage != "RESULT_DURABLE" || *calls != 1 {
		t.Fatalf("reopen mutated/reran result: %v calls=%d", err, *calls)
	}
	if f.reads != 0 || f.gets != 0 || f.writes != 0 {
		t.Fatalf("local path touched Notion: read/get/write=%d/%d/%d", f.reads, f.gets, f.writes)
	}
	t.Log("inert consumer1, immutable result read after reopen, repeat execution0, Notion I/O0, SYNC_CONFIRMED0")
}

func TestLocalRejectsMissingAuthorityAndTaskDrift(t *testing.T) {
	for _, name := range []string{"default-off", "grant", "access", "permission", "notion-reader", "notion-delivery", "owner", "lease", "ownership", "task", "brief", "dependency", "eligibility", "source"} {
		t.Run(name, func(t *testing.T) {
			p, task, a, f, calls := localFixture(t)
			switch name {
			case "default-off":
				a.Enabled = false
			case "grant":
				p.Preflight.Request.Grant = nil
			case "access":
				p.AccessRef = ""
			case "permission":
				p.Preflight.Request.Contract.Permission = "source.write"
			case "notion-reader", "notion-delivery":
				other, _, _ := bridgeFixture(t)
				if name == "notion-reader" {
					p.Reader = other.Reader
				} else {
					p.Delivery = other.Delivery
				}
			case "owner":
				task.Owner = "another-owner"
			case "lease":
				task.LeaseUntil = time.Now().Add(time.Minute)
			case "ownership":
				task.OwnershipKnown = false
			case "task":
				task.TaskID = "outside-allowlist"
			case "brief":
				task.Brief += "changed"
			case "dependency":
				task.DependenciesSatisfied = false
			case "eligibility":
				task.Eligible = false
			case "source":
				if err := os.WriteFile(filepath.Join(p.Preflight.Request.Contract.Workspace, "app.go"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			b, err := inertLocalBridge(t, p, task, a)
			if err == nil {
				defer b.Close()
				_, err = b.Run(context.Background())
			}
			if err == nil || *calls != 0 || f.reads != 0 || f.gets != 0 || f.writes != 0 {
				t.Fatalf("rejected scope executed: err=%v calls=%d Notion=%d/%d/%d", err, *calls, f.reads, f.gets, f.writes)
			}
		})
	}
}

func TestLocalUnknownNeverRetries(t *testing.T) {
	for _, name := range []string{"post-intent-login", "cleanup", "oversized-summary"} {
		t.Run(name, func(t *testing.T) {
			p, task, a, _, calls := localFixture(t)
			qualify := a.Qualify
			spawnChecks := 0
			a.Qualify = func(ctx context.Context, launch NativeLaunch) (NativeQualification, error) {
				q, err := qualify(ctx, launch)
				if launch.Purpose == "spawn" {
					spawnChecks++
					if name == "post-intent-login" && spawnChecks == 2 {
						q.Login = false
					}
				}
				return q, err
			}
			if name == "cleanup" {
				p.Cleanup = func(context.Context, Result, notion.ClaimState) (FixtureCleanup, error) {
					return FixtureCleanup{}, errors.New("cleanup not established")
				}
			}
			if name == "oversized-summary" {
				runner := p.Executor.runner
				p.Executor = NewFake(functionRunner(func(ctx context.Context, invocation Invocation) Process {
					process := runner.Run(ctx, invocation)
					process.Stdout = []byte(streamPrefix + message(`{"status":"completed","summary":"`+strings.Repeat("x", 300000)+`"}`) + streamEnd)
					return process
				}))
			}
			b, err := inertLocalBridge(t, p, task, a)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			want := map[string]error{"post-intent-login": journal.ErrUnknown, "cleanup": ErrCleanup, "oversized-summary": journal.ErrIdentity}[name]
			if _, err := b.Run(context.Background()); err != want {
				t.Fatalf("failure=%v, want %v", err, want)
			}
			before := *calls
			if _, err := b.ReadResult(context.Background()); err != journal.ErrUnknown {
				t.Fatalf("INTENT exposed as durable result: %v", err)
			}
			if _, err := b.Run(context.Background()); err != journal.ErrUnknown || *calls != before {
				t.Fatalf("UNKNOWN retried: %v calls=%d/%d", err, *calls, before)
			}
		})
	}
}

func TestLocalChangedActivationCannotAdoptResult(t *testing.T) {
	p, task, a, _, calls := localFixture(t)
	b, err := inertLocalBridge(t, p, task, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Close()
	a.ApprovalRef = "different-approval"
	b, err = inertLocalBridge(t, p, task, a)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.ReadResult(context.Background()); err != journal.ErrIdentity || *calls != 1 {
		t.Fatalf("changed activation adopted result: %v calls=%d", err, *calls)
	}
}

func TestLocalConcurrentRunConsumesOnce(t *testing.T) {
	p, task, a, _, calls := localFixture(t)
	b, err := inertLocalBridge(t, p, task, a)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Run(context.Background())
			if err != nil && err != journal.ErrConsumed {
				t.Errorf("concurrent Run: %v", err)
			}
		}()
	}
	wg.Wait()
	if *calls != 1 {
		t.Fatalf("consumer calls=%d", *calls)
	}
}
