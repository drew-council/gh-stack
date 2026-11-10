package stack

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	schemaVersion = 1
	stackFileName = "gh-stack"
)

// PullRequestRef holds relatively immutable metadata about an associated PR.
type PullRequestRef struct {
	Number     int    `json:"number"`
	ID         string `json:"id,omitempty"`
	URL        string `json:"url,omitempty"`
	Merged     bool   `json:"merged,omitempty"`
	MergedHead string `json:"mergedHead,omitempty"`
}

// BranchRef represents a branch and its associated commit hash.
// For the trunk, Head stores the HEAD commit SHA.
// For stacked branches, Base stores the parent branch's HEAD SHA
// at the time of last sync/rebase, used to identify unique commits.
type BranchRef struct {
	Branch      string          `json:"branch"`
	Head        string          `json:"head,omitempty"`
	Base        string          `json:"base,omitempty"`
	PullRequest *PullRequestRef `json:"pullRequest,omitempty"`

	// Queued is a transient (not persisted) flag indicating the branch's
	// PR is currently in a merge queue. It is populated by syncStackPRs
	// from the GitHub API on each command run.
	Queued bool `json:"-"`
}

// Stack represents a single stack of branches.
type Stack struct {
	ID       string      `json:"id,omitempty"`
	Number   int         `json:"number,omitempty"`
	Trunk    BranchRef   `json:"trunk"`
	Branches []BranchRef `json:"branches"`
}

// DisplayChain returns a human-readable chain representation of the stack.
// Format: (trunk) <- branch1 <- branch2 <- branch3
func (s *Stack) DisplayChain() string {
	parts := []string{"(" + s.Trunk.Branch + ")"}
	for _, b := range s.Branches {
		parts = append(parts, b.Branch)
	}
	return strings.Join(parts, " <- ")
}

// BranchNames returns the list of branch names in order.
func (s *Stack) BranchNames() []string {
	names := make([]string, len(s.Branches))
	for i, b := range s.Branches {
		names[i] = b.Branch
	}
	return names
}

// IndexOf returns the index of the given branch in the stack, or -1 if not found.
func (s *Stack) IndexOf(branch string) int {
	for i, b := range s.Branches {
		if b.Branch == branch {
			return i
		}
	}
	return -1
}

// Contains returns true if the branch is part of this stack (including trunk).
func (s *Stack) Contains(branch string) bool {
	if s.Trunk.Branch == branch {
		return true
	}
	return s.IndexOf(branch) >= 0
}

// BaseBranch returns the base branch for the given branch in the stack.
// For the first branch, this is the trunk. For others, it's the previous branch.
func (s *Stack) BaseBranch(branch string) string {
	idx := s.IndexOf(branch)
	if idx <= 0 {
		return s.Trunk.Branch
	}
	return s.Branches[idx-1].Branch
}

// IsMerged returns whether a branch's PR has been merged.
func (b *BranchRef) IsMerged() bool {
	return b.PullRequest != nil && b.PullRequest.Merged
}

// IsQueued returns whether a branch's PR is currently in a merge queue.
// This is a transient state populated from the GitHub API on each run.
func (b *BranchRef) IsQueued() bool {
	return b.Queued
}

// IsSkipped returns whether a branch should be skipped during push/sync/submit.
// A branch is skipped if its PR has been merged or is currently queued.
func (b *BranchRef) IsSkipped() bool {
	return b.IsMerged() || b.IsQueued()
}

// ActiveBranches returns only branches that are pushable (not merged, not queued).
func (s *Stack) ActiveBranches() []BranchRef {
	var active []BranchRef
	for _, b := range s.Branches {
		if !b.IsSkipped() {
			active = append(active, b)
		}
	}
	return active
}

// MergedBranches returns only merged branches, preserving order.
func (s *Stack) MergedBranches() []BranchRef {
	var merged []BranchRef
	for _, b := range s.Branches {
		if b.IsMerged() {
			merged = append(merged, b)
		}
	}
	return merged
}

