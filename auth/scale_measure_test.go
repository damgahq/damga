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

package auth_test

import (
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/damgahq/damga/auth"
)

// The fourth wall in docs/PLAN.md §4, which the plan calls "probably not a
// problem" — and "probably" is an estimate, which is the thing this repository
// does not accept anywhere else.
//
// What is actually being measured is not Argon2's speed. It is the bound:
// NewHasher gates concurrent hashes, so a login storm cannot multiply 19MiB by
// the number of requests. The questions worth a number are therefore what one
// verify costs, what the gate does to latency when more arrive than it will run
// at once, and what the peak memory actually is.
//
// Skipped unless DAMGA_SCALE=1: it is a measurement, and a threshold measured
// on one laptop is not a gate to fail a build on.
func TestMeasureTheLoginStorm(t *testing.T) {
	if os.Getenv("DAMGA_SCALE") != "1" {
		t.Skip("measurement: set DAMGA_SCALE=1")
	}
	const password = "correct horse battery staple"

	// The default gate, which is what an install runs with.
	hasher := auth.NewHasher(auth.DefaultParams, 0)
	encoded, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	t.Logf("GOMAXPROCS=%d default gate=max(2, GOMAXPROCS/2)=%d  params=19MiB time=2 par=1",
		runtime.GOMAXPROCS(0), max(2, runtime.GOMAXPROCS(0)/2))

	// One verify, alone, is the floor everything else is measured against.
	var alone time.Duration
	for range 5 {
		at := time.Now()
		if err := hasher.Verify(encoded, password); err != nil {
			t.Fatalf("verify: %v", err)
		}
		alone += time.Since(at)
	}
	t.Logf("one verify, uncontended: %s", (alone / 5).Round(time.Microsecond))

	for _, storm := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
		// Peak resident heap, sampled while the storm runs.
		//
		// The first version of this reported TotalAlloc, which is every byte
		// ever allocated and grows without bound whatever the gate does — it
		// printed 4.9GiB for 128 logins and would have been read as the peak.
		// What the gate bounds is how much is in use AT ONCE, so that is what
		// has to be sampled, during, rather than differenced, after.
		runtime.GC()
		var idle runtime.MemStats
		runtime.ReadMemStats(&idle)
		var peakInuse uint64
		done := make(chan struct{})
		var watcher sync.WaitGroup
		watcher.Go(func() {
			var m runtime.MemStats
			for {
				select {
				case <-done:
					return
				default:
				}
				runtime.ReadMemStats(&m)
				peakInuse = max(peakInuse, m.HeapInuse)
				time.Sleep(2 * time.Millisecond)
			}
		})

		latencies := make([]time.Duration, storm)
		var wg sync.WaitGroup
		start := time.Now()
		for i := range storm {
			wg.Go(func() {
				at := time.Now()
				// Verify and not Hash: a login storm is people (or a script)
				// arriving with passwords to check, and the sign-in path
				// verifies. Hash runs once per password change.
				_ = hasher.Verify(encoded, password)
				latencies[i] = time.Since(at)
			})
		}
		wg.Wait()
		elapsed := time.Since(start)
		close(done)
		watcher.Wait()
		slices.Sort(latencies)

		t.Logf("storm=%-4d wall=%-9s throughput=%6.1f/s  p50=%-9s p95=%-9s max=%-9s  "+
			"peakHeapInuse=%dMiB (idle %dMiB, storm x 19MiB would be %dMiB)",
			storm, elapsed.Round(time.Millisecond),
			float64(storm)/elapsed.Seconds(),
			latencies[len(latencies)/2].Round(time.Millisecond),
			latencies[len(latencies)*95/100].Round(time.Millisecond),
			latencies[len(latencies)-1].Round(time.Millisecond),
			peakInuse/(1<<20), idle.HeapInuse/(1<<20), storm*19)
	}
}
