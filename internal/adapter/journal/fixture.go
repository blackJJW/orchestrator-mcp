// Package journal implements the MTT-574 local fixture contract, without launch or network I/O.
package journal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"orchestrator-mcp/internal/adapter/notion"
	"orchestrator-mcp/internal/config"
	"orchestrator-mcp/internal/core"
)

var ErrBusy = errors.New("LOCK_BUSY")
var ErrUnknown = errors.New("JOURNAL_OR_SPAWN_UNKNOWN")
var ErrIdentity = errors.New("EVIDENCE_OR_OWNER_UNSUPPORTED")
var ErrConsumed = errors.New("RUN_OR_CLAIM_CONSUMED")

const Namespace = ".local/controller-fixture"
const limit = 1 << 20

// Identity is supplied by the fixture caller after S1/S2/S3 checks, never a launch grant.
type Identity struct {
	Contract             core.Contract
	Claim                notion.ClaimState
	Proof                config.SourceProof
	Deadline             time.Time
	DeliveryTargetDigest string `json:"DeliveryTargetDigest,omitempty"`
}

// ProcessEvidence is a provider-neutral model observation, not OS provenance.
type ProcessEvidence struct {
	TaskID, RunID, ClaimKey, BirthToken, ExecutableSHA, SessionID string
	PID, PGID                                                     int
	Complete, Unknown                                             bool
	Remaining                                                     int
}

type Record struct {
	Sequence uint64
	Provider string
	Identity Identity
	Stage    string
	Process  *ProcessEvidence
	Outcome  string
	Payload  []byte
	Delivery *notion.ResultHandoff
}

type ledger struct {
	Version            int
	Project, Workspace string
	StorageRoot        string `json:"StorageRoot,omitempty"`
	Records            []Record
	Digest             string
}

type Fixture struct {
	mu       sync.Mutex
	profile  config.Profile
	dir      string
	lock, fd *os.File
	disk     ledger
	poisoned bool
}

// OpenFixture uses one fixed namespace per canonical, explicitly supplied workspace.
// It holds an advisory kernel lock until Close; it never removes or replaces the lock file.
func OpenFixture(profile config.Profile) (*Fixture, error) {
	return openFixture(profile, profile.WorkspaceRoot, false)
}

// OpenControllerFixture separates controller-owned persistence from read-only source bytes.
func OpenControllerFixture(profile config.Profile, controllerRoot string) (*Fixture, error) {
	return openFixture(profile, controllerRoot, true)
}

func openFixture(profile config.Profile, controllerRoot string, separate bool) (*Fixture, error) {
	root, err := config.ResolveWorkspace(profile.WorkspaceRoot)
	if err != nil || root != profile.WorkspaceRoot || profile.ProjectID == "" || profile.SchemaVersion != 1 {
		return nil, ErrIdentity
	}
	storage, err := config.ResolveWorkspace(controllerRoot)
	if err != nil || storage != controllerRoot {
		return nil, ErrIdentity
	}
	if separate {
		for _, pair := range [][2]string{{root, storage}, {storage, root}} {
			rel, e := filepath.Rel(pair[0], pair[1])
			if e != nil || (rel != ".." && !strings.HasPrefix(rel, "../")) {
				return nil, ErrIdentity
			}
		}
	}
	parent := filepath.Join(storage, ".local")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(parent); err != nil || resolved != parent {
		return nil, ErrIdentity
	}
	dir := filepath.Join(storage, Namespace)
	created := os.Mkdir(dir, 0700) == nil
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, ErrUnknown
	}
	fd, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	lock, err := regular(path, syscall.O_RDWR|syscall.O_CREAT, 0600)
	if err != nil {
		fd.Close()
		return nil, err
	}
	// An uninitialized existing directory cannot take the initializer's kernel lock.
	if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !created && errors.Is(err, os.ErrNotExist) {
		lock.Close()
		fd.Close()
		return nil, ErrUnknown
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		fd.Close()
		return nil, ErrBusy
	}
	f := &Fixture{profile: profile, dir: dir, lock: lock, fd: fd}
	f.profile.TaskAllowlist = append([]string(nil), profile.TaskAllowlist...)
	f.profile.Permissions = append([]string(nil), profile.Permissions...)
	if err := f.paths(); err != nil {
		f.Close()
		return nil, err
	}
	f.disk = ledger{Version: 1, Project: profile.ProjectID, Workspace: root, Records: []Record{}}
	if separate {
		f.disk.StorageRoot = storage
	}
	_, nextErr := os.Lstat(filepath.Join(dir, "journal.next"))
	if !errors.Is(nextErr, os.ErrNotExist) {
		f.Close()
		return nil, ErrUnknown
	}
	data, err := f.read()
	if errors.Is(err, os.ErrNotExist) && created {
		err = f.commit(f.disk, true)
	} else if err == nil {
		err = f.decode(data)
	}
	if err != nil {
		f.Close()
		return nil, ErrUnknown
	}
	return f, nil
}

