package codex

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

var ErrLivePending = errors.New("LIVE_AUTH_PENDING")

type BridgePlan struct {
	DevelopmentRef, AccessRef, ControllerRoot, BirthToken string
	Preflight                                             FixturePreflight
	Observation                                           NativeObservation
	Reader                                                *notion.AccessReader
	Source                                                *intake.GitSource
	Executor                                              *Executor
	Delivery                                              *notion.ResultDelivery
	Cleanup                                               func(context.Context, Result, notion.ClaimState) (FixtureCleanup, error)
}

type FixtureBridge struct {
	mu      sync.Mutex
	plan    BridgePlan
	journal *journal.Fixture
	native  *NativeActivation
}

// Development-only connector. Native mode is refused even with a model launch grant.
func NewFixtureBridge(p BridgePlan) (*FixtureBridge, error) {
	return newBridge(p, nil)
}

func newBridge(p BridgePlan, native *NativeActivation) (*FixtureBridge, error) {
	mode := "fake"
	if native != nil {
		mode = "native"
	}
	if p.DevelopmentRef == "" || strings.TrimSpace(p.DevelopmentRef) != p.DevelopmentRef || strings.ContainsAny(p.DevelopmentRef, "\r\n\x00") ||
		p.Reader == nil || p.Source == nil || p.Executor == nil || p.Executor.mode != mode || p.Executor.runner == nil || p.Delivery == nil || p.Cleanup == nil || p.BirthToken == "" {
		return nil, ErrLivePending
	}
	r := p.Preflight.Request
	if r.Grant == nil || p.AccessRef == "" || !p.Reader.MatchesBinding(r.Contract, p.AccessRef) || !p.Delivery.MatchesBinding(r.Contract) {
		return nil, ErrLivePending
	}
	grant := *r.Grant
	p.Preflight.Request.Grant = &grant
	p.Preflight.Request.Profile.TaskAllowlist = slices.Clone(r.Profile.TaskAllowlist)
	p.Preflight.Request.Profile.Permissions = slices.Clone(r.Profile.Permissions)
	p.Preflight.Environment = maps.Clone(p.Preflight.Environment)
	p.Observation.Environment = maps.Clone(p.Observation.Environment)
	p.Preflight.Source.Receipts = slices.Clone(p.Preflight.Source.Receipts)
	b := &FixtureBridge{plan: p, native: native}
	if native != nil {
		if err := b.checkNative(context.Background(), p.Preflight.Source, "open"); err != nil {
			return nil, err
		}
	}
	j, err := journal.OpenControllerFixture(p.Preflight.Request.Profile, p.ControllerRoot)
	if err != nil {
		return nil, err
	}
	b.journal = j
	return b, nil
}

func (b *FixtureBridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.journal.Close()
}

