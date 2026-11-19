// Package plan decides, from the GitHub event that triggered a Project's CI,
// what the images job compares against, which image tag it builds, whether it
// pushes, and whether the run releases a new version.
package plan

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	exitError = 1
	exitUsage = 2
)

// Plan is the command's output.
type Plan struct {
	// Base is the ref to pass to the affected command; empty means first
	// release (every Deployable is affected).
	Base string
	// Tag is the image tag to build.
	Tag string
	// Push reports whether images are pushed to the registry.
	Push bool
	// Release is the vX.Y.Z git tag the run publishes once its images are
	// pushed, or "" when it publishes none.
	Release string
}

// Event describes what triggered the run.
type Event struct {
	// Name is the GitHub event name, such as push or pull_request.
	Name string
	// Ref is the full git ref that triggered the run.
	Ref string
	// SHA is the commit being built.
	SHA string
	// Before is the branch tip before a push.
	Before string
	// PRBase is a pull request's base commit.
	PRBase string
	// PR is a pull request's number; 0 skips a pull request's release dry
	// run.
	PR int
	// Repository is the GitHub repository, as owner/name, whose pull request
	// labels are read.
	Repository string
}

// Labels returns the label names of pull request pr in the GitHub repository
// ("owner/name").
type Labels func(ctx context.Context, repository string, pr int) ([]string, error)

// Main runs the command with args (excluding the program name), reading pull
// request labels with labels, and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer, labels Labels) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to the Project's git repository")
	var ev Event
	fs.StringVar(&ev.Name, "event", "", "GitHub event name (github.event_name)")
	fs.StringVar(&ev.Ref, "ref", "", "full git ref that triggered the run (github.ref)")
	fs.StringVar(&ev.SHA, "sha", "", "commit being built (github.sha)")
	fs.StringVar(&ev.Before, "before", "", "previous tip of the pushed branch (github.event.before)")
	fs.StringVar(&ev.PRBase, "pr-base", "", "pull request base commit (github.event.pull_request.base.sha)")
	fs.IntVar(&ev.PR, "pr", 0, "pull request number (github.event.pull_request.number)")
	fs.StringVar(&ev.Repository, "github-repo", "", "GitHub repository, as owner/name (github.repository)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "plan: unexpected arguments: %v\n", fs.Args())
		return exitUsage
	}
	if ev.SHA == "" {
		_, _ = fmt.Fprintln(stderr, "plan: -sha is required")
		return exitUsage
	}

	p, err := Decide(ctx, *repo, ev, labels, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plan: %v\n", err)
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "base=%s\ntag=%s\npush=%t\nrelease=%s\n", p.Base, p.Tag, p.Push, p.Release)
	return 0
}

// Decide works out the Plan for ev in the Project repository at repo, reading
// pull request labels with labels and explaining a release decision to log.
//
//   - A push to main of a merge commit releases: it compares against the
//     previous release and pushes the image tagged with the next version,
//     which the run then publishes as a git tag. The merged pull request's
//     labels choose the bump. If the commit already carries a release tag
//     (a re-run), it rebuilds that version and publishes nothing new.
//   - Any other push to main compares against the branch's previous tip and
//     pushes the image tagged sha-<short>.
//   - A push of a vX.Y.Z tag compares against the highest lower vX.Y.Z tag
//     and pushes the image tagged vX.Y.Z.
//   - A pull request compares against its base commit and doesn't push. With
//     its number it also works out, as a dry run, the version its merge
//     would release, and fails when that release would be rejected.
//   - Anything else builds every Deployable and doesn't push.
func Decide(ctx context.Context, repo string, ev Event, labels Labels, log io.Writer) (Plan, error) {
	tag := shortTag(ev.SHA)
	switch {
	case ev.Name == "push" && ev.Ref == "refs/heads/main":
		merge, err := isMerge(ctx, repo, ev.SHA)
		if err != nil {
			return Plan{}, err
		}
		if merge {
			return release(ctx, repo, ev, labels, log)
		}
		_, _ = fmt.Fprintln(log, "plan: not a merge commit, so this push releases nothing")
		return Plan{Base: pushBase(ctx, repo, ev), Tag: tag, Push: true}, nil
	case ev.Name == "push" && strings.HasPrefix(ev.Ref, "refs/tags/"):
		version := strings.TrimPrefix(ev.Ref, "refs/tags/")
		current, ok := parseVersion(version)
		if !ok {
			break
		}
		base, err := previousTag(ctx, repo, current)
		if err != nil {
			return Plan{}, err
		}
		return Plan{Base: base, Tag: version, Push: true}, nil
	case ev.Name == "pull_request":
		if ev.PR != 0 {
			next, err := nextRelease(ctx, repo, ev, ev.PR, labels, log)
			if err != nil {
				return Plan{}, err
			}
			_, _ = fmt.Fprintf(log, "plan: merging pull request #%d would release %s\n", ev.PR, next)
		}
		return Plan{Base: ev.PRBase, Tag: tag}, nil
	}
	return Plan{Tag: tag}, nil
}

