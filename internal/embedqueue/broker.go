package embedqueue

import (
	"context"
	"errors"
	"time"
)

// ErrNoJob is returned by Broker.Lease when no job is currently available. It is
// not a failure — the worker run-loop treats it as "queue empty, back off and
// retry" rather than an error to surface.
var ErrNoJob = errors.New("embedqueue: no job available")

// Lease is a claimed job plus the opaque token a worker uses to Ack or Nack it.
// A lease is held for a broker-defined visibility timeout; if the worker neither
// Acks nor Nacks before the lease expires the job becomes re-claimable by another
// worker (SPEC §8.7.3 lease expiry / at-least-once delivery).
type Lease struct {
	// Job is the leased unit of work.
	Job Job
	// Token identifies this lease to Ack/Nack. It is opaque to the worker.
	Token string
	// Deadline is when the lease expires and the job becomes re-claimable. A
	// worker SHOULD finish (Ack/Nack) before it; a worker that observes the
	// deadline has passed MUST treat its in-flight write as possibly raced by a
	// redelivery (idempotency, keyed by chunk_id, makes that safe — §8.7.3).
	Deadline time.Time

	// Attempts is how many times this job has been delivered INCLUDING this
	// delivery, and MaxAttempts is the broker's redelivery budget (0 = the broker
	// does not bound it). Together they tell a worker whether a Nack it is about
	// to issue will REDELIVER the job or DEAD-LETTER it, which is the difference
	// between a failure the pool may still recover from and a terminal one the
	// chunk has to record (SPEC §8.7.3 failure handling; #709). Without them a
	// worker cannot distinguish the two and a permanently unexecutable job is
	// retried, dead-lettered, and then enqueued again forever.
	Attempts    int
	MaxAttempts int
}

// Final reports whether this delivery is the last one the broker will make: a
// Nack now dead-letters the job instead of redelivering it. A broker that does
// not bound redelivery (MaxAttempts <= 0) is never final.
func (l Lease) Final() bool {
	return l.MaxAttempts > 0 && l.Attempts >= l.MaxAttempts
}

// Broker is the implementation-defined transport that carries embedding jobs
// from the coordinator to the workers (SPEC §8.7.4). Any queue providing
// at-least-once delivery, a redelivery/visibility mechanism, and a dead-letter
// path satisfies the contract — NATS, Redis, SQS, or the default in-process /
// SQLite-backed implementations shipped in this package.
//
// Implementations MUST be safe for concurrent use by the coordinator (Enqueue)
// and many workers (Lease/Ack/Nack) at once.
type Broker interface {
	// Enqueue adds a job to the queue. Enqueuing a job whose chunk_id is already
	// queued is allowed (at-least-once delivery is assumed) and idempotent at the
	// embedding layer because vector writes are keyed by chunk_id (§8.7.3).
	Enqueue(ctx context.Context, job Job) error

	// Lease claims the next available job and returns it with a lease token and
	// deadline. The job is hidden from other workers until the lease expires or
	// the worker Acks/Nacks it. Returns ErrNoJob when the queue is empty.
	Lease(ctx context.Context, visibility time.Duration) (Lease, error)

	// Ack marks a leased job done; it is removed from the queue. Acking an
	// expired/unknown lease token is a no-op (the job may have been redelivered).
	Ack(ctx context.Context, token string) error

	// Nack returns a leased job for redelivery. If the job has already been
	// delivered maxAttempts times it is moved to the dead-letter store instead of
	// being requeued (SPEC §8.7.3 dead-lettering). retryAfter delays the next
	// delivery; zero makes the job immediately re-claimable.
	Nack(ctx context.Context, token string, retryAfter time.Duration) error

	// Stats reports queue depth for observability and for tests/coordinator
	// drain-detection. It never blocks on in-flight work.
	Stats(ctx context.Context) (Stats, error)

	// Close releases any resources the broker holds.
	Close() error
}

// CorpusScopedBroker is an OPTIONAL Broker capability: leasing only the jobs of
// one corpus (SPEC §8.7.2 corpus reference, §8.7.4 corpus isolation on shared
// infrastructure). Both built-in brokers implement it; the worker discovers it
// by type assertion, exactly like the store's optional capabilities, so an
// external adapter that predates it keeps working unchanged.
//
// Worker-side rejection of a foreign corpus's job (worker.prepare) is the
// mandatory half of the isolation and stands on its own. This is the half that
// keeps a shared queue EFFICIENT as well as correct: without it, a worker that
// serves corpus B leases corpus A's jobs only to reject them, burning A's
// redelivery budget and eventually dead-lettering work that nothing was wrong
// with (#708).
type CorpusScopedBroker interface {
	Broker

	// LeaseForCorpus claims the next available job whose CorpusID equals
	// corpusID, ignoring every other corpus's jobs. An empty corpusID means "any
	// corpus" and is identical to Lease. Returns ErrNoJob when this corpus has no
	// claimable job, even if other corpora do.
	LeaseForCorpus(ctx context.Context, corpusID string, visibility time.Duration) (Lease, error)
}

// Stats is a point-in-time view of the queue (SPEC §8.7.4 observability).
type Stats struct {
	// Pending is the number of jobs available to be leased.
	Pending int
	// InFlight is the number of jobs currently leased (not yet Acked/Nacked and
	// not yet expired).
	InFlight int
	// DeadLettered is the number of jobs that exhausted their retries.
	DeadLettered int
}
