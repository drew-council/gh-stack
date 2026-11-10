package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/github/gh-stack/internal/worktree"
)

// A command coordinates with other gh-stack processes through its claim: the
// branches of the stack it works on and the worktrees it uses. Claims of
// different operations never overlap, so commands on independent stacks run
// at the same time. A command that pauses on a conflict leaves its record
// behind, and its claim stands until the operation is continued or aborted.

// mutationRewrites reports whether the command rewrites stack branches in the
// worktrees that hold them, and journals its progress.
func mutationRewrites(kind string) bool {
	switch kind {
	case "rebase", "sync", "modify":
		return true
	}
	return false
}

// recoveryJournalKind names the journal a --continue or --abort resumes.
func recoveryJournalKind(kind string) string {
	for _, journal := range []string{"rebase", "modify"} {
		if kind == journal+"-continue" || kind == journal+"-abort" {
			return journal
		}
	}
	return ""
}

func journalFileName(kind string) string {
	if kind == "modify" {
		return modify.StateFileName
	}
	return rebaseStateFile
}

// claimsApply is false for recovery of state that predates claims: a private
// worktree catalog, or a journal beside the shared catalog. Both block every
// other mutation by themselves.
func claimsApply(m *config.StackMutationContext) bool {
	if m == nil || m.CommonDir == "" || !worktree.SamePath(m.StateDir, m.CommonDir) {
		return false
	}
	return !(m.UnclaimedJournal && recoveryJournalKind(m.Kind) != "")
}

func claimPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func currentWorktree() string {
	root, err := git.RootDir()
	if err != nil || root == "" {
		return ""
	}
	return claimPath(root)
}

// retryWhileBusy runs attempt under the operation lock until it stops
// reporting a running operation in its way, for at most stack.LockTimeout.
// The lock is never held while waiting.
func retryWhileBusy(cfg *config.Config, commonDir string, attempt func() (busy *stack.Operation, err error)) error {
	deadline := time.Now().Add(stack.LockTimeout)
	for {
		lock, err := stack.LockOperation(commonDir)
		if err != nil {
			return stackStateError(cfg, "acquiring stack operation lock", err)
		}
		busy, err := attempt()
		lock.Unlock()
		if err != nil || busy == nil {
			return err
		}
		if time.Now().After(deadline) {
			return reportBusyOperation(cfg, busy)
		}
		time.Sleep(stack.LockRetryInterval)
	}
}

func reportBusyOperation(cfg *config.Config, busy *stack.Operation) error {
	cfg.Errorf("another process is currently editing the stack — try again later")
	command := "gh stack " + strings.Replace(busy.Claim.Kind, "-", " --", 1)
	if busy.Claim.Origin != "" {
		cfg.Printf("`%s` is running in worktree %s", command, busy.Claim.Origin)
	} else {
		cfg.Printf("`%s` is running", command)
	}
	return errors.Join(ErrLockFailed, &stack.LockError{Err: fmt.Errorf("%s holds an overlapping claim", command)})
}

// claim reserves the branches and worktrees for the running command. It waits
// briefly for a running operation that holds any of them, and refuses at once
// when a paused operation does.
func claim(cfg *config.Config, branches, worktrees []string) error {
	m := cfg.StackMutation
	if !claimsApply(m) {
		return nil
	}
	branches, worktrees = unclaimed(m, branches, worktrees)
	if len(branches)+len(worktrees) == 0 {
		return nil
	}
	return retryWhileBusy(cfg, m.CommonDir, func() (*stack.Operation, error) {
		ops, err := stack.ListOperations(m.CommonDir)
		if err != nil {
			cfg.Errorf("%s", err)
			return nil, ErrSilent
		}
		for _, op := range ops {
			if op.Held() || !op.Claims(branches, worktrees) {
				continue
			}
			if op.Running {
				return op, nil
			}
			if err := checkPausedOperation(cfg, op); err != nil {
				return nil, err
			}
		}
		return nil, extendClaim(cfg, m, branches, worktrees)
	})
}

