package codex

import (
	"context"
	"encoding/json"
	"time"

	"orchestrator-mcp/internal/adapter/journal"
	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// LocalResult is private journal content; ordinary Result JSON keeps provider text out.
type LocalResult struct {
	Result  Result `json:"result"`
	Summary string `json:"summary"`
}

// NewLocalNativeBridge takes an explicitly host-approved local task, never Notion authority.
// Reader and Delivery must be nil; the native activation and launch grant stay mandatory.
func NewLocalNativeBridge(p BridgePlan, task core.TaskSnapshot, a NativeActivation) (*NativeBridge, error) {
	return newNativeBridge(p, a, &task)
}

func (b *FixtureBridge) deliveryDigest() string {
	if b.localTask == nil {
		return b.plan.Delivery.BindingDigest()
	}
	payload, _ := json.Marshal(struct {
		Operation, ControllerRoot string
		Contract                  core.Contract
		Task                      core.TaskSnapshot
	}{"local-result-store", b.plan.ControllerRoot, b.plan.Preflight.Request.Contract, *b.localTask})
	return core.Digest(string(payload))
}

func (b *FixtureBridge) identity(proof config.SourceProof) (journal.Identity, error) {
	r := b.plan.Preflight.Request
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, b.plan.AccessRef, proof)
	if err != nil {
		return journal.Identity{}, err
	}
	id := journal.Identity{Contract: r.Contract, Proof: proof, DeliveryTargetDigest: b.deliveryDigest(),
		Claim:    notion.ClaimState{Binding: binding, Owner: binding.Owner, Nonce: r.Grant.ClaimNonce, LeaseUntil: r.Grant.LeaseUntil},
		Deadline: time.Now().Add(r.Timeout)}
	if id.Claim.LeaseUntil.Before(id.Deadline) {
		id.Deadline = id.Claim.LeaseUntil
	}
	if b.native != nil {
		id.Deadline = b.native.Deadline
		id.ExecutionDigest = b.nativeLaunch().Digest()
	}
	return id, nil
}

// ReadResult returns only a copied durable local result. It never runs, writes or confirms delivery.
func (b *NativeBridge) ReadResult(ctx context.Context) (journal.Record, error) {
	ctx, cancel := context.WithDeadline(ctx, b.bridge.native.ReadbackDeadline)
	defer cancel()
	s := b.bridge
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localTask == nil {
		return journal.Record{}, ErrLivePending
	}
	_, proof, err := s.read(ctx)
	if err != nil {
		return journal.Record{}, err
	}
	if err := s.checkNative(ctx, proof, "result"); err != nil {
		return journal.Record{}, err
	}
	id, err := s.identity(proof)
	if err != nil {
		return journal.Record{}, err
	}
	saved, exists, err := s.journal.FindFixture(id, time.Now())
	if err != nil {
		return journal.Record{}, err
	}
	if !exists || saved.Stage == "INTENT" {
		return journal.Record{}, journal.ErrUnknown
	}
	saved.Identity.Proof = proof
	result, err := s.journal.RecoverFixture(saved.Identity, time.Now())
	if ctx.Err() != nil {
		return journal.Record{}, ctx.Err()
	}
	return result, err
}
