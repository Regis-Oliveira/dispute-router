package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

type Options struct {
	Store     *Store
	Deadlines *Deadlines
	Locks     *Locks
	Logger    *slog.Logger

	// Concurrency is how many disputes are decided at once.
	Concurrency int
	// PollInterval is how often the pool asks for due work.
	PollInterval time.Duration
	// BatchSize caps how many ids one poll claims.
	BatchSize int
	// Lookahead claims work slightly before it is due, so the decision lands
	// inside the window rather than exactly on its edge.
	Lookahead time.Duration
	// ReconcileInterval is how often the Redis index is rebuilt from Postgres.
	ReconcileInterval time.Duration
	// ID identifies this process in the audit trail.
	ID string
}

type Pool struct {
	opts Options

	claimed   atomic.Int64
	decided   atomic.Int64
	skipped   atomic.Int64
	stale     atomic.Int64
	failed    atomic.Int64
	contended atomic.Int64
}

func NewPool(opts Options) *Pool {
	if opts.Concurrency < 1 {
		opts.Concurrency = 4
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 100
	}
	return &Pool{opts: opts}
}

// Run starts the reconciler and the poller and blocks until ctx is cancelled.
func (p *Pool) Run(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return p.reconcileLoop(groupCtx) })
	group.Go(func() error { return p.pollLoop(groupCtx) })
	return group.Wait()
}

// reconcileLoop rebuilds the Redis index from Postgres.
//
// This is what lets the index be treated as disposable. Flush Redis, or lose it
// entirely, and the next pass restores it - so nothing that matters is only in
// there. It also repairs the gap the fast path cannot: a dispute written while
// Redis was unreachable was never scheduled, and without this it would sit
// unnoticed until its deadline passed.
func (p *Pool) reconcileLoop(ctx context.Context) error {
	// Once at startup, before any polling, so the pool never runs against an
	// index it has not checked.
	if err := p.reconcile(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(p.opts.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.reconcile(ctx); err != nil {
				// A failed reconcile is survivable: the index is merely stale,
				// and the next pass fixes it.
				p.opts.Logger.ErrorContext(ctx, "reconcile failed", "error", err)
			}
		}
	}
}

func (p *Pool) reconcile(ctx context.Context) error {
	started := time.Now()

	open, err := p.opts.Store.OpenDeadlines(ctx)
	if err != nil {
		return err
	}
	if err := p.opts.Deadlines.ScheduleMany(ctx, open); err != nil {
		return err
	}

	pending, err := p.opts.Deadlines.Pending(ctx)
	if err != nil {
		return err
	}

	p.opts.Logger.InfoContext(ctx, "reconciled deadline index",
		"open_in_postgres", len(open),
		"indexed_in_redis", pending,
		"took_ms", time.Since(started).Milliseconds())
	return nil
}

func (p *Pool) pollLoop(ctx context.Context) error {
	ticker := time.NewTicker(p.opts.PollInterval)
	defer ticker.Stop()

	// A worker that only logs when it acts is indistinguishable from a worker
	// that is stuck: skips and lock contention are silent by design, so a
	// perfectly healthy idle process and a broken one produce the same empty
	// log. This says what it looked at, not only what it changed.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	// Totals are logged on every exit path, including the one where shutdown
	// interrupts a claim.
	defer p.logTotals(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			p.logHeartbeat(ctx)
		case <-ticker.C:
			// Bounded. Draining while batches come back full is what clears a
			// backlog quickly, but an unbounded loop lets one pathological
			// batch hold the poll loop forever - no heartbeat, no shutdown, no
			// other work. The cap costs one tick of latency on a real backlog
			// and makes starvation impossible.
			for range maxBatchesPerTick {
				claimed, err := p.drainOnce(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					p.opts.Logger.ErrorContext(ctx, "claim failed", "error", err)
					break
				}
				p.claimed.Add(int64(claimed))
				// Keep going while the batch came back full, so a backlog
				// clears at full speed rather than one batch per tick.
				if claimed < p.opts.BatchSize {
					break
				}
			}
		}
	}
}

