package web

// fanout.go owns the multi-slot concurrency mechanics shared by the list,
// search, namespaced-type-union, and sidebar-count fan-outs. Each of those
// call sites builds one independent result per input item (a cluster, or a
// sidebar count target) and merges the results afterwards in fixed input
// order. The mechanics -- a bounded worker pool, a stable input-indexed slot
// array, never failing fast -- are identical everywhere, so they live here
// once instead of being re-spelled as a local errgroup at every site.

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// fanoutSlots runs fn over every item concurrently and returns the results in
// the SAME order as items (result[i] is fn's result for items[i]), regardless
// of completion order. At most limit functions run at once. fn never fails the
// group: it returns a value (which may itself carry an error record), so one
// item's failure never cancels its siblings and the caller always gets a full
// slot array to merge.
//
// The per-item ctx is the caller's ctx unchanged -- the helper is mechanism
// only. A caller that wants a total time budget wraps ctx with
// context.WithTimeout before calling; queued items then start with an
// already-expired ctx once the budget fires, and fn surfaces that as its own
// error record. zero items returns an empty slice and starts no goroutines;
// limit >= len(items) runs them all at once.
//
// errgroup drives the pool here. Its SetLimit gives the bounded worker pool
// and its Wait gives the join; the error return is unused on purpose (fn
// always returns nil error so the group never cancels), but it is the smallest
// correct primitive for "run N bounded tasks, wait for all". The result slot
// array carries the real per-item outcome.
func fanoutSlots[I, R any](ctx context.Context, items []I, limit int, fn func(context.Context, I) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i := range items {
		i := i
		g.Go(func() error {
			results[i] = fn(ctx, items[i])
			return nil
		})
	}
	_ = g.Wait()
	return results
}