// QueuedBranches returns only queued branches, preserving order.
func (s *Stack) QueuedBranches() []BranchRef {
	var queued []BranchRef
	for _, b := range s.Branches {
		if b.IsQueued() {
			queued = append(queued, b)
		}
	}
	return queued
}

// FirstActiveBranchIndex returns the index of the first active (not merged, not queued) branch, or -1.
func (s *Stack) FirstActiveBranchIndex() int {
	for i, b := range s.Branches {
		if !b.IsSkipped() {
			return i
		}
	}
	return -1
}

// ActiveBranchIndices returns the indices of all active (not merged, not queued) branches.
func (s *Stack) ActiveBranchIndices() []int {
	var indices []int
	for i, b := range s.Branches {
		if !b.IsSkipped() {
			indices = append(indices, i)
		}
	}
	return indices
}

// ActiveBaseBranch returns the effective parent for a branch, skipping merged
// and queued ancestors. For the first active branch (or any branch whose
// downstack is all merged/queued), this returns the trunk.
func (s *Stack) ActiveBaseBranch(branch string) string {
	idx := s.IndexOf(branch)
	if idx <= 0 {
		return s.Trunk.Branch
	}
	for j := idx - 1; j >= 0; j-- {
		if !s.Branches[j].IsSkipped() {
			return s.Branches[j].Branch
		}
	}
	return s.Trunk.Branch
}

// IsFullyMerged returns true if all branches in the stack have been merged.
func (s *Stack) IsFullyMerged() bool {
	for _, b := range s.Branches {
		if !b.IsMerged() {
			return false
		}
	}
	return len(s.Branches) > 0
}

// NearestSurvivingBranch returns the branch nearest to target within the ordered
// branch-name list `order` for which `survives` reports true, preferring the
// neighbor above (later in the slice, away from the trunk) and then the neighbor
// below (earlier in the slice, toward the trunk).
//
// It returns "" when target is not present in order, or when no other branch in
// order survives. Callers layer their own fallback and any name translation
// around this search. It is shared by the checkout-branch selection in
// `gh stack modify` and `gh stack sync`.
func NearestSurvivingBranch(order []string, target string, survives func(string) bool) string {
	pos := -1
	for i, name := range order {
		if name == target {
			pos = i
			break
		}
	}
	if pos < 0 {
		return ""
	}
	for i := pos + 1; i < len(order); i++ {
		if survives(order[i]) {
			return order[i]
		}
	}
	for i := pos - 1; i >= 0; i-- {
		if survives(order[i]) {
			return order[i]
		}
	}
	return ""
}

// StackFile represents the JSON catalog stored in Git's common directory.
type StackFile struct {
	SchemaVersion int     `json:"schemaVersion"`
	Repository    string  `json:"repository"`
	Stacks        []Stack `json:"stacks"`

	// loadChecksum is the SHA-256 of the raw file bytes this view last read
	// or wrote. nil means the file did not exist when loaded.
	loadChecksum []byte

	// synced holds each stack as this view last read or wrote it. Save
	// publishes only the difference from it, so stacks that other processes
	// changed in the meantime survive.
	synced           []string
	syncedRepository string

	// diverged records that Save merged this view into a catalog holding
	// stacks the view does not have. Such a view MUST NOT be written verbatim.
	diverged bool
}

// FindAllStacksForBranch returns all stacks that contain the given branch.
func (sf *StackFile) FindAllStacksForBranch(branch string) []*Stack {
	var stacks []*Stack
	for i := range sf.Stacks {
		if sf.Stacks[i].Contains(branch) {
			stacks = append(stacks, &sf.Stacks[i])
		}
	}
	return stacks
}

// IndexOfStack returns the index of the given stack within the file by identity
// (pointer), or -1 if it is not part of this file. Use it to locate a stack
// obtained from FindAllStacksForBranch before mutating the Stacks slice.
func (sf *StackFile) IndexOfStack(s *Stack) int {
	for i := range sf.Stacks {
		if &sf.Stacks[i] == s {
			return i
		}
	}
	return -1
}