func (f *Fixture) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poisoned = true
	err := f.lock.Close()
	f.fd.Close()
	return err
}

// IntentFixture consumes run AND claim identities before any caller's fake delivery.
func (f *Fixture) IntentFixture(id Identity, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.intent(id, now)
}

// ClaimIntentFixture atomically binds local fencing and consumption; it never claims Notion.
func (f *Fixture) ClaimIntentFixture(id Identity, now time.Time) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return Identity{}, err
	}
	var fence uint64
	for _, r := range f.disk.Records {
		if r.Identity.Claim.Fence > fence {
			fence = r.Identity.Claim.Fence
		}
	}
	b := id.Claim.Binding
	state, err := notion.ClaimTransition(b, notion.ClaimState{Binding: b, Owner: "Unclaimed", Fence: fence}, "", fence,
		core.Claim{Owner: id.Claim.Owner, Nonce: id.Claim.Nonce, LeaseUntil: id.Claim.LeaseUntil}, id.Proof, now)
	if err != nil {
		return Identity{}, ErrIdentity
	}
	id.Claim = state
	if err := f.intent(id, now); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// FindFixture reads only an already consumed identity; it cannot authorize a new delivery.
func (f *Fixture) FindFixture(id Identity, now time.Time) (Record, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return Record{}, false, err
	}
	for i := len(f.disk.Records) - 1; i >= 0; i-- {
		r := f.disk.Records[i]
		if r.Identity.Claim.Binding.TaskID != id.Claim.Binding.TaskID || r.Identity.Claim.Binding.RunID != id.Claim.Binding.RunID {
			continue
		}
		if r.Identity.Contract != id.Contract || r.Identity.Claim.Binding != id.Claim.Binding || r.Identity.DeliveryTargetDigest != id.DeliveryTargetDigest ||
			r.Identity.Claim.Owner != id.Claim.Owner || r.Identity.Claim.Nonce != id.Claim.Nonce ||
			!r.Identity.Claim.LeaseUntil.Equal(id.Claim.LeaseUntil) {
			return Record{}, true, ErrIdentity
		}
		id.Claim, id.Deadline = r.Identity.Claim, r.Identity.Deadline
		if _, err := f.current(id, now); err != nil {
			return Record{}, true, err
		}
		b, _ := json.Marshal(r)
		var copy Record
		_ = json.Unmarshal(b, &copy)
		return copy, true, nil
	}
	return Record{}, false, nil
}

