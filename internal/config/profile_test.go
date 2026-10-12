package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProfiles(t *testing.T) {
	alpha, err := Load("../../testdata/projects/alpha.json")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := Load("../../testdata/projects/beta.json")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.ProjectID == beta.ProjectID || alpha.WorkspaceRoot == beta.WorkspaceRoot || len(alpha.Permissions) == len(beta.Permissions) {
		t.Fatal("fixtures must have distinct project IDs, roots and permissions")
	}
	if !filepath.IsAbs(alpha.WorkspaceRoot) || !filepath.IsAbs(beta.WorkspaceRoot) {
		t.Fatal("fixture roots must be normalized")
	}
}

func TestFIFOProfileRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO was not rejected: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writerless FIFO blocked profile loading")
	}
}

func TestProfileReplacementCannotBypassDescriptorCheck(t *testing.T) {
	dir := t.TempDir()
	regular, fifo, path := filepath.Join(dir, "regular.json"), filepath.Join(dir, "fifo"), filepath.Join(dir, "profile")
	if err := os.WriteFile(regular, []byte(`{"schema_version":1,"project_id":"regular","workspace_root":"."}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(regular, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	stop, writer := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			for _, target := range []string{regular, fifo} {
				select {
				case <-stop:
					writer <- nil
					return
				default:
				}
				pending := filepath.Join(dir, "replacement")
				if err := os.Symlink(target, pending); err != nil {
					writer <- err
					return
				}
				if err := os.Rename(pending, path); err != nil {
					writer <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-writer; err != nil {
			t.Error(err)
		}
	}()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 128; i++ {
			profile, err := Load(path)
			if err == nil && profile.ProjectID != "regular" {
				done <- fmt.Errorf("nonregular replacement accepted: %+v", profile)
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("profile replacement blocked loading")
	}
}

func TestRejectInvalidProfile(t *testing.T) {
	base := `{"schema_version":1,"project_id":"alpha","workspace_root":".","task_allowlist":["task"],"permissions":["read"]}`
	for name, data := range map[string]string{
		"empty":                "",
		"null":                 "null",
		"array":                "[]",
		"truncated":            `{"schema_version":1`,
		"trailing object":      base + `{}`,
		"trailing junk":        base + `x`,
		"unknown field":        strings.Replace(base, `"schema_version":1`, `"schema_version":1,"launch":true`, 1),
		"case alias":           strings.Replace(base, `"schema_version"`, `"Schema_Version"`, 1),
		"duplicate field":      strings.Replace(base, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"escaped duplicate":    strings.Replace(base, `"schema_version":1`, `"schema_version":1,"\u0073chema_version":1`, 1),
		"version":              strings.Replace(base, `"schema_version":1`, `"schema_version":2`, 1),
		"missing version":      strings.Replace(base, `"schema_version":1,`, ``, 1),
		"missing id":           strings.Replace(base, `"project_id":"alpha",`, ``, 1),
		"blank id":             strings.Replace(base, `"alpha"`, `" "`, 1),
		"wrong type":           strings.Replace(base, `["task"]`, `"task"`, 1),
		"task wildcard":        strings.Replace(base, `["task"]`, `["*"]`, 1),
		"permission wildcard":  strings.Replace(base, `["read"]`, `["read?"]`, 1),
		"duplicate task":       strings.Replace(base, `["task"]`, `["task","task"]`, 1),
		"duplicate permission": strings.Replace(base, `["read"]`, `["read","read"]`, 1),
		"empty task":           strings.Replace(base, `["task"]`, `[""]`, 1),
		"null task":            strings.Replace(base, `["task"]`, `[null]`, 1),
		"padded permission":    strings.Replace(base, `["read"]`, `["read "]`, 1),
		"control permission":   strings.Replace(base, `["read"]`, `["read\u0001"]`, 1),
		"invalid UTF-8":        strings.Replace(base, `alpha`, "a\xff", 1),
		"missing root":         strings.Replace(base, `"workspace_root":".",`, ``, 1),
		"empty root":           strings.Replace(base, `"workspace_root":"."`, `"workspace_root":""`, 1),
		"nonexistent root":     strings.Replace(base, `"workspace_root":"."`, `"workspace_root":"missing"`, 1),
		"filesystem root":      strings.Replace(base, `"workspace_root":"."`, `"workspace_root":"/"`, 1),
		"file root":            strings.Replace(base, `"workspace_root":"."`, `"workspace_root":"profile.json"`, 1),
		"oversize":             strings.Repeat(" ", 64*1024+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing profile accepted")
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("directory profile accepted")
	}
}

func TestMissingPermissionsDefaultDeny(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"project_id":"empty","workspace_root":"."}`), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.TaskAllowlist) != 0 || len(profile.Permissions) != 0 {
		t.Fatal("missing policy must not gain defaults")
	}
}

func TestWorkspaceSymlinks(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	want, err := ResolveWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWorkspace(link)
	if err != nil || got != want {
		t.Fatalf("alias = %q, %v; want %q", got, err, want)
	}
	for _, path := range []string{"", " ", "/", filepath.Join(root, "missing")} {
		if _, err := ResolveWorkspace(path); err == nil {
			t.Fatalf("invalid workspace %q accepted", path)
		}
	}
}
