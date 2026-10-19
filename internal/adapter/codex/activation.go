package codex

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// NativeActivation is a trusted host input, never a task property or CLI flag.
// Qualify must measure sealed ChatGPT login, OS sandbox/network/cleanup and the
// exact approved ResultDelivery transport. No implementation discovers credentials.
type NativeActivation struct {
	Enabled                    bool
	ApprovalRef, GrantIssuer   string
	Deadline, ReadbackDeadline time.Time
	Qualify                    func(context.Context, NativeLaunch) (NativeQualification, error)
}

// NativeLaunch binds host qualification to the exact execution and delivery plan.
// It contains no credential. The returned qualification must echo its digest.
type NativeLaunch struct {
	ApprovalRef, GrantIssuer, ControllerRoot, BirthToken, DeliveryTargetDigest string
	Purpose                                                                    string
	Contract                                                                   core.Contract
	Grant                                                                      Grant
	Source                                                                     config.SourceProof
	Native                                                                     NativeIdentity
	Environment                                                                map[string]string
	Deadline, ReadbackDeadline                                                 time.Time
}

func (p NativeLaunch) Digest() string {
	b, _ := json.Marshal(p)
	return core.Digest(string(b))
}

type NativeQualification struct {
	Binding, AuthMethod                                            string
	ObservedAt, ValidUntil                                         time.Time
	Login, ReadOnlySandbox, NetworkDenied, Cleanup, ResultDelivery bool
}

// NativeBridge shares Stage A/B ownership, durable intent and result-only recovery.
// Zero-value activation is off. There is deliberately no CLI/runtime activation.
type NativeBridge struct{ bridge *FixtureBridge }

func NewNativeBridge(p BridgePlan, a NativeActivation) (*NativeBridge, error) {
	if !a.Enabled || a.Qualify == nil || !hostReference(a.ApprovalRef) || !hostReference(a.GrantIssuer) {
		return nil, ErrLivePending
	}
	e, err := NewNative(p.Preflight.Native.Path, p.Preflight.Native.SHA256)
	if err != nil {
		return nil, ErrPreflight
	}
	p.Executor = e // A fake executor can never supply the production runner.
	b, err := newBridge(p, &a)
	if err != nil {
		return nil, err
	}
	return &NativeBridge{b}, nil
}

func (b *NativeBridge) Run(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithDeadline(ctx, b.bridge.native.ReadbackDeadline)
	defer cancel()
	return b.bridge.Run(ctx)
}
func (b *NativeBridge) Close() error { return b.bridge.Close() }

func hostReference(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\r\n\x00")
}

func (b *FixtureBridge) nativeLaunch() NativeLaunch {
	p, a := b.plan, b.native
	return NativeLaunch{ApprovalRef: a.ApprovalRef, GrantIssuer: a.GrantIssuer,
		ControllerRoot: p.ControllerRoot, BirthToken: p.BirthToken, DeliveryTargetDigest: p.Delivery.BindingDigest(),
		Contract: p.Preflight.Request.Contract, Grant: *p.Preflight.Request.Grant,
		Source: p.Preflight.Source, Native: p.Preflight.Native, Environment: maps.Clone(p.Preflight.Environment), Deadline: a.Deadline, ReadbackDeadline: a.ReadbackDeadline}
}

// Repeated at construction, before intent, immediately before spawn and delivery.
// A host observation is an independent boundary, not proof inferred from a fixture.
func (b *FixtureBridge) checkNative(ctx context.Context, proof config.SourceProof, purpose string) error {
	a, p := b.native, b.plan
	r, now := p.Preflight.Request, time.Now()
	if a == nil || !a.Enabled || ctx.Err() != nil || r.Grant == nil || a.Qualify == nil ||
		!hostReference(a.ApprovalRef) || !hostReference(a.GrantIssuer) || !runID.MatchString(p.BirthToken) ||
		r.Timeout <= 0 || r.Timeout > 5*time.Minute || a.Deadline.IsZero() || !a.ReadbackDeadline.After(now) || a.ReadbackDeadline.Before(a.Deadline) || a.ReadbackDeadline.After(r.Grant.LeaseUntil) ||
		a.Deadline.After(r.Grant.LeaseUntil) || (purpose == "spawn" && (!a.Deadline.After(now) || a.Deadline.After(now.Add(r.Timeout)))) ||
		p.Preflight.Native.Version != "0.161.0" || !fixtureHash(p.Preflight.Native.SHA256) {
		return ErrLivePending
	}
	limit := a.ReadbackDeadline
	if purpose == "spawn" && a.Deadline.Before(limit) {
		limit = a.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	root, err := config.ResolveWorkspace(r.Contract.Workspace)
	if err != nil || root != r.Contract.Workspace || root != r.Profile.WorkspaceRoot {
		return ErrPreflight
	}
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, p.AccessRef, proof)
	g := r.Grant
	if err != nil || g.Reference == "" || g.TaskID != binding.TaskID || g.RunID != binding.RunID ||
		g.Workspace != binding.Workspace || g.SourceIdentity != binding.Source || g.BriefDigest != binding.BriefDigest ||
		!runID.MatchString(g.ClaimNonce) || !g.LeaseUntil.After(now) || r.Contract.Permission != "source.read" {
		return ErrPreflight
	}
	native := p.Preflight.Native
	resolved, err := filepath.EvalSymlinks(native.Path)
	if err != nil || resolved != native.Path || !filepath.IsAbs(native.Path) || filepath.Base(native.Path) != "codex" || !pinned(native.Path, native.SHA256) {
		return ErrPreflight
	}
	if !privateNativeEnvironment(p.ControllerRoot, root, p.Preflight.Environment, purpose == "spawn") {
		return ErrPreflight
	}
	launch := b.nativeLaunch()
	// Use fresh proof without mutating the plan or the caller-owned evidence slices.
	launch.Source = proof
	launch.Source.Receipts = append([]config.EvidencePin(nil), proof.Receipts...)
	launch.Purpose = purpose
	expected := launch.Digest()
	q, err := a.Qualify(ctx, launch)
	now = time.Now()
	if err != nil || ctx.Err() != nil || !limit.After(now) || q.Binding != expected || !q.ResultDelivery ||
		(purpose == "spawn" && (q.AuthMethod != "chatgpt" || !q.Login || !q.ReadOnlySandbox || !q.NetworkDenied || !q.Cleanup)) ||
		q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) > time.Minute ||
		!q.ValidUntil.After(now) || q.ValidUntil.Before(a.ReadbackDeadline) {
		return ErrLivePending
	}
	return nil
}

func privateNativeEnvironment(storage, workspace string, env map[string]string, empty bool) bool {
	resolved, err := config.ResolveWorkspace(storage)
	if err != nil || resolved != storage || len(env) != 3 {
		return false
	}
	for _, pair := range [][2]string{{storage, workspace}, {workspace, storage}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return false
		}
	}
	seen := map[string]bool{}
	for _, name := range []string{"HOME", "CODEX_HOME", "TMPDIR"} {
		path := env[name]
		if !fixturePath(storage, path) || seen[path] {
			return false
		}
		seen[path] = true
		rel, _ := filepath.Rel(storage, path)
		// Sealed child state must never overlap the ownership journal namespace.
		if rel == ".local" || strings.HasPrefix(rel, ".local"+string(filepath.Separator)) {
			return false
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Getuid() {
			return false
		}
		entries, err := os.ReadDir(path)
		if err != nil || (empty && len(entries) != 0) {
			return false
		}
	}
	return true
}
