package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"orchestrator-mcp/internal/adapter/intake"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

// Grant is supplied by the trusted host, never inferred from a Notion task or model output.
type Grant struct {
	Reference, TaskID, RunID, Workspace, SourceIdentity, BriefDigest, ClaimNonce string
	LeaseUntil                                                                   time.Time
}

type Request struct {
	Profile  config.Profile
	Contract core.Contract
	RunID    string
	Timeout  time.Duration
	Grant    *Grant
	Store    intake.Store
	Source   intake.Source
}

type Invocation struct {
	Environment []string
	Args        []string
	Dir, Prompt string
}
type Process struct {
	PID, ExitCode  int
	Stdout, Stderr []byte
	Err            error
	Overflow       bool
}
type Runner interface {
	Run(context.Context, Invocation) Process
}

type Executor struct {
	runner Runner
	mode   string
}

type executionIdentity struct{ workspace, task, id string }

var dispatchMu sync.Mutex
var dispatchedRuns = map[executionIdentity]bool{}
var dispatchedClaims = map[executionIdentity]bool{}

// Retain reservations across Executor instances and failures; retries cannot reacquire them.
func reserve(g Grant) bool {
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	run := executionIdentity{g.Workspace, g.TaskID, g.RunID}
	claim := executionIdentity{g.Workspace, g.TaskID, g.ClaimNonce}
	if dispatchedRuns[run] || dispatchedClaims[claim] {
		return false
	}
	dispatchedRuns[run], dispatchedClaims[claim] = true, true
	return true
}

// NewFake only accepts a host-owned fixture runner; it never falls back to the official CLI.
func NewFake(runner Runner) *Executor { return &Executor{runner: runner, mode: "fake"} }

func NewNative(executable, expectedSHA string) (*Executor, error) {
	path, err := filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(path) || filepath.Base(path) != "codex" || !pinned(path, expectedSHA) {
		return nil, os.ErrPermission
	}
	return &Executor{runner: nativeRunner{path: path, sha: expectedSHA}, mode: "native"}, nil
}