// FindStackByPRNumber returns the first stack and branch whose PR number matches.
// Returns nil, nil if no match is found.
func (sf *StackFile) FindStackByPRNumber(prNumber int) (*Stack, *BranchRef) {
	for i := range sf.Stacks {
		for j := range sf.Stacks[i].Branches {
			b := &sf.Stacks[i].Branches[j]
			if b.PullRequest != nil && b.PullRequest.Number == prNumber {
				return &sf.Stacks[i], b
			}
		}
	}
	return nil, nil
}

// ValidateNoDuplicateBranch checks that the branch is not already in any stack.
func (sf *StackFile) ValidateNoDuplicateBranch(branch string) error {
	for _, s := range sf.Stacks {
		if s.Contains(branch) {
			return fmt.Errorf("branch %q is already part of a stack", branch)
		}
	}
	return nil
}

// AddStack adds a new stack to the file.
func (sf *StackFile) AddStack(s Stack) {
	sf.Stacks = append(sf.Stacks, s)
}

// RemoveStack removes the stack at the given index.
func (sf *StackFile) RemoveStack(idx int) {
	sf.Stacks = append(sf.Stacks[:idx], sf.Stacks[idx+1:]...)
}

// RemoveStackForBranch removes the stack containing the given branch.
func (sf *StackFile) RemoveStackForBranch(branch string) bool {
	for i := range sf.Stacks {
		if sf.Stacks[i].Contains(branch) {
			sf.RemoveStack(i)
			return true
		}
	}
	return false
}

// stackFilePath returns the path to the gh-stack file.
func stackFilePath(gitDir string) string {
	return filepath.Join(gitDir, stackFileName)
}

// Load reads the catalog from the given directory, normally Git's common
// directory. Legacy recovery may explicitly use a worktree's original directory.
// Returns an empty StackFile if the file does not exist.
// The returned StackFile records a checksum of the on-disk content so that
// Save can detect concurrent modifications.
func Load(gitDir string) (*StackFile, error) {
	path := stackFilePath(gitDir)
	data, err := readStateFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// loadChecksum stays nil — sentinel for "file absent at load time".
			return &StackFile{
				SchemaVersion: schemaVersion,
				Stacks:        []Stack{},
			}, nil
		}
		return nil, fmt.Errorf("reading stack file: %w", err)
	}

	return parseStackFile(data)
}

func parseStackFile(data []byte) (*StackFile, error) {
	var sf StackFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing stack file: %w", err)
	}

	if sf.SchemaVersion > schemaVersion {
		return nil, fmt.Errorf("stack file has schema version %d, but this version of gh-stack only supports up to version %d — please upgrade gh-stack", sf.SchemaVersion, schemaVersion)
	}

	sum := sha256.Sum256(data)
	sf.loadChecksum = sum[:]
	if err := sf.markSynced(); err != nil {
		return nil, err
	}
	return &sf, nil
}

func encodeStacks(stacks []Stack) ([]string, error) {
	encoded := make([]string, len(stacks))
	for i := range stacks {
		data, err := json.Marshal(&stacks[i])
		if err != nil {
			return nil, fmt.Errorf("marshaling stack: %w", err)
		}
		encoded[i] = string(data)
	}
	return encoded, nil
}

func (sf *StackFile) markSynced() error {
	synced, err := encodeStacks(sf.Stacks)
	if err != nil {
		return err
	}
	sf.synced, sf.syncedRepository = synced, sf.Repository
	return nil
}

// pendingChanges splits this view against its last synced state: the indexes
// of stacks it added or changed, and the synced form of stacks it changed or
// removed. Stacks are compared as whole values.
func (sf *StackFile) pendingChanges() (changed []int, removed []string, err error) {
	mine, err := encodeStacks(sf.Stacks)
	if err != nil {
		return nil, nil, err
	}
	unmatched := make(map[string]int, len(sf.synced))
	for _, stack := range sf.synced {
		unmatched[stack]++
	}
	for i, stack := range mine {
		if unmatched[stack] > 0 {
			unmatched[stack]--
		} else {
			changed = append(changed, i)
		}
	}
	for _, stack := range sf.synced {
		if unmatched[stack] > 0 {
			unmatched[stack]--
			removed = append(removed, stack)
		}
	}
	return changed, removed, nil
}