func (f *Fixture) intent(id Identity, now time.Time) error {
	if err := f.check(); err != nil {
		return err
	}
	if err := f.identity(id, now); err != nil {
		return err
	}
	if !id.Deadline.After(now) {
		return ErrIdentity
	}
	latest := map[string]Record{}
	for _, r := range f.disk.Records {
		b := r.Identity.Claim.Binding
		if b.TaskID == id.Claim.Binding.TaskID && (b.RunID == id.Claim.Binding.RunID || r.Identity.Claim.Nonce == id.Claim.Nonce) {
			return ErrConsumed
		}
		latest[b.TaskID+"\x00"+b.RunID] = r
	}
	for _, r := range latest {
		if r.Stage != "SYNC_CONFIRMED" {
			return ErrUnknown
		}
	}
	return f.append(Record{Provider: "codex", Identity: id, Stage: "INTENT"})
}

// ResultFixture records bounded immutable output after caller-supplied cleanup model checks.
func (f *Fixture) ResultFixture(id Identity, process ProcessEvidence, outcome string, payload []byte, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.current(id, now)
	if err != nil {
		return err
	}
	handoff, err := notion.NewResultHandoff(id.Claim, payload)
	if err != nil || r.Stage != "INTENT" || !id.Deadline.After(now) || len(payload) == 0 || len(payload) > limit/4 ||
		(outcome != "COMPLETED_UNVERIFIED" && outcome != "BLOCKED" && outcome != "FAILED") ||
		process.TaskID != id.Claim.Binding.TaskID || process.RunID != id.Claim.Binding.RunID ||
		process.ClaimKey != handoff.Key || process.BirthToken == "" || !hash(process.ExecutableSHA) ||
		process.PID <= 0 || process.PGID != process.PID || !process.Complete || process.Unknown || process.Remaining != 0 {
		return ErrIdentity
	}
	r.Stage, r.Process, r.Outcome, r.Payload, r.Delivery = "RESULT_DURABLE", &process, outcome, append([]byte(nil), payload...), &handoff
	return f.append(r)
}

// RecoverFixture returns data only. INTENT never permits another delivery, even after lock reacquisition.
func (f *Fixture) RecoverFixture(id Identity, now time.Time) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.current(id, now)
	if err != nil {
		return Record{}, err
	}
	if r.Stage == "INTENT" {
		return Record{}, ErrUnknown
	}
	b, _ := json.Marshal(r)
	var copy Record
	_ = json.Unmarshal(b, &copy)
	return copy, nil
}

// ReadbackFixture performs no write/retry to Notion. Nil/ambiguous observations remain UNKNOWN.
func (f *Fixture) ReadbackFixture(id Identity, actual *notion.ResultHandoff, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.current(id, now)
	if err != nil {
		return err
	}
	if r.Delivery == nil {
		return ErrUnknown
	}
	stage := "SYNC_UNKNOWN"
	if actual != nil && notion.ConfirmResultReadback(*r.Delivery, *actual) == nil {
		stage = "SYNC_CONFIRMED"
	}
	if r.Stage != stage {
		if r.Stage == "SYNC_CONFIRMED" {
			return ErrUnknown
		}
		r.Stage = stage
		if err := f.append(r); err != nil {
			return err
		}
	}
	if stage != "SYNC_CONFIRMED" {
		return ErrUnknown
	}
	return nil
}

func (f *Fixture) identity(id Identity, now time.Time) error {
	b, err := notion.BindClaim(f.profile, id.Contract, id.Claim.Binding.RunID, id.Claim.Binding.AccessRef, id.Proof)
	_, schemaErr := notion.NewResultHandoff(id.Claim, []byte("fixture identity validation"))
	if err != nil || schemaErr != nil || b != id.Claim.Binding || id.Claim.Owner != b.Owner ||
		!id.Claim.Consumed || id.Claim.Fence == 0 || id.Claim.Nonce == "" ||
		now.IsZero() || !id.Claim.LeaseUntil.After(now) || id.Deadline.IsZero() || id.Deadline.After(id.Claim.LeaseUntil) ||
		id.Deadline.After(now.Add(5*time.Minute)) ||
		(f.disk.StorageRoot != "" && !hash(id.DeliveryTargetDigest)) || (id.DeliveryTargetDigest != "" && !hash(id.DeliveryTargetDigest)) {
		return ErrIdentity
	}
	return nil
}

