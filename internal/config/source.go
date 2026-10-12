package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"
)

var ErrSource = errors.New("SOURCE_UNPROVEN")
var ErrCompatibility = errors.New("SOURCE_INCOMPATIBLE")

// EvidencePin is supplied by the trusted host, never derived from the receipt being checked.
type EvidencePin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type SourceSpec struct {
	Workspace, ProjectID, CandidateRef, BaseRef string
	Head, Tree, SourceSet, ReviewBase           string
	Brief, BriefDigest                          string
	Inputs, CompatibilityInputs                 []string
	Receipts                                    []EvidencePin
}

// SourceReceipt is a pinned projection of canonical/review evidence, not a new validation runner.
type SourceReceipt struct {
	Version     int               `json:"version"`
	Kind        string            `json:"kind"`
	Result      string            `json:"result"`
	ProjectID   string            `json:"project_id"`
	Base        string            `json:"base"`
	Binding     string            `json:"binding"`
	Head        string            `json:"head"`
	Tree        string            `json:"tree"`
	SourceSet   string            `json:"source_set"`
	BriefDigest string            `json:"brief_digest"`
	Inputs      map[string]string `json:"inputs"`
	Coverage    []string          `json:"coverage"`
	Artifacts   []EvidencePin     `json:"artifacts"`
}

type SourceProof struct {
	Commit, Tree, SourceSet, ReviewBase, CurrentBase, BriefDigest string
	Receipts                                                      []EvidencePin
}

