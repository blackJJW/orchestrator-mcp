package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
)

// Reuse Stage B's real local Git/Reader/journal and inert test-process consumer.
// These observations deliberately grant no real auth, sandbox or provider qualification.
func activationFixture(t *testing.T) (BridgePlan, NativeActivation, *bridgeHTTP, *int) {
	t.Helper()
	p, f, calls := bridgeFixture(t)
	for name, old := range p.Preflight.Environment {
		path := filepath.Join(p.ControllerRoot, name)
		if err := os.Rename(old, path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
		p.Preflight.Environment[name] = path
	}
	old := p.Preflight.Native.Path
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.Rename(old, path); err != nil {
		t.Fatal(err)
	}
	p.Preflight.Native.Path = path
	a := NativeActivation{Enabled: true, ApprovalRef: "test-only-approval", GrantIssuer: "test-only-host",
		Deadline: time.Now().Add(20 * time.Second), ReadbackDeadline: time.Now().Add(time.Minute),
		Qualify: func(_ context.Context, p NativeLaunch) (NativeQualification, error) {
			return NativeQualification{Binding: p.Digest(), AuthMethod: "chatgpt", ObservedAt: time.Now(), ValidUntil: p.ReadbackDeadline,
				Login: true, ReadOnlySandbox: true, NetworkDenied: true, Cleanup: true, ResultDelivery: true}, nil
		}}
	return p, a, f, calls
}

// Only tests replace the newly constructed native runner with the existing inert
// consumer. Production constructor always installs nativeRunner, never a caller runner.
func inertNativeBridge(t *testing.T, p BridgePlan, a NativeActivation) (*NativeBridge, error) {
	t.Helper()
	b, err := NewNativeBridge(p, a)
	if err == nil {
		b.bridge.plan.Executor.runner = p.Executor.runner
	}
	return b, err
}

func TestNativeActivationDefaultOff(t *testing.T) {
	p, _, f, calls := activationFixture(t)
	if b, err := NewNativeBridge(p, NativeActivation{}); err != ErrLivePending || b != nil {
		t.Fatal("default enabled", err)
	}
	if *calls != 0 || f.reads != 0 || f.writes != 0 {
		t.Fatal("default-off side effect")
	}
	if _, err := os.Stat(filepath.Join(p.ControllerRoot, journal.Namespace)); !os.IsNotExist(err) {
		t.Fatal("default-off created journal")
	}
}

func TestNativeActivationRejects(t *testing.T) {
	for _, name := range []string{"approval", "issuer", "grant", "deadline", "readback-deadline", "login", "api-auth", "sandbox", "network", "cleanup-proof", "delivery", "observation-error", "binding", "stale", "expired-proof", "identity", "alias", "version", "env-extra", "env-escape", "env-config", "env-permissions", "env-shared", "owner", "source", "brief"} {
		t.Run(name, func(t *testing.T) {
			p, a, f, calls := activationFixture(t)
			qualify := a.Qualify
			a.Qualify = func(ctx context.Context, l NativeLaunch) (NativeQualification, error) {
				q, err := qualify(ctx, l)
				switch name {
				case "login":
					q.Login = false
				case "api-auth":
					q.AuthMethod = "api-key"
				case "sandbox":
					q.ReadOnlySandbox = false
				case "network":
					q.NetworkDenied = false
				case "cleanup-proof":
					q.Cleanup = false
				case "delivery":
					q.ResultDelivery = false
				case "observation-error":
					err = ErrLivePending
				case "binding":
					q.Binding = "wrong"
				case "stale":
					q.ObservedAt = time.Now().Add(-2 * time.Minute)
				case "expired-proof":
					q.ValidUntil = time.Now().Add(-time.Second)
				}
				return q, err
			}
			switch name {
			case "approval":
				a.ApprovalRef = ""
			case "issuer":
				a.GrantIssuer = ""
			case "grant":
				p.Preflight.Request.Grant = nil
			case "deadline":
				a.Deadline = time.Now().Add(-time.Second)
			case "readback-deadline":
				a.ReadbackDeadline = time.Now().Add(-time.Second)
			case "identity":
				os.WriteFile(p.Preflight.Native.Path, []byte("drift"), 0700)
			case "alias":
				alias := filepath.Join(t.TempDir(), "codex")
				os.Symlink(p.Preflight.Native.Path, alias)
				p.Preflight.Native.Path = alias
			case "version":
				p.Preflight.Native.Version = "alpha"
			case "env-extra":
				p.Preflight.Environment["OPENAI_API_KEY"] = "inert-forbidden"
			case "env-escape":
				p.Preflight.Environment["HOME"] = t.TempDir()
			case "env-config":
				os.WriteFile(filepath.Join(p.Preflight.Environment["CODEX_HOME"], "config.toml"), []byte("inert"), 0600)
			case "env-permissions":
				os.Chmod(p.Preflight.Environment["HOME"], 0755)
			case "env-shared":
				p.Preflight.Environment["HOME"] = p.Preflight.Environment["TMPDIR"]
			case "owner":
				f.owner = "Human"
			case "source":
				os.WriteFile(filepath.Join(p.Preflight.Request.Contract.Workspace, "app.go"), []byte("drift"), 0600)
			case "brief":
				f.brief = bridgeBrief + "\ndrift"
			}
			b, err := inertNativeBridge(t, p, a)
			if err == nil {
				defer b.Close()
				_, err = b.Run(context.Background())
			}
			if err == nil || *calls != 0 || f.writes != 0 {
				t.Fatalf("not denied err=%v consumer=%d write=%d", err, *calls, f.writes)
			}
		})
	}
}

func TestNativeActivationDeliveryOnlyRecovery(t *testing.T) {
	p, a, f, calls := activationFixture(t)
	a.Deadline = time.Now().Add(5 * time.Second)
	f.lost = true
	b, err := inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.Run(context.Background())
	if err != nil || result.Mode != "native" || result.Outcome != "COMPLETED_UNVERIFIED" || *calls != 1 || f.writes != 1 {
		t.Fatalf("result=%+v err=%v calls=%d writes=%d", result, err, *calls, f.writes)
	}
	b.Close()
	// Expired execution approval and no login must not require a provider restart
	// merely to confirm the exact already durable result under valid writer approval.
	time.Sleep(time.Until(a.Deadline) + time.Millisecond)
	b, err = inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.bridge.native.Qualify = func(_ context.Context, l NativeLaunch) (NativeQualification, error) {
		if l.Purpose == "spawn" {
			t.Error("recovery requested spawn")
		}
		return NativeQualification{Binding: l.Digest(), ObservedAt: time.Now(), ValidUntil: l.ReadbackDeadline, ResultDelivery: true}, nil
	}
	_, err = b.Run(context.Background())
	if err != nil || *calls != 1 || f.writes != 1 {
		t.Fatalf("recovery err=%v calls=%d write=%d", err, *calls, f.writes)
	}
}

func TestNativeActivationUnknownAndCleanup(t *testing.T) {
	for _, name := range []string{"post-intent-login", "post-intent-identity", "cleanup-failure", "unknown-delivery"} {
		t.Run(name, func(t *testing.T) {
			p, a, f, calls := activationFixture(t)
			q := a.Qualify
			a.Qualify = func(ctx context.Context, l NativeLaunch) (NativeQualification, error) {
				v, e := q(ctx, l)
				if name == "post-intent-login" && l.Purpose == "spawn" && f.reads >= 3 {
					v.Login = false
				}
				return v, e
			}
			if name == "post-intent-identity" {
				f.afterRead = func(n int) {
					if n == 3 {
						os.WriteFile(p.Preflight.Native.Path, []byte("drift"), 0700)
					}
				}
			}
			if name == "cleanup-failure" {
				old := p.Cleanup
				p.Cleanup = func(ctx context.Context, r Result, c notion.ClaimState) (FixtureCleanup, error) {
					v, _ := old(ctx, r, c)
					v.Unknown = true
					return v, nil
				}
			}
			if name == "unknown-delivery" {
				f.unknown = true
			}
			b, err := inertNativeBridge(t, p, a)
			if err != nil {
				t.Fatal(err)
			}
			_, err = b.Run(context.Background())
			if err == nil {
				t.Fatal("ambiguous operation accepted")
			}
			before := *calls
			if name == "post-intent-login" || name == "post-intent-identity" {
				if before != 0 {
					t.Fatal("guard dispatched")
				}
			} else if before != 1 {
				t.Fatal("missing positive consumer")
			}
			b.Close()
			// Restoring an observation cannot remove the durable consumed intent.
			if name == "post-intent-identity" {
				return
			} // identity drift itself remains blocked
			b, err = inertNativeBridge(t, p, a)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			_, err = b.Run(context.Background())
			if err == nil || *calls != before {
				t.Fatalf("replayed consumed/unknown run: %v %d/%d", err, *calls, before)
			}
		})
	}
}

func TestNativeActivationSingletonAndPin(t *testing.T) {
	p, a, f, calls := activationFixture(t)
	b, err := inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, e := NewNativeBridge(p, a)
			if e == nil {
				other.Close()
				t.Error("second controller acquired lock")
			}
		}()
	}
	wg.Wait()
	_, err = b.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	a.ApprovalRef = "different-approval"
	b, err = inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Run(context.Background())
	if !errors.Is(err, journal.ErrIdentity) || *calls != 1 || f.writes != 1 {
		t.Fatalf("execution pin drift accepted %v", err)
	}
}

