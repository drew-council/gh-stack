package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stacksRepo is a clone whose main worktree stays on main, with one linked
// worktree per independent stack. Stack "a" has branches a1 and a2, and its
// worktree has a2 checked out, so a1 is rebased through that worktree too.
type stacksRepo struct {
	root   string
	common string
	remote string
	pusher string
	trees  map[string]string
}

func setupStacksRepo(t *testing.T, names ...string) stacksRepo {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	root := filepath.Join(t.TempDir(), "clone")
	issue250Git(t, ".", "-c", "safe.bareRepository=all", "init", "--bare", "-b", "main", remote)
	issue250Git(t, ".", "-c", "safe.bareRepository=all", "-C", remote, "config", "receive.advertisePushOptions", "true")
	issue250Git(t, ".", "clone", remote, root)
	for key, value := range map[string]string{
		"user.name": "Test", "user.email": "test@example.com",
		"commit.gpgSign": "false", "core.hooksPath": os.DevNull, "rerere.enabled": "true",
	} {
		issue250Git(t, root, "config", key, value)
	}
	issue250WriteFile(t, root, "base.txt", "base\n")
	issue250Git(t, root, "add", ".")
	issue250Git(t, root, "commit", "-m", "base")
	issue250Git(t, root, "push", "-u", "origin", "main")
	mainSHA := issue250Git(t, root, "rev-parse", "main")

	repo := stacksRepo{
		root: root, common: filepath.Join(root, ".git"), remote: remote,
		pusher: filepath.Join(t.TempDir(), "pusher"), trees: make(map[string]string),
	}
	var stacks []stack.Stack
	for _, name := range names {
		tree := filepath.Join(t.TempDir(), "wt-"+name)
		lower, upper := name+"1", name+"2"
		issue250Git(t, root, "worktree", "add", "-b", lower, tree, "main")
		issue250WriteFile(t, tree, lower+".txt", lower+"\n")
		issue250Git(t, tree, "add", ".")
		issue250Git(t, tree, "commit", "-m", lower)
		lowerSHA := issue250Git(t, tree, "rev-parse", "HEAD")
		issue250Git(t, tree, "checkout", "-b", upper)
		issue250WriteFile(t, tree, upper+".txt", upper+"\n")
		issue250Git(t, tree, "add", ".")
		issue250Git(t, tree, "commit", "-m", upper)
		issue250Git(t, tree, "push", "-u", "origin", lower, upper)
		repo.trees[name] = tree
		stacks = append(stacks, stack.Stack{
			Trunk: stack.BranchRef{Branch: "main", Head: mainSHA},
			Branches: []stack.BranchRef{
				{Branch: lower, Base: mainSHA},
				{Branch: upper, Base: lowerSHA},
			},
		})
	}
	writeStackFileMulti(t, repo.common, stacks...)
	return repo
}

// advanceTrunk moves main on the remote from another clone. This clone is
// left with a stale tracking ref and without the new commit, so every command
// has a real fetch to do.
func (r stacksRepo) advanceTrunk(t *testing.T, content string) string {
	t.Helper()
	if _, err := os.Stat(r.pusher); err != nil {
		issue250Git(t, ".", "clone", r.remote, r.pusher)
		issue250Git(t, r.pusher, "config", "commit.gpgSign", "false")
	}
	issue250Git(t, r.pusher, "pull", "origin", "main")
	issue250WriteFile(t, r.pusher, "base.txt", content)
	issue250Git(t, r.pusher, "commit", "-am", "trunk: "+strings.TrimSpace(content))
	issue250Git(t, r.pusher, "push", "origin", "main")
	return issue250Git(t, r.pusher, "rev-parse", "HEAD")
}

// conflict makes the stack's upper branch collide with a trunk change.
func (r stacksRepo) conflict(t *testing.T, name string) {
	t.Helper()
	issue250WriteFile(t, r.trees[name], "base.txt", name+" change\n")
	issue250Git(t, r.trees[name], "commit", "-am", name+" conflicts with trunk")
}