// VerifySource reads only a standalone, complete local Git checkout. No fetch, filters or shell.
func VerifySource(ctx context.Context, spec SourceSpec, taskBase string) (SourceProof, error) {
	var proof SourceProof
	root, err := ResolveWorkspace(spec.Workspace)
	if err != nil || root != spec.Workspace || !validID(spec.Head, 40) || !validID(spec.Tree, 40) || !validID(spec.ReviewBase, 40) || !validID(taskBase, 40) || !validID(spec.SourceSet, 64) || !validID(spec.BriefDigest, 64) || sourceDigest([]byte(spec.Brief)) != spec.BriefDigest || spec.Brief == "" || !exactValue(spec.ProjectID) {
		return proof, ErrSource
	}
	if !validRef(spec.CandidateRef) || !validRef(spec.BaseRef) || len(spec.Inputs) == 0 || len(spec.Inputs) > 256 || len(spec.Receipts) != 2 || len(spec.CompatibilityInputs) == 0 {
		return proof, ErrSource
	}
	inputs := make(map[string]bool)
	for _, path := range spec.Inputs {
		if !sourcePath(path) || strings.HasPrefix(path, ".git/") || strings.HasPrefix(path, ".local/") || path == ".git" || path == ".local" || inputs[path] {
			return proof, ErrSource
		}
		inputs[path] = true
	}
	compatibility := make(map[string]bool)
	for _, path := range spec.CompatibilityInputs {
		if !inputs[path] || compatibility[path] {
			return proof, ErrSource
		}
		compatibility[path] = true
	}
	for path := range inputs {
		if (strings.HasSuffix(path, ".go") || path == "go.mod" || path == "go.sum" || path == "Makefile") && !compatibility[path] {
			return proof, ErrSource
		}
	}
	if safeRepository(ctx, root) != nil {
		return proof, ErrSource
	}
	base, err := gitRead(ctx, root, "rev-parse", "--verify", spec.BaseRef)
	if err != nil || !validID(base, 40) || !ancestor(ctx, root, spec.ReviewBase, spec.Head) {
		return proof, ErrSource
	}
	for _, advanced := range []string{taskBase, base} {
		if advanced == spec.ReviewBase {
			continue
		}
		comparison := spec.ReviewBase
		if ancestor(ctx, root, spec.Head, advanced) {
			comparison = spec.Head
		} else if !ancestor(ctx, root, spec.ReviewBase, advanced) {
			return proof, ErrCompatibility
		}
		listing, err := gitRead(ctx, root, "ls-tree", "-r", "-z", "--full-tree", advanced)
		if err != nil {
			return proof, ErrCompatibility
		}
		for _, item := range strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00") {
			parts := strings.SplitN(item, "\t", 2)
			if len(parts) != 2 || !inputs[parts[1]] {
				return proof, ErrCompatibility
			}
		}
		for path := range compatibility {
			old, e1 := gitRead(ctx, root, "ls-tree", comparison, "--", path)
			next, e2 := gitRead(ctx, root, "ls-tree", advanced, "--", path)
			if e1 != nil || e2 != nil || old != next {
				return proof, ErrCompatibility
			}
		}
	}
	actual, err := sourceInputs(ctx, spec)
	if err != nil {
		return proof, err
	}
	kinds := make(map[string]bool)
	pins := make(map[string]string)
	for _, pin := range spec.Receipts {
		data, err := pinnedFile(root, pin, pins, 64*1024)
		if err != nil {
			return proof, ErrSource
		}
		var receipt SourceReceipt
		if decodeReceipt(data, &receipt) != nil || receipt.Version != 1 || receipt.ProjectID != spec.ProjectID || receipt.Base != spec.ReviewBase || receipt.Tree != spec.Tree || receipt.SourceSet != spec.SourceSet || receipt.BriefDigest != spec.BriefDigest || !reflect.DeepEqual(receipt.Inputs, actual) || len(receipt.Artifacts) == 0 || len(receipt.Artifacts) > 32 || kinds[receipt.Kind] {
			return proof, ErrSource
		}
		switch receipt.Kind {
		case "canonical":
			if receipt.Result != "PASS" || !slices.Contains(receipt.Coverage, "make check") {
				return proof, ErrSource
			}
		case "review":
			if receipt.Result != "ACCEPTED" || !slices.Contains(receipt.Coverage, "independent-review") {
				return proof, ErrSource
			}
		default:
			return proof, ErrSource
		}
		kinds[receipt.Kind] = true
		switch receipt.Binding {
		case "commit":
			if !validID(receipt.Head, 40) || !ancestor(ctx, root, receipt.Head, spec.Head) {
				return proof, ErrSource
			}
			tree, err := gitRead(ctx, root, "rev-parse", "--verify", receipt.Head+"^{tree}")
			if err != nil || tree != spec.Tree {
				return proof, ErrSource
			}
		case "source":
			// Trusted pre-commit evidence applies only to this identical final tree/input/config set.
			if receipt.Head != "" {
				return proof, ErrSource
			}
		default:
			return proof, ErrSource
		}
		for _, artifact := range receipt.Artifacts {
			if _, err := pinnedFile(root, artifact, pins, 4*1024*1024); err != nil {
				return proof, ErrSource
			}
		}
	}
	// Re-read identities and evidence before returning; intake repeats verification around its claim.
	end, err := sourceInputs(ctx, spec)
	endBase, baseErr := gitRead(ctx, root, "rev-parse", "--verify", spec.BaseRef)
	if err != nil || baseErr != nil || base != endBase || !reflect.DeepEqual(actual, end) || safeRepository(ctx, root) != nil {
		return proof, ErrSource
	}
	for path, hash := range pins {
		data, err := sourceFile(root, path, 4*1024*1024)
		if err != nil || sourceDigest(data) != hash {
			return proof, ErrSource
		}
	}
	return SourceProof{Commit: spec.Head, Tree: spec.Tree, SourceSet: spec.SourceSet, ReviewBase: spec.ReviewBase, CurrentBase: base, BriefDigest: spec.BriefDigest, Receipts: slices.Clone(spec.Receipts)}, nil
}

