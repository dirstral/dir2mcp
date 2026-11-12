package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

const (
	// libreofficeInputName is the file name the .doc bytes get inside the
	// private work directory. A fixed ASCII name keeps the source path (which
	// can hold any characters) out of the command line, and LibreOffice names
	// its output after it (input.docx).
	libreofficeInputName = "input.doc"
	// maxLibreOfficeDocxBytes caps the .docx read back from the conversion. A
	// .docx is a zip of the same content, so it is never much larger than the
	// .doc; the cap only bounds a pathological conversion.
	maxLibreOfficeDocxBytes = 256 * 1024 * 1024
	// maxLibreOfficeLogBytes caps the LibreOffice stdout+stderr kept for an
	// error message.
	maxLibreOfficeLogBytes = 64 * 1024
)

// libreofficeExtractor is the libreoffice extraction engine (T2, spec 0.93.0).
// It converts a legacy binary Word document (.doc) to .docx with headless
// LibreOffice in a private temp directory, then reads that .docx with the
// existing pandoc docx path. The output is therefore the same flat Markdown,
// with the same provenance, as a .docx the pandoc engine reads: no pages, no
// bboxes, no region spans.
//
// It never writes into the corpus: it gets the bytes, not the path, and every
// file it creates (the input copy, the per-conversion profile and the output
// directory) lives under one os.MkdirTemp directory that it removes after the
// read step, on success, failure and timeout alike.
type libreofficeExtractor struct {
	// soffice is the configured LibreOffice binary (ingest.libreoffice.command).
	// The whole value is the path, so a path with spaces works. Empty resolves
	// soffice (then libreoffice) from PATH at call time.
	soffice string
	// timeout bounds one extraction of one document: the conversion AND the
	// pandoc read together (ingest.libreoffice.timeout_sec).
	timeout time.Duration
	// pandoc reads the converted .docx.
	pandoc *pandocExtractor
}

// NewLibreOfficeExtractor returns the libreoffice engine. sofficeCmd is the
// configured ingest.libreoffice.command (blank resolves soffice, then
// libreoffice, from PATH). pandocCmd is the configured ingest.pandoc.command
// (blank resolves pandoc from PATH), so the .docx step uses exactly the pandoc
// the pandoc engine uses. A timeout of 0 or less falls back to the default.
func NewLibreOfficeExtractor(sofficeCmd, pandocCmd string, timeout time.Duration) *libreofficeExtractor {
	if timeout <= 0 {
		timeout = defaultLibreOfficeTimeout
	}
	return &libreofficeExtractor{
		soffice: strings.TrimSpace(sofficeCmd),
		timeout: timeout,
		pandoc:  NewPandocExtractor(pandocCmd),
	}
}

// defaultLibreOfficeTimeout mirrors config.DefaultLibreOfficeTimeoutSec for
// callers that build the extractor without a config.
const defaultLibreOfficeTimeout = 300 * time.Second

// libreofficeTimeoutError is the per-document error for an extraction that hit
// ingest.libreoffice.timeout_sec. It names the limit so the operator knows
// which setting to raise.
type libreofficeTimeoutError struct {
	limit time.Duration
}

func (e *libreofficeTimeoutError) Error() string {
	return fmt.Sprintf("libreoffice extraction timed out after %s (ingest.libreoffice.timeout_sec=%d)", e.limit, int64(e.limit/time.Second))
}

// Extract converts data (a .doc) to .docx and returns the Markdown pandoc
// reads from it. One deadline (l.timeout) covers both steps; when it expires,
// the LibreOffice process group or the pandoc process is killed. relPath is not
// used: the conversion works on the bytes only, so the source path never
// reaches a command line or a temp file name (the caller names the document in
// the error).
func (l *libreofficeExtractor) Extract(ctx context.Context, _ string, data []byte) (string, error) {
	bin, err := l.sofficeBinary()
	if err != nil {
		return "", err
	}
	work, err := os.MkdirTemp("", "dir2mcp-libreoffice-*")
	if err != nil {
		return "", fmt.Errorf("create libreoffice work dir: %w", err)
	}
	// Removed after the read step, and after a failure or a timeout in either
	// step.
	defer func() { _ = os.RemoveAll(work) }()

	dctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	docx, err := l.convertToDocx(dctx, bin, work, data)
	if err != nil {
		return "", l.deadlineErr(ctx, dctx, err)
	}
	// The pandoc step infers the input format from the extension, so the
	// converted bytes are handed over under a .docx name.
	md, err := l.pandoc.Extract(dctx, "converted.docx", docx)
	if err != nil {
		return "", l.deadlineErr(ctx, dctx, fmt.Errorf("read converted .docx: %w", err))
	}
	return md, nil
}

