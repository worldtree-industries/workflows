// Package plan decides, from the GitHub event that triggered a Project's CI,
// what the images job compares against, which image tag it builds and
// whether it pushes.
package plan

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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
}

// Main runs the command with args (excluding the program name) and returns
// the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to the Project's git repository")
	var ev Event
	fs.StringVar(&ev.Name, "event", "", "GitHub event name (github.event_name)")
	fs.StringVar(&ev.Ref, "ref", "", "full git ref that triggered the run (github.ref)")
	fs.StringVar(&ev.SHA, "sha", "", "commit being built (github.sha)")
	fs.StringVar(&ev.Before, "before", "", "previous tip of the pushed branch (github.event.before)")
	fs.StringVar(&ev.PRBase, "pr-base", "", "pull request base commit (github.event.pull_request.base.sha)")
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

	p, err := Decide(ctx, *repo, ev)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "plan: %v\n", err)
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "base=%s\ntag=%s\npush=%t\n", p.Base, p.Tag, p.Push)
	return 0
}

// Decide works out the Plan for ev in the Project repository at repo.
//
//   - A push of a vX.Y.Z tag compares against the highest lower vX.Y.Z tag
//     and pushes the image tagged vX.Y.Z.
//   - A push to main compares against the branch's previous tip and pushes
//     the image tagged sha-<short>.
//   - A pull request compares against its base commit and doesn't push.
//   - Anything else builds every Deployable and doesn't push.
func Decide(ctx context.Context, repo string, ev Event) (Plan, error) {
	tag := shortTag(ev.SHA)
	switch {
	case ev.Name == "push" && ev.Ref == "refs/heads/main":
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
		return Plan{Base: ev.PRBase, Tag: tag}, nil
	}
	return Plan{Tag: tag}, nil
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
	best, found := "", false
	var bestV semver
	for name := range strings.FieldsSeq(out) {
		v, ok := parseVersion(name)
		if !ok || !v.less(current) {
			continue
		}
		if !found || bestV.less(v) {
			best, bestV, found = name, v, true
		}
	}
	return best, nil
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