func (r stacksRepo) resolve(t *testing.T, name string) {
	t.Helper()
	issue250WriteFile(t, r.trees[name], "base.txt", "resolved by "+name+"\n")
	issue250Git(t, r.trees[name], "add", "base.txt")
}

// requireStacked asserts that the stack sits on the given trunk commit and
// that the catalog records it so.
func (r stacksRepo) requireStacked(t *testing.T, name, trunkSHA string) {
	t.Helper()
	lower, upper := name+"1", name+"2"
	require.NoError(t, issue250GitMayFail(t, r.root, "merge-base", "--is-ancestor", trunkSHA, lower), "%s must contain the trunk", lower)
	require.NoError(t, issue250GitMayFail(t, r.root, "merge-base", "--is-ancestor", lower, upper), "%s must contain %s", upper, lower)
	sf, err := stack.Load(r.common)
	require.NoError(t, err)
	stacks := sf.FindAllStacksForBranch(lower)
	require.Len(t, stacks, 1)
	assert.Equal(t, trunkSHA, stacks[0].Branches[0].Base)
	assert.Equal(t, issue250Git(t, r.root, "rev-parse", lower), stacks[0].Branches[1].Base)
}

// runIn runs a command from dir and returns everything it printed.
func runIn(t *testing.T, dir string, run func(cfg *config.Config) error) (string, error) {
	t.Helper()
	original, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	defer func() { require.NoError(t, os.Chdir(original)) }()
	cfg, outR, errR := config.NewTestConfig()
	defer outR.Close()
	defer errR.Close()
	runErr := run(cfg)
	require.NoError(t, cfg.Out.Close())
	require.NoError(t, cfg.Err.Close())
	out, err := io.ReadAll(outR)
	require.NoError(t, err)
	errOut, err := io.ReadAll(errR)
	require.NoError(t, err)
	return string(out) + string(errOut), runErr
}

func rebaseIn(t *testing.T, dir string, opts rebaseOptions) (string, error) {
	t.Helper()
	if !opts.cont && !opts.abort {
		opts.remote = "origin"
	}
	return runIn(t, dir, func(cfg *config.Config) error { return runRebase(cfg, &opts) })
}

func pushIn(t *testing.T, dir string) (string, error) {
	t.Helper()
	return runIn(t, dir, func(cfg *config.Config) error { return runPush(cfg, &pushOptions{remote: "origin"}) })
}

func readClaim(t *testing.T, record string) stack.Claim {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(record, "claim.json"))
	require.NoError(t, err)
	var claim stack.Claim
	require.NoError(t, json.Unmarshal(data, &claim))
	return claim
}

func shortLockTimeout(t *testing.T) {
	t.Helper()
	original := stack.LockTimeout
	stack.LockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { stack.LockTimeout = original })
}

