package run

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/realtime"
)

// SchedulerConfig tunes the claim loop.
type SchedulerConfig struct {
	// Poll is the fallback interval. Assignment is event-driven — enqueueing a
	// run and a daemon connecting both wake the loop immediately — so this only
	// covers the cases nothing signalled, such as another API instance's write.
	Poll time.Duration
	// Sweep is how often lost runs are reaped.
	Sweep time.Duration
}

// DefaultSchedulerConfig meets the contract's one-second assignment target
// with room to spare: the wake-ups do the work, and the poll is a backstop.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{Poll: time.Second, Sweep: 10 * time.Second}
}

// Scheduler hands queued runs to connected daemons.
//
// It is the only thing in the process that claims from the queue. Per ADR-005
// the claim is `FOR UPDATE SKIP LOCKED` on the runs table rather than an
// advisory lock: the work item is a row we already have to read, SKIP LOCKED
// lets N schedulers poll concurrently while each still wins a distinct run, and
// an advisory lock would need a prior lookup to decide WHICH key to lock —
// which reintroduces the race it is supposed to remove.
type Scheduler struct {
	svc      *Service
	registry *realtime.Registry
	logger   *slog.Logger
	cfg      SchedulerConfig

	// wake is buffered to one: several enqueues between two passes need one
	// pass, not one each.
	wake chan struct{}
}

// NewScheduler builds the scheduler and connects it to the service, so that
// creating a run wakes the loop instead of waiting for the next poll.
func NewScheduler(svc *Service, registry *realtime.Registry, logger *slog.Logger, cfg SchedulerConfig) *Scheduler {
	defaults := DefaultSchedulerConfig()
	if cfg.Poll <= 0 {
		cfg.Poll = defaults.Poll
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = defaults.Sweep
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Scheduler{svc: svc, registry: registry, logger: logger, cfg: cfg, wake: make(chan struct{}, 1)}
	svc.wake = s.Wake
	return s
}

// Wake asks for a scheduling pass as soon as possible. It never blocks.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run drives the loop until ctx is cancelled. It is started once at
// process start-up and returns only on shutdown, so there is no path where the
// goroutine outlives the process's context.
func (s *Scheduler) Run(ctx context.Context) {
	connected, stopWatching := s.registry.Notify()
	defer stopWatching()

	poll := time.NewTicker(s.cfg.Poll)
	defer poll.Stop()
	sweep := time.NewTicker(s.cfg.Sweep)
	defer sweep.Stop()

	s.logger.InfoContext(ctx, "run scheduler started",
		"poll", s.cfg.Poll.String(), "sweep", s.cfg.Sweep.String())

	for {
		select {
		case <-ctx.Done():
			s.logger.InfoContext(ctx, "run scheduler stopped")
			return
		case <-sweep.C:
			s.sweepLostRuns(ctx)
			continue
		case <-s.wake:
		case <-connected:
		case <-poll.C:
		}
		s.dispatch(ctx)
	}
}

// dispatch offers work to every daemon connected to this process.
//
// The registry is the work list rather than a database query for "online
// runtimes": a run can only be assigned down a socket this process holds, so
// asking the database which runtimes heartbeated recently would produce
// candidates we cannot reach.
func (s *Scheduler) dispatch(ctx context.Context) {
	for _, target := range s.registry.Connected() {
		s.dispatchTo(ctx, target)
	}
}

func (s *Scheduler) dispatchTo(ctx context.Context, target realtime.Target) {
	claimed, err := s.svc.store.ClaimQueuedRunForRuntime(ctx, dbgen.ClaimQueuedRunForRuntimeParams{
		OrgID:     target.OrgID,
		RuntimeID: uuid.NullUUID{UUID: target.RuntimeID, Valid: true},
	})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			// Nothing queued for this runtime, or it is already busy. Both are
			// the ordinary case, not a failure.
			return
		}
		s.logger.ErrorContext(ctx, "could not claim a queued run",
			"err", db.Classify(err), "runtime_id", target.RuntimeID)
		return
	}

	logger := s.logger.With("run_id", claimed.ID, "runtime_id", target.RuntimeID)

	frame, err := s.svc.buildAssignFrame(ctx, claimed)
	if err != nil {
		// The run cannot be described to a daemon — a missing project, a
		// payload the contract rejects. Retrying would fail identically every
		// tick, so it is finished as an error the user can see rather than
		// left cycling through the queue.
		logger.ErrorContext(ctx, "could not build run.assign", "err", err)
		s.failRun(ctx, claimed, "assign_failed", "this run could not be handed to a runtime")
		return
	}

	if err := s.registry.Send(ctx, target, frame); err != nil {
		// The daemon went away between the claim and the send. Put the run
		// back so the next connected runtime gets it a tick later, rather than
		// leaving it to time out on a lease.
		if _, releaseErr := s.svc.store.ReleaseClaimedRun(ctx, dbgen.ReleaseClaimedRunParams{
			OrgID: claimed.OrgID, ID: claimed.ID,
		}); releaseErr != nil {
			logger.ErrorContext(ctx, "could not release an unsent run", "err", db.Classify(releaseErr))
		}
		if !errors.Is(err, realtime.ErrRuntimeOffline) {
			logger.WarnContext(ctx, "could not deliver run.assign", "err", err)
		}
		return
	}

	logger.InfoContext(ctx, "run assigned", "mode", claimed.Mode)
	s.svc.publishStatus(claimed)
}

