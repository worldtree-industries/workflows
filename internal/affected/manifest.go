package affected

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"

	"go.yaml.in/yaml/v3"
)

const (
	manifestPath = ".forge.yaml"

	// DefaultBaseImage is the base image used when a Deployable sets none.
	DefaultBaseImage = "gcr.io/distroless/static-debian12:nonroot"

	buildKo         = "ko"
	buildDockerfile = "dockerfile"
)

var (
	errVersion = errors.New("unsupported manifest version")
	errEntry   = errors.New("invalid binary entry")
	nameRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// manifest is a parsed .forge.yaml.
type manifest struct {
	Version  int              `yaml:"version"`
	Binaries map[string]entry `yaml:"binaries"`
}

// entry is one binary's settings. raw keeps every key the user wrote, so
// that a change to any of them counts as a change to the entry.
type entry struct {
	Kind      string `yaml:"kind"`
	BaseImage string `yaml:"base_image"`
	Build     string `yaml:"build"`
	raw       map[string]any
}

func (e *entry) UnmarshalYAML(node *yaml.Node) error {
	type plain entry
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	var raw map[string]any
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*e = entry(p)
	e.raw = raw
	return nil
}

func parseManifest(data []byte) (manifest, error) {
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return manifest{}, err
	}
	if m.Version != 1 {
		return manifest{}, fmt.Errorf("%w %d (want 1)", errVersion, m.Version)
	}
	for name, e := range m.Binaries {
		if !nameRE.MatchString(name) {
			return manifest{}, fmt.Errorf("%w %q: name must match %s", errEntry, name, nameRE)
		}
		switch e.Kind {
		case "http", "job", "tool":
		default:
			return manifest{}, fmt.Errorf("%w %q: unknown kind %q", errEntry, name, e.Kind)
		}
		switch e.Build {
		case "", buildKo, buildDockerfile:
		default:
			return manifest{}, fmt.Errorf("%w %q: unknown build %q", errEntry, name, e.Build)
		}
	}
	return m, nil
}

// deployables returns the names of every binary that isn't a Tool, sorted.
func (m manifest) deployables() []string {
	var out []string
	for _, name := range sortedKeys(m.Binaries) {
		if m.Binaries[name].Kind != "tool" {
			out = append(out, name)
		}
	}
	return out
}

func (e entry) buildMethod() string {
	if e.Build == buildDockerfile {
		return buildDockerfile
	}
	return buildKo
}

func (e entry) baseImage() string {
	if e.BaseImage == "" {
		return DefaultBaseImage
	}
	return e.BaseImage
}

func (e entry) equal(other entry) bool {
	return reflect.DeepEqual(e.raw, other.raw)
}