func sourceInputs(ctx context.Context, spec SourceSpec) (map[string]string, error) {
	for _, ref := range []string{"HEAD", spec.CandidateRef} {
		value, err := gitRead(ctx, spec.Workspace, "rev-parse", "--verify", ref)
		if err != nil || value != spec.Head {
			return nil, ErrSource
		}
	}
	tree, err := gitRead(ctx, spec.Workspace, "rev-parse", "--verify", spec.Head+"^{tree}")
	if err != nil || tree != spec.Tree {
		return nil, ErrSource
	}
	listing, err := gitRead(ctx, spec.Workspace, "ls-tree", "-r", "-z", "--full-tree", spec.Head)
	if err != nil {
		return nil, ErrSource
	}
	allowed := make(map[string]bool)
	for _, path := range spec.Inputs {
		allowed[path] = true
	}
	actual := make(map[string]string)
	for _, item := range strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00") {
		parts := strings.SplitN(item, "\t", 2)
		if len(parts) != 2 {
			return nil, ErrSource
		}
		meta, path := strings.Fields(parts[0]), parts[1]
		if len(meta) != 3 || meta[1] != "blob" || (meta[0] != "100644" && meta[0] != "100755") || !validID(meta[2], 40) || !allowed[path] || actual[path] != "" {
			return nil, ErrSource
		}
		data, err := sourceFile(spec.Workspace, path, 4*1024*1024)
		if err != nil {
			return nil, ErrSource
		}
		info, err := os.Lstat(filepath.Join(spec.Workspace, path))
		if err != nil || (info.Mode()&0111 != 0) != (meta[0] == "100755") {
			return nil, ErrSource
		}
		blob, err := gitRead(ctx, spec.Workspace, "cat-file", "blob", meta[2])
		if err != nil || sourceDigest(data) != sourceDigest([]byte(blob)) {
			return nil, ErrSource
		}
		actual[path] = sourceDigest(data)
	}
	if len(actual) != len(allowed) {
		return nil, ErrSource
	}
	data, _ := json.Marshal(actual)
	if sourceDigest(data) != spec.SourceSet {
		return nil, ErrSource
	}
	// Ignored .local evidence is not a source input. Every other file must be approved and tracked.
	err = filepath.WalkDir(spec.Workspace, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return ErrSource
		}
		rel, err := filepath.Rel(spec.Workspace, path)
		if err != nil {
			return ErrSource
		}
		if rel == ".git" || rel == ".local" {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return ErrSource
			}
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !allowed[filepath.ToSlash(rel)]) {
			return ErrSource
		}
		return nil
	})
	if err != nil {
		return nil, ErrSource
	}
	return actual, nil
}

func safeRepository(ctx context.Context, root string) error {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrSource
	}
	for _, path := range []string{".git/objects/info/alternates", ".git/info/grafts", ".git/shallow", ".git/refs/replace"} {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			return ErrSource
		}
	}
	count := 0
	if filepath.WalkDir(filepath.Join(root, ".git"), func(_ string, e os.DirEntry, err error) error {
		count++
		if err != nil || e.Type()&os.ModeSymlink != 0 || (!e.IsDir() && !e.Type().IsRegular()) || count > 10000 {
			return ErrSource
		}
		return nil
	}) != nil {
		return ErrSource
	}
	config, err := gitRead(ctx, root, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return ErrSource
	}
	for _, field := range strings.Split(strings.TrimSuffix(config, "\x00"), "\x00") {
		kv := strings.SplitN(field, "\n", 2)
		if len(kv) != 2 {
			return ErrSource
		}
		key := kv[0]
		switch key {
		case "core.repositoryformatversion", "core.filemode", "core.logallrefupdates", "core.ignorecase", "core.precomposeunicode", "remote.origin.url", "remote.origin.fetch", "user.name", "user.email":
		case "core.bare":
			if kv[1] != "false" {
				return ErrSource
			}
		default:
			if !strings.HasPrefix(key, "branch.") || (!strings.HasSuffix(key, ".remote") && !strings.HasSuffix(key, ".merge")) {
				return ErrSource
			}
		}
	}
	return nil
}

func ancestor(ctx context.Context, root, base, head string) bool {
	if !validID(base, 40) || !validID(head, 40) {
		return false
	}
	_, err := gitRead(ctx, root, "merge-base", "--is-ancestor", base, head)
	return err == nil
}

