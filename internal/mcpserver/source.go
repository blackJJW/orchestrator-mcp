package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// These are host inputs, never MCP arguments or evidence-derived expectations.
type sourceBinding struct {
	Contract   core.Contract     `json:"contract"`
	SourceSpec config.SourceSpec `json:"source_spec"`
	TaskBase   string            `json:"task_base"`
}

type sourceInput struct {
	TaskID string `json:"task_id"`
}

type sourceProof struct {
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	SourceSet   string `json:"source_set"`
	ReviewBase  string `json:"review_base"`
	CurrentBase string `json:"current_base"`
	BriefDigest string `json:"brief_digest"`
}

type sourceResult struct {
	SourceVerified   bool         `json:"source_verified"`
	Reason           string       `json:"reason"`
	ExecutionStarted bool         `json:"execution_started"`
	Proof            *sourceProof `json:"proof"`
}

func verifySource(ctx context.Context, profile config.Profile, binding sourceBinding, taskID string) sourceResult {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := sourceResult{Reason: "SOURCE_UNPROVEN"}
	decision := core.Plan(&profile, core.Request{TaskID: taskID, Workspace: profile.WorkspaceRoot, Permission: "source.read"})
	if !decision.ScopeAllowed {
		result.Reason = decision.Reason
		return result
	}
	if taskID != binding.Contract.TaskID {
		return result
	}
	source, err := intake.NewGitSource(profile, binding.Contract, binding.SourceSpec)
	if err != nil {
		return result
	}
	proof, err := source.Check(ctx, binding.TaskBase)
	if ctx.Err() != nil {
		return result
	}
	if errors.Is(err, config.ErrCompatibility) {
		result.Reason = "SOURCE_INCOMPATIBLE"
	}
	if err != nil {
		return result
	}
	result.SourceVerified, result.Reason = true, "SOURCE_VERIFIED"
	result.Proof = &sourceProof{proof.Commit, proof.Tree, proof.SourceSet, proof.ReviewBase, proof.CurrentBase, proof.BriefDigest}
	return result
}

func loadSourceBinding(profile config.Profile, path string) (*sourceBinding, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, config.ErrSource
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, config.ErrSource
	}
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 || !utf8.Valid(data) {
		return nil, config.ErrSource
	}
	fields, err := bindingObject(data, "contract", "source_spec", "task_base")
	if err != nil || !bindingStrings(fields, "task_base") {
		return nil, config.ErrSource
	}
	contractFields := []string{"TaskID", "PageID", "ProjectID", "Workspace", "Permission", "Executor", "Owner", "AuthorityRef", "BriefDigest", "FrozenSource"}
	contract, err := bindingObject(fields["contract"], contractFields...)
	if err != nil || !bindingStrings(contract, contractFields...) {
		return nil, config.ErrSource
	}
	specFields := []string{"Workspace", "ProjectID", "CandidateRef", "BaseRef", "Head", "Tree", "SourceSet", "ReviewBase", "Brief", "BriefDigest"}
	spec, err := bindingObject(fields["source_spec"], append(specFields, "Inputs", "CompatibilityInputs", "Receipts")...)
	if err != nil || !bindingStrings(spec, specFields...) {
		return nil, config.ErrSource
	}
	for _, name := range []string{"Inputs", "CompatibilityInputs"} {
		var paths []json.RawMessage
		if json.Unmarshal(spec[name], &paths) != nil || len(paths) == 0 {
			return nil, config.ErrSource
		}
		for _, path := range paths {
			if !bindingStrings(map[string]json.RawMessage{"path": path}, "path") {
				return nil, config.ErrSource
			}
		}
	}
	var receipts []json.RawMessage
	if json.Unmarshal(spec["Receipts"], &receipts) != nil || len(receipts) != 2 {
		return nil, config.ErrSource
	}
	for _, receipt := range receipts {
		pin, err := bindingObject(receipt, "path", "sha256")
		if err != nil || !bindingStrings(pin, "path", "sha256") {
			return nil, config.ErrSource
		}
	}
	var binding sourceBinding
	if json.Unmarshal(data, &binding) != nil || binding.Contract.Permission != "source.read" {
		return nil, config.ErrSource
	}
	// Reuse S2 for startup validity; every call still checks fresh bytes and receipts.
	if !verifySource(context.Background(), profile, binding, binding.Contract.TaskID).SourceVerified {
		return nil, config.ErrSource
	}
	return &binding, nil
}

// Exact keys and cardinality prevent JSON duplicate/case aliases at this startup boundary.
func bindingObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, config.ErrSource
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok || fields[name] != nil {
			return nil, config.ErrSource
		}
		found := false
		for _, allowed := range names {
			found = found || name == allowed
		}
		if !found {
			return nil, config.ErrSource
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, config.ErrSource
		}
		fields[name] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') || len(fields) != len(names) || d.Decode(new(any)) != io.EOF {
		return nil, config.ErrSource
	}
	return fields, nil
}

func bindingStrings(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		var value *string
		if json.Unmarshal(fields[name], &value) != nil || value == nil || *value == "" || (name != "Brief" && (strings.TrimSpace(*value) != *value || strings.ContainsAny(*value, "\x00\r\n\t"))) {
			return false
		}
	}
	return true
}
