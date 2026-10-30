// Package build holds the checks that keep builder-config.yaml a faithful
// derivative of the pinned Dynatrace distribution manifest.
package build

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	pinnedManifest = "dynatrace-manifest-v0.58.1.yaml"
	modulePath     = "github.com/pawelsiwek/bluebox-k8s-collector/collector"
)

type entry struct {
	GoMod  string `yaml:"gomod"`
	Import string `yaml:"import"`
}

type manifest struct {
	Receivers  []entry  `yaml:"receivers"`
	Exporters  []entry  `yaml:"exporters"`
	Extensions []entry  `yaml:"extensions"`
	Processors []entry  `yaml:"processors"`
	Connectors []entry  `yaml:"connectors"`
	Providers  []entry  `yaml:"providers"`
	Replaces   []string `yaml:"replaces"`
}

func load(t *testing.T, path string) manifest {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func key(e entry) string {
	if e.Import != "" {
		return e.Import
	}
	return e.GoMod
}

func set(m manifest) map[string]bool {
	out := map[string]bool{}
	for _, list := range [][]entry{m.Receivers, m.Exporters, m.Extensions, m.Processors, m.Connectors, m.Providers} {
		for _, e := range list {
			out[key(e)] = true
		}
	}
	return out
}

// builder-config.yaml = pinned Dynatrace manifest - eecprovider + groupbyattrs + 3 Bluebox components,
// every shared component at the identical version.
func TestBuilderConfigDerivesFromPinnedDynatraceManifest(t *testing.T) {
	dt := set(load(t, pinnedManifest))
	bb := set(load(t, filepath.Join("..", "builder-config.yaml")))

	var missing, added []string
	for k := range dt {
		if !bb[k] && !strings.Contains(k, "eecprovider") {
			missing = append(missing, k)
		}
	}
	allowedAdds := map[string]bool{
		"github.com/open-telemetry/opentelemetry-collector-contrib/processor/groupbyattrsprocessor v0.162.0": true,
		modulePath + "/processor/dtk8stopology":        true,
		modulePath + "/processor/dtk8seventsprocessor": true,
		modulePath + "/connector/alertingengine":       true,
	}
	for k := range bb {
		if !dt[k] && !allowedAdds[k] {
			added = append(added, k)
		}
	}
	for k := range allowedAdds {
		if !bb[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	if len(missing) > 0 || len(added) > 0 {
		t.Fatalf("builder-config.yaml drifted from %s\nmissing or version-changed: %v\nunexpected: %v", pinnedManifest, missing, added)
	}
}

// The module's own collector API pins must match the release the distribution builds against,
// or OCB's module resolution silently upgrades the whole distribution.
func TestModulePinsMatchDistribution(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[0], "go.opentelemetry.io/collector/") {
			continue
		}
		if f[1] != "v0.162.0" && f[1] != "v1.68.0" {
			t.Errorf("%s %s: want v0.162.0 / v1.68.0 to match the pinned distribution", f[0], f[1])
		}
	}
}
