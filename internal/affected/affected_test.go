package affected_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/worldtree-industries/workflows/internal/affected"
)

// project is a throwaway git repository holding a Project.
type project struct {
	t   *testing.T
	dir string
}

func newProject(t *testing.T) *project {
	t.Helper()
	p := &project{t: t, dir: t.TempDir()}
	p.git("init", "-q", "-b", "main")
	return p
}

func (p *project) git(args ...string) string {
	p.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// write writes files (path -> content) into the working tree.
func (p *project) write(files map[string]string) {
	p.t.Helper()
	for name, content := range files {
		path := filepath.Join(p.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			p.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			p.t.Fatal(err)
		}
	}
}

func (p *project) remove(name string) {
	p.t.Helper()
	p.git("rm", "-q", name)
}

// commit writes files, commits everything and returns the commit SHA.
func (p *project) commit(files map[string]string) string {
	p.t.Helper()
	p.write(files)
	p.git("add", "-A")
	p.git("commit", "-q", "--allow-empty", "-m", "change")
	return p.git("rev-parse", "HEAD")
}

type deployable struct {
	Name      string `json:"name"`
	Build     string `json:"build"`
	BaseImage string `json:"base_image"`
}

const defaultBase = "gcr.io/distroless/static-debian12:nonroot"

// affectedBetween runs the command and decodes its JSON output.
func affectedBetween(t *testing.T, p *project, base, head string) []deployable {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := affected.Main(t.Context(), []string{"-repo", p.dir, "-base", base, "-head", head}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr.String())
	}
	var got []deployable
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decoding output %q: %v", stdout.String(), err)
	}
	if got == nil {
		t.Fatalf("output %q is not a JSON array", stdout.String())
	}
	return got
}

func names(ds []deployable) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

func assertNames(t *testing.T, got []deployable, want ...string) {
	t.Helper()
	gotNames := names(got)
	if want == nil {
		want = []string{}
	}
	if len(gotNames) != len(want) {
		t.Fatalf("affected = %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("affected = %v, want %v", gotNames, want)
		}
	}
}

const goMod = "module example.com/proj\n\ngo 1.27\n"

func mainImporting(imports ...string) string {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, imp := range imports {
		b.WriteString("\t_ \"example.com/proj/" + imp + "\"\n")
	}
	b.WriteString(")\n\nfunc main() {}\n")
	return b.String()
}

// baseProject is a Project with two http Deployables sharing the models
// layer, one of them also using service, a job with no layer imports, and
// a Tool that imports models.
func baseProject() map[string]string {
	return map[string]string{
		"go.mod": goMod,
		".forge.yaml": `version: 1
binaries:
  api:
    kind: http
    port: 8080
  admin:
    kind: http
    port: 8081
  reaper:
    kind: job
    schedule: daily
  wolfctl:
    kind: tool
`,
		"models/models.go":       "package models\n",
		"data/data.go":           "package data\n\nimport _ \"example.com/proj/models\"\n",
		"service/service.go":     "package service\n\nimport _ \"example.com/proj/data\"\n",
		"cmd/api/main.go":        mainImporting("service"),
		"cmd/admin/main.go":      mainImporting("models"),
		"cmd/reaper/main.go":     mainImporting(),
		"cmd/wolfctl/main.go":    mainImporting("models"),
		"README.md":              "# proj\n",
		"service/testdata/x.txt": "x\n",
	}
}

func TestFirstReleaseAffectsEveryDeployableButNoTool(t *testing.T) {
	p := newProject(t)
	head := p.commit(baseProject())

	got := affectedBetween(t, p, "", head)

	want := []deployable{
		{Name: "admin", Build: "ko", BaseImage: defaultBase},
		{Name: "api", Build: "ko", BaseImage: defaultBase},
		{Name: "reaper", Build: "ko", BaseImage: defaultBase},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestChangeInPackageOnlyOneDeployableDependsOnAffectsOnlyIt(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{
		"service/service.go": "package service\n\nimport _ \"example.com/proj/data\"\n\n// X changed.\nvar X = 1\n",
	})

	assertNames(t, affectedBetween(t, p, base, head), "api")
}

func TestChangeInSharedPackageAffectsEveryDependentDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{"models/user.go": "package models\n\n// User is new.\ntype User struct{}\n"})

	// reaper doesn't import models; wolfctl does but is a Tool.
	assertNames(t, affectedBetween(t, p, base, head), "admin", "api")
}