// changedBranches names every branch of a stack this view added, changed, or
// removed since it was last synced.
func (sf *StackFile) changedBranches() ([]string, error) {
	changed, removed, err := sf.pendingChanges()
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, i := range changed {
		branches = append(branches, sf.Stacks[i].BranchNames()...)
	}
	for _, encoded := range removed {
		var before Stack
		if err := json.Unmarshal([]byte(encoded), &before); err != nil {
			return nil, err
		}
		branches = append(branches, before.BranchNames()...)
	}
	return branches, nil
}

// mergeInto applies this view's pending changes to the stacks on disk. A
// changed stack takes the place of the stack it shares a branch with; any
// other new stack is appended. It fails when another process changed or
// removed a stack this view also changed, or when the result would put one
// branch in two stacks.
func (sf *StackFile) mergeInto(disk []Stack) ([]Stack, error) {
	changed, removed, err := sf.pendingChanges()
	if err != nil {
		return nil, err
	}
	current, err := encodeStacks(disk)
	if err != nil {
		return nil, err
	}
	// replaced[i] marks disk stack i as superseded by this view.
	replaced := make(map[int]struct{}, len(removed))
	for _, before := range removed {
		index := -1
		for i, stack := range current {
			if _, taken := replaced[i]; !taken && stack == before {
				index = i
				break
			}
		}
		if index < 0 {
			var lost Stack
			_ = json.Unmarshal([]byte(before), &lost)
			return nil, fmt.Errorf("stack %s was changed by another process", lost.DisplayChain())
		}
		replaced[index] = struct{}{}
	}

	successor := make(map[int]int, len(changed))
	var appended []int
	for _, mine := range changed {
		slot := -1
		for i := range disk {
			_, superseded := replaced[i]
			_, filled := successor[i]
			if superseded && !filled && sharesBranch(&disk[i], &sf.Stacks[mine]) {
				slot = i
				break
			}
		}
		if slot < 0 {
			appended = append(appended, mine)
		} else {
			successor[slot] = mine
		}
	}

	merged := make([]Stack, 0, len(disk)+len(appended))
	fromView := make(map[int]struct{}, len(changed))
	for i := range disk {
		if mine, ok := successor[i]; ok {
			fromView[len(merged)] = struct{}{}
			merged = append(merged, sf.Stacks[mine])
		} else if _, superseded := replaced[i]; !superseded {
			merged = append(merged, disk[i])
		}
	}
	for _, mine := range appended {
		fromView[len(merged)] = struct{}{}
		merged = append(merged, sf.Stacks[mine])
	}

	for i := range fromView {
		for _, branch := range merged[i].Branches {
			for j := range merged {
				if i != j && merged[j].IndexOf(branch.Branch) >= 0 {
					return nil, fmt.Errorf("branch %q was added to stack %s by another process", branch.Branch, merged[j].DisplayChain())
				}
			}
		}
	}
	return merged, nil
}

func sharesBranch(a, b *Stack) bool {
	for _, branch := range a.Branches {
		if b.IndexOf(branch.Branch) >= 0 {
			return true
		}
	}
	return false
}

// Save acquires an exclusive lock on the stack file, publishes the stacks sf
// added, changed, or removed since it was loaded, and releases the lock. The
// lock is held only for the read-merge-write window.
//
// Stacks that sf left untouched are written as they are on disk, so commands
// working on different stacks do not overwrite each other. A command that
// changes an existing stack MUST hold a claim on it from before it loaded the
// catalog; read-only refreshes should use SaveNonBlocking.
// Returns *LockError if the lock times out, or *StaleError if another process
// changed a stack that sf also changed.
func Save(gitDir string, sf *StackFile) error {
	lock, err := Lock(gitDir)
	if err != nil {
		return err // *LockError for contention, plain error for I/O failures
	}
	defer lock.Unlock()
	return saveLocked(gitDir, sf)
}

