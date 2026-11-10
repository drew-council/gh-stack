package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	operationsDirName = "gh-stack-operations"
	claimFileName     = "claim.json"
	claimLockFileName = "lock"
)

// Claim names what one operation reserves in a clone. Claims of different
// operations MUST NOT overlap, which is what lets operations on independent
// stacks run at the same time.
type Claim struct {
	Kind      string    `json:"kind"`
	Origin    string    `json:"origin,omitempty"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	Branches  []string  `json:"branches"`
	Worktrees []string  `json:"worktrees"`
}

// Empty reports whether the claim reserves nothing.
func (c Claim) Empty() bool {
	return len(c.Branches) == 0 && len(c.Worktrees) == 0
}

// Operation is one command's record under <common-dir>/gh-stack-operations.
// It holds the command's claim and its recovery journals. A running command
// holds the record's lock. A record that outlives its command with a journal
// is a paused operation whose claim still stands. A record without a journal
// that no process holds is debris.
//
// Records are read and changed only under the operation lock.
type Operation struct {
	Dir      string
	Claim    Claim
	Running  bool
	Journals []string

	// ClaimErr is set when a record that must be kept has no readable claim.
	// Such an operation is treated as claiming everything, so that nothing
	// runs beside it and its own recovery can still proceed.
	ClaimErr error

	lock *FileLock
}

// Claims reports whether the operation reserves any of the branches or
// worktrees.
func (o *Operation) Claims(branches, worktrees []string) bool {
	if o.ClaimErr != nil {
		return true
	}
	for _, branch := range branches {
		if slices.Contains(o.Claim.Branches, branch) {
			return true
		}
	}
	for _, path := range worktrees {
		if slices.ContainsFunc(o.Claim.Worktrees, func(claimed string) bool { return sameDir(claimed, path) }) {
			return true
		}
	}
	return false
}

// UsesWorktree reports whether the operation's claim names the worktree, or
// the operation started there. Unlike Claims, an unreadable claim names
// nothing.
func (o *Operation) UsesWorktree(path string) bool {
	return sameDir(o.Claim.Origin, path) ||
		slices.ContainsFunc(o.Claim.Worktrees, func(claimed string) bool { return sameDir(claimed, path) })
}

// sameDir also matches two spellings of one directory, as a case-insensitive
// filesystem allows.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

var heldOperations = struct {
	sync.Mutex
	byDir map[string]*Operation
}{byDir: make(map[string]*Operation)}

// OperationsDir returns the directory that holds a clone's operation records.
func OperationsDir(commonDir string) string {
	return filepath.Join(commonDir, operationsDirName)
}

// Held reports whether this process holds the operation.
func (o *Operation) Held() bool {
	return o != nil && o.lock != nil
}

// StartOperation records a new running operation with the given claim. The
// caller MUST hold the operation lock and MUST have checked the claim against
// ListOperations.
func StartOperation(commonDir string, claim Claim) (*Operation, error) {
	root := OperationsDir(commonDir)
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("creating operations directory: %w", err)
	}
	dir, err := os.MkdirTemp(root, time.Now().UTC().Format("20060102T150405")+"-")
	if err != nil {
		return nil, fmt.Errorf("creating operation record: %w", err)
	}
	op := &Operation{Dir: dir}
	acquired, err := op.Resume()
	if err == nil && !acquired {
		err = errors.New("new operation record is already locked")
	}
	if err == nil {
		err = op.SetClaim(claim)
	}
	if err != nil {
		op.Suspend()
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return op, nil
}

// Resume takes the record's lock for this process. It returns false when
// another process is running the operation.
func (o *Operation) Resume() (bool, error) {
	if o.lock != nil {
		return true, nil
	}
	lock, acquired, err := acquireLock(filepath.Join(o.Dir, claimLockFileName), "operation", false)
	if err != nil || !acquired {
		return false, err
	}
	o.lock, o.Running = lock, true
	heldOperations.Lock()
	heldOperations.byDir[o.Dir] = o
	heldOperations.Unlock()
	return true, nil
}

// SetClaim replaces the claim of an operation this process holds.
func (o *Operation) SetClaim(claim Claim) error {
	if o.lock == nil {
		return errors.New("cannot change the claim of an operation this process does not hold")
	}
	claim.Branches = normalizeClaimed(claim.Branches)
	claim.Worktrees = normalizeClaimed(claim.Worktrees)
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling operation claim: %w", err)
	}
	if err := WriteAtomic(filepath.Join(o.Dir, claimFileName), data); err != nil {
		return fmt.Errorf("writing operation claim: %w", err)
	}
	o.Claim = claim
	return nil
}

func normalizeClaimed(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// Suspend releases the record's lock and leaves the record in place.
func (o *Operation) Suspend() {
	if o == nil || o.lock == nil {
		return
	}
	heldOperations.Lock()
	delete(heldOperations.byDir, o.Dir)
	heldOperations.Unlock()
	o.lock.Unlock()
	o.lock, o.Running = nil, false
}

// Remove releases and deletes the record. The lock is released first because
// Windows cannot delete an open file.
func (o *Operation) Remove() error {
	o.Suspend()
	return os.RemoveAll(o.Dir)
}

// RefreshJournals rereads which journals the record holds.
func (o *Operation) RefreshJournals() error {
	journals, err := operationJournals(o.Dir)
	if err != nil {
		return err
	}
	o.Journals = journals
	return nil
}

// ListOperations returns the clone's operation records, oldest first, and
// deletes debris. The caller MUST hold the operation lock.
func ListOperations(commonDir string) ([]*Operation, error) {
	root := OperationsDir(commonDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading operation records: %w", err)
	}
	var ops []*Operation
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		heldOperations.Lock()
		held := heldOperations.byDir[dir]
		heldOperations.Unlock()
		if held != nil {
			if err := held.RefreshJournals(); err != nil {
				return nil, err
			}
			ops = append(ops, held)
			continue
		}
		op, err := inspectOperation(dir)
		if err != nil {
			return nil, err
		}
		if op != nil {
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func inspectOperation(dir string) (*Operation, error) {
	// Probe before reading the journals: a command that is not running
	// cannot write one afterwards.
	running, err := operationRunning(dir)
	if err != nil {
		return nil, fmt.Errorf("checking operation record %s: %w", dir, err)
	}
	journals, err := operationJournals(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !running && len(journals) == 0 {
		// The command died before it could pause. It changed nothing that
		// needs recovery, so its claim is void. A record that cannot be
		// removed now is removed by a later scan.
		_ = os.RemoveAll(dir)
		return nil, nil
	}
	op := &Operation{Dir: dir, Running: running, Journals: journals}
	data, err := readStateFile(filepath.Join(dir, claimFileName))
	if err == nil {
		err = json.Unmarshal(data, &op.Claim)
	}
	if err != nil {
		op.Claim, op.ClaimErr = Claim{}, fmt.Errorf("operation record %s has no readable claim: %w", dir, err)
	}
	return op, nil
}

func operationJournals(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var journals []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "gh-stack-") && strings.HasSuffix(name, "-state") {
			journals = append(journals, name)
		}
	}
	return journals, nil
}

// operationRunning probes the record's lock. Probes cannot disturb each other
// because every prober holds the operation lock.
func operationRunning(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, claimLockFileName), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := tryLockFile(f); err != nil {
		if isLockBusy(err) {
			return true, nil
		}
		return false, err
	}
	unlockFile(f)
	return false, nil
}

// claimedByOthers reports whether an operation this process does not hold
// claims any of the branches. The caller MUST hold the operation lock.
func claimedByOthers(commonDir string, branches []string) (bool, error) {
	ops, err := ListOperations(commonDir)
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if !op.Held() && op.Claims(branches, nil) {
			return true, nil
		}
	}
	return false, nil
}