func unclaimed(m *config.StackMutationContext, branches, worktrees []string) ([]string, []string) {
	if m.Operation == nil {
		return branches, worktrees
	}
	held := m.Operation.Claim
	missing := func(values, have []string) []string {
		var out []string
		for _, value := range values {
			if !slices.Contains(have, value) {
				out = append(out, value)
			}
		}
		return out
	}
	return missing(branches, held.Branches), missing(worktrees, held.Worktrees)
}

// extendClaim adds to the command's claim, starting its operation record on
// first use. The caller must hold the operation lock and have found the
// additions free.
func extendClaim(cfg *config.Config, m *config.StackMutationContext, branches, worktrees []string) error {
	if m.Operation == nil {
		op, err := stack.StartOperation(m.CommonDir, stack.Claim{
			Kind:      m.Kind,
			Origin:    currentWorktree(),
			PID:       os.Getpid(),
			StartedAt: time.Now().UTC(),
			Branches:  branches,
			Worktrees: worktrees,
		})
		if err != nil {
			cfg.Errorf("%s", err)
			return ErrSilent
		}
		m.Operation = op
		if mutationRewrites(m.Kind) && !m.UnclaimedJournal {
			m.RestoreJournal = stack.SetJournalDir(m.StateDir, op.Dir)
		}
		return nil
	}
	next := m.Operation.Claim
	next.Branches = append(slices.Clone(next.Branches), branches...)
	next.Worktrees = append(slices.Clone(next.Worktrees), worktrees...)
	if err := m.Operation.SetClaim(next); err != nil {
		cfg.Errorf("%s", err)
		return ErrSilent
	}
	return nil
}

// checkPausedOperation reports the recovery a paused operation needs. A
// completed modify that only awaits submit blocks nothing: its claim is
// dropped if the command that completed it could not.
func checkPausedOperation(cfg *config.Config, op *stack.Operation) error {
	journals, err := readJournals(cfg, []string{op.Dir})
	if err != nil {
		return err
	}
	for _, journal := range journals {
		if journal.kind == "modify" && journal.phase == modify.PhasePendingSubmit {
			continue
		}
		return reportActiveJournal(cfg, journal)
	}
	if len(journals) == 0 {
		// Whatever keeps the record is not a journal this build can read.
		cfg.Errorf("operation record %s holds recovery state that cannot be read; restore or remove it before another mutation", op.Dir)
		return ErrSilent
	}
	if acquired, err := op.Resume(); err == nil && acquired {
		parkOperation(op)
		op.Suspend()
	}
	return nil
}

func parkOperation(op *stack.Operation) {
	parked := op.Claim
	parked.Branches, parked.Worktrees = nil, nil
	_ = op.SetClaim(parked)
}

func reportActiveJournal(cfg *config.Config, journal stackJournal) error {
	cfg.Errorf("a %s operation is already in progress (%s)", journal.kind, journal.path)
	if journal.origin != "" {
		cfg.Printf("Operation started in worktree %s", journal.origin)
	}
	cfg.Printf("Run `gh stack %s --continue` or `gh stack %s --abort` before another mutation", journal.kind, journal.kind)
	if journal.kind == "rebase" {
		return ErrRebaseActive
	}
	return ErrModifyRecovery
}