// TestOperationHolderProcess is not a test. It is the body of the process
// that holdOperation starts to stand in for a running gh-stack command.
func TestOperationHolderProcess(t *testing.T) {
	spec := os.Getenv("GH_STACK_TEST_HOLD_OPERATION")
	if spec == "" {
		t.Skip("helper process only")
	}
	var hold struct {
		Common string
		Claim  stack.Claim
	}
	require.NoError(t, json.Unmarshal([]byte(spec), &hold))
	lock, err := stack.LockOperation(hold.Common)
	require.NoError(t, err)
	_, err = stack.StartOperation(hold.Common, hold.Claim)
	require.NoError(t, err)
	lock.Unlock()
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

// holdOperation holds a claim from another process until the returned
// function kills that process, as a crash would.
func holdOperation(t *testing.T, common string, claim stack.Claim) (kill func()) {
	t.Helper()
	spec, err := json.Marshal(map[string]any{"Common": common, "Claim": claim})
	require.NoError(t, err)
	holder := exec.Command(os.Args[0], "-test.run=^TestOperationHolderProcess$")
	holder.Env = append(os.Environ(), "GH_STACK_TEST_HOLD_OPERATION="+string(spec))
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	var once sync.Once
	kill = func() {
		once.Do(func() {
			_ = holder.Process.Kill()
			_ = holder.Wait()
		})
	}
	t.Cleanup(kill)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	return kill
}

func TestOperations_PausedRebaseBlocksOnlyItsStack(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	trunk := repo.advanceTrunk(t, "trunk change\n")
	repo.conflict(t, "a")

	out, err := rebaseIn(t, repo.trees["a"], rebaseOptions{})
	require.ErrorIs(t, err, ErrConflict, out)
	record := pausedOperationDir(t, repo.common)
	assert.NoFileExists(t, filepath.Join(repo.common, rebaseStateFile), "the journal belongs to the operation, not the clone")
	state, err := loadRebaseState(record)
	require.NoError(t, err)
	assert.Equal(t, []string{"a1", "a2"}, state.StackBranches)
	claimed := readClaim(t, record)
	assert.Equal(t, []string{"a1", "a2"}, claimed.Branches)
	assert.Equal(t, []string{claimPath(repo.trees["a"])}, claimed.Worktrees, "the main worktree was only borrowed to fast-forward the trunk")
	assert.Equal(t, trunk, issue250Git(t, repo.root, "rev-parse", "main"))

	// The other stack rebases and pushes while the first one is paused.
	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.NoError(t, err, out)
	repo.requireStacked(t, "b", trunk)
	out, err = pushIn(t, repo.trees["b"])
	require.NoError(t, err, out)
	assert.Equal(t, issue250Git(t, repo.root, "rev-parse", "b2"), issue250Git(t, repo.root, "rev-parse", "origin/b2"))
	assert.Equal(t, []string{record}, operationDirs(t, repo.common), "a finished command leaves no record")

	// The paused stack and its worktree stay reserved, from anywhere.
	for name, attempt := range map[string]func() (string, error){
		"push from the paused worktree":   func() (string, error) { return pushIn(t, repo.trees["a"]) },
		"rebase from the paused worktree": func() (string, error) { return rebaseIn(t, repo.trees["a"], rebaseOptions{}) },
		"rebase by name from another worktree": func() (string, error) {
			return rebaseIn(t, repo.trees["b"], rebaseOptions{branch: "a1"})
		},
		"checkout of a paused branch in another worktree": func() (string, error) {
			return runIn(t, repo.trees["b"], func(cfg *config.Config) error { return checkoutWorktreeBranch(cfg, "a1", false) })
		},
	} {
		out, err := attempt()
		require.ErrorIs(t, err, ErrRebaseActive, "%s: %s", name, out)
		assert.Contains(t, out, "a rebase operation is already in progress", name)
		assert.Contains(t, out, "gh stack rebase --continue", name)
	}
	assert.Equal(t, "b2", issue250Git(t, repo.trees["b"], "branch", "--show-current"))

	// Recovery of the only paused operation works from an uninvolved worktree.
	repo.resolve(t, "a")
	out, err = rebaseIn(t, repo.root, rebaseOptions{cont: true})
	require.NoError(t, err, out)
	repo.requireStacked(t, "a", trunk)
	repo.requireStacked(t, "b", trunk)
	assert.Equal(t, "a2", issue250Git(t, repo.trees["a"], "branch", "--show-current"))
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
	assertNoJournal(t, repo.common, rebaseStateFile)
}

func TestOperations_TwoPausedRebasesRecoverIndependently(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	trunk := repo.advanceTrunk(t, "trunk change\n")
	repo.conflict(t, "a")
	repo.conflict(t, "b")
	before := issue250Git(t, repo.root, "rev-parse", "b1", "b2")

	out, err := rebaseIn(t, repo.trees["a"], rebaseOptions{})
	require.ErrorIs(t, err, ErrConflict, out)
	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.ErrorIs(t, err, ErrConflict, out)
	require.Len(t, operationDirs(t, repo.common), 2)

	// Neither operation uses the main worktree, so it cannot pick one.
	for _, opts := range []rebaseOptions{{cont: true}, {abort: true}} {
		out, err = rebaseIn(t, repo.root, opts)
		require.ErrorIs(t, err, ErrRebaseActive, out)
		assert.Contains(t, out, "2 rebase operations are paused")
		assert.Contains(t, out, repo.trees["a"])
		assert.Contains(t, out, repo.trees["b"])
	}
	require.Len(t, operationDirs(t, repo.common), 2)

	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{abort: true})
	require.NoError(t, err, out)
	assert.Equal(t, before, issue250Git(t, repo.root, "rev-parse", "b1", "b2"), "abort restores its own stack")
	assert.Equal(t, "b2", issue250Git(t, repo.trees["b"], "branch", "--show-current"))
	record := pausedOperationDir(t, repo.common)
	state, err := loadRebaseState(record)
	require.NoError(t, err)
	assert.Equal(t, []string{"a1", "a2"}, state.StackBranches, "the other operation is untouched")

	repo.resolve(t, "a")
	out, err = rebaseIn(t, repo.trees["a"], rebaseOptions{cont: true})
	require.NoError(t, err, out)
	repo.requireStacked(t, "a", trunk)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))

	// The aborted stack is free again.
	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.ErrorIs(t, err, ErrConflict, out)
	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{abort: true})
	require.NoError(t, err, out)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