func TestChangeOutsideAnyPackageAffectsNothing(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{
		"README.md":              "# proj\n\nMore words.\n",
		"service/testdata/x.txt": "y\n",
		"cmd/api/notes.md":       "notes\n",
	})

	assertNames(t, affectedBetween(t, p, base, head))
}

func TestManifestEntryChangeAffectsThatDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{".forge.yaml": `version: 1
binaries:
  api:
    kind: http
    port: 8080
  admin:
    kind: http
    port: 9090
  reaper:
    kind: job
    schedule: daily
  wolfctl:
    kind: tool
`})

	assertNames(t, affectedBetween(t, p, base, head), "admin")
}

func withManifest(files map[string]string, manifest string) map[string]string {
	files[".forge.yaml"] = manifest
	return files
}

func TestBaseImageChangeAffectsThatDeployableAndReportsTheImage(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{".forge.yaml": `version: 1
binaries:
  api:
    kind: http
    port: 8080
  admin:
    kind: http
    port: 8081
  reaper:
    kind: job
    schedule: daily
    base_image: registry.werewolves.fyi/library/base-with-curl:1
  wolfctl:
    kind: tool
`})

	got := affectedBetween(t, p, base, head)

	want := deployable{Name: "reaper", Build: "ko", BaseImage: "registry.werewolves.fyi/library/base-with-curl:1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
}

const dockerfileManifest = `version: 1
binaries:
  api:
    kind: http
    port: 8080
  admin:
    kind: http
    port: 8081
    build: dockerfile
  reaper:
    kind: job
    schedule: daily
  wolfctl:
    kind: tool
`

func TestDockerfileChangeAffectsDockerfileDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(withManifest(baseProject(), dockerfileManifest))
	head := p.commit(map[string]string{"cmd/admin/Dockerfile": "FROM scratch\n"})

	got := affectedBetween(t, p, base, head)

	want := deployable{Name: "admin", Build: "dockerfile", BaseImage: defaultBase}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want [%+v]", got, want)
	}
}

func TestDockerfileChangeDoesNotAffectKoDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(withManifest(baseProject(), dockerfileManifest))
	head := p.commit(map[string]string{"cmd/api/Dockerfile": "FROM scratch\n"})

	assertNames(t, affectedBetween(t, p, base, head))
}

func TestGoModChangeAffectsEveryDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{"go.mod": goMod + "\n// bumped\n"})

	assertNames(t, affectedBetween(t, p, base, head), "admin", "api", "reaper")
}

func TestGoSumChangeAffectsEveryDeployable(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{"go.sum": "example.org/x v1.0.0 h1:abc=\n"})

	assertNames(t, affectedBetween(t, p, base, head), "admin", "api", "reaper")
}

func TestZeroSHABaseIsFirstRelease(t *testing.T) {
	p := newProject(t)
	head := p.commit(baseProject())

	assertNames(t, affectedBetween(t, p, "0000000000000000000000000000000000000000", head),
		"admin", "api", "reaper")
}

