package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultDoclingCommand = "docling --to md --output - {input}"

const (
	maxDoclingStdoutBytes = 100 * 1024 * 1024
	maxDoclingStderrBytes = 1 * 1024 * 1024
)

var errDoclingOutputTooLarge = errors.New("docling output exceeded configured limit")

type doclingExtractor struct {
	commandTemplate string
}

type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if w.limit > 0 && w.buf.Len()+len(p) > w.limit {
		return 0, errDoclingOutputTooLarge
	}
	return w.buf.Write(p)
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
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write temp doc file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp doc file: %w", err)
	}

	args, err := buildDoclingCommandArgs(d.commandTemplate, tmpPath)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &stdout, limit: maxDoclingStdoutBytes}
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: maxDoclingStderrBytes}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, errDoclingOutputTooLarge) {
			return "", fmt.Errorf("docling command output exceeded limit")
		}
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

func buildDoclingCommandArgs(template, inputPath string) ([]string, error) {
	parts := strings.Fields(strings.TrimSpace(template))
	if len(parts) == 0 {
		return nil, fmt.Errorf("docling command is empty")
	}
	replaced := false
	for i := range parts {
		if strings.Contains(parts[i], "{input}") {
			parts[i] = strings.ReplaceAll(parts[i], "{input}", inputPath)
			replaced = true
		}
	}
	if !replaced {
		parts = append(parts, inputPath)
	}
	return parts, nil
}