// failRun finishes a run with a domain error. The message is one this API
// wrote, never a driver message.
func (s *Scheduler) failRun(ctx context.Context, claimed dbgen.Run, code, message string) {
	failed, err := s.svc.store.FinishRun(ctx, dbgen.FinishRunParams{
		OrgID: claimed.OrgID, ID: claimed.ID,
		Status: dbgen.RunStatusError, ErrorCode: code, ErrorMessage: message,
	})
	if err != nil {
		if !errors.Is(db.Classify(err), db.ErrNotFound) {
			s.logger.ErrorContext(ctx, "could not fail a run", "err", db.Classify(err), "run_id", claimed.ID)
		}
		return
	}
	s.svc.publishStatus(failed)
}

// sweepLostRuns deals with runs whose daemon stopped reporting.
//
// A runtime that has not heartbeated within the online window is offline by
// definition (runtimes.online is derived from last_seen_at, never stored), and
// the runs it was holding cannot make progress. With MaxAttempts at 1 they are
// finished as `error` with a reason; a deployment that raises MaxAttempts gets
// the requeue path instead.
func (s *Scheduler) sweepLostRuns(ctx context.Context) {
	stale := pgtype.Interval{Microseconds: s.svc.cfg.OnlineWithin.Microseconds(), Valid: true}

	requeued, err := s.svc.store.RequeueStaleRuns(ctx, dbgen.RequeueStaleRunsParams{
		StaleAfter: stale, MaxAttempts: s.svc.cfg.MaxAttempts,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "could not requeue stale runs", "err", db.Classify(err))
	} else if len(requeued) > 0 {
		s.logger.WarnContext(ctx, "requeued runs whose runtime stopped reporting", "runs", len(requeued))
		for _, row := range requeued {
			s.republish(ctx, row.OrgID, row.ID)
		}
		s.Wake()
	}

	failed, err := s.svc.store.FailStaleRuns(ctx, dbgen.FailStaleRunsParams{
		StaleAfter: stale, MaxAttempts: s.svc.cfg.MaxAttempts,
	})
	if err != nil {
		s.logger.ErrorContext(ctx, "could not fail stale runs", "err", db.Classify(err))
		return
	}
	if len(failed) == 0 {
		return
	}
	s.logger.WarnContext(ctx, "failed runs whose runtime stopped reporting", "runs", len(failed))
	for _, row := range failed {
		s.republish(ctx, row.OrgID, row.ID)
	}
}

// republish pushes a swept run's new state to anyone watching it. The sweeps
// return ids only — deliberately, so a cross-tenant sweeper never carries row
// contents — so the row is read back here, org-scoped.
func (s *Scheduler) republish(ctx context.Context, orgID, runID uuid.UUID) {
	current, err := s.svc.store.GetRun(ctx, dbgen.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		return
	}
	s.svc.publishStatus(current)
}