func TestFirstReleaseWithBaseOmitted(t *testing.T) {
	p := newProject(t)
	p.commit(baseProject())
	var stdout, stderr bytes.Buffer

	code := affected.Main(t.Context(), []string{"-repo", p.dir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code %d, stderr:\n%s", code, stderr.String())
	}
	want := `[{"name":"admin","build":"ko","base_image":"` + defaultBase + `"},` +
		`{"name":"api","build":"ko","base_image":"` + defaultBase + `"},` +
		`{"name":"reaper","build":"ko","base_image":"` + defaultBase + `"}]` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestToolsNeverAppear(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	head := p.commit(map[string]string{
		"cmd/wolfctl/main.go": mainImporting("models") + "\n// changed\n",
		".forge.yaml": `version: 1
binaries:
  api:
    kind: http
    port: 8080
  admin:
    kind: tool
  reaper:
    kind: job
    schedule: daily
  wolfctl:
    kind: tool
    base_image: example.com/ignored
`,
	})

	// admin became a Tool, wolfctl's code and entry changed: neither appears.
	assertNames(t, affectedBetween(t, p, base, head))
}

func TestNewDeployableIsAffected(t *testing.T) {
	p := newProject(t)
	base := p.commit(baseProject())
	files := withManifest(map[string]string{"cmd/pinger/main.go": mainImporting()}, baseProject()[".forge.yaml"]+
		"  pinger:\n    kind: job\n    schedule: none\n")
	head := p.commit(files)

	assertNames(t, affectedBetween(t, p, base, head), "pinger")
}

func TestDeletedFileInDependencyAffectsDependents(t *testing.T) {
	p := newProject(t)
	files := baseProject()
	files["data/extra.go"] = "package data\n"
	base := p.commit(files)
	p.remove("data/extra.go")
	head := p.commit(nil)

	assertNames(t, affectedBetween(t, p, base, head), "api")
}

func TestComparesAgainstTags(t *testing.T) {
	p := newProject(t)
	p.commit(baseProject())
	p.git("tag", "v1.0.0")
	p.commit(map[string]string{"cmd/reaper/main.go": mainImporting() + "\n// changed\n"})
	p.git("tag", "v1.1.0")

	assertNames(t, affectedBetween(t, p, "v1.0.0", "v1.1.0"), "reaper")
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		args     func(p *project) []string
		wantCode int
	}{
		{name: "unknown base ref", args: func(p *project) []string {
			return []string{"-repo", p.dir, "-base", "v9.9.9"}
		}, wantCode: 1},
		{name: "no manifest", manifest: "-", wantCode: 1},
		{name: "unsupported version", manifest: "version: 2\nbinaries: {}\n", wantCode: 1},
		{name: "unknown kind", manifest: "version: 1\nbinaries:\n  api:\n    kind: daemon\n", wantCode: 1},
		{
			name:     "unknown build",
			manifest: "version: 1\nbinaries:\n  api:\n    kind: http\n    build: bazel\n",
			wantCode: 1,
		},
		{name: "option-like ref", args: func(p *project) []string {
			return []string{"-repo", p.dir, "-head", "--output=/tmp/x"}
		}, wantCode: 1},
		{name: "bad name", manifest: "version: 1\nbinaries:\n  ../x:\n    kind: http\n", wantCode: 1},
		{name: "stray argument", args: func(p *project) []string {
			return []string{"-repo", p.dir, "extra"}
		}, wantCode: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProject(t)
			files := baseProject()
			switch tt.manifest {
			case "":
			case "-":
				delete(files, ".forge.yaml")
			default:
				files[".forge.yaml"] = tt.manifest
			}
			p.commit(files)
			args := []string{"-repo", p.dir}
			if tt.args != nil {
				args = tt.args(p)
			}
			var stdout, stderr bytes.Buffer

			code := affected.Main(t.Context(), args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Fatal("stderr is empty, want an error message")
			}
		})
	}
}

func TestChangeToEmbeddedFileAffectsDependents(t *testing.T) {
	p := newProject(t)
	files := baseProject()
	files["models/schema.json"] = "{}\n"
	files["models/embed.go"] = "package models\n\nimport _ \"embed\"\n\n//go:embed schema.json\nvar Schema string\n"
	base := p.commit(files)
	head := p.commit(map[string]string{"models/schema.json": "{\"v\":2}\n"})

	assertNames(t, affectedBetween(t, p, base, head), "admin", "api")
}
