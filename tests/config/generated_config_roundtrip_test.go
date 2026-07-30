package tests

// Anti-drift guard: every scalar key SaveFile writes — i.e. every key
// `dir2mcp config init` puts in the generated .dir2mcp.yaml — must be readable
// back by LoadFile.
//
// #624 was an instance of this failure mode: SaveFile emitted
// recognize_provider / recognize_serve_url / recognize_serve_command, LoadFile
// dispatches on the dotted spellings, and the configKeyAliases table had no
// recognize entries — so an operator who enabled recognition by editing the
// generated file got silence. The keys were dropped, recognition stayed off with
// no error and no warning, and because the provider never reached the runtime
// config, validateRecognizeProvider never fired on an incomplete `serve` binding
// either.
//
// The gap was invisible because writer and loader are separate code paths with
// separate key spellings, bridged only by a hand-maintained alias table. This
// test closes the loop: add a snapshot key without a matching loader binding and
// it fails here instead of silently ignoring operator config.
//
// Method: write the generated config with ONE key replaced by a sentinel value,
// load it, and assert the result differs from loading the untouched file. A key
// whose sentinel changes nothing is being ignored. (A validation error also
// proves the key was read.)

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// roundTripSentinels returns values that are type-plausible for the current
// value and certain to differ from it, so one of them must move the field.
func roundTripSentinels(existing string) []string {
	e := strings.TrimSpace(existing)
	switch e {
	case "true":
		return []string{"false"}
	case "false":
		return []string{"true"}
	case "":
		// Unset: type unknown, so try each shape.
		return []string{"zzsentinel", "4242", "true", "42s"}
	}
	if strings.IndexFunc(e, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return []string{e + "7"} // integer
	}
	if strings.HasSuffix(e, "s") || strings.HasSuffix(e, "m") || strings.HasSuffix(e, "h") {
		return []string{"99h9m9s"} // duration
	}
	return []string{"zzsentinel", "4242", "true"}
}

func TestGeneratedConfig_EveryWrittenKeyIsReadable(t *testing.T) {
	dir := t.TempDir()
	genPath := filepath.Join(dir, ".dir2mcp.yaml")
	if err := config.SaveFile(genPath, config.Default()); err != nil {
		t.Fatalf("SaveFile (what `config init` writes): %v", err)
	}
	raw, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	base, err := config.LoadFile(genPath)
	if err != nil {
		t.Fatalf("baseline LoadFile of the generated config: %v", err)
	}

	var ignored []string
	for i, line := range lines {
		key, value, ok := topLevelScalar(lines, i)
		if !ok {
			continue
		}
		read := false
		for _, sentinel := range roundTripSentinels(value) {
			mutated := append([]string{}, lines...)
			mutated[i] = key + ": " + sentinel
			p := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			if err := os.WriteFile(p, []byte(strings.Join(mutated, "\n")), 0o600); err != nil {
				t.Fatalf("write mutated config: %v", err)
			}
			got, loadErr := config.LoadFile(p)
			// A validation failure also proves the value reached the loader.
			if loadErr != nil || !reflect.DeepEqual(got, base) {
				read = true
				break
			}
		}
		if !read {
			ignored = append(ignored, key)
		}
		_ = line
	}

	if len(ignored) > 0 {
		t.Errorf("SaveFile writes %d key(s) that LoadFile ignores: %s\n"+
			"An operator editing the generated config would be silently ignored (see #624). "+
			"Add a configKeyAliases entry mapping each to its canonical dotted key.",
			len(ignored), strings.Join(ignored, ", "))
	}
}

// topLevelScalar reports the key/value of lines[i] when it is a top-level
// `key: value` scalar, skipping comments, blanks, indented children and the
// header lines of nested sections/lists.
func topLevelScalar(lines []string, i int) (key, value string, ok bool) {
	line := lines[i]
	trimmed := strings.TrimLeft(line, " \t")
	if line != trimmed || trimmed == "" || strings.HasPrefix(trimmed, "#") ||
		!strings.Contains(trimmed, ":") {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, ":", 2)
	key = strings.TrimSpace(parts[0])
	value = strings.TrimSpace(parts[1])
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	// A section/list header has no inline value and indented children below it.
	if value == "" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") {
		return "", "", false
	}
	return key, value, true
}
