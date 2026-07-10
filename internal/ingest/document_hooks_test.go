package ingest

import (
	"context"
	"errors"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// docErrorStubStore is the minimal model.Store needed to drive
// persistNonFatalDocError. upsertErr lets a test simulate a failing store.
type docErrorStubStore struct {
	mu        sync.Mutex
	upserted  []model.Document
	upsertErr error
}

func (s *docErrorStubStore) Init(context.Context) error { return nil }

func (s *docErrorStubStore) UpsertDocument(_ context.Context, doc model.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserted = append(s.upserted, doc)
	return s.upsertErr
}

func (s *docErrorStubStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}

func (s *docErrorStubStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}

func (s *docErrorStubStore) Close() error { return nil }

func newDocErrorService(store model.Store) *Service {
	return &Service{store: store, logger: log.New(io.Discard, "", 0)}
}

// TestPersistNonFatalDocError_NotifiesWithRedactedMessage is the security-
// relevant assertion for #414: the per-document `file_error` stream event must
// carry the redacted message, never the raw error text. A stream consumer is an
// untrusted sink (server.log, a piped process), so a leaked credential here is
// as bad as one in the store.
func TestPersistNonFatalDocError_NotifiesWithRedactedMessage(t *testing.T) {
	store := &docErrorStubStore{}
	svc := newDocErrorService(store)

	var (
		gotRelPath string
		gotDocType string
		gotMessage string
		calls      int
	)
	svc.SetOnDocumentError(func(relPath, docType, message string) {
		calls++
		gotRelPath, gotDocType, gotMessage = relPath, docType, message
	})

	secret := regexp.MustCompile(`s3cr3t-[a-z0-9]+`)
	doc := model.Document{RelPath: "docs/broken.pdf", DocType: "pdf"}
	cause := errors.New("ocr call failed: token s3cr3t-abc123 rejected")

	svc.persistNonFatalDocError(context.Background(), doc, cause, []*regexp.Regexp{secret})

	if calls != 1 {
		t.Fatalf("callback invoked %d times, want exactly 1", calls)
	}
	if gotRelPath != "docs/broken.pdf" {
		t.Errorf("relPath = %q, want docs/broken.pdf", gotRelPath)
	}
	if gotDocType != "pdf" {
		t.Errorf("docType = %q, want pdf", gotDocType)
	}
	if strings.Contains(gotMessage, "s3cr3t-abc123") {
		t.Fatalf("callback received an unredacted secret: %q", gotMessage)
	}
	if !strings.Contains(gotMessage, "ocr call failed") {
		t.Errorf("callback lost the actionable error text: %q", gotMessage)
	}

	// The persisted document must carry the same redacted message.
	if len(store.upserted) != 1 {
		t.Fatalf("upserted %d docs, want 1", len(store.upserted))
	}
	if store.upserted[0].Status != "error" {
		t.Errorf("persisted status = %q, want error", store.upserted[0].Status)
	}
	if store.upserted[0].ErrorMessage != gotMessage {
		t.Errorf("persisted message %q != streamed message %q", store.upserted[0].ErrorMessage, gotMessage)
	}
}

// TestPersistNonFatalDocError_NotifiesEvenWhenUpsertFails pins the deliberate
// ordering choice: the document genuinely failed, so the operator tailing
// `--json` must still learn about it even if we could not record it.
func TestPersistNonFatalDocError_NotifiesEvenWhenUpsertFails(t *testing.T) {
	store := &docErrorStubStore{upsertErr: errors.New("disk full")}
	svc := newDocErrorService(store)

	called := false
	svc.SetOnDocumentError(func(string, string, string) { called = true })

	svc.persistNonFatalDocError(context.Background(), model.Document{RelPath: "a.pdf"}, errors.New("boom"), nil)

	if !called {
		t.Fatal("callback was not invoked when the store upsert failed")
	}
}

// TestNotifyDocumentError_ContainsCallbackPanic guards the ingest run against a
// buggy stream consumer: a panicking callback must not abort indexing.
func TestNotifyDocumentError_ContainsCallbackPanic(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	svc.SetOnDocumentError(func(string, string, string) { panic("consumer blew up") })

	svc.notifyDocumentError(model.Document{RelPath: "a.pdf"}) // must not panic
}

func TestNotifyDocumentError_NilCallbackIsNoop(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	svc.notifyDocumentError(model.Document{RelPath: "a.pdf"})

	svc.SetOnDocumentError(func(string, string, string) { t.Fatal("cleared callback was invoked") })
	svc.SetOnDocumentError(nil)
	svc.notifyDocumentError(model.Document{RelPath: "a.pdf"})
}

// --- file_skip (#414) ---

// captureSkips registers a skip callback and returns the slice it appends to.
func captureSkips(svc *Service) *[]string {
	var got []string
	svc.SetOnDocumentSkip(func(relPath, docType, reason string) {
		got = append(got, relPath+"|"+docType+"|"+reason)
	})
	return &got
}

func TestNotifyDocumentSkip_CarriesReason(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	got := captureSkips(svc)

	svc.notifyDocumentSkip(model.Document{
		RelPath:    "notes/report.odt",
		DocType:    "document",
		Status:     "skipped",
		SkipReason: model.SkipReasonUnsupportedFormat,
	})

	want := "notes/report.odt|document|" + model.SkipReasonUnsupportedFormat
	if len(*got) != 1 || (*got)[0] != want {
		t.Fatalf("skip events = %v, want [%s]", *got, want)
	}
}

// A pre-#570 row has no skip_reason column value. The event must still name a
// reason rather than emit an empty string, which would break the closed enum.
func TestNotifyDocumentSkip_BlankReasonFallsBackToDocType(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	got := captureSkips(svc)

	svc.notifyDocumentSkip(model.Document{RelPath: "a.zip", DocType: "archive", Status: "skipped"})
	svc.notifyDocumentSkip(model.Document{RelPath: "a.bin", DocType: "binary_ignored", Status: "skipped"})
	svc.notifyDocumentSkip(model.Document{RelPath: ".env", DocType: "ignore", Status: "skipped"})
	svc.notifyDocumentSkip(model.Document{RelPath: "k.pem", DocType: "text", Status: "secret_excluded"})

	want := []string{
		"a.zip|archive|" + model.SkipReasonArchive,
		"a.bin|binary_ignored|" + model.SkipReasonBinaryIgnored,
		".env|ignore|" + model.SkipReasonIgnoreRule,
		"k.pem|text|" + model.SkipReasonSecretExcluded,
	}
	if len(*got) != len(want) {
		t.Fatalf("skip events = %v, want %v", *got, want)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, (*got)[i], want[i])
		}
	}
}

