package intake

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

var ErrAccess = errors.New("NOTION_ACCESS_UNAVAILABLE")
var ErrSchema = errors.New("NOTION_SCHEMA_UNSUPPORTED")
var ErrClaim = errors.New("CLAIM_WRITE_UNAUTHORIZED")

type Store interface {
	Read(context.Context, string) (core.TaskSnapshot, error)
	Claim(context.Context, string, core.Claim) error
}

type Source interface {
	Verify(context.Context, string) (core.SourceSnapshot, error)
}

type Result struct {
	Admitted         bool   `json:"admitted"`
	ExecutionStarted bool   `json:"execution_started"`
	Reason           string `json:"reason"`
	SourceIdentity   string `json:"source_identity,omitempty"`
	BriefDigest      string `json:"brief_digest,omitempty"`
}

func Admit(ctx context.Context, profile config.Profile, contract core.Contract, store Store, source Source, now func() time.Time) Result {
	result := Result{Reason: "AUTHORITY_UNPROVEN"}
	if store == nil || source == nil || now == nil {
		return result
	}
	// Deny before touching external coordination, including tasks outside the selected finite allowlist.
	if reason := core.CheckContract(profile, contract, now()); reason != "CONTRACT_VALID" {
		result.Reason = reason
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	task, err := store.Read(ctx, contract.PageID)
	if err != nil {
		result.Reason = readError(err)
		return result
	}
	before, err := source.Verify(ctx, task.Base)
	if err != nil {
		result.Reason = "SOURCE_UNPROVEN"
		return result
	}
	result.Reason = core.Admission(profile, contract, task, before, now(), nil)
	if result.Reason != "ADMITTED" {
		return result
	}
	claim := core.Claim{Owner: contract.Owner, Nonce: rand.Text(), LeaseUntil: now().Add(15 * time.Minute)}
	if err := store.Claim(ctx, contract.PageID, claim); err != nil {
		result.Reason = "CLAIM_WRITE_UNAUTHORIZED"
		return result
	}
	task, err = store.Read(ctx, contract.PageID)
	if err != nil {
		result.Reason = readError(err)
		return result
	}
	after, err := source.Verify(ctx, task.Base)
	if err != nil || before.Identity != after.Identity {
		result.Reason = "SOURCE_CHANGED_DURING_CLAIM"
		return result
	}
	result.Reason = core.Admission(profile, contract, task, after, now(), &claim)
	if result.Reason == "ADMITTED" {
		result.Admitted, result.SourceIdentity, result.BriefDigest = true, after.Identity, contract.BriefDigest
	}
	return result
}

func readError(err error) string {
	if errors.Is(err, ErrAccess) || errors.Is(err, ErrSchema) {
		return err.Error()
	}
	return "COORDINATION_UNAVAILABLE"
}
