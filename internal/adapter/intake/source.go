package intake

import (
	"context"
	"slices"

	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// GitSource supplies read-only applicability proof. It cannot grant claims, launch or acceptance.
type GitSource struct{ spec config.SourceSpec }

func NewGitSource(profile config.Profile, contract core.Contract, spec config.SourceSpec) (*GitSource, error) {
	if spec.ProjectID != profile.ProjectID || spec.ProjectID != contract.ProjectID || spec.Workspace != profile.WorkspaceRoot || spec.Workspace != contract.Workspace || spec.BriefDigest != contract.BriefDigest || core.Digest(spec.Brief) != contract.BriefDigest || (contract.FrozenSource != "" && contract.FrozenSource != spec.SourceSet) {
		return nil, config.ErrSource
	}
	spec.Inputs = slices.Clone(spec.Inputs)
	spec.CompatibilityInputs = slices.Clone(spec.CompatibilityInputs)
	spec.Receipts = slices.Clone(spec.Receipts)
	return &GitSource{spec: spec}, nil
}

func (source *GitSource) Check(ctx context.Context, taskBase string) (config.SourceProof, error) {
	if source == nil {
		return config.SourceProof{}, config.ErrSource
	}
	return config.VerifySource(ctx, source.spec, taskBase)
}

func (source *GitSource) Verify(ctx context.Context, taskBase string) (core.SourceSnapshot, error) {
	proof, err := source.Check(ctx, taskBase)
	if err != nil {
		return core.SourceSnapshot{}, err
	}
	return core.SourceSnapshot{Identity: proof.SourceSet, Compatibility: "compatible"}, nil
}
