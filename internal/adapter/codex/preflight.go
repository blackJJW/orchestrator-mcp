package codex

import (
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
)

var ErrPreflight = errors.New("NATIVE_PREFLIGHT_UNPROVEN")
var ErrCleanup = errors.New("PROCESS_CLEANUP_UNPROVEN")

type NativeIdentity struct{ Path, SHA256, Version string }

// FixturePreflight checks task-owned fixtures only; it never creates a launch Grant.
type FixturePreflight struct {
	Request     Request
	Claim       notion.ClaimState
	Source      config.SourceProof
	Native      NativeIdentity
	Environment map[string]string
}

// Isolation and sandbox observations are fixture inputs, not operating-system attestations.
type NativeObservation struct {
	Native                                   NativeIdentity
	Workspace                                string
	Environment                              map[string]string
	Isolated, ReadOnlySandbox, NetworkDenied bool
}

// CheckFixturePreflight returns a fixture deadline, never a production execution permission.
func CheckFixturePreflight(p FixturePreflight, observed NativeObservation, now time.Time) (time.Time, error) {
	r, state := p.Request, p.Claim
	root, err := config.ResolveWorkspace(r.Contract.Workspace)
	if err != nil || root != r.Contract.Workspace || root != r.Profile.WorkspaceRoot {
		return time.Time{}, ErrPreflight
	}
	binding, err := notion.BindClaim(r.Profile, r.Contract, r.RunID, state.Binding.AccessRef, p.Source)
	g := r.Grant
	if err != nil || now.IsZero() || binding != state.Binding || !state.Consumed || state.Owner != binding.Owner ||
		state.Fence == 0 || !runID.MatchString(state.Nonce) || !state.LeaseUntil.After(now) ||
		g == nil || g.Reference == "" || g.TaskID != binding.TaskID || g.RunID != binding.RunID ||
		g.Workspace != binding.Workspace || g.SourceIdentity != binding.Source || g.BriefDigest != binding.BriefDigest ||
		g.ClaimNonce != state.Nonce || !g.LeaseUntil.Equal(state.LeaseUntil) || r.Contract.Permission != "source.read" ||
		r.Timeout <= 0 || r.Timeout > 5*time.Minute {
		return time.Time{}, ErrPreflight
	}
	if observed.Native != p.Native || observed.Workspace != root || p.Native.Version != "0.161.0" ||
		!fixtureHash(p.Native.SHA256) || !fixturePath(root, p.Native.Path) || filepath.Base(p.Native.Path) != "codex" ||
		!observed.Isolated || !observed.ReadOnlySandbox || !observed.NetworkDenied ||
		len(p.Environment) != 3 || !maps.Equal(p.Environment, observed.Environment) {
		return time.Time{}, ErrPreflight
	}
	for _, name := range []string{"HOME", "CODEX_HOME", "TMPDIR"} {
		path := p.Environment[name]
		if !fixturePath(root, path) {
			return time.Time{}, ErrPreflight
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			return time.Time{}, ErrPreflight
		}
	}
	if _, err := NewNative(p.Native.Path, p.Native.SHA256); err != nil {
		return time.Time{}, ErrPreflight
	}
	deadline := now.Add(r.Timeout)
	if state.LeaseUntil.Before(deadline) {
		deadline = state.LeaseUntil
	}
	return deadline, nil
}

func fixturePath(root, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) || resolved != path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fixtureHash(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && len(value) == 64 && value == strings.ToLower(value)
}

// ClaimKey is the S1 result identity key binding workspace/task/run/owner/nonce/fence/source.
type FixtureProcessIdentity struct {
	TaskID, RunID, ClaimKey, BirthToken, ExecutableSHA string
	PID, PGID                                          int
}

type FixtureCleanup struct {
	Identity          FixtureProcessIdentity
	Complete, Unknown bool
	Remaining         []FixtureProcessIdentity
}

// CheckFixtureCleanup models readback only: no process lookup, signal, retry or recovery.
func CheckFixtureCleanup(expected FixtureProcessIdentity, observed FixtureCleanup) error {
	if expected.TaskID == "" || !runID.MatchString(expected.RunID) || !runID.MatchString(expected.BirthToken) ||
		!fixtureHash(expected.ClaimKey) || !fixtureHash(expected.ExecutableSHA) || expected.PID <= 0 || expected.PGID != expected.PID ||
		observed.Identity != expected || !observed.Complete || observed.Unknown || len(observed.Remaining) != 0 {
		return ErrCleanup
	}
	return nil
}
