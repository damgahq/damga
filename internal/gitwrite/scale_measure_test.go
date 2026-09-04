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

package gitwrite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// The second wall in docs/PLAN.md §4, measured — and it is not the wall the
// plan describes.
//
// §4 calls it "git repo sayısı ve forge API bütçesi", which assumes the
// platform talks to a forge's REST API to create repositories. It does not:
// there is no forge client in this repository, no SDK in go.mod, and nothing
// that creates a repository anywhere. What damga does is clone and push over
// git, with credentials from server.GitAuth.
//
// So the cost that actually grows is this one: gitwrite clones the whole
// directory of the target repository on every operation — twice over now, once
// in commit and once in Files — and a tenant's repository accumulates one
// directory per placement. Every deploy of any application therefore pays for
// every other application's manifests in the same repository.
//
// Shallow and single-branch, so history does not accumulate; breadth does.
func TestMeasureTheCloneCost(t *testing.T) {
	if os.Getenv("DAMGA_SCALE") != "1" {
		t.Skip("measurement: set DAMGA_SCALE=1")
	}
	for _, dirs := range []int{10, 100, 500, 1000, 2000} {
		origin := buildRepo(t, dirs)
		// Three clones, reporting the middle one: the first pays for the OS
		// page cache and would report the disk rather than the repository.
		times := make([]time.Duration, 0, 3)
		for range 3 {
			at := time.Now()
			_, err := git.CloneContext(context.Background(), memory.NewStorage(), memfs.New(),
				&git.CloneOptions{URL: origin, Depth: 1, SingleBranch: true})
			if err != nil {
				t.Fatalf("cloning %d dirs: %v", dirs, err)
			}
			times = append(times, time.Since(at))
		}
		mid := times[1]
		if times[0] < mid && times[2] < mid {
			mid = max(times[0], times[2])
		}
		t.Logf("dirs=%-5d clone=%-9s  per-directory=%s",
			dirs, mid.Round(time.Millisecond),
			(mid / time.Duration(dirs)).Round(time.Microsecond))
	}
}

// buildRepo makes a bare repository holding one directory per placement, which
// is the shape a tenant's state repository has.
func buildRepo(t *testing.T, dirs int) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	repo, err := git.PlainInit(work, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for i := range dirs {
		name := fmt.Sprintf("app%05d", i)
		if err := os.MkdirAll(filepath.Join(work, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// A Workload manifest is roughly this size, which is what makes the
		// number comparable to a real tenant's repository rather than to a
		// directory of empty files.
		body := fmt.Sprintf("apiVersion: platform.damga.co/v1alpha1\nkind: Workload\n"+
			"metadata:\n  name: %s\n  namespace: t-scale\nspec:\n  image: example.test/app:1\n"+
			"  port: 3000\n  replicas: 2\n  env:\n    - name: A\n      value: \"%d\"\n", name, i)
		if err := os.WriteFile(filepath.Join(work, name, "workload.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := tree.AddGlob("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := tree.Commit("one directory per placement", &git.CommitOptions{
		Author: &object.Signature{Name: "scale", Email: "s@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return work
}
