package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const SchemaVersion = 1

type Profile struct {
	SchemaVersion int      `json:"schema_version"`
	ProjectID     string   `json:"project_id"`
	WorkspaceRoot string   `json:"workspace_root"`
	TaskAllowlist []string `json:"task_allowlist"`
	Permissions   []string `json:"permissions"`
}

func Load(path string) (Profile, error) {
	var profile Profile
	// Validate the opened descriptor; a replacement FIFO must neither block nor pass the type check.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return profile, fmt.Errorf("read profile: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return profile, errors.New("profile must be a regular file")
	}
	// Bound input before decoding; profiles contain policy, never credential material.
	data, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return profile, errors.New("profile unreadable or larger than 64 KiB")
	}
	if !utf8.Valid(data) {
		return profile, errors.New("profile must be UTF-8")
	}
	if err := uniqueFields(data); err != nil {
		return profile, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return profile, fmt.Errorf("invalid profile: %w", err)
	}
	if profile.SchemaVersion != SchemaVersion {
		return profile, errors.New("schema_version must be 1")
	}
	if !exactValue(profile.ProjectID) {
		return profile, errors.New("project_id must be a nonempty exact identifier")
	}
	for name, values := range map[string][]string{"task_allowlist": profile.TaskAllowlist, "permissions": profile.Permissions} {
		seen := make(map[string]bool)
		for _, value := range values {
			if !exactValue(value) || seen[value] {
				return profile, fmt.Errorf("%s must contain unique exact values", name)
			}
			seen[value] = true
		}
	}
	if strings.TrimSpace(profile.WorkspaceRoot) == "" {
		return profile, errors.New("workspace_root is required")
	}
	root := profile.WorkspaceRoot
	if !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(path), root)
	}
	profile.WorkspaceRoot, err = ResolveWorkspace(root)
	if err != nil {
		return profile, fmt.Errorf("workspace_root: %w", err)
	}
	return profile, nil
}

func ResolveWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace is required")
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("workspace must exist")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || filepath.Dir(root) == root {
		return "", errors.New("workspace must be a project directory, not a filesystem root")
	}
	return root, nil
}

func exactValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "*?") && strings.IndexFunc(value, unicode.IsControl) == -1
}

func uniqueFields(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("profile must be a JSON object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid profile field")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate or invalid profile field")
		}
		switch key {
		case "schema_version", "project_id", "workspace_root", "task_allowlist", "permissions":
		default:
			return errors.New("unknown profile field (names are case-sensitive)")
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return errors.New("invalid profile value")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return errors.New("unterminated profile object")
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("profile must contain exactly one JSON object")
	}
	return nil
}