type Result struct {
	SchemaVersion  int    `json:"schema_version"`
	TaskID         string `json:"task_id"`
	RunID          string `json:"run_id"`
	Mode           string `json:"mode"`
	Outcome        string `json:"outcome"`
	PID            int    `json:"pid,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ExitCode       int    `json:"exit_code"`
	SourceIdentity string `json:"source_identity,omitempty"`
	BriefDigest    string `json:"brief_digest,omitempty"`
	StdoutDigest   string `json:"stdout_sha256,omitempty"`
	StderrDigest   string `json:"stderr_sha256,omitempty"`
}

var runID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var sessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (e *Executor) Execute(ctx context.Context, r Request, now func() time.Time) Result {
	result := Result{SchemaVersion: 1, TaskID: r.Contract.TaskID, RunID: r.RunID, Mode: e.mode, Outcome: "LAUNCH_UNAUTHORIZED", ExitCode: -1}
	if !runID.MatchString(r.RunID) {
		result.RunID = ""
	}
	g := r.Grant
	if g == nil || g.Reference == "" || g.TaskID != r.Contract.TaskID || g.RunID != r.RunID || g.Workspace != r.Contract.Workspace || g.BriefDigest != r.Contract.BriefDigest || g.SourceIdentity == "" || g.ClaimNonce == "" || r.Contract.Permission != "source.read" {
		return result
	}
	grant := *g
	g = &grant
	if e.runner == nil || r.Store == nil || r.Source == nil || now == nil || !runID.MatchString(r.RunID) || r.Timeout <= 0 || r.Timeout > 5*time.Minute {
		result.Outcome = "INVALID_INVOCATION"
		return result
	}
	if reason := core.CheckContract(r.Profile, r.Contract, now()); reason != "CONTRACT_VALID" {
		result.Outcome = reason
		return result
	}
	root, err := config.ResolveWorkspace(r.Contract.Workspace)
	if err != nil || root != r.Contract.Workspace || root != r.Profile.WorkspaceRoot {
		result.Outcome = "WORKSPACE_SCOPE_MISMATCH"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	ctx, leaseCancel := context.WithDeadline(ctx, g.LeaseUntil)
	defer leaseCancel()
	if !g.LeaseUntil.After(now()) || !g.LeaseUntil.After(time.Now()) {
		result.Outcome = "CLAIM_NOT_CONFIRMED"
		return result
	}
	if ctx.Err() != nil {
		result.Outcome = contextReason(ctx)
		return result
	}
	if !reserve(*g) {
		result.Outcome = "EXECUTION_ALREADY_RESERVED"
		return result
	}
	task, err := r.Store.Read(ctx, r.Contract.PageID)
	if err != nil {
		result.Outcome = "COORDINATION_UNAVAILABLE"
		return result
	}
	source, err := r.Source.Verify(ctx, task.Base)
	if err != nil || source.Identity != g.SourceIdentity {
		result.Outcome = "SOURCE_UNPROVEN"
		return result
	}
	claim := core.Claim{Owner: r.Contract.Owner, Nonce: g.ClaimNonce, LeaseUntil: g.LeaseUntil}
	if reason := core.Admission(r.Profile, r.Contract, task, source, now(), &claim); reason != "ADMITTED" {
		result.Outcome = reason
		return result
	}
	if ctx.Err() != nil {
		result.Outcome = contextReason(ctx)
		return result
	}
	result.SourceIdentity, result.BriefDigest = source.Identity, r.Contract.BriefDigest
	invocation := Invocation{Dir: root, Args: []string{"exec", "--json", "--sandbox", "read-only", "-c", `approval_policy="never"`, "-C", root, "--ephemeral", "--skip-git-repo-check", "-"}, Prompt: task.Brief + "\n\nController output contract: final response must be one JSON object with status (completed or blocked) and summary (string). completed is unverified evidence; it grants no acceptance, task lifecycle, Git or runtime authority."}
	p := e.runner.Run(ctx, invocation)
	result.PID, result.ExitCode = p.PID, p.ExitCode
	result.StdoutDigest, result.StderrDigest = core.Digest(string(p.Stdout)), core.Digest(string(p.Stderr))
	switch {
	case !g.LeaseUntil.After(now()) || !g.LeaseUntil.After(time.Now()):
		result.Outcome = "LEASE_EXPIRED"
	case ctx.Err() != nil:
		result.Outcome = contextReason(ctx)
	case p.Overflow:
		result.Outcome = "OUTPUT_LIMIT"
	case quota(string(p.Stderr)):
		result.Outcome = "USAGE_LIMIT"
	case p.Err != nil || p.ExitCode != 0:
		result.Outcome = "CLI_FAILED"
	default:
		result.Outcome, result.SessionID = parse(p.Stdout)
	}
	if !g.LeaseUntil.After(now()) || !g.LeaseUntil.After(time.Now()) {
		result.Outcome = "LEASE_EXPIRED"
	}
	return result
}

func contextReason(ctx context.Context) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "TIMEOUT"
	}
	return "CANCELLED"
}
func quota(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "usage limit") || strings.Contains(s, "quota exceeded") || strings.Contains(s, "rate limit")
}

func parse(data []byte) (string, string) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	phase := 0
	finals := 0
	session, final, failure := "", "", ""
	for scanner.Scan() {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				Status   string          `json:"status"`
				Output   string          `json:"aggregated_output"`
				ExitCode *int            `json:"exit_code"`
				Phase    json.RawMessage `json:"phase"`
			} `json:"item"`
		}
		if !uniqueJSON(scanner.Bytes()) || !exactEventKeys(scanner.Bytes()) || json.Unmarshal(scanner.Bytes(), &event) != nil {
			return "MALFORMED_OUTPUT", session
		}
		switch event.Type {
		case "thread.started":
			if phase != 0 || !sessionID.MatchString(event.ThreadID) {
				return "UNSUPPORTED_OUTPUT", session
			}
			session = event.ThreadID
			phase = 1
		case "turn.started":
			if phase != 1 {
				return "UNSUPPORTED_OUTPUT", session
			}
			phase = 2
		case "turn.completed":
			if phase != 2 {
				return "UNSUPPORTED_OUTPUT", session
			}
			phase = 3
		case "turn.failed", "error":
			failure = "PROVIDER_ERROR"
			if quota(event.Message + event.Error.Message) {
				failure = "USAGE_LIMIT"
			}
		case "item.started", "item.updated", "item.completed":
			if phase != 2 {
				return "UNSUPPORTED_OUTPUT", session
			}
			switch event.Item.Type {
			case "agent_message":
				if len(event.Item.Phase) != 0 {
					return "UNSUPPORTED_OUTPUT", session
				}
				if event.Type == "item.completed" {
					final = event.Item.Text
					if strings.HasPrefix(strings.TrimSpace(final), "{") {
						var object map[string]json.RawMessage
						if !uniqueJSON([]byte(final)) || json.Unmarshal([]byte(final), &object) != nil {
							return "MALFORMED_OUTPUT", session
						}
						for key := range object {
							if strings.EqualFold(key, "status") || strings.EqualFold(key, "summary") {
								if _, valid := finalStatus(final); !valid {
									return "MALFORMED_OUTPUT", session
								}
								finals++
								break
							}
						}
						if finals > 1 {
							return "UNSUPPORTED_OUTPUT", session
						}
					}
				}
			case "command_execution":
				if event.Item.Status == "failed" || (event.Item.ExitCode != nil && *event.Item.ExitCode != 0) {
					failure = "TOOL_FAILED"
					if strings.Contains(strings.ToLower(event.Item.Output), "denied") {
						failure = "TOOL_DENIED"
					}
				}
			case "file_change":
				failure = "UNAUTHORIZED_ACTION"
			case "reasoning", "mcp_tool_call", "web_search", "todo_list":
				if event.Item.Status == "failed" {
					failure = "TOOL_FAILED"
				}
			default:
				return "UNSUPPORTED_OUTPUT", session
			}
		default:
			return "UNSUPPORTED_OUTPUT", session
		}
	}
	if scanner.Err() != nil {
		return "MALFORMED_OUTPUT", session
	}
	if failure != "" {
		return failure, session
	}
	if phase != 3 || final == "" {
		return "INCOMPLETE_OUTPUT", session
	}
	status, valid := finalStatus(final)
	if !valid || finals != 1 {
		return "MALFORMED_OUTPUT", session
	}
	switch status {
	case "completed":
		return "COMPLETED_UNVERIFIED", session
	case "blocked":
		return "SOFT_DENIED", session
	default:
		return "UNSUPPORTED_OUTPUT", session
	}
}

// Validate every JSON object before the decoder can overwrite a duplicate member.
func uniqueJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func() bool
	value = func() bool {
		token, err := d.Token()
		if err != nil {
			return false
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] || !value() {
					return false
				}
				seen[name] = true
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case json.Delim('['):
			for d.More() {
				if !value() {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		case json.Delim('}'), json.Delim(']'):
			return false
		default:
			return true
		}
	}
	return value() && d.Decode(new(any)) == io.EOF
}

func exactEventKeys(data []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return false
	}
	for key, raw := range object {
		for _, field := range []string{"type", "thread_id", "id", "message", "error", "item", "text", "status", "phase", "aggregated_output", "exit_code"} {
			if strings.EqualFold(key, field) && key != field {
				return false
			}
		}
		if (key == "item" || key == "error") && !exactEventKeys(raw) {
			return false
		}
	}
	return true
}

func finalStatus(text string) (string, bool) {
	d := json.NewDecoder(strings.NewReader(text))
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return "", false
	}
	seen := map[string]bool{}
	status := ""
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] || (name != "status" && name != "summary") {
			return "", false
		}
		seen[name] = true
		token, err := d.Token()
		value, ok := token.(string)
		if err != nil || !ok {
			return "", false
		}
		if name == "status" {
			status = value
		}
	}
	if _, err := d.Token(); err != nil || !seen["status"] || !seen["summary"] || d.Decode(new(any)) != io.EOF {
		return "", false
	}
	return status, true
}

type nativeRunner struct{ path, sha string }

func (n nativeRunner) Run(ctx context.Context, i Invocation) Process {
	if !pinned(n.path, n.sha) {
		return Process{ExitCode: -1, Err: os.ErrPermission}
	}
	return runProcessEnvironment(ctx, n.path, i.Args, i.Dir, i.Prompt, i.Environment)
}

func pinned(path, sha string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	const maxExecutable = 512 * 1024 * 1024
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > maxExecutable {
		return false
	}
	var header [4]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return false
	}
	magic := string(header[:])
	if magic != "\x7fELF" && magic != "\xcf\xfa\xed\xfe" && magic != "\xfe\xed\xfa\xcf" && magic != "\xca\xfe\xba\xbe" && magic != "\xbe\xba\xfe\xca" {
		return false
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxExecutable+1))
	return err == nil && n <= maxExecutable && len(sha) == 64 && hex.EncodeToString(h.Sum(nil)) == sha
}

type bounded struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *bounded) Write(p []byte) (int, error) {
	n := len(p)
	room := 1024*1024 - b.buf.Len()
	if len(p) > room {
		p = p[:room]
		b.overflow = true
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

func runProcess(ctx context.Context, path string, args []string, dir, prompt string) Process {
	return runProcessEnvironment(ctx, path, args, dir, prompt, nil)
}

func runProcessEnvironment(ctx context.Context, path string, args []string, dir, prompt string, environment []string) Process {
	root, err := config.ResolveWorkspace(dir)
	if err != nil || root != dir {
		return Process{ExitCode: -1, Err: os.ErrPermission}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	if environment != nil {
		cmd.Env = append([]string(nil), environment...)
	}
	cmd.Stdin = strings.NewReader(prompt)
	// Own a new process group so deadline/cancel cannot leave inherited-pipe children running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 250 * time.Millisecond
	var stdout, stderr bounded
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return Process{ExitCode: -1, Err: err}
	}
	pid := cmd.Process.Pid
	err = cmd.Wait()
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return Process{PID: pid, ExitCode: cmd.ProcessState.ExitCode(), Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Err: err, Overflow: stdout.overflow || stderr.overflow}
}