// claimStack reserves s for the running command and returns the catalog as it
// stands under that claim. A rewriting command also reserves the worktrees
// that hold the stack's branches.
//
// The catalog is reloaded because another command may have changed the stack
// between the caller's load and the claim. Callers must not have changed sf.
func claimStack(cfg *config.Config, gitDir string, sf *stack.StackFile, s *stack.Stack) (*stack.StackFile, *stack.Stack, error) {
	m := cfg.StackMutation
	if s == nil || !claimsApply(m) {
		return sf, s, nil
	}
	// What the stack needs can change until the claim covers it: another
	// command may restructure the stack, or check one of its branches out
	// elsewhere, while this one waits. Claim, look again, and stop once
	// nothing is missing.
	for range 4 {
		branches, worktrees, err := stackClaim(cfg, m, sf, s)
		if err != nil {
			return nil, nil, err
		}
		if missingBranches, missingWorktrees := unclaimed(m, branches, worktrees); len(missingBranches)+len(missingWorktrees) == 0 {
			return sf, s, nil
		}
		if err := claim(cfg, branches, worktrees); err != nil {
			return nil, nil, err
		}
		fresh, err := stack.Load(gitDir)
		if err != nil {
			cfg.Errorf("failed to load stack state: %s", err)
			return nil, nil, fmt.Errorf("failed to load stack state: %w", err)
		}
		names := s.BranchNames()
		var same, successor *stack.Stack
		for i := range fresh.Stacks {
			candidate := &fresh.Stacks[i]
			if candidate.Trunk.Branch == s.Trunk.Branch && slices.Equal(candidate.BranchNames(), names) {
				same = candidate
				break
			}
			if successor == nil && slices.ContainsFunc(names, func(name string) bool { return candidate.IndexOf(name) >= 0 }) {
				successor = candidate
			}
		}
		if same == nil {
			// The stack gained, lost, or renamed branches.
			same = successor
		}
		if same == nil {
			break
		}
		sf, s = fresh, same
	}
	cfg.Errorf("the stack changed while this command was starting — try again")
	return nil, nil, ErrLockFailed
}

// reclaimStack extends the command's claim to what s needs now, after the
// command itself changed the stack. The command keeps its view of the catalog.
func reclaimStack(cfg *config.Config, sf *stack.StackFile, s *stack.Stack) error {
	m := cfg.StackMutation
	if !claimsApply(m) {
		return nil
	}
	branches, worktrees, err := stackClaim(cfg, m, sf, s)
	if err != nil {
		return err
	}
	return claim(cfg, branches, worktrees)
}

// stackClaim lists what a command needs to work on s.
func stackClaim(cfg *config.Config, m *config.StackMutationContext, sf *stack.StackFile, s *stack.Stack) (branches, worktrees []string, err error) {
	names := s.BranchNames()
	branches = append(slices.Clone(names), dependentTrunk(sf, s.Trunk.Branch)...)
	if !mutationRewrites(m.Kind) {
		return branches, nil, nil
	}
	trees, err := git.Worktrees()
	if err != nil {
		cfg.Errorf("listing worktrees: %s", err)
		return nil, nil, ErrSilent
	}
	for _, tree := range trees {
		if !tree.Bare && tree.Path != "" && slices.Contains(names, tree.Branch) {
			worktrees = append(worktrees, claimPath(tree.Path))
		}
	}
	// The command returns this worktree to the branch it started on.
	if current, err := git.CurrentBranch(); err == nil && current != "" {
		branches = append(branches, current)
	}
	return branches, worktrees, nil
}

// dependentTrunk returns trunk when it is a branch of a tracked stack. A
// stack built on such a branch moves with it, so the two share one claim.
func dependentTrunk(sf *stack.StackFile, trunk string) []string {
	for i := range sf.Stacks {
		if sf.Stacks[i].IndexOf(trunk) >= 0 {
			return []string{trunk}
		}
	}
	return nil
}

// tryClaimWorktree reserves one more worktree for a short step without
// waiting. It reports false when another operation holds the worktree. The
// release function is nil when the command already held the worktree.
func tryClaimWorktree(cfg *config.Config, path string) (release func(), ok bool) {
	m := cfg.StackMutation
	if !claimsApply(m) {
		return nil, true
	}
	path = claimPath(path)
	if m.Operation != nil && slices.Contains(m.Operation.Claim.Worktrees, path) {
		return nil, true
	}
	lock, err := stack.LockOperation(m.CommonDir)
	if err != nil {
		return nil, false
	}
	defer lock.Unlock()
	ops, err := stack.ListOperations(m.CommonDir)
	if err != nil {
		return nil, false
	}
	for _, op := range ops {
		if !op.Held() && op.Claims(nil, []string{path}) {
			return nil, false
		}
	}
	if err := extendClaim(cfg, m, nil, []string{path}); err != nil {
		return nil, false
	}
	m.Borrowed = append(m.Borrowed, path)
	return func() {
		if lock, err := stack.LockOperation(m.CommonDir); err == nil {
			returnBorrowed(m)
			lock.Unlock()
		}
		// Otherwise endMutation returns it.
	}, true
}