// Run is shared with the separately gated native adapter; the CLI stays dry-run.
func (b *FixtureBridge) Run(ctx context.Context) (Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.plan
	r := p.Preflight.Request
	denied := Result{SchemaVersion: 1, TaskID: r.Contract.TaskID, RunID: r.RunID, Mode: p.Executor.mode, Outcome: "BLOCKED", ExitCode: -1}
	if ctx.Err() != nil {
		return denied, ctx.Err()
	}
	raw, proof, err := b.read(ctx)
	if err != nil {
		return denied, err
	}
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, p.AccessRef, proof)
	if err != nil {
		return denied, err
	}
	id := journal.Identity{Contract: r.Contract, Proof: proof, DeliveryTargetDigest: p.Delivery.BindingDigest(), Claim: notion.ClaimState{Binding: binding, Owner: binding.Owner,
		Nonce: r.Grant.ClaimNonce, LeaseUntil: r.Grant.LeaseUntil}, Deadline: time.Now().Add(r.Timeout)}
	if id.Claim.LeaseUntil.Before(id.Deadline) {
		id.Deadline = id.Claim.LeaseUntil
	}
	if b.native != nil {
		id.Deadline = b.native.Deadline
		id.ExecutionDigest = b.nativeLaunch().Digest()
	}
	saved, exists, err := b.journal.FindFixture(id, time.Now())
	if err != nil {
		return denied, err
	}
	if exists {
		if saved.Stage == "INTENT" {
			return denied, journal.ErrUnknown
		}
		// Reporting may outlive execution deadline; current claim/source proof is still required.
		saved.Identity.Proof = proof
		return denied, b.deliver(ctx, saved.Identity)
	}
	// Validate a proposed local claim before consuming it. Journal assigns the real local fence.
	proposed := p.Preflight
	proposed.Source = proof
	proposed.Claim = id.Claim
	proposed.Claim.Consumed = true
	proposed.Claim.Fence = 1
	if err := b.preflight(ctx, proposed); err != nil {
		return denied, err
	}
	if ctx.Err() != nil {
		return denied, ctx.Err()
	}
	id, err = b.journal.ClaimIntentFixture(id, time.Now())
	if err != nil {
		return denied, err
	}
	store := bridgeStore{bridge: b, id: id, raw: raw}
	r.Store = &store
	r.Source = p.Source
	guard := bridgeRunner{bridge: b, id: id, inner: p.Executor.runner}
	if b.native != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, b.native.Deadline)
		defer cancel()
	}
	result := (&Executor{runner: &guard, mode: p.Executor.mode}).Execute(ctx, r, time.Now)
	if !guard.started {
		return result, journal.ErrUnknown
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanupCancel()
	observation, err := p.Cleanup(cleanupCtx, result, id.Claim)
	if err != nil {
		return result, ErrCleanup
	}
	h, _ := notion.NewResultHandoff(id.Claim, []byte("process binding"))
	expected := FixtureProcessIdentity{TaskID: r.Contract.TaskID, RunID: r.RunID, ClaimKey: h.Key,
		BirthToken: p.BirthToken, ExecutableSHA: p.Preflight.Native.SHA256, PID: result.PID, PGID: result.PID}
	if err := CheckFixtureCleanup(expected, observation); err != nil {
		return result, err
	}
	outcome := result.Outcome
	if outcome != "COMPLETED_UNVERIFIED" && outcome != "BLOCKED" {
		outcome = "FAILED"
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	process := journal.ProcessEvidence{TaskID: expected.TaskID, RunID: expected.RunID, ClaimKey: expected.ClaimKey,
		BirthToken: expected.BirthToken, ExecutableSHA: expected.ExecutableSHA, SessionID: result.SessionID,
		PID: expected.PID, PGID: expected.PGID, Complete: true}
	if err := b.journal.ResultFixture(id, process, outcome, payload, time.Now()); err != nil {
		return result, err
	}
	return result, b.deliver(ctx, id)
}

func (b *FixtureBridge) read(ctx context.Context) (core.TaskSnapshot, config.SourceProof, error) {
	r := b.plan.Preflight.Request
	task, err := b.plan.Reader.Read(ctx, r.Contract.PageID)
	if err != nil {
		return task, config.SourceProof{}, err
	}
	// Never hide a raw Notion owner/lease with the local claim overlay.
	if task.Owner != "Unclaimed" || task.ClaimNonce != "" || task.LeaseUntil.After(time.Now()) {
		return task, config.SourceProof{}, journal.ErrIdentity
	}
	proof, err := b.plan.Source.Check(ctx, task.Base)
	if err != nil {
		return task, proof, err
	}
	if core.Admission(r.Profile, r.Contract, task, core.SourceSnapshot{Identity: proof.SourceSet, Compatibility: "compatible"}, time.Now(), nil) != "ADMITTED" {
		return task, proof, journal.ErrIdentity
	}
	return task, proof, nil
}

type bridgeStore struct {
	bridge *FixtureBridge
	id     journal.Identity
	raw    core.TaskSnapshot
}

func (s *bridgeStore) Read(ctx context.Context, page string) (core.TaskSnapshot, error) {
	if page != s.id.Contract.PageID {
		return core.TaskSnapshot{}, journal.ErrIdentity
	}
	task, proof, err := s.bridge.read(ctx)
	if err != nil {
		return task, err
	}
	binding, err := notion.BindClaim(s.bridge.plan.Preflight.Request.Profile, s.id.Contract, s.id.Claim.Binding.RunID, s.id.Claim.Binding.AccessRef, proof)
	if err != nil || binding != s.id.Claim.Binding || task.Base != s.raw.Base {
		return task, journal.ErrIdentity
	}
	// Trusted local authority is kept separate; the raw Notion snapshot was checked first.
	task.Owner, task.ClaimNonce, task.LeaseUntil = s.id.Claim.Owner, s.id.Claim.Nonce, s.id.Claim.LeaseUntil
	return task, nil
}
func (*bridgeStore) Claim(context.Context, string, core.Claim) error { return intake.ErrClaim }

type bridgeRunner struct {
	bridge  *FixtureBridge
	id      journal.Identity
	inner   Runner
	started bool
}

func (r *bridgeRunner) Run(ctx context.Context, i Invocation) Process {
	p := r.bridge.plan
	if ctx.Err() != nil {
		return Process{ExitCode: -1, Err: journal.ErrUnknown}
	}
	_, proof, err := r.bridge.read(ctx)
	if err != nil {
		return Process{ExitCode: -1, Err: err}
	}
	plan := p.Preflight
	plan.Source = proof
	plan.Claim = r.id.Claim
	if err := r.bridge.preflight(ctx, plan); err != nil {
		return Process{ExitCode: -1, Err: err}
	}
	if record, ok, err := r.bridge.journal.FindFixture(r.id, time.Now()); err != nil || !ok || record.Stage != "INTENT" {
		return Process{ExitCode: -1, Err: journal.ErrUnknown}
	}
	i.Environment = []string{"PATH=/usr/bin:/bin", "HOME=" + plan.Environment["HOME"], "CODEX_HOME=" + plan.Environment["CODEX_HOME"], "TMPDIR=" + plan.Environment["TMPDIR"]}
	r.started = true
	return r.inner.Run(ctx, i)
}

func (b *FixtureBridge) deliver(ctx context.Context, id journal.Identity) error {
	_, proof, err := b.read(ctx)
	if err != nil {
		return err
	}
	id.Proof = proof
	if b.native != nil {
		if err := b.checkNative(ctx, proof, "result"); err != nil {
			return err
		}
	}
	record, err := b.journal.RecoverFixture(id, time.Now())
	if err != nil {
		return err
	}
	if record.Stage == "SYNC_CONFIRMED" {
		return nil
	}
	matching, empty, err := b.plan.Delivery.Observe(ctx, *record.Delivery, record.Payload)
	if ctx.Err() != nil {
		_ = b.journal.ReadbackFixture(id, nil, time.Now())
		return journal.ErrUnknown
	}
	if err == nil && matching {
		return b.journal.ReadbackFixture(id, record.Delivery, time.Now())
	}
	// After uncertainty, readback is the only automatic action, even if the slot looks empty.
	if record.Stage == "SYNC_UNKNOWN" || err != nil || !empty {
		_ = b.journal.ReadbackFixture(id, nil, time.Now())
		return journal.ErrUnknown
	}
	// Persist uncertainty BEFORE the possible external effect.
	if err := b.journal.ReadbackFixture(id, nil, time.Now()); err != journal.ErrUnknown {
		return err
	}
	if ctx.Err() != nil {
		return journal.ErrUnknown
	}
	_ = b.plan.Delivery.Replace(ctx, *record.Delivery, record.Payload)
	if ctx.Err() != nil {
		return journal.ErrUnknown
	}
	matching, _, err = b.plan.Delivery.Observe(ctx, *record.Delivery, record.Payload)
	if err != nil || !matching || ctx.Err() != nil {
		return journal.ErrUnknown
	}
	return b.journal.ReadbackFixture(id, record.Delivery, time.Now())
}

func (b *FixtureBridge) preflight(ctx context.Context, p FixturePreflight) error {
	if b.native != nil {
		return b.checkNative(ctx, p.Source, "spawn")
	}
	_, err := CheckFixturePreflight(p, b.plan.Observation, time.Now())
	return err
}
