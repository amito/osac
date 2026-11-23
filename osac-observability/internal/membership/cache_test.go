/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package membership

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// clock is a manually advanced clock so the tests never sleep.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: baseTime} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeLister records calls and answers from a table keyed by subject.
type fakeLister struct {
	mu       sync.Mutex
	calls    int
	subjects []string
	tokens   []string
	projects map[string][]string
	err      map[string]error
	block    chan struct{}
	entered  chan struct{}
}

func newFakeLister(projects map[string][]string) *fakeLister {
	return &fakeLister{projects: projects, err: map[string]error{}}
}

func (f *fakeLister) Lister(ctx context.Context, subject, token string) ([]string, error) {
	f.mu.Lock()
	f.calls++
	f.subjects = append(f.subjects, subject)
	f.tokens = append(f.tokens, token)
	err := f.err[subject]
	projects := f.projects[subject]
	block, entered := f.block, f.entered
	f.mu.Unlock()
	if block != nil && entered != nil {
		entered <- struct{}{}
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return append([]string(nil), projects...), nil
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestCache(t *testing.T, lister Lister, c *clock) *Cache {
	t.Helper()
	return &Cache{TTL: 30 * time.Second, Now: c.Now, Lister: lister}
}

func TestCacheServesWithinWindow(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}})
	cache := newTestCache(t, lister.Lister, c)
	first, err := cache.Projects(context.Background(), "alice", "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c.Advance(29 * time.Second)
	second, err := cache.Projects(context.Background(), "alice", "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(first) != 1 || first[0] != "team-a" || len(second) != 1 || second[0] != "team-a" {
		t.Errorf("unexpected projects: %v %v", first, second)
	}
	if lister.callCount() != 1 {
		t.Errorf("the window must be served from cache: %d calls", lister.callCount())
	}
}

func TestCacheRefreshesAtExpiry(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}})
	cache := newTestCache(t, lister.Lister, c)
	if _, err := cache.Projects(context.Background(), "alice", "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lister.mu.Lock()
	lister.projects["alice"] = []string{"team-a", "team-b"}
	lister.mu.Unlock()
	c.Advance(31 * time.Second)
	projects, err := cache.Projects(context.Background(), "alice", "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 2 || projects[1] != "team-b" {
		t.Errorf("the new set must replace the old: %v", projects)
	}
	if lister.callCount() != 2 {
		t.Errorf("expiry must refresh: %d calls", lister.callCount())
	}
}

func TestCacheExpiryWithFailedRefreshFallsBack(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}})
	cache := newTestCache(t, lister.Lister, c)
	if _, err := cache.Projects(context.Background(), "alice", "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lister.mu.Lock()
	lister.err["alice"] = errors.New("fulfillment is down")
	lister.mu.Unlock()
	c.Advance(31 * time.Second)
	projects, err := cache.Projects(context.Background(), "alice", "token")
	if err == nil {
		t.Fatal("a failed refresh must report the error")
	}
	if projects != nil {
		t.Errorf("a failed refresh must never serve the stale set: %v", projects)
	}
	lister.mu.Lock()
	delete(lister.err, "alice")
	lister.mu.Unlock()
	c.Advance(time.Second)
	projects, err = cache.Projects(context.Background(), "alice", "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 || projects[0] != "team-a" {
		t.Errorf("unexpected projects: %v", projects)
	}
	if lister.callCount() != 3 {
		t.Errorf("the failed entry must be discarded, not extended: %d calls", lister.callCount())
	}
}

func TestCacheColdFailureFallsBackToDefault(t *testing.T) {
	c := newClock()
	lister := newFakeLister(nil)
	lister.err["alice"] = errors.New("fulfillment is down")
	cache := newTestCache(t, lister.Lister, c)
	projects, err := cache.Projects(context.Background(), "alice", "token")
	if err == nil {
		t.Fatal("a cold failure must report the error")
	}
	if projects != nil {
		t.Errorf("a cold failure must not invent a project set: %v", projects)
	}
}

func TestCacheIsPerSubject(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}, "bob": {"team-b"}})
	cache := newTestCache(t, lister.Lister, c)
	alice, err := cache.Projects(context.Background(), "alice", "token-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bob, err := cache.Projects(context.Background(), "bob", "token-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if alice[0] != "team-a" || bob[0] != "team-b" {
		t.Errorf("one subject's entry served another: %v %v", alice, bob)
	}
	if lister.callCount() != 2 {
		t.Errorf("each subject needs its own fetch: %d calls", lister.callCount())
	}
}

func TestCacheConcurrentRefreshCoalesces(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}})
	lister.block = make(chan struct{})
	lister.entered = make(chan struct{}, 2)
	cache := newTestCache(t, lister.Lister, c)

	firstDone := make(chan []string, 1)
	go func() {
		projects, _ := cache.Projects(context.Background(), "alice", "token")
		firstDone <- projects
	}()
	select {
	case <-lister.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first refresh never started")
	}
	secondDone := make(chan []string, 1)
	go func() {
		projects, _ := cache.Projects(context.Background(), "alice", "token")
		secondDone <- projects
	}()
	close(lister.block)
	first, second := <-firstDone, <-secondDone
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("both callers must get the refresh result: %v %v", first, second)
	}
	if lister.callCount() != 1 {
		t.Errorf("only one refresh must run: %d calls", lister.callCount())
	}
}

func TestCacheDiscardsOnFailure(t *testing.T) {
	c := newClock()
	lister := newFakeLister(map[string][]string{"alice": {"team-a"}})
	cache := newTestCache(t, lister.Lister, c)
	if _, err := cache.Projects(context.Background(), "alice", "token"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lister.mu.Lock()
	lister.err["alice"] = errors.New("fulfillment is down")
	lister.mu.Unlock()
	c.Advance(31 * time.Second)
	if _, err := cache.Projects(context.Background(), "alice", "token"); err == nil {
		t.Fatal("expected the refresh error")
	}
	if lister.callCount() != 2 {
		t.Fatalf("unexpected calls: %d", lister.callCount())
	}
	c.Advance(time.Second)
	if _, err := cache.Projects(context.Background(), "alice", "token"); err == nil {
		t.Fatal("the entry must have been discarded, so this must refetch and fail")
	}
	if lister.callCount() != 3 {
		t.Errorf("a discarded entry must be refetched: %d calls", lister.callCount())
	}
}