func (p *Pool) drainOnce(ctx context.Context) (int, error) {
	upto := time.Now().Add(p.opts.Lookahead)

	ids, err := p.opts.Deadlines.Claim(ctx, upto, p.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(p.opts.Concurrency)

	for _, id := range ids {
		group.Go(func() error {
			p.handle(groupCtx, id)
			// One dispute's failure is not the batch's failure; every error is
			// counted and logged inside handle, and the id is rescheduled so
			// nothing is silently dropped.
			return nil
		})
	}
	_ = group.Wait()

	return len(ids), nil
}

// handle decides one dispute.
func (p *Pool) handle(ctx context.Context, disputeID int64) {
	logger := p.opts.Logger.With("dispute_id", disputeID)

	token, acquired, err := p.opts.Locks.Acquire(ctx, disputeID)
	if err != nil {
		p.failed.Add(1)
		logger.ErrorContext(ctx, "lock failed", "error", err)
		p.rescheduleSoon(ctx, disputeID)
		return
	}
	if !acquired {
		// Somebody else has it. The claim already removed the id from the
		// index, so it has to go back or it would never be looked at again.
		p.contended.Add(1)
		p.rescheduleSoon(ctx, disputeID)
		return
	}
	defer func() {
		held, releaseErr := p.opts.Locks.Release(ctx, disputeID, token)
		if releaseErr != nil {
			logger.ErrorContext(ctx, "lock release failed", "error", releaseErr)
			return
		}
		if !held {
			// The TTL lapsed while this worker was still going. Worth knowing:
			// it means another worker may have been working the same dispute,
			// and the version check is what kept that safe.
			logger.WarnContext(ctx, "lock had already expired when releasing")
		}
	}()

	current, err := p.opts.Store.Load(ctx, disputeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted between scheduling and claiming. Nothing to reschedule.
			p.skipped.Add(1)
			return
		}
		p.failed.Add(1)
		logger.ErrorContext(ctx, "load failed", "error", err)
		p.rescheduleSoon(ctx, disputeID)
		return
	}

	decision := Decide(current.Candidate, time.Now())

	switch decision.Action {
	case ActionSkip:
		p.skipped.Add(1)
		return

	case ActionEscalate:
		// The worker is declining to decide, so the dispute goes back for a
		// human - to be looked at again just after its deadline, where the
		// next claim records it as expired if nobody acted.
		//
		// nextVisit is not padding, it is the fix for a livelock. Claiming
		// takes everything due before now+lookahead, so rescheduling inside
		// that window makes a dispute instantly claimable again: escalate,
		// reschedule, claim, escalate, thousands of times a second. Worse,
		// drainOnce kept receiving full batches of the same few ids, so its
		// inner loop never ended, the poll loop never returned to its select,
		// and the other 1,663 disputes were never looked at at all. The
		// symptom was a worker that logged nothing and pinned a core.
		p.skipped.Add(1)
		if err := p.opts.Deadlines.Schedule(ctx, disputeID, p.nextVisit(current.DeadlineAt)); err != nil {
			logger.ErrorContext(ctx, "reschedule after escalation failed", "error", err)
		}
		return
	}

	if err := p.opts.Store.Apply(ctx, current, decision, p.opts.ID); err != nil {
		if errors.Is(err, ErrStaleCandidate) {
			// Somebody changed it underneath us. Correct outcome: the write
			// was refused rather than applied to a dispute that had moved on.
			p.stale.Add(1)
			logger.InfoContext(ctx, "skipped a dispute that changed underneath us")
			return
		}
		p.failed.Add(1)
		logger.ErrorContext(ctx, "apply failed", "error", err, "action", decision.Action)
		p.rescheduleSoon(ctx, disputeID)
		return
	}

	p.decided.Add(1)
	logger.InfoContext(ctx, "decided",
		"action", decision.Action,
		"to_state", decision.ToState,
		"reason", decision.Reason,
		"amount_minor", current.AmountMinor,
		"currency", current.Currency)
}

const (
	// retryBackoff is how long a dispute waits before being looked at again,
	// on top of the claim window.
	retryBackoff = 30 * time.Second

	// maxBatchesPerTick bounds one poll tick so the loop always returns to its
	// select.
	maxBatchesPerTick = 20
)

// nextVisit returns when a dispute should be looked at again, guaranteed to be
// outside the current claim window.
//
// Everything scored at or before now+lookahead is claimable immediately, so any
// reschedule inside that window is an instant re-claim. This is the single
// place that invariant is enforced; the escalation path and the failure path
// both go through it.
func (p *Pool) nextVisit(preferred time.Time) time.Time {
	earliest := time.Now().Add(p.opts.Lookahead + retryBackoff)
	if preferred.After(earliest) {
		return preferred
	}
	return earliest
}

// rescheduleSoon puts a dispute back after a failure.
//
// The backoff has to clear the lookahead window, not just `now`. Claiming asks
// for everything due before now+lookahead, so a dispute rescheduled 30s out
// while the lookahead is 72h is instantly claimable again - which is how one
// broken refund turned into 491 identical retries per dispute and 23,671 log
// lines in under a minute.
func (p *Pool) rescheduleSoon(ctx context.Context, disputeID int64) {
	if err := p.opts.Deadlines.Schedule(ctx, disputeID, p.nextVisit(time.Time{})); err != nil {
		p.opts.Logger.ErrorContext(ctx, "reschedule failed", "error", err, "dispute_id", disputeID)
	}
}

func (p *Pool) logHeartbeat(ctx context.Context) {
	pending, err := p.opts.Deadlines.Pending(ctx)
	if err != nil {
		pending = -1
	}
	p.opts.Logger.InfoContext(ctx, "worker heartbeat",
		"indexed", pending,
		"claimed", p.claimed.Load(),
		"decided", p.decided.Load(),
		"skipped", p.skipped.Load(),
		"stale", p.stale.Load(),
		"contended", p.contended.Load(),
		"failed", p.failed.Load())
}

func (p *Pool) logTotals(ctx context.Context) {
	p.opts.Logger.InfoContext(ctx, "worker stopping",
		"claimed", p.claimed.Load(),
		"decided", p.decided.Load(),
		"skipped", p.skipped.Load(),
		"stale", p.stale.Load(),
		"contended", p.contended.Load(),
		"failed", p.failed.Load())
}
