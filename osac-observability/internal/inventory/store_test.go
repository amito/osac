/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package inventory

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(duration)
}

func newTestStore(clock *fakeClock) *Store {
	return NewStore(StoreConfig{Now: clock.now})
}

func TestStoreColdStartReturnsUnavailable(t *testing.T) {
	store := newTestStore(newFakeClock())
	if _, ok := store.Snapshot(store.now()); ok {
		t.Fatal("an empty store must not be available")
	}
}

func TestStoreOneInformerStaleBlocksEveryQuery(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	if _, ok := store.Snapshot(clock.now()); ok {
		t.Fatal("two of three informers must not be available")
	}
	store.ingest(KindVolumes, nil)
	if _, ok := store.Snapshot(clock.now()); !ok {
		t.Fatal("all three informers must make the store available")
	}
}

func TestStoreGraceWindow(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	store.ingest(KindVolumes, nil)
	clock.advance(29 * time.Second)
	if _, ok := store.Snapshot(clock.now()); !ok {
		t.Fatal("expected the last good list to serve inside the 30s window")
	}
	clock.advance(2 * time.Second)
	if _, ok := store.Snapshot(clock.now()); ok {
		t.Fatal("expected the store to be unavailable after the window")
	}
}

func TestStoreWatchFailureDoesNotExtendWindow(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	store.ingest(KindVolumes, nil)
	clock.advance(5 * time.Second)
	store.markWatchFailed(KindClusterOrders)
	clock.advance(26 * time.Second)
	if _, ok := store.Snapshot(clock.now()); ok {
		t.Fatal("a failed watch must not extend the grace window")
	}
}

func TestStoreRecoversWithoutRestart(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	store.ingest(KindVolumes, nil)
	clock.advance(40 * time.Second)
	if _, ok := store.Snapshot(clock.now()); ok {
		t.Fatal("expected the store to be unavailable before recovery")
	}
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	store.ingest(KindVolumes, nil)
	if _, ok := store.Snapshot(clock.now()); !ok {
		t.Fatal("a successful list must recover the store without a restart")
	}
}

func TestStoreListReplacesSnapshot(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	gone := Object{Kind: KindVolumes, UID: "vol-1", Tenant: "acme", Namespace: "vm-ns", Name: "vol-1"}
	store.ingest(KindClusterOrders, nil)
	store.ingest(KindComputeInstances, nil)
	store.ingest(KindVolumes, []Object{gone})
	snapshot, ok := store.Snapshot(clock.now())
	if !ok {
		t.Fatal("expected an available store")
	}
	if got := snapshot.Organizations["acme"].Namespaces; len(got) != 1 || got[0] != "vm-ns" {
		t.Fatalf("expected the volume namespace, got %v", got)
	}
	store.ingest(KindVolumes, nil)
	snapshot, ok = store.Snapshot(clock.now())
	if !ok {
		t.Fatal("expected an available store after the second list")
	}
	org := snapshot.Organizations["acme"]
	if org != nil && len(org.Namespaces) != 0 {
		t.Fatalf("a deleted resource must disappear from the predicate: %v", org.Namespaces)
	}
}

// fakeSource scripts list and watch results for one kind.
type fakeSource struct {
	mu        sync.Mutex
	lists     [][]Object
	listCalls int
	watch     *fakeWatch
}

func (s *fakeSource) List(_ context.Context, _ Kind) ([]Object, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if len(s.lists) == 0 {
		return nil, "1", nil
	}
	next := s.lists[0]
	s.lists = s.lists[1:]
	return next, "1", nil
}

func (s *fakeSource) Watch(_ context.Context, _ Kind, _ string) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watch == nil {
		s.watch = &fakeWatch{events: make(chan Event)}
	}
	return s.watch, nil
}

type fakeWatch struct {
	events chan Event
	once   sync.Once
}

func (w *fakeWatch) ResultChan() <-chan Event { return w.events }

func (w *fakeWatch) Stop() {}

func (w *fakeWatch) close() { w.once.Do(func() { close(w.events) }) }

func TestStoreAppliesWatchEvents(t *testing.T) {
	store := newTestStore(newFakeClock())
	store.ingest(KindVolumes, nil)
	store.apply(KindVolumes, Event{Type: EventAdd, Object: Object{
		Kind: KindVolumes, UID: "a", Tenant: "acme", Namespace: "ns-a", Name: "a",
	}})
	store.apply(KindVolumes, Event{Type: EventDelete, UID: "a"})
	if len(store.objects[KindVolumes]) != 0 {
		t.Fatal("a delete event must remove the object")
	}
}

func TestSyncKindRelistsAfterTheRefreshInterval(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(clock)
	store.RefreshInterval = 10 * time.Millisecond
	store.RetryInterval = time.Millisecond
	source := &fakeSource{lists: [][]Object{
		{{Kind: KindVolumes, UID: "a", Tenant: "acme", Namespace: "ns-a", Name: "a"}},
		{{Kind: KindVolumes, UID: "b", Tenant: "acme", Namespace: "ns-b", Name: "b"}},
	}}
	store.Source = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.syncKind(ctx, KindVolumes)

	deadline := time.Now().Add(2 * time.Second)
	for {
		source.mu.Lock()
		calls := source.listCalls
		source.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected a re-list after the refresh interval, got %d calls", calls)
		}
		time.Sleep(time.Millisecond)
	}
}