func gitRead(ctx context.Context, root string, args ...string) (string, error) {
	info, err := os.Lstat("/usr/bin/git")
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", ErrSource
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = root
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1"}
	cmd.WaitDelay = 200 * time.Millisecond
	var output sourceOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return "", ErrSource
	}
	end, err := os.Lstat("/usr/bin/git")
	if err != nil || !os.SameFile(info, end) || info.Size() != end.Size() || !info.ModTime().Equal(end.ModTime()) {
		return "", ErrSource
	}
	// cat-file returns raw bytes; trimming would silently change the source identity.
	if len(args) > 0 && args[0] == "cat-file" {
		return output.String(), nil
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

type sourceOutput struct{ buffer bytes.Buffer }

func (b *sourceOutput) String() string { return b.buffer.String() }

func (b *sourceOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 4*1024*1024 {
		return 0, ErrSource
	}
	return b.buffer.Write(p)
}

func sourceFile(root, path string, limit int64) ([]byte, error) {
	if !sourcePath(path) {
		return nil, ErrSource
	}
	current := root
	for _, part := range strings.Split(path, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrSource
		}
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrSource
	}
	defer directory.Close()
	file, err := directory.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrSource
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrSource
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrSource
	}
	end, err := os.Lstat(current)
	if err != nil || !os.SameFile(info, end) || info.Size() != end.Size() || !info.ModTime().Equal(end.ModTime()) {
		return nil, ErrSource
	}
	return data, nil
}

func pinnedFile(root string, pin EvidencePin, seen map[string]string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(pin.Path, ".local/") || !sourcePath(pin.Path) || !validID(pin.SHA256, 64) || seen[pin.Path] != "" {
		return nil, ErrSource
	}
	data, err := sourceFile(root, pin.Path, limit)
	if err != nil || sourceDigest(data) != pin.SHA256 {
		return nil, ErrSource
	}
	seen[pin.Path] = pin.SHA256
	return data, nil
}

func validID(id string, length int) bool {
	if len(id) != length || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validRef(ref string) bool {
	return strings.HasPrefix(ref, "refs/heads/") && sourcePath(ref)
}

func sourcePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsAny(path, "\\\x00\n\r\t :*?[") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func sourceDigest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func decodeReceipt(data []byte, receipt *SourceReceipt) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var visit func() error
	visit = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if token == json.Delim('{') {
			seen := make(map[string]bool)
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return ErrSource
				}
				seen[name] = true
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		} else if token == json.Delim('[') {
			for d.More() {
				if err := visit(); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if visit() != nil || d.Decode(new(any)) != io.EOF {
		return ErrSource
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 13 {
		return ErrSource
	}
	for _, name := range []string{"version", "kind", "result", "project_id", "base", "binding", "head", "tree", "source_set", "brief_digest", "inputs", "coverage", "artifacts"} {
		if fields[name] == nil {
			return ErrSource
		}
	}
	for _, name := range []string{"kind", "result", "project_id", "base", "binding", "head", "tree", "source_set", "brief_digest"} {
		var value any
		if json.Unmarshal(fields[name], &value) != nil {
			return ErrSource
		}
		if _, ok := value.(string); !ok {
			return ErrSource
		}
	}
	var coverage []any
	if json.Unmarshal(fields["coverage"], &coverage) != nil || len(coverage) == 0 {
		return ErrSource
	}
	for _, item := range coverage {
		if text, ok := item.(string); !ok || text == "" {
			return ErrSource
		}
	}
	// Nested typed keys are exact too; the standard decoder otherwise accepts case aliases.
	var artifacts []map[string]json.RawMessage
	if json.Unmarshal(fields["artifacts"], &artifacts) != nil {
		return ErrSource
	}
	for _, artifact := range artifacts {
		if len(artifact) != 2 || artifact["path"] == nil || artifact["sha256"] == nil {
			return ErrSource
		}
		for _, name := range []string{"path", "sha256"} {
			var value any
			if json.Unmarshal(artifact[name], &value) != nil {
				return ErrSource
			}
			if _, ok := value.(string); !ok {
				return ErrSource
			}
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(receipt) != nil {
		return ErrSource
	}
	return nil
}
