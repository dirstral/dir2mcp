package cli

import (
	"testing"
)

func TestParseGlobalOptions_FlagPositionAgnostic(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "global before subcommand",
			args: []string{"--dir", "/tmp/x", "up", "--listen", "127.0.0.1:0"},
		},
		{
			name: "global after subcommand",
			args: []string{"up", "--dir", "/tmp/x", "--listen", "127.0.0.1:0"},
		},
		{
			name: "global at end",
			args: []string{"up", "--listen", "127.0.0.1:0", "--dir", "/tmp/x"},
		},
		{
			name: "global on both sides",
			args: []string{"--quiet", "up", "--dir", "/tmp/x", "--listen", "127.0.0.1:0"},
		},
		{
			name: "equals form after subcommand",
			args: []string{"up", "--dir=/tmp/x", "--listen", "127.0.0.1:0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, remaining, err := parseGlobalOptions(tc.args)
			if err != nil {
				t.Fatalf("parseGlobalOptions: %v", err)
			}
			if opts.rootDir != "/tmp/x" {
				t.Errorf("rootDir: want /tmp/x, got %q", opts.rootDir)
			}
			if len(remaining) == 0 || remaining[0] != "up" {
				t.Fatalf("remaining: want [up ...], got %#v", remaining)
			}
			upOpts, err := parseUpOptions(opts, remaining[1:])
			if err != nil {
				t.Fatalf("parseUpOptions: %v", err)
			}
			if upOpts.listen != "127.0.0.1:0" {
				t.Errorf("listen: want 127.0.0.1:0, got %q", upOpts.listen)
			}
			if upOpts.rootDir != "/tmp/x" {
				t.Errorf("upOpts.rootDir not propagated: %q", upOpts.rootDir)
			}
		})
	}
}

func TestParseGlobalOptions_UnknownFlagBeforeCommandStillErrors(t *testing.T) {
	_, _, err := parseGlobalOptions([]string{"--bogus", "up"})
	if err == nil {
		t.Fatal("expected error for unknown flag before command, got nil")
	}
}

func TestParseGlobalOptions_DoubleDashStopsParsing(t *testing.T) {
	opts, remaining, err := parseGlobalOptions([]string{"up", "--", "--dir", "/should/be/ignored"})
	if err != nil {
		t.Fatalf("parseGlobalOptions: %v", err)
	}
	if opts.rootDir != "" {
		t.Errorf("rootDir should be empty after --, got %q", opts.rootDir)
	}
	if len(remaining) != 4 || remaining[0] != "up" || remaining[1] != "--" {
		t.Errorf("remaining should preserve -- and trailing args, got %#v", remaining)
	}
}