func TestOperations_StackOnAnotherStackSharesItsClaim(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	// Stack c is built on a2, a branch of stack a.
	tree := filepath.Join(t.TempDir(), "wt-c")
	issue250Git(t, repo.root, "worktree", "add", "-b", "c1", tree, "a2")
	issue250WriteFile(t, tree, "c1.txt", "c1\n")
	issue250Git(t, tree, "add", ".")
	issue250Git(t, tree, "commit", "-m", "c1")
	sf, err := stack.Load(repo.common)
	require.NoError(t, err)
	sf.AddStack(stack.Stack{
		Trunk:    stack.BranchRef{Branch: "a2"},
		Branches: []stack.BranchRef{{Branch: "c1", Base: issue250Git(t, repo.root, "rev-parse", "a2")}},
	})
	require.NoError(t, stack.Save(repo.common, sf))

	trunk := repo.advanceTrunk(t, "trunk change\n")
	repo.conflict(t, "a")
	// a2 is in two stacks now, so name a branch that is only in stack a.
	out, err := rebaseIn(t, repo.trees["a"], rebaseOptions{branch: "a1"})
	require.ErrorIs(t, err, ErrConflict, out)

	// c would rebase onto a branch that is being rewritten. b is unrelated.
	out, err = rebaseIn(t, tree, rebaseOptions{noTrunk: true})
	require.ErrorIs(t, err, ErrRebaseActive, out)
	out, err = rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.NoError(t, err, out)
	repo.requireStacked(t, "b", trunk)

	out, err = rebaseIn(t, repo.trees["a"], rebaseOptions{abort: true})
	require.NoError(t, err, out)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

func TestOperations_OriginBranchIsReserved(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	trunk := repo.advanceTrunk(t, "trunk change\n")
	repo.conflict(t, "a")

	// Rebase stack a by name from the worktree of stack b. The command
	// borrows that worktree for a1 and pauses on a2 in its own worktree.
	out, err := rebaseIn(t, repo.trees["b"], rebaseOptions{branch: "a1"})
	require.ErrorIs(t, err, ErrConflict, out)
	assert.Equal(t, "a1", issue250Git(t, repo.trees["b"], "branch", "--show-current"))
	claim := readClaim(t, pausedOperationDir(t, repo.common))
	assert.Equal(t, []string{"a1", "a2", "b2"}, claim.Branches, "the origin returns to b2, so b2 is reserved")
	assert.ElementsMatch(t, []string{claimPath(repo.trees["a"]), claimPath(repo.trees["b"])}, claim.Worktrees)

	// Stack b cannot be rewritten elsewhere while its worktree is on loan.
	out, err = rebaseIn(t, repo.root, rebaseOptions{branch: "b1"})
	require.ErrorIs(t, err, ErrRebaseActive, out)

	repo.resolve(t, "a")
	out, err = rebaseIn(t, repo.trees["a"], rebaseOptions{cont: true})
	require.NoError(t, err, out)
	assert.Equal(t, "b2", issue250Git(t, repo.trees["b"], "branch", "--show-current"))
	repo.requireStacked(t, "a", trunk)
	out, err = rebaseIn(t, repo.root, rebaseOptions{branch: "b1"})
	require.NoError(t, err, out)
	repo.requireStacked(t, "b", trunk)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

// A branch of the stack can be checked out in another worktree while the
// command waits for its claim. The claim must cover that worktree too.
func TestOperations_ClaimCoversWorktreeThatTookABranchWhileWaiting(t *testing.T) {
	repo := setupStacksRepo(t, "a")
	repo.advanceTrunk(t, "trunk change\n")
	repo.conflict(t, "a")
	spare := filepath.Join(t.TempDir(), "spare")
	issue250Git(t, repo.root, "worktree", "add", "--detach", spare, "main")

	// Another command is moving the spare worktree onto a1.
	kill := holdOperation(t, repo.common, stack.Claim{
		Kind: "checkout", Origin: claimPath(spare), Branches: []string{"a1"}, Worktrees: []string{claimPath(spare)},
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(600 * time.Millisecond)
		issue250Git(t, spare, "checkout", "a1")
		kill()
	}()
	out, err := rebaseIn(t, repo.trees["a"], rebaseOptions{})
	<-done
	require.ErrorIs(t, err, ErrConflict, out)

	claim := readClaim(t, pausedOperationDir(t, repo.common))
	assert.ElementsMatch(t, []string{claimPath(repo.trees["a"]), claimPath(spare)}, claim.Worktrees)
	assert.Equal(t, "a1", issue250Git(t, spare, "branch", "--show-current"))
	out, err = rebaseIn(t, spare, rebaseOptions{abort: true})
	require.NoError(t, err, out)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

func TestOperations_NewBranchesAreClaimedBeforeTheyAreCreated(t *testing.T) {
	shortLockTimeout(t)
	repo := setupStacksRepo(t, "a")
	kill := holdOperation(t, repo.common, stack.Claim{Kind: "modify", Branches: []string{"taken"}})
	for name, attempt := range map[string]func(cfg *config.Config) error{
		"init": func(cfg *config.Config) error {
			return runInit(cfg, &initOptions{branches: []string{"fresh", "taken"}, base: "main"})
		},
		"add": func(cfg *config.Config) error { return runAdd(cfg, &addOptions{}, []string{"taken"}) },
	} {
		dir := repo.root
		if name == "add" {
			dir = repo.trees["a"]
		}
		out, err := runIn(t, dir, attempt)
		require.ErrorIs(t, err, ErrLockFailed, "%s: %s", name, out)
		for _, branch := range []string{"fresh", "taken"} {
			assert.Error(t, issue250GitMayFail(t, repo.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch), "%s must not create %s", name, branch)
		}
	}
	kill()
	out, err := runIn(t, repo.trees["a"], func(cfg *config.Config) error { return runAdd(cfg, &addOptions{}, []string{"taken"}) })
	require.NoError(t, err, out)
	sf, err := stack.Load(repo.common)
	require.NoError(t, err)
	assert.Equal(t, []string{"a1", "a2", "taken"}, sf.Stacks[0].BranchNames())
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

func TestChooseOperation(t *testing.T) {
	a := &stack.Operation{Dir: "a", Claim: stack.Claim{Origin: "/wt/a", Branches: []string{"a1", "a2"}, Worktrees: []string{"/wt/a"}}}
	b := &stack.Operation{Dir: "b", Claim: stack.Claim{Origin: "/wt/b", Branches: []string{"b1"}, Worktrees: []string{"/wt/b", "/wt/shared"}}}
	parked := &stack.Operation{Dir: "parked", Claim: stack.Claim{Origin: "/wt/a"}}
	// Its claim is unreadable, so it reserves everything. Only the origin
	// read from its journal identifies it.
	damaged := &stack.Operation{Dir: "damaged", ClaimErr: errors.New("unreadable"), Claim: stack.Claim{Origin: "/wt/damaged"}}
	for _, tc := range []struct {
		name            string
		active, pending []*stack.Operation
		here, current   string
		want            *stack.Operation
		wantErr         error
	}{
		{name: "nothing paused"},
		{name: "the only one, from anywhere", active: []*stack.Operation{a}, here: "/elsewhere", want: a},
		{name: "by origin worktree", active: []*stack.Operation{a, b}, here: "/wt/a", want: a},
		{name: "by claimed worktree", active: []*stack.Operation{a, b}, here: "/wt/shared", want: b},
		{name: "by checked-out branch when its worktrees are gone", active: []*stack.Operation{a, b}, here: "/elsewhere", current: "a2", want: a},
		{name: "ambiguous", active: []*stack.Operation{a, b}, here: "/elsewhere", current: "main", wantErr: ErrRebaseActive},
		{name: "an unreadable claim does not shadow another operation", active: []*stack.Operation{a, damaged}, here: "/wt/a", want: a},
		{name: "an unreadable claim is found by its origin", active: []*stack.Operation{a, damaged}, here: "/wt/damaged", want: damaged},
		{name: "a paused operation outranks a completed modify", active: []*stack.Operation{b}, pending: []*stack.Operation{parked}, here: "/wt/a", want: b},
		{name: "the only completed modify", pending: []*stack.Operation{parked}, here: "/elsewhere", want: parked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, outR, errR := config.NewTestConfig()
			got, err := chooseOperation(cfg, "rebase", tc.active, tc.pending, tc.here, tc.current)
			_, diagnostics := commandOutput(t, cfg, outR, errR)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, diagnostics, "2 rebase operations are paused")
				assert.Contains(t, diagnostics, "a1, a2 (worktrees: /wt/a)")
				return
			}
			require.NoError(t, err)
			assert.Same(t, tc.want, got)
		})
	}
}

// Two commands fetched different trunk tips. The one with the older tip must
// not move the trunk back once the other has moved it forward.
func TestFastForwardTrunk_NeverMovesTheTrunkBackwards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		want    string
		moves   []string
		wantErr string
	}{
		{name: "unchanged since it was read", current: "x0", want: "x1", moves: []string{"main=x1"}},
		{name: "already at the fetched tip", current: "x1", want: "x1"},
		{name: "already past the fetched tip", current: "x2", want: "x2"},
		{name: "moved, still behind the fetched tip", current: "x0b", want: "x1", moves: []string{"main=x1"}},
		{name: "diverged", current: "y", wantErr: "no longer fast-forwards"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ancestors := map[string]bool{"x1<x2": true, "x0b<x1": true, "x0<x1": true}
			var moves []string
			restore := git.SetOps(&git.MockOps{
				GitDirFn:          func() (string, error) { return t.TempDir(), nil },
				RevParseFn:        func(string) (string, error) { return tc.current, nil },
				IsAncestorFn:      func(a, d string) (bool, error) { return ancestors[a+"<"+d], nil },
				UpdateBranchRefFn: func(branch, sha string) error { moves = append(moves, branch+"="+sha); return nil },
				MergeFFFn:         func(string) error { t.Fatal("the trunk is not checked out"); return nil },
			})
			defer restore()
			cfg, outR, errR := config.NewTestConfig()
			defer commandOutput(t, cfg, outR, errR)
			got, err := fastForwardTrunk(cfg, nil, "main", "origin/main", "x0", "x1", "feature")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, moves)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.moves, moves)
		})
	}
}