// mergeSubjectRE matches the subject GitHub writes on a merge commit, which
// names the pull request it merged.
var mergeSubjectRE = regexp.MustCompile(`^Merge pull request #([1-9][0-9]*) `)

// release is the Plan for a push of the merge commit ev.SHA to main.
func release(ctx context.Context, repo string, ev Event, labels Labels, log io.Writer) (Plan, error) {
	// A re-run: the release already happened, so rebuild it and publish
	// nothing. Checked before the labels, which would bump past it.
	existing, released, err := releaseAt(ctx, repo, ev.SHA)
	if err != nil {
		return Plan{}, err
	}
	if released {
		var base string
		if base, err = previousTag(ctx, repo, existing); err != nil {
			return Plan{}, err
		}
		_, _ = fmt.Fprintf(log, "plan: %s is already released as %s; rebuilding it\n", ev.SHA, existing)
		return Plan{Base: base, Tag: existing.String(), Push: true}, nil
	}

	subject, err := git(ctx, repo, "log", "-1", "--format=%s", "--end-of-options", ev.SHA)
	if err != nil {
		return Plan{}, err
	}
	m := mergeSubjectRE.FindStringSubmatch(subject)
	if m == nil {
		return Plan{}, fmt.Errorf(
			"merge commit %s doesn't name a pull request in its subject (%q), so its release can't be decided: "+
				"tag it by hand, and keep GitHub's default merge commit message",
			ev.SHA, strings.TrimSpace(subject))
	}
	pr, err := strconv.Atoi(m[1])
	if err != nil {
		return Plan{}, fmt.Errorf("pull request number %q: %w", m[1], err)
	}

	next, err := nextRelease(ctx, repo, ev, pr, labels, log)
	if err != nil {
		return Plan{}, err
	}
	base, err := latestTag(ctx, repo, ev.SHA)
	if err != nil {
		return Plan{}, err
	}
	_, _ = fmt.Fprintf(log, "plan: merging pull request #%d releases %s\n", pr, next)
	return Plan{Base: base, Tag: next.String(), Push: true, Release: next.String()}, nil
}

// nextRelease is the version a merge of pull request pr at ev.SHA releases:
// the highest release tag reachable from ev.SHA, bumped as the pull request's
// labels ask. It fails when the Project's module path doesn't match that
// version's major.
func nextRelease(
	ctx context.Context, repo string, ev Event, pr int, labels Labels, log io.Writer,
) (semver, error) {
	names, err := labels(ctx, ev.Repository, pr)
	if err != nil {
		return semver{}, fmt.Errorf("reading the labels of pull request #%d: %w", pr, err)
	}
	b := bumpFor(names)
	latest, err := latestTag(ctx, repo, ev.SHA)
	if err != nil {
		return semver{}, err
	}
	var current semver
	if latest != "" {
		current, _ = parseVersion(latest)
	}
	next := current.bump(b)
	_, _ = fmt.Fprintf(log, "plan: pull request #%d asks for a %s release after %s\n", pr, b, orNone(latest))

	module, err := modulePath(ctx, repo, ev.SHA)
	if err != nil {
		return semver{}, err
	}
	if err = checkMajor(module, next); err != nil {
		return semver{}, err
	}
	return next, nil
}

func orNone(tag string) string {
	if tag == "" {
		return "no release"
	}
	return tag
}

type bump string

const (
	patch bump = "patch"
	minor bump = "minor"
	major bump = "major"
)

// bumpFor is the bump a pull request's labels ask for. A pull request without
// a semver label is a patch: a missing label can only under-promise a
// version, never inflate one.
func bumpFor(labels []string) bump {
	b := patch
	for _, l := range labels {
		switch l {
		case "semver:major":
			return major
		case "semver:minor":
			b = minor
		}
	}
	return b
}

func (v semver) bump(b bump) semver {
	switch b {
	case major:
		return semver{v[0] + 1, 0, 0}
	case minor:
		return semver{v[0], v[1] + 1, 0}
	case patch:
	}
	return semver{v[0], v[1], v[2] + 1}
}

func (v semver) String() string {
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2])
}

var majorSuffixRE = regexp.MustCompile(`/v([0-9]+)$`)

// checkMajor fails unless module's /vN suffix matches next's major: Go needs
// /vN on the module path for every major N >= 2, and a vN tag without it is
// never listed as a version of the module. v0 and v1 take no suffix.
func checkMajor(module string, next semver) error {
	const firstSuffixedMajor = 2
	want := ""
	if next[0] >= firstSuffixedMajor {
		want = fmt.Sprintf("/v%d", next[0])
	}
	got := ""
	if m := majorSuffixRE.FindStringSubmatch(module); m != nil {
		got = "/v" + m[1]
	}
	if got == want {
		return nil
	}
	if want != "" {
		return fmt.Errorf(
			"releasing %s needs the module path to end in %s, but it is %s: "+
				"rename it, and every import of it, in the same pull request",
			next, want, module)
	}
	return fmt.Errorf(
		"module path %s ends in %s, but this release is %s: a %s module path releases as %s",
		module, got, next, got, strings.TrimPrefix(got, "/"))
}