func (f *Fixture) current(id Identity, now time.Time) (Record, error) {
	if err := f.check(); err != nil {
		return Record{}, err
	}
	if err := f.identity(id, now); err != nil {
		return Record{}, err
	}
	for i := len(f.disk.Records) - 1; i >= 0; i-- {
		r := f.disk.Records[i]
		b := r.Identity.Claim.Binding
		if b.TaskID == id.Claim.Binding.TaskID && b.RunID == id.Claim.Binding.RunID {
			if r.Identity.Contract != id.Contract || r.Identity.DeliveryTargetDigest != id.DeliveryTargetDigest ||
				!sameClaim(r.Identity.Claim, id.Claim) || !r.Identity.Deadline.Equal(id.Deadline) {
				return Record{}, ErrIdentity
			}
			return r, nil
		}
	}
	return Record{}, ErrIdentity
}

// JSON persistence drops monotonic clock metadata, not the approved lease instant.
func sameClaim(a, b notion.ClaimState) bool {
	return a.Binding == b.Binding && a.Owner == b.Owner && a.Nonce == b.Nonce && a.Fence == b.Fence &&
		a.Consumed == b.Consumed && a.LeaseUntil.Equal(b.LeaseUntil)
}

func (f *Fixture) append(r Record) error {
	next := f.disk
	next.Records = append(append([]Record(nil), f.disk.Records...), r)
	next.Records[len(next.Records)-1].Sequence = uint64(len(next.Records))
	if len(next.Records) > 128 {
		return ErrUnknown
	}
	return f.commit(next, false)
}

func (f *Fixture) paths() error {
	for _, item := range []struct {
		path string
		file *os.File
	}{{f.dir, f.fd}, {filepath.Join(f.dir, "lock"), f.lock}} {
		open, err := item.file.Stat()
		named, e := os.Lstat(item.path)
		mode := os.FileMode(0600)
		if item.file == f.fd {
			mode = 0700
		}
		if err != nil || e != nil || !os.SameFile(open, named) || named.Mode().Perm() != mode ||
			named.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			f.poisoned = true
			return ErrUnknown
		}
	}
	return nil
}

func (f *Fixture) check() error {
	if f.poisoned || f.paths() != nil {
		return ErrUnknown
	}
	data, err := f.read()
	expected, _ := json.Marshal(f.disk)
	if err != nil || !bytes.Equal(data, expected) {
		f.poisoned = true
		return ErrUnknown
	}
	return nil
}

func regular(path string, flags int, mode uint32) (*os.File, error) {
	n, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(n), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		file.Close()
		return nil, ErrUnknown
	}
	return file, nil
}

