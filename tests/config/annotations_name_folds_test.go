package tests

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// annotations.name_folds (SPEC §7.11.4, §16.2): the optional character folds of
// editorial person ids. Default empty; an invalid list is CONFIG_INVALID at
// load (conformance case 6).

func TestAnnotationsNameFolds_DefaultsEmpty(t *testing.T) {
	if got := config.Default().AnnotationsNameFolds; len(got) != 0 {
		t.Fatalf("annotations.name_folds must default to empty, got %#v", got)
	}
}

func loadNameFolds(t *testing.T, lines ...string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, strings.Join(append([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
	}, lines...), "\n")+"\n")
	return config.LoadFile(path)
}

func TestAnnotationsNameFolds_NestedBlockAndFlowList(t *testing.T) {
	for name, lines := range map[string][]string{
		"block list": {"annotations:", "  name_folds:", "    - \"ъ=е\"", "    - \"ё=е\""},
		"flow list":  {"annotations:", "  name_folds: [\"ъ=е\", \"ё=е\"]"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadNameFolds(t, lines...)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if want := []string{"ъ=е", "ё=е"}; !reflect.DeepEqual(cfg.AnnotationsNameFolds, want) {
				t.Fatalf("name_folds = %#v, want %#v", cfg.AnnotationsNameFolds, want)
			}
		})
	}
}

func TestAnnotationsNameFolds_InvalidListsAreConfigInvalid(t *testing.T) {
	for name, value := range map[string]string{
		"chain":                    `["a=b", "b=c"]`,
		"to changed by lower case": `["ё=Е"]`,
		"white space side":         `["x= "]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadNameFolds(t, "annotations:", "  name_folds: "+value)
			if err == nil {
				t.Fatalf("name_folds %s was accepted", value)
			}
			if !strings.Contains(err.Error(), "CONFIG_INVALID") {
				t.Fatalf("error %q does not carry CONFIG_INVALID", err)
			}
		})
	}
}

func TestAnnotationsNameFolds_RoundTripsThroughSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.AnnotationsNameFolds = []string{"ё=е"}
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(loaded.AnnotationsNameFolds, cfg.AnnotationsNameFolds) {
		t.Fatalf("name_folds did not round-trip: %#v", loaded.AnnotationsNameFolds)
	}
}
