/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/damgahq/damga/evidence"
	"github.com/damgahq/damga/evidence/sqlite"
)

// The third wall in docs/PLAN.md §4, measured rather than assumed.
//
// It is not what the plan's one line suggests. "SQLite tek yazar" reads as a
// correctness problem — writers colliding and failing — and that is not what
// this store does: the writer pool is one connection (SetMaxOpenConns(1)) with
// WAL, busy_timeout and _txlock=immediate, so a concurrent write does not
// error, it queues. What there is to measure is therefore latency and
// throughput under a queue, and where the queue gets long enough to matter.
//
// Skipped unless DAMGA_SCALE=1, because it is a measurement and not a gate: it
// takes seconds, it is sensitive to the disk underneath it, and a number
// measured on somebody's laptop is not a threshold to fail a build on.
// envProd is named once: three spellings of the same literal in one package
// is what goconst is for.
const envProd = "prod"

func TestMeasureTheSingleWriter(t *testing.T) {
	if os.Getenv("DAMGA_SCALE") != "1" {
		t.Skip("measurement: set DAMGA_SCALE=1")
	}

	// On a file and not in memory. The in-memory path skips WAL entirely, so
	// it would measure a database this product never runs.
	dir := t.TempDir()
	store, err := sqlite.Open(context.Background(),
		filepath.Join(dir, "evidence.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const perWorker = 40
	for _, workers := range []int{1, 2, 4, 8, 16, 32, 64} {
		latencies := make([][]time.Duration, workers)
		var wg sync.WaitGroup
		start := time.Now()
		for w := range workers {
			wg.Go(func() {
				mine := make([]time.Duration, 0, perWorker)
				for i := range perWorker {
					rec := evidence.Record{
						IdempotencyKey: fmt.Sprintf("scale:%d:%d:%d", workers, w, i),
						// A Ref per worker, because Seq is per-Ref and gapless:
						// one Ref for everybody would measure contention on a
						// counter this product spreads across apps.
						Ref: evidence.Ref{
							TenantID: "t_scale",
							App:      fmt.Sprintf("app%02d", w),
							Env:      envProd,
						},
						Actor:  evidence.Actor{ID: "a_scale", Kind: "user"},
						Source: evidence.Source{CommitSHA: fmt.Sprintf("%040d", i)},
						Image:  evidence.Image{RequestedRef: "example.test/app:1"},
					}
					at := time.Now()
					if _, err := store.Append(context.Background(), rec); err != nil {
						t.Errorf("append at %d workers: %v", workers, err)
						return
					}
					mine = append(mine, time.Since(at))
				}
				latencies[w] = mine
			})
		}
		wg.Wait()
		elapsed := time.Since(start)

		var all []time.Duration
		for _, m := range latencies {
			all = append(all, m...)
		}
		slices.Sort(all)
		total := workers * perWorker

		// Two derivations of the same quantity, which is the rule this
		// repository pays for skipping: the mean below is the sum of measured
		// latencies over the count, and the throughput is the wall clock over
		// the same count. With one writer they should agree to within the
		// scheduling overhead — and if they ever stop agreeing, one of the two
		// is measuring something else.
		var sum time.Duration
		for _, d := range all {
			sum += d
		}
		meanLatency := sum / time.Duration(len(all))
		perWrite := elapsed / time.Duration(total)

		t.Logf("workers=%-3d writes=%-5d wall=%-8s throughput=%6.0f/s  "+
			"p50=%-9s p95=%-9s p99=%-9s max=%-9s  mean=%-9s wall/write=%s",
			workers, total, elapsed.Round(time.Millisecond),
			float64(total)/elapsed.Seconds(),
			all[len(all)/2].Round(time.Microsecond),
			all[len(all)*95/100].Round(time.Microsecond),
			all[len(all)*99/100].Round(time.Microsecond),
			all[len(all)-1].Round(time.Microsecond),
			meanLatency.Round(time.Microsecond),
			(perWrite * time.Duration(workers)).Round(time.Microsecond))
	}
}

// The same store, written to for long enough that WAL checkpoints happen.
//
// The burst above cannot see them. SQLite checkpoints the write-ahead log at
// wal_autocheckpoint pages — 1000 by default, which is roughly a thousand
// small transactions — so a run of forty writes per worker finishes inside one
// WAL generation and reports a latency profile with the stall taken out of it.
//
// A platform's evidence store is not written to in bursts of forty. It is
// written to for as long as the install is up, which means the number that
// matters is the one with the checkpoints in it: the tail, not the median.
func TestMeasureTheSingleWriterSustained(t *testing.T) {
	if os.Getenv("DAMGA_SCALE") != "1" {
		t.Skip("measurement: set DAMGA_SCALE=1")
	}
	dir := t.TempDir()
	store, err := sqlite.Open(context.Background(),
		filepath.Join(dir, "evidence.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const workers = 16
	const perWorker = 1500 // 24,000 writes: many WAL generations, not one

	latencies := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range workers {
		wg.Go(func() {
			mine := make([]time.Duration, 0, perWorker)
			for i := range perWorker {
				rec := evidence.Record{
					IdempotencyKey: fmt.Sprintf("sustained:%d:%d", w, i),
					Ref: evidence.Ref{
						TenantID: "t_scale", App: fmt.Sprintf("app%02d", w), Env: envProd,
					},
					Actor:  evidence.Actor{ID: "a_scale", Kind: "user"},
					Source: evidence.Source{CommitSHA: fmt.Sprintf("%040d", i)},
					Image:  evidence.Image{RequestedRef: "example.test/app:1"},
				}
				at := time.Now()
				if _, err := store.Append(context.Background(), rec); err != nil {
					t.Errorf("append: %v", err)
					return
				}
				mine = append(mine, time.Since(at))
			}
			latencies[w] = mine
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	var all []time.Duration
	for _, m := range latencies {
		all = append(all, m...)
	}
	slices.Sort(all)
	total := len(all)

	// How far the tail is from the middle. A queue with a fixed service rate
	// has a long-ish tail by construction; a checkpoint stall shows up as a
	// max that is orders of magnitude above p99 rather than a few times it.
	stat, err := os.Stat(filepath.Join(dir, "evidence.db"))
	var size int64
	if err == nil {
		size = stat.Size()
	}
	t.Logf("sustained: workers=%d writes=%d wall=%s throughput=%.0f/s db=%dKiB",
		workers, total, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds(), size/1024)
	t.Logf("sustained: p50=%s p95=%s p99=%s p999=%s max=%s  (max/p99 = %.1fx)",
		all[total/2].Round(time.Microsecond),
		all[total*95/100].Round(time.Microsecond),
		all[total*99/100].Round(time.Microsecond),
		all[total*999/1000].Round(time.Microsecond),
		all[total-1].Round(time.Microsecond),
		float64(all[total-1])/float64(all[total*99/100]))
}