func TestOperations_RunningCommandBlocksOnlyItsStack(t *testing.T) {
	shortLockTimeout(t)
	repo := setupStacksRepo(t, "a", "b")
	trunk := repo.advanceTrunk(t, "trunk change\n")
	kill := holdOperation(t, repo.common, stack.Claim{
		Kind: "rebase", Origin: claimPath(repo.trees["a"]),
		Branches: []string{"a1", "a2"}, Worktrees: []string{claimPath(repo.trees["a"])},
	})

	out, err := rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.NoError(t, err, out)
	repo.requireStacked(t, "b", trunk)

	start := time.Now()
	out, err = pushIn(t, repo.trees["a"])
	require.ErrorIs(t, err, ErrLockFailed, out)
	var lockErr *stack.LockError
	assert.ErrorAs(t, err, &lockErr)
	assert.GreaterOrEqual(t, time.Since(start), stack.LockTimeout, "a running command is waited for")
	assert.Contains(t, out, "another process is currently editing the stack")
	assert.Contains(t, out, "`gh stack rebase` is running in worktree "+claimPath(repo.trees["a"]))

	// A best-effort metadata refresh leaves the claimed stack alone.
	view, err := stack.Load(repo.common)
	require.NoError(t, err)
	view.Stacks[0].Branches[0].Head = "refreshed"
	view.Stacks[1].Branches[0].Head = "refreshed"
	stack.SaveNonBlocking(repo.common, view)
	saved, err := stack.Load(repo.common)
	require.NoError(t, err)
	assert.Empty(t, saved.Stacks[0].Branches[0].Head)

	// A command that dies leaves nothing to recover, so its claim is void.
	kill()
	out, err = pushIn(t, repo.trees["a"])
	require.NoError(t, err, out)
	assert.NoDirExists(t, stack.OperationsDir(repo.common))
}

