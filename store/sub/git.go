package sub

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/fatih/color"
	"github.com/justwatchcom/gopass/store"
	"github.com/justwatchcom/gopass/utils/ctxutil"
	"github.com/justwatchcom/gopass/utils/fsutil"
	"github.com/justwatchcom/gopass/utils/out"
	"github.com/pkg/errors"
)

func (s *Store) gitCmd(ctx context.Context, name string, args ...string) error {
	buf := &bytes.Buffer{}

	cmd := exec.CommandContext(ctx, "git", args[0:]...)
	cmd.Dir = s.path
	cmd.Stdout = buf
	cmd.Stderr = buf

	if ctxutil.IsDebug(ctx) {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	out.Debug(ctx, "store.%s: %s %+v", name, cmd.Path, cmd.Args)

	if err := cmd.Run(); err != nil {
		out.Debug(ctx, "Output of '%s %+v': '%s'", cmd.Path, cmd.Args, buf.String())
		return errors.Wrapf(err, "failed to run command %s %+v", cmd.Path, cmd.Args)
	}

	return nil
}

func (s *Store) gitFixConfig(ctx context.Context) error {
	// set push default, to avoid issues with
	// "fatal: The current branch master has multiple upstream branches, refusing to push"
	// https://stackoverflow.com/questions/948354/default-behavior-of-git-push-without-a-branch-specified
	if err := s.gitConfigSet(ctx, "push.default", "matching"); err != nil {
		return errors.Wrapf(err, "failed to set git config for push.default")
	}

	// setup for proper diffs
	if err := s.gitConfigSet(ctx, "diff.gpg.binary", "true"); err != nil {
		out.Yellow(ctx, "Error while initializing git: %s", err)
	}
	if err := s.gitConfigSet(ctx, "diff.gpg.textconv", "gpg --no-tty --decrypt"); err != nil {
		out.Yellow(ctx, "Error while initializing git: %s", err)
	}

	return s.gitFixConfigOSDep(ctx)
}

// GitInitConfig initialized and preparse the git config
func (s *Store) GitInitConfig(ctx context.Context, signKey, userName, userEmail string) error {
	// set commit identity
	if err := s.gitConfigSet(ctx, "user.name", userName); err != nil {
		return errors.Wrapf(err, "failed to set git config user.name")
	}
	if err := s.gitConfigSet(ctx, "user.email", userEmail); err != nil {
		return errors.Wrapf(err, "failed to set git config user.email")
	}

	// ensure sane git config
	if err := s.gitFixConfig(ctx); err != nil {
		return errors.Wrapf(err, "failed to fix git config")
	}

	if err := ioutil.WriteFile(filepath.Join(s.path, ".gitattributes"), []byte("*.gpg diff=gpg\n"), fileMode); err != nil {
		return errors.Errorf("Failed to initialize git: %s", err)
	}
	if err := s.gitAdd(ctx, s.path+"/.gitattributes"); err != nil {
		out.Yellow(ctx, "Warning: Failed to add .gitattributes to git")
	}
	if err := s.gitCommit(ctx, "Configure git repository for gpg file diff."); err != nil {
		out.Yellow(ctx, "Warning: Failed to commit .gitattributes to git")
	}

	// set GPG signkey
	if err := s.gitSetSignKey(ctx, signKey); err != nil {
		color.Yellow("Failed to configure Git GPG Commit signing: %s\n", err)
	}

	return nil
}

// GitInit initializes this store's git repo
func (s *Store) GitInit(ctx context.Context, signKey, userName, userEmail string) error {
	// the git repo may be empty (i.e. no branches, cloned from a fresh remote)
	// or already initialized. Only run git init if the folder is completely empty
	if !s.isGit() {
		if err := s.gitCmd(ctx, "GitInit", "init"); err != nil {
			return errors.Errorf("Failed to initialize git: %s", err)
		}
	}

	// initialize the local git config
	if err := s.GitInitConfig(ctx, signKey, userName, userEmail); err != nil {
		return errors.Errorf("failed to configure git: %s", err)
	}

	// add current content of the store
	if err := s.gitAdd(ctx, s.path); err != nil {
		return errors.Wrapf(err, "failed to add '%s' to git", s.path)
	}

	// commit if there is something to commit
	if !s.gitStagedChanges(ctx) {
		return nil
	}

	if err := s.gitCommit(ctx, "Add current content of password store."); err != nil {
		return errors.Wrapf(err, "failed to commit changes to git")
	}

	return nil
}

func (s *Store) gitSetSignKey(ctx context.Context, sk string) error {
	if sk == "" {
		return errors.Errorf("SignKey not set")
	}

	if err := s.gitConfigSet(ctx, "user.signingkey", sk); err != nil {
		return errors.Wrapf(err, "failed to set git sign key")
	}

	return s.gitConfigSet(ctx, "commit.gpgsign", "true")
}

// GitVersion returns the git version as major, minor and patch level
func (s *Store) GitVersion(ctx context.Context) semver.Version {
	v := semver.Version{}

	cmd := exec.CommandContext(ctx, "git", "version")
	cmdout, err := cmd.Output()
	if err != nil {
		out.Debug(ctx, "[DEBUG] Failed to run 'git version': %s", err)
		return v
	}

	svStr := strings.TrimPrefix(string(cmdout), "git version ")
	if p := strings.Fields(svStr); len(p) > 0 {
		svStr = p[0]
	}

	sv, err := semver.ParseTolerant(svStr)
	if err != nil {
		out.Debug(ctx, "Failed to parse '%s' as semver: %s", svStr, err)
		return v
	}
	return sv
}

// Git runs arbitrary git commands on this store
func (s *Store) Git(ctx context.Context, args ...string) error {
	return s.gitCmd(ctx, "Git", args...)
}

// isGit returns true if this stores has an (probably) initialized .git folder
func (s *Store) isGit() bool {
	return fsutil.IsFile(filepath.Join(s.path, ".git", "config"))
}

// gitAdd adds the listed files to the git index
func (s *Store) gitAdd(ctx context.Context, files ...string) error {
	if !s.isGit() {
		return store.ErrGitNotInit
	}
	for i := range files {
		files[i] = strings.TrimPrefix(files[i], s.path+"/")
	}

	args := []string{"add", "--all"}
	args = append(args, files...)

	return s.gitCmd(ctx, "gitAdd", args...)
}

// gitStagedChanges returns true if there are any staged changes which can be committed
func (s *Store) gitStagedChanges(ctx context.Context) bool {
	if err := s.gitCmd(ctx, "gitDiffIndex", "diff-index", "--quiet", "HEAD"); err != nil {
		return true
	}
	return false
}

// gitCommit creates a new git commit with the given commit message
func (s *Store) gitCommit(ctx context.Context, msg string) error {
	if !s.isGit() {
		return store.ErrGitNotInit
	}

	if !s.gitStagedChanges(ctx) {
		return store.ErrGitNothingToCommit
	}

	return s.gitCmd(ctx, "gitCommit", "commit", "-m", msg)
}

func (s *Store) gitConfigSet(ctx context.Context, key, value string) error {
	return s.gitCmd(ctx, "gitConfigSet", "config", "--local", key, value)
}

func (s *Store) gitConfigGet(ctx context.Context, key string) (string, error) {
	if !s.isGit() {
		return "", store.ErrGitNotInit
	}

	buf := &bytes.Buffer{}

	cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
	cmd.Dir = s.path
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr

	out.Debug(ctx, "store.gitConfigValue: %s %+v", cmd.Path, cmd.Args)
	if err := cmd.Run(); err != nil {
		return "", err
	}

	return strings.TrimSpace(buf.String()), nil
}

// gitPush pushes the repo to it's origin.
// optional arguments: remote and branch
func (s *Store) gitPushPull(ctx context.Context, op, remote, branch string) error {
	if !s.isGit() {
		return store.ErrGitNotInit
	}

	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "master"
	}

	if v, err := s.gitConfigGet(ctx, "remote."+remote+".url"); err != nil || v == "" {
		return store.ErrGitNoRemote
	}

	if err := s.gitCmd(ctx, "gitPush", "pull", remote, branch); err != nil {
		if op == "pull" {
			return err
		}
		out.Yellow(ctx, "Failed to pull before git push: %s", err)
	}
	if op == "pull" {
		return nil
	}

	return s.gitCmd(ctx, "gitPush", "push", remote, branch)
}

// GitPush pushes to the git remote
func (s *Store) GitPush(ctx context.Context, remote, branch string) error {
	return s.gitPushPull(ctx, "push", remote, branch)
}

// GitPull pulls from the git remote
func (s *Store) GitPull(ctx context.Context, remote, branch string) error {
	return s.gitPushPull(ctx, "pull", remote, branch)
}
