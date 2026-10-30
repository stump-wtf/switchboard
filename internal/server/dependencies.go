package server

// The Dependency Recheck Sweep
//
// A tracker sends nothing when a dependency link is added or removed, and sends a close only for the
// issue that closed — never for the issues that were waiting behind it. So intake alone would leave a
// hold waiting forever on a link someone deleted by hand, and would leave a released todo claimable
// after its blocker was reopened. This sweep is the safety net both of those gaps need: every interval
// it re-resolves the live holds whose recorded check has gone stale and acts on what the tracker says
// now, releasing holds whose blockers all closed, re-holding work whose blockers reopened, and
// reporting a hold whose links are gone entirely rather than keeping it waiting on nothing.
//
// It runs the same context-managed loop as the reaper and the retention pruner, and it does nothing at
// all on an instance with no provider connection wired (deps.Checker reports itself disabled), so an
// installation that has not enabled the feature pays no tick.
//
// Governing: stump.wtf/switchboard#583; ADR-0055 (proposed: what a provider read may be used for).

import (
	"context"
	"log/slog"
	"time"

	"github.com/stump-wtf/switchboard/internal/deps"
)

// dependencyRechecker is the sweep's whole dependence on the checker, so the loop wiring is testable
// without a database or a provider to dial (mirrors pruneStore and reapStore).
type dependencyRechecker interface {
	Recheck(ctx context.Context) (deps.RecheckReport, error)
}

// recheckDependents runs one pass and then ticks. The first pass runs immediately: a restart should not
// leave holds unread for a whole interval, since the instance may have been down while blockers closed.
func recheckDependents(ctx context.Context, c dependencyRechecker, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = deps.DefaultRecheckInterval
	}
	pass := func() {
		report, err := c.Recheck(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown cancelled the in-flight pass; not a failure worth logging
			}
			log.Warn("dependency recheck failed", "err", err)
			return
		}
		if report.Released > 0 || report.ReHeld > 0 || report.Orphans > 0 {
			log.Info("dependency recheck",
				"scanned", report.Scanned, "released", report.Released,
				"reheld", report.ReHeld, "orphan_holds", report.Orphans)
		}
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