func TestOperations_TrunkOwnerInUseFallsBackToRemoteTrunk(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	trunk := repo.advanceTrunk(t, "trunk change\n")
	local := issue250Git(t, repo.root, "rev-parse", "main")
	kill := holdOperation(t, repo.common, stack.Claim{
		Kind: "checkout", Origin: claimPath(repo.root), Worktrees: []string{claimPath(repo.root)},
	})

	out, err := rebaseIn(t, repo.trees["b"], rebaseOptions{})
	require.NoError(t, err, out)
	assert.Contains(t, out, "Could not update local main")
	assert.Contains(t, out, "in use by another gh-stack operation")
	assert.Equal(t, local, issue250Git(t, repo.root, "rev-parse", "main"), "a worktree in use must not be fast-forwarded")
	repo.requireStacked(t, "b", trunk)

	kill()
	out, err = rebaseIn(t, repo.trees["a"], rebaseOptions{})
	require.NoError(t, err, out)
	assert.Contains(t, out, "Trunk main fast-forwarded")
	assert.Equal(t, trunk, issue250Git(t, repo.root, "rev-parse", "main"))
	assert.Empty(t, issue250Git(t, repo.root, "status", "--porcelain"))
	repo.requireStacked(t, "a", trunk)
}

func TestOperations_PendingModifyBelongsToItsStack(t *testing.T) {
	repo := setupStacksRepo(t, "a", "b")
	sf, err := stack.Load(repo.common)
	require.NoError(t, err)

	// A completed modify of stack a awaits submit.
	lock, err := stack.LockOperation(repo.common)
	require.NoError(t, err)
	op, err := stack.StartOperation(repo.common, stack.Claim{Kind: "modify", Origin: claimPath(repo.trees["a"]), Branches: []string{"a1", "a2"}})
	require.NoError(t, err)
	lock.Unlock()
	state := &modify.StateFile{SchemaVersion: 1, Phase: modify.PhasePendingSubmit}
	state.RecordStack(&sf.Stacks[0])
	require.NoError(t, modify.SaveState(op.Dir, state))
	endMutation(&config.StackMutationContext{CommonDir: repo.common, StateDir: repo.common, Kind: "modify", Operation: op})
	require.Equal(t, []string{op.Dir}, operationDirs(t, repo.common))

	// It reserves nothing: both stacks keep rebasing.
	trunk := repo.advanceTrunk(t, "trunk change\n")
	for _, name := range []string{"a", "b"} {
		out, err := rebaseIn(t, repo.trees[name], rebaseOptions{})
		require.NoError(t, err, out)
		repo.requireStacked(t, name, trunk)
	}
	require.Equal(t, []string{op.Dir}, operationDirs(t, repo.common))

	for name, wantPending := range map[string]bool{"a": true, "b": false} {
		out, err := runIn(t, repo.trees[name], func(cfg *config.Config) error {
			release, err := beginStackMutation(cfg, "submit")
			if err != nil {
				return err
			}
			defer release()
			result, err := loadStack(cfg, "")
			if err != nil {
				return err
			}
			pending, err := hasPendingModify(cfg, result.Stack)
			if err != nil {
				return err
			}
			assert.Equal(t, wantPending, pending, name)
			if err := adoptPendingModify(cfg, result.Stack); err != nil {
				return err
			}
			loaded, err := modify.LoadState(result.GitDir)
			if err != nil {
				return err
			}
			if !wantPending {
				assert.Nil(t, loaded, "another stack's pending modify is not this command's")
				return nil
			}
			require.NotNil(t, loaded)
			assert.Equal(t, modify.PhasePendingSubmit, loaded.Phase)
			return clearPendingModifyState(cfg, result.Stack, result.GitDir)
		})
		require.NoError(t, err, out)
	}
	assert.NoDirExists(t, stack.OperationsDir(repo.common), "a consumed journal ends its record")
}

