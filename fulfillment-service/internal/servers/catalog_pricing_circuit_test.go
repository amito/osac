/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servers

import (
	"testing"
	"time"
)

func TestCatalogPricingCircuitBreakerOpensAndAllowsSingleProbe(t *testing.T) {
	breaker := &catalogPricingCircuitBreaker{}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	for i := 0; i < catalogPricingCircuitFailureThreshold-1; i++ {
		allowed, probe := breaker.allow(now)
		if !allowed || probe {
			t.Fatalf("allow() before threshold = (%t, %t), want regular request", allowed, probe)
		}
		breaker.recordFailure(now, false)
	}
	if allowed, _ := breaker.allow(now); !allowed {
		t.Fatal("allow() opened before reaching the failure threshold")
	}
	breaker.recordFailure(now, false)

	if allowed, _ := breaker.allow(now.Add(catalogPricingCircuitOpenDuration - time.Second)); allowed {
		t.Fatal("allow() = true while circuit is open")
	}
	probeTime := now.Add(catalogPricingCircuitOpenDuration)
	allowed, probe := breaker.allow(probeTime)
	if !allowed || !probe {
		t.Fatalf("allow() after open interval = (%t, %t), want a single probe", allowed, probe)
	}
	if allowed, _ := breaker.allow(probeTime); allowed {
		t.Fatal("allow() admitted a second concurrent probe")
	}
	breaker.recordSuccess(probe)
	if allowed, probe := breaker.allow(probeTime); !allowed || probe {
		t.Fatalf("allow() after successful probe = (%t, %t), want regular request", allowed, probe)
	}
}
