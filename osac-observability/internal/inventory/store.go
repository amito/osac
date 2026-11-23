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
	"time"
)

// DefaultRefreshInterval is how often each informer re-lists. It is also the
// maximum age of a successful list: the authorizer serves the last good list
// for at most one refresh interval and then refuses every tenant query.
const DefaultRefreshInterval = 30 * time.Second

// EventType is a watch event kind.
type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
)

// Event is one normalized watch event.
type Event struct {
	Type   EventType
	UID    string
	Object Object
}

// Watch is one open watch. ResultChan is closed when the watch ends.
type Watch interface {
	ResultChan() <-chan Event
	Stop()
}

// Source is the list/watch provider. Tests replace it; production uses the
// Kubernetes dynamic client.
type Source interface {
	List(ctx context.Context, kind Kind) (objects []Object, resourceVersion string, err error)
	Watch(ctx context.Context, kind Kind, resourceVersion string) (Watch, error)
}

// StoreConfig configures a Store.
type StoreConfig struct {
	Source          Source
	Now             func() time.Time
	RefreshInterval time.Duration
	RetryInterval   time.Duration
}

// Store keeps the last successfully listed objects of every kind and decides
// whether that view is fresh enough to serve.
type Store struct {
	Source          Source
	RefreshInterval time.Duration
	RetryInterval   time.Duration

	nowFn func() time.Time

	mu             sync.Mutex
	objects        map[Kind]map[string]Object
	lastList       map[Kind]time.Time
	lastWatchError map[Kind]time.Time
	version        uint64
	cached         *Snapshot
	cachedVersion  uint64
}

// NewStore creates a store. All informers start cold: until each one has
// listed successfully, Snapshot reports not available.
func NewStore(config StoreConfig) *Store {
	refresh := config.RefreshInterval
	if refresh <= 0 {
		refresh = DefaultRefreshInterval
	}
	retry := config.RetryInterval
	if retry <= 0 {
		retry = time.Second
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		Source:          config.Source,
		RefreshInterval: refresh,
		RetryInterval:   retry,
		nowFn:           now,
		objects:         map[Kind]map[string]Object{},
		lastList:        map[Kind]time.Time{},
		lastWatchError:  map[Kind]time.Time{},
	}
}

func (s *Store) now() time.Time {
	if s.nowFn == nil {
		return time.Now()
	}
	return s.nowFn()
}

// Snapshot returns the current scope view and whether every informer has a
// successful list inside the refresh window. A stale or cold store yields
// ok=false; callers must answer 503 rather than forward anything.
func (s *Store) Snapshot(now time.Time) (*Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kind := range kinds {
		listed, ok := s.lastList[kind]
		if !ok || listed.IsZero() || now.Sub(listed) > s.RefreshInterval {
			return nil, false
		}
	}
	if s.cached != nil && s.cachedVersion == s.version {
		return s.cached, true
	}
	objects := make([]Object, 0)
	for _, kind := range kinds {
		for _, object := range s.objects[kind] {
			objects = append(objects, object)
		}
	}
	s.cached = BuildSnapshot(objects)
	s.cachedVersion = s.version
	return s.cached, true
}

// ingest replaces one kind's objects with a successful list result.
func (s *Store) ingest(kind Kind, objects []Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byUID := make(map[string]Object, len(objects))
	for _, object := range objects {
		object.Kind = kind
		byUID[objectKey(object)] = object
	}
	s.objects[kind] = byUID
	s.lastList[kind] = s.now()
	s.version++
}

// apply folds one watch event into the last listed view.
func (s *Store) apply(kind Kind, event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[kind] == nil {
		s.objects[kind] = map[string]Object{}
	}
	key := event.UID
	if key == "" {
		key = event.Object.UID
	}
	if key == "" {
		key = objectKey(event.Object)
	}
	switch event.Type {
	case EventDelete:
		delete(s.objects[kind], key)
	default:
		object := event.Object
		object.Kind = kind
		s.objects[kind][key] = object
	}
	s.version++
}

// markWatchFailed records that a watch ended unexpectedly. It deliberately
// does not touch lastList: a failed watch can shorten, never extend, the time
// the last good list is served.
func (s *Store) markWatchFailed(kind Kind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWatchError[kind] = s.now()
}

// objectKey is the map key for an object that has no UID (only in tests).
func objectKey(object Object) string {
	return string(object.Kind) + "/" + object.Namespace + "/" + object.Name
}