// returnBorrowed drops the worktrees claimed for a short step. A command
// that pauses must not keep them. The caller must hold the operation lock.
func returnBorrowed(m *config.StackMutationContext) {
	if m.Operation == nil || !m.Operation.Held() || len(m.Borrowed) == 0 {
		return
	}
	next := m.Operation.Claim
	next.Worktrees = slices.DeleteFunc(slices.Clone(next.Worktrees), func(held string) bool {
		return slices.Contains(m.Borrowed, held)
	})
	if m.Operation.SetClaim(next) == nil {
		m.Borrowed = nil
	}
}

// resumeOperation takes over the paused operation that a --continue or
// --abort should recover, and binds its journal.
func resumeOperation(cfg *config.Config, m *config.StackMutationContext, kind string) error {
	here := currentWorktree()
	current, err := git.CurrentBranch()
	if err != nil {
		current = ""
	}
	return retryWhileBusy(cfg, m.CommonDir, func() (*stack.Operation, error) {
		ops, err := stack.ListOperations(m.CommonDir)
		if err != nil {
			cfg.Errorf("%s", err)
			return nil, ErrSilent
		}
		var active, pending []*stack.Operation
		for _, op := range ops {
			if !slices.Contains(op.Journals, journalFileName(kind)) {
				continue
			}
			journals, err := readJournals(cfg, []string{op.Dir})
			if err != nil {
				return nil, err
			}
			for _, journal := range journals {
				if journal.kind != kind {
					continue
				}
				if op.ClaimErr != nil && journal.origin != "" {
					// The journal still says where the operation started.
					op.Claim.Origin = claimPath(journal.origin)
				}
				if kind == "modify" && journal.phase == modify.PhasePendingSubmit {
					pending = append(pending, op)
				} else {
					active = append(active, op)
				}
			}
		}
		chosen, err := chooseOperation(cfg, kind, active, pending, here, current)
		if err != nil || chosen == nil {
			return nil, err
		}
		acquired, err := chosen.Resume()
		if err != nil {
			cfg.Errorf("resuming %s operation: %s", kind, err)
			return nil, ErrSilent
		}
		if !acquired {
			return chosen, nil
		}
		m.Operation = chosen
		m.RestoreJournal = stack.SetJournalDir(m.StateDir, chosen.Dir)
		return nil, nil
	})
}

// chooseOperation picks the operation to recover: the one that uses this
// worktree or holds the branch checked out here, or else the only one in the
// clone.
func chooseOperation(cfg *config.Config, kind string, active, pending []*stack.Operation, here, current string) (*stack.Operation, error) {
	pool := active
	if len(pool) == 0 {
		// A completed modify has nothing to recover, but the command still
		// reports it.
		pool = pending
	}
	var local []*stack.Operation
	for _, op := range pool {
		usesWorktree := here != "" && op.UsesWorktree(here)
		holdsBranch := current != "" && slices.Contains(op.Claim.Branches, current)
		if usesWorktree || holdsBranch {
			local = append(local, op)
		}
	}
	if len(local) == 1 {
		return local[0], nil
	}
	if len(pool) == 1 {
		return pool[0], nil
	}
	if len(active) == 0 {
		return nil, nil
	}
	cfg.Errorf("%d %s operations are paused, and this worktree does not identify one", len(active), kind)
	cfg.Printf("Run this command from a worktree of the operation to recover, or with one of its branches checked out:")
	for _, op := range active {
		if op.ClaimErr != nil {
			cfg.Printf("  the operation started in %s (its claim is unreadable: %s)", op.Claim.Origin, op.Dir)
			continue
		}
		cfg.Printf("  %s (worktrees: %s)", strings.Join(op.Claim.Branches, ", "), strings.Join(op.Claim.Worktrees, ", "))
	}
	if kind == "rebase" {
		return nil, ErrRebaseActive
	}
	return nil, ErrModifyRecovery
}

// findPendingModify returns the record of a completed modify of s that still
// awaits submit. The caller must hold the operation lock.
func findPendingModify(commonDir string, s *stack.Stack) (*stack.Operation, error) {
	ops, err := stack.ListOperations(commonDir)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if !slices.Contains(op.Journals, modify.StateFileName) {
			continue
		}
		state, err := readModifyState(op.Dir)
		if err == nil && state != nil && state.Phase == modify.PhasePendingSubmit && modify.MatchesStack(state, s) {
			return op, nil
		}
	}
	return nil, nil
}