func (f *Fixture) read() ([]byte, error) {
	file, err := regular(filepath.Join(f.dir, "journal.json"), syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if len(data) > limit {
		return nil, ErrUnknown
	}
	return data, err
}

func digest(l ledger) string {
	l.Digest = ""
	b, _ := json.Marshal(l)
	return core.Digest(string(b))
}

func hash(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == 64 && strings.ToLower(s) == s
}

func (f *Fixture) commit(next ledger, initial bool) error {
	if f.paths() != nil || (!initial && f.check() != nil) {
		return ErrUnknown
	}
	next.Digest = digest(next)
	b, err := json.Marshal(next)
	if err != nil || len(b) > limit {
		return ErrUnknown
	}
	temp := filepath.Join(f.dir, "journal.next")
	file, err := regular(temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0600)
	if err == nil {
		_, err = file.Write(b)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = f.paths()
	}
	if err == nil {
		err = os.Rename(temp, filepath.Join(f.dir, "journal.json"))
	}
	if err == nil {
		err = f.fd.Sync()
	}
	if err != nil {
		f.poisoned = true
		return ErrUnknown
	}
	// Caller-owned slices must not mutate the committed in-memory snapshot.
	if err := json.Unmarshal(b, &f.disk); err != nil {
		f.poisoned = true
		return ErrUnknown
	}
	return nil
}

func (f *Fixture) decode(data []byte) error {
	var disk ledger
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&disk); err != nil {
		return ErrUnknown
	}
	canonical, _ := json.Marshal(disk)
	if !bytes.Equal(canonical, data) || disk.Version != 1 || disk.Project != f.profile.ProjectID ||
		disk.Workspace != f.profile.WorkspaceRoot || disk.StorageRoot != f.disk.StorageRoot || disk.Records == nil || len(disk.Records) > 128 || disk.Digest != digest(disk) {
		return ErrUnknown
	}
	latest := map[string]Record{}
	for i, r := range disk.Records {
		id := r.Identity
		b, err := notion.BindClaim(f.profile, id.Contract, id.Claim.Binding.RunID, id.Claim.Binding.AccessRef, id.Proof)
		_, schemaErr := notion.NewResultHandoff(id.Claim, []byte("fixture identity validation"))
		if err != nil || schemaErr != nil || b != id.Claim.Binding || r.Provider != "codex" || r.Sequence != uint64(i+1) ||
			!id.Claim.Consumed || id.Claim.Owner != b.Owner || id.Claim.Fence == 0 || id.Claim.Nonce == "" ||
			id.Deadline.IsZero() || id.Deadline.After(id.Claim.LeaseUntil) ||
			(disk.StorageRoot != "" && !hash(id.DeliveryTargetDigest)) || (id.DeliveryTargetDigest != "" && !hash(id.DeliveryTargetDigest)) {
			return ErrUnknown
		}
		key := b.TaskID + "\x00" + b.RunID
		prev, exists := latest[key]
		if !exists {
			if r.Stage != "INTENT" || r.Process != nil || r.Delivery != nil || r.Outcome != "" || len(r.Payload) != 0 {
				return ErrUnknown
			}
			for _, other := range latest {
				if other.Stage != "SYNC_CONFIRMED" || (other.Identity.Claim.Binding.TaskID == b.TaskID && other.Identity.Claim.Nonce == id.Claim.Nonce) {
					return ErrUnknown
				}
			}
		} else {
			if !reflect.DeepEqual(prev.Identity, id) || (prev.Stage == "SYNC_CONFIRMED") ||
				(prev.Stage == "INTENT" && r.Stage != "RESULT_DURABLE") ||
				(prev.Stage != "INTENT" && r.Stage != "SYNC_UNKNOWN" && r.Stage != "SYNC_CONFIRMED") {
				return ErrUnknown
			}
			handoff, err := notion.NewResultHandoff(id.Claim, r.Payload)
			if err != nil || r.Delivery == nil || *r.Delivery != handoff || r.Process == nil ||
				r.Process.TaskID != b.TaskID || r.Process.RunID != b.RunID || r.Process.ClaimKey != handoff.Key ||
				r.Process.BirthToken == "" || !hash(r.Process.ExecutableSHA) || r.Process.PID <= 0 ||
				r.Process.PGID != r.Process.PID || !r.Process.Complete || r.Process.Unknown || r.Process.Remaining != 0 ||
				len(r.Payload) == 0 || len(r.Payload) > limit/4 || (r.Outcome != "COMPLETED_UNVERIFIED" && r.Outcome != "BLOCKED" && r.Outcome != "FAILED") {
				return ErrUnknown
			}
			if prev.Stage != "INTENT" && (!bytes.Equal(prev.Payload, r.Payload) || prev.Outcome != r.Outcome ||
				!reflect.DeepEqual(prev.Process, r.Process) || *prev.Delivery != *r.Delivery) {
				return ErrUnknown
			}
		}
		latest[key] = r
	}
	f.disk = disk
	return nil
}