// creditInitialStatus must NOT raise a file_skip for an archive container: the
// container is credited as skipped up front but reverts to an error if member
// extraction fails, and SPEC §3.2 forbids one document raising both events.
// Its file_skip is deferred to handleArchiveDocumentAndNotify.
func TestCreditInitialStatus_DefersArchiveSkipEvent(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	got := captureSkips(svc)

	svc.creditInitialStatus(model.Document{RelPath: "bundle.zip", DocType: "archive", Status: "skipped", SkipReason: model.SkipReasonArchive})
	if len(*got) != 0 {
		t.Fatalf("archive container raised a premature file_skip: %v", *got)
	}

	svc.creditInitialStatus(model.Document{RelPath: "a.bin", DocType: "binary_ignored", Status: "skipped", SkipReason: model.SkipReasonBinaryIgnored})
	if len(*got) != 1 {
		t.Fatalf("non-archive skip did not raise file_skip: %v", *got)
	}
}

func TestNotifyDocumentSkip_ContainsCallbackPanic(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	svc.SetOnDocumentSkip(func(string, string, string) { panic("consumer blew up") })

	svc.notifyDocumentSkip(model.Document{RelPath: "a.zip", DocType: "archive", Status: "skipped"})
}

func TestNotifyDocumentSkip_NilCallbackIsNoop(t *testing.T) {
	svc := newDocErrorService(&docErrorStubStore{})
	svc.notifyDocumentSkip(model.Document{RelPath: "a.zip"})

	svc.SetOnDocumentSkip(func(string, string, string) { t.Fatal("cleared callback was invoked") })
	svc.SetOnDocumentSkip(nil)
	svc.notifyDocumentSkip(model.Document{RelPath: "a.zip"})
}