// modulePath is the module path in go.mod at commit sha.
func modulePath(ctx context.Context, repo, sha string) (string, error) {
	out, err := git(ctx, repo, "show", "--end-of-options", sha+":go.mod")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`), nil
		}
	}
	return "", errors.New("go.mod has no module directive")
}

// isMerge reports whether sha is a merge commit.
func isMerge(ctx context.Context, repo, sha string) (bool, error) {
	out, err := git(ctx, repo, "rev-list", "--parents", "-n", "1", "--end-of-options", sha)
	if err != nil {
		return false, err
	}
	return len(strings.Fields(out)) > 2, nil //nolint:mnd // the commit and at least two parents
}

// releaseAt returns the highest release tag pointing at sha, if any.
func releaseAt(ctx context.Context, repo, sha string) (semver, bool, error) {
	out, err := git(ctx, repo, "tag", "--points-at", sha, "--list", "v*")
	if err != nil {
		return semver{}, false, err
	}
	v, ok := highest(out, nil)
	return v, ok, nil
}

// latestTag returns the highest release tag reachable from sha, or "" when
// there is none. Tags on other branches are ignored on purpose: the previous
// release is the one in this commit's history.
func latestTag(ctx context.Context, repo, sha string) (string, error) {
	out, err := git(ctx, repo, "tag", "--merged", sha, "--list", "v*")
	if err != nil {
		return "", err
	}
	if v, ok := highest(out, nil); ok {
		return v.String(), nil
	}
	return "", nil
}

var repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHubLabels reads a pull request's labels with the gh CLI, which
// authenticates from GH_TOKEN. It tries three times, because the labels are
// the only input that needs the network, and guessing a version instead would
// publish the wrong one.
func GitHubLabels(ctx context.Context, repository string, pr int) ([]string, error) {
	if !repositoryRE.MatchString(repository) {
		return nil, fmt.Errorf("-github-repo %q isn't a GitHub repository (owner/name)", repository)
	}
	const attempts = 3
	var err error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second): //nolint:mnd // a pause between attempts
			}
		}
		// Labels are on the issue form of a pull request: there is no
		// /pulls/{n}/labels endpoint.
		cmd := exec.CommandContext(ctx, "gh", "api", //nolint:gosec // repository is checked against repositoryRE
			fmt.Sprintf("repos/%s/issues/%d/labels", repository, pr), "--jq", ".[].name")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		var out []byte
		out, err = cmd.Output()
		if err == nil {
			return strings.Fields(string(out)), nil
		}
		err = fmt.Errorf("gh api: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil, err
}

// shortSHALength matches docker/metadata-action's type=sha,format=short.
const shortSHALength = 7

// shortTag is the image tag for a commit: sha-<first 7 hex digits>.
func shortTag(sha string) string {
	return "sha-" + sha[:min(len(sha), shortSHALength)]
}

var versionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type semver [3]int

func parseVersion(s string) (semver, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	var v semver
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v semver) less(o semver) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// previousTag returns the highest vX.Y.Z tag in repo below current, or ""
// when there is none.
func previousTag(ctx context.Context, repo string, current semver) (string, error) {
	out, err := git(ctx, repo, "tag", "--list", "v*")
	if err != nil {
		return "", err
	}
	if v, ok := highest(out, &current); ok {
		return v.String(), nil
	}
	return "", nil
}

// highest returns the highest vX.Y.Z version among the tag names in list,
// considering only versions below *below when below isn't nil.
func highest(list string, below *semver) (semver, bool) {
	var best semver
	found := false
	for name := range strings.FieldsSeq(list) {
		v, ok := parseVersion(name)
		if !ok || (below != nil && !v.less(*below)) {
			continue
		}
		if !found || best.less(v) {
			best, found = v, true
		}
	}
	return best, found
}

var zeroSHA = regexp.MustCompile(`^0+$`)

// pushBase is the commit a push to a branch compares against: the branch's
// previous tip, or the pushed commit's parent when the previous tip isn't in
// the repository (a force push). It is "" when the push created the branch,
// or when the previous tip is gone and the pushed commit has no parent.
func pushBase(ctx context.Context, repo string, ev Event) string {
	if ev.Before == "" || zeroSHA.MatchString(ev.Before) {
		return ""
	}
	if _, ok := resolveCommit(ctx, repo, ev.Before); ok {
		return ev.Before
	}
	parent, _ := resolveCommit(ctx, repo, ev.SHA+"^")
	return parent
}

// resolveCommit returns the commit SHA rev names, or false when it names no
// commit in repo.
func resolveCommit(ctx context.Context, repo, rev string) (string, bool) {
	out, err := git(ctx, repo, "rev-parse", "--quiet", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	// Refs from the caller only ever reach git after --end-of-options.
	gitArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
