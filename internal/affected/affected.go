// Package affected works out which Deployables of a Project need building
// between two git refs.
package affected

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path"
	"slices"
	"strings"
)

// Deployable is one entry of the command's JSON output.
type Deployable struct {
	Name      string `json:"name"`
	Build     string `json:"build"`
	BaseImage string `json:"base_image"`
}

const (
	exitError = 1
	exitUsage = 2
)

// Main runs the command with args (excluding the program name) and returns
// the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("affected", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "path to the Project's git repository")
	base := fs.String("base", "", "base ref; empty means first release (every Deployable is affected)")
	head := fs.String("head", "HEAD", "head ref")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "affected: unexpected arguments: %v\n", fs.Args())
		return exitUsage
	}

	result, err := Compute(ctx, *repo, *base, *head)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected: %v\n", err)
		return exitError
	}
	out, err := json.Marshal(result)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "affected: %v\n", err)
		return exitError
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", out)
	return 0
}

// Compute returns the Deployables in repo affected between base and head,
// sorted by name. An empty base means every Deployable is affected.
func Compute(ctx context.Context, repo, base, head string) ([]Deployable, error) {
	g := gitRepo{dir: repo}
	headSHA, err := g.resolve(ctx, head)
	if err != nil {
		return nil, err
	}
	headManifest, err := loadManifest(ctx, g, headSHA)
	if err != nil {
		return nil, err
	}

	deployables := headManifest.deployables()
	everything := func(string) bool { return true }
	if isFirstRelease(base) {
		return output(deployables, headManifest.Binaries, everything), nil
	}
	baseSHA, err := g.resolve(ctx, base)
	if err != nil {
		return nil, err
	}

	changed, err := g.changedFiles(ctx, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	if slices.Contains(changed, "go.mod") || slices.Contains(changed, "go.sum") {
		return output(deployables, headManifest.Binaries, everything), nil
	}

	inClosure, err := closures(ctx, g, headSHA, deployables)
	if err != nil {
		return nil, err
	}
	// A base Manifest that is missing or invalid can't be compared, so every
	// entry counts as changed.
	baseManifest, err := loadManifest(ctx, g, baseSHA)
	if err != nil {
		baseManifest = manifest{}
	}

	isAffected := func(name string) bool {
		if old, ok := baseManifest.Binaries[name]; !ok || !old.equal(headManifest.Binaries[name]) {
			return true
		}
		dockerfile := path.Join("cmd", name, "Dockerfile")
		usesDockerfile := headManifest.Binaries[name].buildMethod() == buildDockerfile
		for _, f := range changed {
			if inClosure[name](f) || (usesDockerfile && f == dockerfile) {
				return true
			}
		}
		return false
	}
	return output(deployables, headManifest.Binaries, isAffected), nil
}

// isFirstRelease reports whether base names no previous release: empty, or
// the all-zero SHA GitHub reports as the "before" of a newly created ref.
func isFirstRelease(base string) bool {
	isZeroSHA := (len(base) == 40 || len(base) == 64) && strings.Trim(base, "0") == ""
	return base == "" || isZeroSHA
}

func output(names []string, entries map[string]entry, isAffected func(string) bool) []Deployable {
	out := []Deployable{}
	for _, name := range names {
		if !isAffected(name) {
			continue
		}
		e := entries[name]
		out = append(out, Deployable{Name: name, Build: e.buildMethod(), BaseImage: e.baseImage()})
	}
	return out
}

type gitRepo struct {
	dir string
}

func (g gitRepo) run(ctx context.Context, args ...string) ([]byte, error) {
	// Refs from the caller only ever reach git after --end-of-options.
	gitArgs := append([]string{"-C", g.dir}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g gitRepo) resolve(ctx context.Context, ref string) (string, error) {
	out, err := g.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolving ref %q: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// changedFiles lists the repo-relative paths that differ between the trees
// of two commits. Renames are reported as a deletion plus an addition.
func (g gitRepo) changedFiles(ctx context.Context, base, head string) ([]string, error) {
	out, err := g.run(ctx, "diff", "--name-only", "--no-renames", "-z", base, head, "--")
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range strings.SplitSeq(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// show returns the contents of path at commit, and false if it doesn't exist.
func (g gitRepo) show(ctx context.Context, commit, file string) ([]byte, bool, error) {
	if _, err := g.run(ctx, "cat-file", "-e", commit+":"+file); err != nil {
		return nil, false, nil //nolint:nilerr // a missing path is not an error
	}
	out, err := g.run(ctx, "cat-file", "blob", commit+":"+file)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

var errNoManifest = errors.New("no " + manifestPath + " found")

func loadManifest(ctx context.Context, g gitRepo, commit string) (manifest, error) {
	data, ok, err := g.show(ctx, commit, manifestPath)
	if err != nil {
		return manifest{}, err
	}
	if !ok {
		return manifest{}, fmt.Errorf("%w at %s", errNoManifest, commit)
	}
	m, err := parseManifest(data)
	if err != nil {
		return manifest{}, fmt.Errorf("%s at %s: %w", manifestPath, commit, err)
	}
	return m, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
