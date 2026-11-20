package plan_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// write writes a file in the work tree and stages it.
func (r *repo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "-A")
}

// merge commits a change on a branch and merges it into main with subject,
// as GitHub merges a pull request, and returns the merge commit's SHA.
func (r *repo) merge(subject string) string {
	r.t.Helper()
	branch := "topic-" + r.git("rev-list", "--all", "--count")
	r.git("checkout", "-q", "-b", branch)
	r.commit()
	r.git("checkout", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", subject, branch)
	return r.git("rev-parse", "HEAD")
}

// labels is a fake GitHub holding each pull request's labels. A pull request
// it doesn't hold fails the test when read.
type labels struct {
	t    *testing.T
	byPR map[int][]string
	err  error
	read []int
}

func (l *labels) get(_ context.Context, repository string, pr int) ([]string, error) {
	l.t.Helper()
	l.read = append(l.read, pr)
	if repository != "acme/widget" {
		l.t.Errorf("labels read from %q, want acme/widget", repository)
	}
	if l.err != nil {
		return nil, l.err
	}
	names, ok := l.byPR[pr]
	if !ok {
		l.t.Errorf("labels of pull request #%d read, want them unread", pr)
	}
	return names, nil
}

// noLabels is a fake GitHub that fails the test if any labels are read.
func noLabels(t *testing.T) *labels {
	return &labels{t: t}
}

// run runs the command and returns its exit code, stdout and stderr.
func run(t *testing.T, r *repo, l *labels, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append([]string{"-repo", r.dir, "-github-repo", "acme/widget"}, args...)
	code := plan.Main(t.Context(), args, &stdout, &stderr, l.get)
	return code, stdout.String(), stderr.String()
}

// planFor runs the command without reading labels and returns its stdout,
// failing on a non-zero exit.
func planFor(t *testing.T, r *repo, args ...string) string {
	t.Helper()
	return planWithLabels(t, r, noLabels(t), args...)
}

// planWithLabels runs the command and returns its stdout, failing on a
// non-zero exit.
func planWithLabels(t *testing.T, r *repo, l *labels, args ...string) string {
	t.Helper()
	code, stdout, stderr := run(t, r, l, args...)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	return stdout
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

	assertPlan(t, got, "base=v1.0.0\ntag=v1.1.0\npush=true\nrelease=\n")
}

func TestFirstTagIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	sha := r.commit()
	r.tag("v1.0.0")

	got := planFor(t, r, "-event", "push", "-ref", "refs/tags/v1.0.0", "-sha", sha)

	assertPlan(t, got, "base=\ntag=v1.0.0\npush=true\nrelease=\n")
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

	assertPlan(t, got, "base=v1.10.0\ntag=v1.11.0\npush=true\nrelease=\n")
}

func TestMainPushComparesAgainstTheCommitBeforeThePushAndPushesShortSHA(t *testing.T) {
	r := newRepo(t)
	before := r.commit()
	r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha, "-before", before)

	assertPlan(t, got, "base="+before+"\ntag=sha-"+sha[:7]+"\npush=true\nrelease=\n")
}

func TestMainPushCreatingTheBranchIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "0000000000000000000000000000000000000000")

	assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=true\nrelease=\n")
}

func TestMainPushWhosePreviousTipIsGoneComparesAgainstTheParent(t *testing.T) {
	r := newRepo(t)
	parent := r.commit()
	sha := r.commit()

	// A force push: GitHub's "before" is a commit the clone doesn't have.
	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "1234567890123456789012345678901234567890")

	assertPlan(t, got, "base="+parent+"\ntag=sha-"+sha[:7]+"\npush=true\nrelease=\n")
}

func TestMainPushOfARootCommitWhosePreviousTipIsGoneIsAFirstRelease(t *testing.T) {
	r := newRepo(t)
	sha := r.commit()

	got := planFor(t, r, "-event", "push", "-ref", "refs/heads/main", "-sha", sha,
		"-before", "1234567890123456789012345678901234567890")

	assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=true\nrelease=\n")
}