// saveLocked publishes sf's pending changes. The caller must hold the catalog
// lock.
func saveLocked(gitDir string, sf *StackFile) error {
	data, err := readStateFile(stackFilePath(gitDir))
	if errors.Is(err, os.ErrNotExist) {
		// A catalog that vanished is recreated: this is not a lost update.
		return writeStackFile(gitDir, sf)
	}
	if err != nil {
		return fmt.Errorf("reading stack file for staleness check: %w", err)
	}
	sum := sha256.Sum256(data)
	if !sf.diverged && sf.loadChecksum != nil && bytes.Equal(sf.loadChecksum, sum[:]) {
		return writeStackFile(gitDir, sf)
	}

	disk, err := parseStackFile(data)
	if err != nil {
		return err
	}
	stacks, err := sf.mergeInto(disk.Stacks)
	if err != nil {
		return &StaleError{Err: fmt.Errorf("stack file was modified by another process since it was loaded: %w", err)}
	}
	merged := &StackFile{Repository: disk.Repository, Stacks: stacks}
	if sf.Repository != sf.syncedRepository {
		merged.Repository = sf.Repository
	}
	if err := writeStackFile(gitDir, merged); err != nil {
		return err
	}
	// The view keeps its own stacks, so pointers into them stay valid. It no
	// longer mirrors the file.
	sf.loadChecksum = merged.loadChecksum
	sf.diverged = true
	return sf.markSynced()
}

// SaveWithLock writes the stack file while the caller already holds the catalog
// lock (not merely the operation lock).
// The caller is responsible for acquiring and releasing the lock.
// Panics if lock is nil to catch programming errors.
func SaveWithLock(gitDir string, sf *StackFile, lock *FileLock) error {
	if lock == nil {
		panic("SaveWithLock called with nil lock")
	}
	return writeStackFile(gitDir, sf)
}

// SaveNonBlocking attempts a best-effort metadata refresh without waiting for
// either the operation or catalog lock. Contention, stale data, I/O errors,
// and a change to a stack that another operation claims all skip the refresh.
// It is the only way to write a stack without holding a claim on it. Use Save
// for critical writes.
func SaveNonBlocking(gitDir string, sf *StackFile) {
	operation, acquired, err := TryLockOperation(gitDir)
	if err != nil || !acquired {
		return
	}
	defer operation.Unlock()

	lock, acquired, err := acquireLock(filepath.Join(gitDir, lockFileName), "stack", false)
	if err != nil || !acquired {
		return
	}
	defer lock.Unlock()

	// Do not change a published migration snapshot before its backups finish.
	if _, err := os.Lstat(filepath.Join(gitDir, migrationFileName)); !errors.Is(err, os.ErrNotExist) {
		return
	}
	// An operation claims its stacks before it loads them and saves them
	// when it ends. Writing one in between would invalidate that save.
	branches, err := sf.changedBranches()
	if err != nil {
		return
	}
	if claimed, err := claimedByOthers(gitDir, branches); err != nil || claimed {
		return
	}
	_ = saveLocked(gitDir, sf)
}

func writeStackFile(gitDir string, sf *StackFile) error {
	data, err := marshalStackFile(sf)
	if err != nil {
		return err
	}
	if err := WriteAtomic(stackFilePath(gitDir), data); err != nil {
		return fmt.Errorf("writing stack file: %w", err)
	}
	// Refresh the synced state so a second Save on the same StackFile
	// publishes only what changed after this one.
	sum := sha256.Sum256(data)
	sf.loadChecksum = sum[:]
	return sf.markSynced()
}

func marshalStackFile(sf *StackFile) ([]byte, error) {
	sf.SchemaVersion = schemaVersion
	if sf.Stacks == nil {
		sf.Stacks = []Stack{}
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling stack file: %w", err)
	}
	return data, nil
}