type activationHTTP func(*http.Request) (*http.Response, error)

func (f activationHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeActivationQualificationDeadline(t *testing.T) {
	p, a, _, calls := activationFixture(t)
	a.Deadline = time.Now().Add(20 * time.Millisecond)
	a.ReadbackDeadline = time.Now().Add(50 * time.Millisecond)
	a.Qualify = func(_ context.Context, l NativeLaunch) (NativeQualification, error) {
		// An uncooperative observer cannot extend the operator's approved window.
		time.Sleep(time.Until(l.ReadbackDeadline) + 10*time.Millisecond)
		return NativeQualification{Binding: l.Digest(), ObservedAt: time.Now(), ValidUntil: l.ReadbackDeadline.Add(time.Minute), ResultDelivery: true}, nil
	}
	b, err := inertNativeBridge(t, p, a)
	if b != nil {
		b.Close()
	}
	if err == nil || *calls != 0 {
		t.Fatalf("late qualification admitted: %v consumer=%d", err, *calls)
	}
}

func TestNativeActivationReadbackDeadline(t *testing.T) {
	p, a, f, calls := activationFixture(t)
	a.Deadline = time.Now().Add(8 * time.Second)
	a.ReadbackDeadline = time.Now().Add(10 * time.Second)
	armed := false
	transport := activationHTTP(func(r *http.Request) (*http.Response, error) {
		response, err := f.RoundTrip(r)
		if armed && r.Method == http.MethodGet {
			deadline, ok := r.Context().Deadline()
			if !ok || deadline.After(a.ReadbackDeadline) {
				t.Error("readback I/O lacks approved deadline")
			}
			// Model a response returned after cancellation, not a new network operation.
			time.Sleep(time.Until(a.ReadbackDeadline) + 10*time.Millisecond)
		}
		return response, err
	})
	var err error
	c := p.Preflight.Request.Contract
	p.Delivery, err = notion.NewResultDelivery(notion.ResultTarget{Reference: "fixture-result-grant", Project: c.ProjectID, Workspace: c.Workspace, Task: c.TaskID, Page: c.PageID, Block: f.block}, "fixture-only-token", transport)
	if err != nil {
		t.Fatal(err)
	}
	f.unknown = true
	b, err := inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Run(context.Background())
	if err == nil || *calls != 1 || f.writes != 1 {
		t.Fatal("missing uncertain positive effect", err)
	}
	b.Close()
	f.unknown = false
	armed = true
	b, err = inertNativeBridge(t, p, a)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Run(context.Background())
	if err == nil || *calls != 1 || f.writes != 1 {
		t.Fatalf("late readback confirmed or replayed: %v %d/%d", err, *calls, f.writes)
	}
	// Neither deadline nor a late exact response promotes the durable record.
	data, readErr := os.ReadFile(filepath.Join(p.ControllerRoot, journal.Namespace, "journal.json"))
	var disk struct{ Records []journal.Record }
	if readErr != nil || json.Unmarshal(data, &disk) != nil || len(disk.Records) == 0 || disk.Records[len(disk.Records)-1].Stage != "SYNC_UNKNOWN" {
		t.Fatal("late response promoted durable state", readErr)
	}
}