func TestPullRequestComparesAgainstItsBaseAndNeverPushes(t *testing.T) {
	r := newRepo(t)
	prBase := r.commit()
	sha := r.commit()

	got := planFor(t, r, "-event", "pull_request", "-ref", "refs/pull/7/merge", "-sha", sha, "-pr-base", prBase)

	assertPlan(t, got, "base="+prBase+"\ntag=sha-"+sha[:7]+"\npush=false\nrelease=\n")
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

			assertPlan(t, got, "base=\ntag=sha-"+sha[:7]+"\npush=false\nrelease=\n")
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
			code := plan.Main(t.Context(), tc.args, &stdout, &stderr, noLabels(t).get)
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

// project is a repository standing in for a Project with module path module,
// released as v1.2.3.
func project(t *testing.T, module string) *repo {
	t.Helper()
	r := newRepo(t)
	r.write("go.mod", "module "+module+"\n\ngo 1.27\n")
	r.commit()
	r.tag("v1.2.3")
	return r
}

func pushToMain(sha string) []string {
	return []string{"-event", "push", "-ref", "refs/heads/main", "-sha", sha, "-before", sha + "^"}
}

func TestMergeToMainReleasesAPatchWithoutASemverLabel(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	sha := r.merge("Merge pull request #12 from acme/topic")
	l := &labels{t: t, byPR: map[int][]string{12: {"bug", "ready-for-agent"}}}

	got := planWithLabels(t, r, l, pushToMain(sha)...)

	assertPlan(t, got, "base=v1.2.3\ntag=v1.2.4\npush=true\nrelease=v1.2.4\n")
}

func TestMergeToMainReleasesTheBumpItsLabelsAskFor(t *testing.T) {
	for _, tc := range []struct {
		name, module, want string
		labels             []string
	}{
		{"minor", "github.com/acme/widget", "v1.3.0", []string{"semver:minor"}},
		{"major", "github.com/acme/widget/v2", "v2.0.0", []string{"semver:major"}},
		{"major beats minor", "github.com/acme/widget/v2", "v2.0.0", []string{"semver:minor", "semver:major"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := project(t, tc.module)
			sha := r.merge("Merge pull request #12 from acme/topic")
			l := &labels{t: t, byPR: map[int][]string{12: tc.labels}}

			got := planWithLabels(t, r, l, pushToMain(sha)...)

			assertPlan(t, got, "base=v1.2.3\ntag="+tc.want+"\npush=true\nrelease="+tc.want+"\n")
		})
	}
}

func TestFirstMergeToMainReleasesV0_0_1(t *testing.T) {
	r := newRepo(t)
	r.write("go.mod", "module github.com/acme/widget\n")
	r.commit()
	sha := r.merge("Merge pull request #1 from acme/topic")
	l := &labels{t: t, byPR: map[int][]string{1: nil}}

	got := planWithLabels(t, r, l, pushToMain(sha)...)

	assertPlan(t, got, "base=\ntag=v0.0.1\npush=true\nrelease=v0.0.1\n")
}

func TestMergeToMainBumpsTheHighestReleaseInItsOwnHistory(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	r.git("checkout", "-q", "-b", "abandoned")
	r.commit()
	r.tag("v9.0.0")
	r.git("checkout", "-q", "main")
	r.commit()
	r.tag("v1.10.0")
	sha := r.merge("Merge pull request #12 from acme/topic")
	l := &labels{t: t, byPR: map[int][]string{12: nil}}

	got := planWithLabels(t, r, l, pushToMain(sha)...)

	assertPlan(t, got, "base=v1.10.0\ntag=v1.10.1\npush=true\nrelease=v1.10.1\n")
}

func TestReRunOfAReleasedMergeRebuildsItAndReleasesNothing(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	sha := r.merge("Merge pull request #12 from acme/topic")
	r.tag("v1.2.4")

	got := planFor(t, r, pushToMain(sha)...)

	assertPlan(t, got, "base=v1.2.3\ntag=v1.2.4\npush=true\nrelease=\n")
}

func TestMergeToMainFailsWhenTheModulePathDoesNotMatchTheMajor(t *testing.T) {
	for _, tc := range []struct {
		name, module, want string
		labels             []string
	}{
		{"major without /v2", "github.com/acme/widget", "end in /v2", []string{"semver:major"}},
		{"/v2 released as v1", "github.com/acme/widget/v2", "releases as v2", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := project(t, tc.module)
			sha := r.merge("Merge pull request #12 from acme/topic")
			l := &labels{t: t, byPR: map[int][]string{12: tc.labels}}

			code, stdout, stderr := run(t, r, l, pushToMain(sha)...)

			assertFailed(t, code, stdout, stderr, tc.want)
		})
	}
}

func TestMergeToMainFailsWhenItsSubjectNamesNoPullRequest(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	sha := r.merge("Merge branch 'topic'")

	code, stdout, stderr := run(t, r, noLabels(t), pushToMain(sha)...)

	assertFailed(t, code, stdout, stderr, "doesn't name a pull request")
}

func TestMergeToMainFailsWhenItsLabelsCannotBeRead(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	sha := r.merge("Merge pull request #12 from acme/topic")
	l := &labels{t: t, err: errors.New("HTTP 502")}

	code, stdout, stderr := run(t, r, l, pushToMain(sha)...)

	assertFailed(t, code, stdout, stderr, "HTTP 502")
}

func TestPullRequestDryRunsItsReleaseWithoutChangingThePlan(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	prBase := r.git("rev-parse", "HEAD")
	sha := r.commit()
	l := &labels{t: t, byPR: map[int][]string{7: {"semver:minor"}}}

	code, stdout, stderr := run(t, r, l,
		"-event", "pull_request", "-ref", "refs/pull/7/merge", "-sha", sha, "-pr-base", prBase, "-pr", "7")

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr)
	}
	assertPlan(t, stdout, "base="+prBase+"\ntag=sha-"+sha[:7]+"\npush=false\nrelease=\n")
	if !strings.Contains(stderr, "would release v1.3.0") {
		t.Errorf("stderr doesn't say the merge would release v1.3.0:\n%s", stderr)
	}
}

func TestPullRequestFailsWhenItsReleaseWouldBeRejected(t *testing.T) {
	r := project(t, "github.com/acme/widget")
	prBase := r.git("rev-parse", "HEAD")
	sha := r.commit()
	l := &labels{t: t, byPR: map[int][]string{7: {"semver:major"}}}

	code, stdout, stderr := run(t, r, l,
		"-event", "pull_request", "-ref", "refs/pull/7/merge", "-sha", sha, "-pr-base", prBase, "-pr", "7")

	assertFailed(t, code, stdout, stderr, "end in /v2")
}

func assertFailed(t *testing.T, code int, stdout, stderr, want string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code %d, want 1; stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr doesn't contain %q:\n%s", want, stderr)
	}
}
