package notion

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

var ErrClaimUnknown = errors.New("CLAIM_EFFECT_UNKNOWN")
var ErrResult = errors.New("RESULT_UNCONFIRMED")

// ClaimBinding and ClaimState describe a conditional writer contract, not a live writer.
type ClaimBinding struct {
	ProjectID, Workspace, TaskID, PageID, RunID, AccessRef, Owner string
	Commit, Tree, Source, BriefDigest, ReceiptDigest              string
}

type ClaimState struct {
	Binding      ClaimBinding
	Owner, Nonce string
	Fence        uint64
	LeaseUntil   time.Time
	Consumed     bool
}

func BindClaim(profile config.Profile, contract core.Contract, runID, accessRef string, proof config.SourceProof) (ClaimBinding, error) {
	if core.CheckContract(profile, contract, time.Now()) != "CONTRACT_VALID" ||
		!uuid.MatchString(contract.PageID) || !claimText(runID) || !claimText(accessRef) ||
		!claimHash(proof.Commit, 40) || !claimHash(proof.Tree, 40) || !claimHash(proof.SourceSet, 64) ||
		!claimHash(proof.ReviewBase, 40) || !claimHash(proof.CurrentBase, 40) ||
		proof.BriefDigest != contract.BriefDigest || proof.SourceSet != contract.FrozenSource || len(proof.Receipts) != 2 {
		return ClaimBinding{}, intake.ErrClaim
	}
	for _, pin := range proof.Receipts {
		if !strings.HasPrefix(pin.Path, ".local/") || !claimHash(pin.SHA256, 64) {
			return ClaimBinding{}, intake.ErrClaim
		}
	}
	pins, _ := json.Marshal(proof.Receipts)
	return ClaimBinding{profile.ProjectID, contract.Workspace, contract.TaskID, contract.PageID, runID, accessRef, contract.Owner,
		proof.Commit, proof.Tree, proof.SourceSet, proof.BriefDigest, core.Digest(string(pins))}, nil
}

// ClaimTransition must be checked and committed in one authoritative conditional operation.
// Only the test-local writer supplies that operation; production Claim stays denied.
func ClaimTransition(binding ClaimBinding, state ClaimState, expectedNonce string, expectedFence uint64, claim core.Claim, proof config.SourceProof, now time.Time) (ClaimState, error) {
	pins, _ := json.Marshal(proof.Receipts)
	if now.IsZero() || !validBinding(binding) || state.Binding != binding ||
		proof.Commit != binding.Commit || proof.Tree != binding.Tree || proof.SourceSet != binding.Source ||
		proof.BriefDigest != binding.BriefDigest || core.Digest(string(pins)) != binding.ReceiptDigest ||
		state.Fence != expectedFence || state.Nonce != expectedNonce || state.Fence == math.MaxUint64 ||
		state.Consumed || state.LeaseUntil.After(now) || (state.Owner != "Unclaimed" && state.LeaseUntil.IsZero()) ||
		claim.Owner != binding.Owner || !claimText(claim.Nonce) || claim.Nonce == state.Nonce ||
		!claim.LeaseUntil.After(now) || claim.LeaseUntil.After(now.Add(15*time.Minute)) {
		return ClaimState{}, intake.ErrClaim
	}
	return ClaimState{binding, claim.Owner, claim.Nonce, state.Fence + 1, claim.LeaseUntil, true}, nil
}

// ResultHandoff is an immutable delivery/readback contract. It performs no write or retry.
type ResultHandoff struct {
	Binding                       ClaimBinding
	ClaimNonce                    string
	Fence                         uint64
	Operation, Key, PayloadDigest string
}

func NewResultHandoff(state ClaimState, payload []byte) (ResultHandoff, error) {
	if !validBinding(state.Binding) || !state.Consumed || state.Owner != state.Binding.Owner || !claimText(state.Nonce) || state.Fence == 0 ||
		!claimHash(state.Binding.Source, 64) || !claimHash(state.Binding.BriefDigest, 64) ||
		len(payload) == 0 || len(payload) > 1024*1024 {
		return ResultHandoff{}, ErrResult
	}
	identity, _ := json.Marshal(struct {
		Binding ClaimBinding
		Nonce   string
		Fence   uint64
	}{state.Binding, state.Nonce, state.Fence})
	return ResultHandoff{state.Binding, state.Nonce, state.Fence, "conditional-replace", core.Digest(string(identity)), core.Digest(string(payload))}, nil
}

func ConfirmResultReadback(expected, actual ResultHandoff) error {
	identity, _ := json.Marshal(struct {
		Binding ClaimBinding
		Nonce   string
		Fence   uint64
	}{expected.Binding, expected.ClaimNonce, expected.Fence})
	if expected != actual || !validBinding(expected.Binding) || !claimText(expected.ClaimNonce) || expected.Fence == 0 ||
		expected.Key != core.Digest(string(identity)) || expected.Operation != "conditional-replace" || !claimHash(expected.Key, 64) ||
		!claimHash(expected.PayloadDigest, 64) {
		return ErrResult
	}
	return nil
}

func validBinding(b ClaimBinding) bool {
	root, err := config.ResolveWorkspace(b.Workspace)
	return err == nil && root == b.Workspace && claimText(b.ProjectID) && claimText(b.TaskID) &&
		uuid.MatchString(b.PageID) && claimText(b.RunID) && claimText(b.AccessRef) && claimText(b.Owner) &&
		claimHash(b.Commit, 40) && claimHash(b.Tree, 40) && claimHash(b.Source, 64) &&
		claimHash(b.BriefDigest, 64) && claimHash(b.ReceiptDigest, 64)
}

func claimText(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

func claimHash(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
