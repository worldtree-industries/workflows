package plan_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/worldtree-industries/workflows/internal/plan"
)

// repo is a throwaway git repository standing in for a Project.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// commit makes a commit and returns its SHA.
func (r *repo) commit() string {
	r.t.Helper()
	content := r.t.Name() + r.git("rev-list", "--all", "--count")
	if err := os.WriteFile(filepath.Join(r.dir, "f"), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "change")
	return r.git("rev-parse", "HEAD")
}

func (r *repo) tag(name string) {
	r.t.Helper()
	r.git("tag", name)
}

// planFor runs the command and returns its stdout, failing on a non-zero exit.
func planFor(t *testing.T, r *repo, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := plan.Main(t.Context(), append([]string{"-repo", r.dir}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr.String())
	}
	return stdout.String()
}

func assertPlan(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
}

func TestTagPushComparesAgainstPreviousTagAndPushesTheTag(t *testing.T) {
	r := newRepo(t)
	r.commit()
	r.tag("v1.0.0")
	sha := r.commit()
	r.tag("v1.1.0")

	got := planFor(t, r, "-event", "push", "-ref", "refs/tags/v1.1.0", "-sha", sha)

	assertPlan(t, got, "base=v1.0.0\ntag=v1.1.0\npush=true\n")
}

func TestFirstTagIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	sha := r.commit()
	r.tag("v1.0.0")

	got := planFor(t, r, "-event", "push", "-ref", "refs/tags/v1.0.0", "-sha", sha)

	assertPlan(t, got, "base=\ntag=v1.0.0\npush=true\n")
}

func TestPreviousTagIsTheHighestReleaseVersionBelowTheTag(t *testing.T) {
	r := newRepo(t)
	r.commit()
	r.tag("v1.9.0")
	r.commit()
	r.tag("v1.10.0") // numerically, not lexically, above v1.9.0
	r.commit()
	r.tag("v2.0.0") // above the tag being built: a later release line
	r.tag("v1.11.0-rc.1")
	r.tag("release-1")
	r.tag("v1.10")
	sha := r.commit()
	r.tag("v1.11.0")

	got := planFor(t, r, "-event", "push", "-ref", "refs/tags/v1.11.0", "-sha", sha)

	assertPlan(t, got, "base=v1.10.0\ntag=v1.11.0\npush=true\n")
}

func TestMainPushComparesAgainstTheCommitBeforeThePushAndPushesShortSHA(t *testing.T) {
	r := newRepo(t)
	before := r.commit()
	r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha, "-before", before)

	assertPlan(t, got, "base="+before+"\ntag=sha-"+sha[:7]+"\npush=true\n")
}

func TestMainPushCreatingTheBranchIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "0000000000000000000000000000000000000000")

	assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=true\n")
}

func TestMainPushWhosePreviousTipIsGoneComparesAgainstTheParent(t *testing.T) {
	r := newRepo(t)
	parent := r.commit()
	sha := r.commit()

	// A force push: GitHub's "before" is a commit the clone doesn't have.
	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "1234567890123456789012345678901234567890")

	assertPlan(t, got, "base="+parent+"\ntag=sha-"+sha[:7]+"\npush=true\n")
}

func TestMainPushOfARootCommitWhosePreviousTipIsGoneIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "1234567890123456789012345678901234567890")

	assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=true\n")
}

func TestPullRequestComparesAgainstItsBaseAndNeverPushes(t *testing.T) {
	r := newRepo(t)
	prBase := r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "pull_request", "-ref", "refs/pull/7/merge", "-sha", sha, "-pr-base", prBase)

	assertPlan(t, got, "base="+prBase+"\ntag=sha-"+sha[:7]+"\npush=false\n")
}

func TestAnythingElseBuildsEveryDeployableWithoutPushing(t *testing.T) {
	r := newRepo(t)
	before := r.commit()
	sha := r.commit()
	r.tag("v1.0.0")

	for _, tc := range []struct{ name, event, ref string }{
		{"push to another branch", "push", "refs/heads/feature"},
		{"push of a non-release tag", "push", "refs/tags/v1.0"},
		{"push of a pre-release tag", "push", "refs/tags/v1.1.0-rc.1"},
		{"manual run on main", "workflow_dispatch", "refs/heads/main"},
		{"manual run on a release tag", "workflow_dispatch", "refs/tags/v1.0.0"},
		{"self-test", "self-test", "refs/pull/7/merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planFor(t, r, "-event", tc.event, "-ref", tc.ref, "-sha", sha, "-before", before, "-pr-base", before)

			assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=false\n")
		})
	}
}

func TestErrors(t *testing.T) {
	r := newRepo(t)
	sha := r.commit()

	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"unknown flag", []string{"-repo", r.dir, "-sha", sha, "-bogus"}, 2},
		{"positional argument", []string{"-repo", r.dir, "-sha", sha, "extra"}, 2},
		{"missing sha", []string{"-repo", r.dir, "-event", "push", "-ref", "refs/heads/main"}, 2},
		{"not a git repository", []string{"-repo", t.TempDir(), "-event", "push", "-ref", "refs/tags/v1.0.0", "-sha", sha}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := plan.Main(t.Context(), tc.args, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit code %d, want %d; stderr:\n%s", code, tc.code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("stderr is empty, want a message")
			}
		})
	}
}
