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
	"time"
)

var kinds = []Kind{KindClusterOrders, KindComputeInstances, KindVolumes}

// Run starts one informer per kind. It returns immediately; the informers stop
// when the context is cancelled.
func (s *Store) Run(ctx context.Context) {
	for _, kind := range kinds {
		go s.syncKind(ctx, kind)
	}
}

// syncKind lists, watches and periodically re-lists one kind. A list success
// refreshes lastList; a watch ending or failing sends the loop back to a list,
// which is the only thing that can make the view fresh again.
func (s *Store) syncKind(ctx context.Context, kind Kind) {
	if s.Source == nil {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		objects, resourceVersion, err := s.Source.List(ctx, kind)
		if err != nil {
			if !s.wait(ctx) {
				return
			}
			continue
		}
		s.ingest(kind, objects)
		watch, err := s.Source.Watch(ctx, kind, resourceVersion)
		if err != nil {
			if !s.wait(ctx) {
				return
			}
			continue
		}
		s.consume(ctx, kind, watch)
		watch.Stop()
	}
}

// consume drains one watch until it ends, the refresh interval elapses (a
// scheduled re-list) or the context is cancelled.
func (s *Store) consume(ctx context.Context, kind Kind, watch Watch) {
	ticker := time.NewTicker(s.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watch.ResultChan():
			if !ok {
				s.markWatchFailed(kind)
				return
			}
			s.apply(kind, event)
		case <-ticker.C:
			return
		}
	}
}

func (s *Store) wait(ctx context.Context) bool {
	timer := time.NewTimer(s.RetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