// deadlineErr maps an error that happened because the per-document deadline
// expired (and not because the caller cancelled) to libreofficeTimeoutError.
func (l *libreofficeExtractor) deadlineErr(parent, dctx context.Context, err error) error {
	if errors.Is(dctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		return &libreofficeTimeoutError{limit: l.timeout}
	}
	return err
}

// sofficeBinary returns the LibreOffice executable to run: the configured
// ingest.libreoffice.command, else soffice or libreoffice from PATH. It uses
// the same resolution as the availability probe.
func (l *libreofficeExtractor) sofficeBinary() (string, error) {
	if bin, ok := resolveSofficeBinary(config.Config{IngestLibreOfficeCommand: l.soffice}); ok {
		return bin, nil
	}
	return "", errors.New("soffice (LibreOffice) not found on PATH")
}

// convertToDocx runs one headless LibreOffice conversion inside work and
// returns the .docx bytes. work holds the input copy, the output directory and
// a per-conversion LibreOffice profile, so conversions can run in parallel
// (LibreOffice locks its profile) and two documents with the same name never
// share an output file. The caller owns and removes work.
func (l *libreofficeExtractor) convertToDocx(ctx context.Context, bin, work string, data []byte) ([]byte, error) {
	profileDir := filepath.Join(work, "profile")
	outDir := filepath.Join(work, "out")
	for _, dir := range []string{profileDir, outDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create libreoffice work dir: %w", err)
		}
	}
	inPath := filepath.Join(work, libreofficeInputName)
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write libreoffice input: %w", err)
	}
	if err := runSofficeConversion(ctx, bin, profileDir, outDir, inPath); err != nil {
		return nil, err
	}
	return readConvertedDocx(filepath.Join(outDir, strings.TrimSuffix(libreofficeInputName, ".doc")+".docx"))
}

// runSofficeConversion runs soffice under ctx. A cancelled ctx (the deadline)
// stops the whole process tree (prepareLibreOfficeCmd), and on unix a finished
// run also kills what is left of the process group, so no soffice.bin helper
// survives the conversion. cmd.Dir is left unset on purpose: every path on the
// command line is absolute, and a working directory would change how a
// relative configured binary is found (Go resolves it against cmd.Dir).
func runSofficeConversion(ctx context.Context, bin, profileDir, outDir, inPath string) error {
	cmd := exec.CommandContext(ctx, bin,
		"--headless", "--norestore",
		"-env:UserInstallation="+fileURL(profileDir),
		"--convert-to", "docx",
		"--outdir", outDir,
		inPath,
	)
	cmd.Env = os.Environ()
	prepareLibreOfficeCmd(cmd)
	cmd.WaitDelay = pandocWaitDelay
	var logs bytes.Buffer
	sink := &limitedBuffer{buf: &logs, limit: maxLibreOfficeLogBytes}
	cmd.Stdout = sink
	cmd.Stderr = sink
	runErr := cmd.Run()
	reapLibreOfficeGroup(cmd)
	if runErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	msg := strings.TrimSpace(logs.String())
	if msg == "" {
		msg = runErr.Error()
	}
	return fmt.Errorf("libreoffice conversion failed: %s", msg)
}

// readConvertedDocx reads the conversion output. soffice can exit 0 without
// writing the output (an unreadable or encrypted input), so a missing or empty
// file is an error, not an empty document.
func readConvertedDocx(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("libreoffice conversion produced no .docx output")
		}
		return nil, fmt.Errorf("stat libreoffice output: %w", err)
	}
	if info.Size() == 0 {
		return nil, errors.New("libreoffice conversion produced an empty .docx")
	}
	if info.Size() > maxLibreOfficeDocxBytes {
		return nil, errors.New("libreoffice conversion output exceeded limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read libreoffice output: %w", err)
	}
	return data, nil
}

// fileURL renders an absolute directory as the file:// URL LibreOffice expects
// for -env:UserInstallation. A Windows drive path (C:/x) gets the leading slash
// a file URL needs (file:///C:/x); special characters are percent-encoded.
func fileURL(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
