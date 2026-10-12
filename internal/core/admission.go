package core

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"orchestrator-mcp/internal/config"
)

type Contract struct {
	TaskID, PageID, ProjectID, Workspace, Permission string
	Executor, Owner, AuthorityRef, BriefDigest       string
	FrozenSource                                     string
}

type TaskSnapshot struct {
	TaskID, PageID, Executor, Owner, Base, Brief, ClaimNonce string
	LeaseUntil                                               time.Time
	Eligible, DependenciesSatisfied, OwnershipKnown          bool
}

type SourceSnapshot struct {
	Identity, Compatibility string
}

type Claim struct {
	Owner, Nonce string
	LeaseUntil   time.Time
}

func Digest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// Admission checks supplied authority; a successful check never grants launch or lifecycle acceptance.
func Admission(profile config.Profile, contract Contract, task TaskSnapshot, source SourceSnapshot, now time.Time, claim *Claim) string {
	if reason := CheckContract(profile, contract, now); reason != "CONTRACT_VALID" {
		return reason
	}
	if !task.OwnershipKnown {
		return "OWNERSHIP_UNPROVEN"
	}
	if task.TaskID != contract.TaskID || task.PageID != contract.PageID {
		return "TASK_IDENTITY_MISMATCH"
	}
	if task.Executor != contract.Executor {
		return "ASSIGNMENT_MISMATCH"
	}
	if !task.Eligible {
		return "TASK_STATE_INELIGIBLE"
	}
	if task.Brief == "" || Digest(task.Brief) != contract.BriefDigest {
		return "BRIEF_CONTRACT_MISMATCH"
	}
	if !task.DependenciesSatisfied {
		return "DEPENDENCY_UNRESOLVED"
	}
	if source.Identity == "" || source.Compatibility == "unknown" || source.Compatibility == "" {
		return "SOURCE_UNPROVEN"
	}
	if source.Compatibility != "compatible" || (contract.FrozenSource != "" && contract.FrozenSource != source.Identity) {
		return "SOURCE_INCOMPATIBLE"
	}
	if claim == nil {
		// An agent label alone cannot identify the process that holds a live lease.
		if task.LeaseUntil.After(now) {
			return "LEASE_CONFLICT"
		}
	} else if claim.Nonce == "" || task.ClaimNonce != claim.Nonce || task.Owner != claim.Owner || claim.Owner != contract.Owner || !task.LeaseUntil.Equal(claim.LeaseUntil) || !task.LeaseUntil.After(now) {
		return "CLAIM_NOT_CONFIRMED"
	}
	return "ADMITTED"
}

func CheckContract(profile config.Profile, contract Contract, now time.Time) string {
	if contract.TaskID == "" || contract.PageID == "" || contract.ProjectID == "" || contract.Executor == "" || contract.Owner == "" || contract.AuthorityRef == "" || len(contract.BriefDigest) != 64 || now.IsZero() {
		return "AUTHORITY_UNPROVEN"
	}
	if contract.ProjectID != profile.ProjectID {
		return "PROJECT_MISMATCH"
	}
	plan := Plan(&profile, Request{TaskID: contract.TaskID, Workspace: contract.Workspace, Permission: contract.Permission})
	if !plan.ScopeAllowed {
		return plan.Reason
	}
	return "CONTRACT_VALID"
}
