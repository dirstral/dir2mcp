// Package usage provides per-query cost and latency observability primitives
// for dir2mcp (issue #327). It is purely additive: it never alters tool
// results or provider behavior. Providers OPTIONALLY report token usage into a
// context-attached Sink; the retrieval service times each stage, maps tokens
// to an approximate USD cost via a configurable price table, and emits a
// single structured `query_metrics` event.
//
// Privacy: this package records only counts, costs, and latency. It never
// stores prompts, documents, queries, API keys, or any raw payload.
package usage

import (
	"context"
	"sync"
	"time"
)

// Stage identifies one provider-backed phase of a query.
type Stage string

const (
	// StageEmbed is query embedding (search + ask).
	StageEmbed Stage = "embed"
	// StageRerank is the optional cross-encoder rerank phase.
	StageRerank Stage = "rerank"
	// StageGenerate is RAG answer generation (ask only).
	StageGenerate Stage = "generate"
)

// Usage holds token counts reported by a provider for a single call. A
// provider that does not report usage contributes nothing (all zero). Counts
// are additive across multiple calls within the same stage.
type Usage struct {
	// PromptTokens counts input/prompt tokens (a.k.a. input tokens).
	PromptTokens int64
	// CompletionTokens counts generated/output tokens. Always 0 for
	// embed/rerank stages.
	CompletionTokens int64
	// TotalTokens is the provider-reported total when present; otherwise it is
	// derived as PromptTokens+CompletionTokens by add.
	TotalTokens int64
	// Reported is true once at least one provider call reported usage for this
	// stage. When false, token counts are unknown (not "zero cost").
	Reported bool
}

// add merges other into u, deriving TotalTokens when a provider only reported
// the prompt/completion split.
func (u *Usage) add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	if other.TotalTokens > 0 {
		u.TotalTokens += other.TotalTokens
	} else {
		u.TotalTokens += other.PromptTokens + other.CompletionTokens
	}
	u.Reported = true
}

// Sink is a thread-safe, per-query accumulator of provider token usage and
// per-stage wall-clock latency, keyed by stage. It is attached to a context so
// provider clients can report usage without any change to the model
// interfaces, and so the retrieval service can record stage latency near each
// provider call. A nil Sink (the default) makes all methods no-ops, so the
// system behaves exactly as before when observability is not wired.
type Sink struct {
	mu        sync.Mutex
	byStage   map[Stage]Usage
	latencies map[Stage]time.Duration
}

// NewSink returns an empty Sink ready for concurrent use.
func NewSink() *Sink {
	return &Sink{
		byStage:   make(map[Stage]Usage),
		latencies: make(map[Stage]time.Duration),
	}
}

// AddLatency accumulates wall-clock latency for a stage. Safe on a nil Sink.
func (s *Sink) AddLatency(stage Stage, d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latencies[stage] += d
}

// Latency returns the accumulated latency recorded for a stage.
func (s *Sink) Latency(stage Stage) time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latencies[stage]
}

// TimeStage runs fn, recording its wall-clock duration against stage. The
// stage is recorded even when fn returns an error so a failed provider call
// still contributes latency. Safe on a nil Sink (fn still runs).
func (s *Sink) TimeStage(stage Stage, fn func() error) error {
	start := time.Now()
	err := fn()
	s.AddLatency(stage, time.Since(start))
	return err
}

// Report records usage for a stage. Safe to call concurrently and on a nil
// Sink (no-op). Zero-valued usage is still recorded as "reported" so the
// distinction between "provider reported 0 tokens" and "no usage available"
// is preserved.
func (s *Sink) Report(stage Stage, u Usage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.byStage[stage]
	cur.add(u)
	s.byStage[stage] = cur
}

// Stage returns the accumulated usage for a stage and whether any provider
// reported usage for it.
func (s *Sink) Stage(stage Stage) (Usage, bool) {
	if s == nil {
		return Usage{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byStage[stage]
	return u, ok && u.Reported
}

type sinkKey struct{}

// WithSink returns a context carrying sink so provider clients can report
// usage for the in-flight query. Passing a nil sink returns ctx unchanged.
func WithSink(ctx context.Context, sink *Sink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, sink)
}

// SinkFrom extracts the Sink attached to ctx, or nil when none is present.
// Provider clients call this; a nil result makes all reporting a no-op.
func SinkFrom(ctx context.Context) *Sink {
	if ctx == nil {
		return nil
	}
	if s, ok := ctx.Value(sinkKey{}).(*Sink); ok {
		return s
	}
	return nil
}

// Report is a convenience that extracts the Sink from ctx (if any) and records
// usage for stage. It is the entry point provider clients use, keeping their
// call sites a single line and inert when no sink is attached.
func Report(ctx context.Context, stage Stage, u Usage) {
	SinkFrom(ctx).Report(stage, u)
}

// TimeStage runs fn, recording its wall-clock duration against stage on the
// Sink attached to ctx (if any). Inert (but still runs fn) when no sink is
// attached.
func TimeStage(ctx context.Context, stage Stage, fn func() error) error {
	return SinkFrom(ctx).TimeStage(stage, fn)
}