func readModifyState(dir string) (*modify.StateFile, error) {
	data, err := stack.ReadStateFile(filepath.Join(dir, modify.StateFileName))
	if err != nil {
		return nil, err
	}
	var state *modify.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return state, nil
}

// adoptPendingModify binds the journal of a completed modify of s, if one
// awaits submit, so the running command can consume it.
func adoptPendingModify(cfg *config.Config, s *stack.Stack) error {
	m := cfg.StackMutation
	if !claimsApply(m) || m.Adopted != nil {
		return nil
	}
	return retryWhileBusy(cfg, m.CommonDir, func() (*stack.Operation, error) {
		op, err := findPendingModify(m.CommonDir, s)
		if err != nil {
			cfg.Errorf("%s", err)
			return nil, ErrSilent
		}
		if op == nil || op.Held() {
			return nil, nil
		}
		acquired, err := op.Resume()
		if err != nil {
			cfg.Errorf("reading modify recovery state: %s", err)
			return nil, ErrModifyRecovery
		}
		if !acquired {
			return op, nil
		}
		m.Adopted = op
		if m.RestoreJournal != nil {
			m.RestoreJournal()
		}
		m.RestoreJournal = stack.SetJournalDir(m.StateDir, op.Dir)
		return nil, nil
	})
}

// hasPendingModify reports whether a completed modify of s awaits submit in
// another operation record.
func hasPendingModify(cfg *config.Config, s *stack.Stack) (bool, error) {
	m := cfg.StackMutation
	if !claimsApply(m) {
		return false, nil
	}
	lock, err := stack.LockOperation(m.CommonDir)
	if err != nil {
		return false, stackStateError(cfg, "acquiring stack operation lock", err)
	}
	defer lock.Unlock()
	op, err := findPendingModify(m.CommonDir, s)
	if err != nil {
		cfg.Errorf("%s", err)
		return false, ErrSilent
	}
	return op != nil && !op.Held(), nil
}

// startMutation gives a recovery command the operation it resumes, and any
// other command a claim on the worktree it runs in. One mutation per worktree
// keeps a command out of a checkout that a running or paused operation uses,
// whichever stack either of them works on.
func startMutation(cfg *config.Config, m *config.StackMutationContext) error {
	if !claimsApply(m) {
		return nil
	}
	if journal := recoveryJournalKind(m.Kind); journal != "" {
		return resumeOperation(cfg, m, journal)
	}
	if here := currentWorktree(); here != "" {
		return claim(cfg, nil, []string{here})
	}
	return nil
}

// endMutation releases the command's records. A record with a journal is a
// paused operation and stays, as does its claim.
func endMutation(m *config.StackMutationContext) {
	if m.RestoreJournal != nil {
		m.RestoreJournal()
	}
	if m.Operation == nil && m.Adopted == nil {
		return
	}
	lock, err := stack.LockOperation(m.CommonDir)
	if err == nil {
		returnBorrowed(m)
	}
	for _, op := range []*stack.Operation{m.Operation, m.Adopted} {
		if op == nil {
			continue
		}
		if err != nil || op.RefreshJournals() != nil {
			// Left as debris for the next command to collect.
			op.Suspend()
			continue
		}
		switch {
		case len(op.Journals) == 0:
			_ = op.Remove()
		case awaitsSubmitOnly(op):
			parkOperation(op)
			op.Suspend()
		default:
			op.Suspend()
		}
	}
	if err == nil {
		// Fails while other records remain, which is the intent.
		_ = os.Remove(stack.OperationsDir(m.CommonDir))
		lock.Unlock()
	}
}

func awaitsSubmitOnly(op *stack.Operation) bool {
	if len(op.Journals) != 1 || op.Journals[0] != modify.StateFileName {
		return false
	}
	state, err := readModifyState(op.Dir)
	return err == nil && state != nil && state.Phase == modify.PhasePendingSubmit
}
