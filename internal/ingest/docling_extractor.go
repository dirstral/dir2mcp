package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultDoclingCommand = "docling --to md --output - {input}"

type doclingExtractor struct {
	commandTemplate string
}

// NewDoclingExtractor returns a document extractor backed by a local docling
// CLI invocation. If commandTemplate is blank, a default `docling` command is
// used. The template may include `{input}`; when omitted, input is appended.
func NewDoclingExtractor(commandTemplate string) *doclingExtractor {
	tpl := strings.TrimSpace(commandTemplate)
	if tpl == "" {
		tpl = defaultDoclingCommand
	}
	return &doclingExtractor{commandTemplate: tpl}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (d *doclingExtractor) Extract(ctx context.Context, relPath string, data []byte) (string, error) {
	ext := filepath.Ext(relPath)
	if ext == "" {
		ext = ".bin"
	}
	tmpFile, err := os.CreateTemp("", "dir2mcp-docling-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp doc file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write temp doc file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp doc file: %w", err)
	}

	quoted := shellQuote(tmpPath)
	cmdline := d.commandTemplate
	if strings.Contains(cmdline, "{input}") {
		cmdline = strings.ReplaceAll(cmdline, "{input}", quoted)
	} else {
		cmdline = cmdline + " " + quoted
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docling command failed: %s", msg)
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("docling command produced empty output")
	}
	return out, nil
}