// TestCommandProcess is not a test. It is the body of the processes that
// TestOperations_ConcurrentRebases starts to run real commands at once.
func TestCommandProcess(t *testing.T) {
	dir := os.Getenv("GH_STACK_TEST_COMMAND_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	start, err := strconv.ParseInt(os.Getenv("GH_STACK_TEST_COMMAND_START"), 10, 64)
	require.NoError(t, err)
	time.Sleep(time.Until(time.Unix(0, start)))
	out, err := rebaseIn(t, dir, rebaseOptions{})
	code := 0
	var exit *ExitError
	if errors.As(err, &exit) {
		code = exit.Code
	} else if err != nil {
		code = -1
	}
	fmt.Printf("%s\nexit=%d\n", out, code)
}

func TestOperations_ConcurrentRebases(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	repo := setupStacksRepo(t, names...)
	if runtime.GOOS != "windows" {
		// Give fetches the latency of a network remote. Without it the
		// commands rarely fetch the shared trunk at the same moment.
		hook := filepath.Join(t.TempDir(), "slow-pack")
		require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nsleep 0.3\nexec \"$@\"\n"), 0755))
		global := filepath.Join(t.TempDir(), "gitconfig")
		require.NoError(t, os.WriteFile(global, []byte("[uploadpack]\n\tpackObjectsHook = "+hook+"\n"), 0644))
		t.Setenv("GIT_CONFIG_GLOBAL", global)
	}
	for round := range 3 {
		trunk := repo.advanceTrunk(t, fmt.Sprintf("trunk round %d\n", round))
		start := time.Now().Add(500 * time.Millisecond).UnixNano()
		outputs := make([]string, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Go(func() {
				command := exec.Command(os.Args[0], "-test.run=^TestCommandProcess$")
				command.Env = append(os.Environ(),
					"GH_STACK_TEST_COMMAND_DIR="+repo.trees[name],
					"GH_STACK_TEST_COMMAND_START="+strconv.FormatInt(start, 10),
				)
				out, _ := command.CombinedOutput()
				outputs[i] = string(out)
			})
		}
		wg.Wait()
		for i, name := range names {
			require.Contains(t, outputs[i], "\nexit=0\n", "round %d, stack %s:\n%s", round, name, outputs[i])
			repo.requireStacked(t, name, trunk)
			assert.Equal(t, name+"2", issue250Git(t, repo.trees[name], "branch", "--show-current"))
			assert.Empty(t, issue250Git(t, repo.trees[name], "status", "--porcelain"))
		}
		assert.Equal(t, trunk, issue250Git(t, repo.root, "rev-parse", "main"), "one of the commands fast-forwards the shared trunk")
		assert.Empty(t, issue250Git(t, repo.root, "status", "--porcelain"))
		assert.NoDirExists(t, stack.OperationsDir(repo.common))
	}
}
