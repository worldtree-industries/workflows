package affected

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// goPackage is the subset of `go list -json` output this command uses.
type goPackage struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Deps       []string `json:"Deps"`
	EmbedFiles []string `json:"EmbedFiles"`
	Module     *struct {
		Main bool `json:"Main"`
	} `json:"Module"`
}

// packageFiles maps each Deployable to the predicate "does this changed
// path belong to a Go package in its dependency closure?".
type packageFiles map[string]func(changed string) bool

// isSourceFile reports whether the go command compiles or links a file with
// this name into its package. A changed or deleted file like this in a
// package's directory counts as a change to that package.
func isSourceFile(name string) bool {
	switch path.Ext(name) {
	case ".go", ".c", ".h", ".s", ".S", ".sx", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx",
		".m", ".f", ".F", ".for", ".f90", ".swig", ".swigcxx", ".syso":
		return true
	default:
		return false
	}
}

// closures lists the Go packages of the Project at commit and, for each
// Deployable, returns which repo-relative paths belong to its dependency
// closure.
func closures(ctx context.Context, g gitRepo, commit string, deployables []string) (packageFiles, error) {
	tree, err := os.MkdirTemp("", "affected-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tree) }()

	if err = g.export(ctx, commit, tree); err != nil {
		return nil, err
	}
	pkgs, err := goList(ctx, tree)
	if err != nil {
		return nil, err
	}
	mod := newModulePackages(tree, pkgs)

	out := packageFiles{}
	for _, name := range deployables {
		out[name] = mod.closure(path.Join("cmd", name))
	}
	return out, nil
}

// modulePackages indexes the main module's packages by import path, with
// directories relative to the repo root.
type modulePackages struct {
	byPath map[string]goPackage
	dirOf  map[string]string
}

func newModulePackages(tree string, pkgs []goPackage) modulePackages {
	m := modulePackages{byPath: map[string]goPackage{}, dirOf: map[string]string{}}
	for _, p := range pkgs {
		if p.Module == nil || !p.Module.Main {
			continue
		}
		rel, ok := relDir(tree, p.Dir)
		if !ok {
			continue
		}
		m.byPath[p.ImportPath] = p
		m.dirOf[p.ImportPath] = rel
	}
	return m
}

// closure returns the predicate for the binary whose main package lives in
// mainDir. The directory itself always counts, even when it holds no
// buildable package.
func (m modulePackages) closure(mainDir string) func(string) bool {
	dirs := map[string]bool{mainDir: true}
	embeds := map[string]bool{}
	for importPath, dir := range m.dirOf {
		if dir != mainDir {
			continue
		}
		for _, dep := range append([]string{importPath}, m.byPath[importPath].Deps...) {
			depDir, inModule := m.dirOf[dep]
			if !inModule {
				continue
			}
			dirs[depDir] = true
			for _, f := range m.byPath[dep].EmbedFiles {
				embeds[path.Join(depDir, f)] = true
			}
		}
	}
	return func(changed string) bool {
		return embeds[changed] || (dirs[path.Dir(changed)] && isSourceFile(changed))
	}
}

func relDir(root, dir string) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// goList runs `go list` over every package in the module rooted at dir, as
// it would be built for a linux/amd64 image.
func goList(ctx context.Context, dir string) ([]goPackage, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json=ImportPath,Dir,Deps,EmbedFiles,Module", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOWORK=off")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []goPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p goPackage
		if decErr := dec.Decode(&p); errors.Is(decErr, io.EOF) {
			return pkgs, nil
		} else if decErr != nil {
			return nil, fmt.Errorf("decoding go list output: %w", decErr)
		}
		pkgs = append(pkgs, p)
	}
}

// export writes the tree of commit into dir.
func (g gitRepo) export(ctx context.Context, commit, dir string) error {
	archive, err := g.run(ctx, "archive", "--format=tar", commit)
	if err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return fmt.Errorf("reading archive of %s: %w", commit, nextErr)
		}
		if err = extract(dir, hdr, tr); err != nil {
			return err
		}
	}
}

func extract(dir string, hdr *tar.Header, r io.Reader) error {
	target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
	if _, ok := relDir(dir, target); !ok {
		return fmt.Errorf("archive entry %q escapes the tree", hdr.Name)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o750)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err = io.Copy(f, r); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	default:
		// Symlinks and git's pax header are skipped; the go command
		// doesn't follow symlinks when matching packages.
		return nil
	}
}
