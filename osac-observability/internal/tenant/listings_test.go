/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package tenant

import (
	"errors"
	"testing"
)

func TestExpandMatchSelectors(t *testing.T) {
	selectors, err := ExpandMatch([]string{`up{job="x"}`, `down{job="y"}`}, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		`up{cluster="management",job="x",namespace="ns-a"}`,
		`up{cluster="c-a",job="x"}`,
		`down{cluster="management",job="y",namespace="ns-a"}`,
		`down{cluster="c-a",job="y"}`,
	}
	if len(selectors) != len(want) {
		t.Fatalf("want %d selectors, got %d: %v", len(want), len(selectors), selectors)
	}
	for index := range want {
		if selectors[index] != want[index] {
			t.Errorf("selector %d: want %s, got %s", index, want[index], selectors[index])
		}
	}
}

func TestExpandMatchAbsentUsesScope(t *testing.T) {
	selectors, err := ExpandMatch(nil, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		`{cluster="management",namespace="ns-a"}`,
		`{cluster="c-a"}`,
	}
	if len(selectors) != len(want) {
		t.Fatalf("want %d selectors, got %d: %v", len(want), len(selectors), selectors)
	}
	for index := range want {
		if selectors[index] != want[index] {
			t.Errorf("selector %d: want %s, got %s", index, want[index], selectors[index])
		}
	}
}

func TestExpandMatchEmptyStringUsesScope(t *testing.T) {
	selectors, err := ExpandMatch([]string{""}, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selectors) != 2 {
		t.Fatalf("want two scope selectors, got %v", selectors)
	}
	if selectors[0] != `{cluster="management",namespace="ns-a"}` {
		t.Errorf("unexpected first selector: %s", selectors[0])
	}
}

func TestExpandMatchRejectsBadSelector(t *testing.T) {
	if _, err := ExpandMatch([]string{"a{b"}, testScope()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExpandMatchRejectsNonSelectorExpression(t *testing.T) {
	if _, err := ExpandMatch([]string{"rate(up[5m])"}, testScope()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestExpandMatchEmptyScope(t *testing.T) {
	if _, err := ExpandMatch([]string{"up"}, Scope{}); !errors.Is(err, ErrNoScope) {
		t.Errorf("want ErrNoScope, got %v", err)
	}
}

func TestExpandMatchPreservesTheCallerNameMatcher(t *testing.T) {
	// An unconstrained selector expands to the scope only; a constrained one
	// keeps its __name__ matcher in every branch.
	selectors, err := ExpandMatch([]string{`{__name__="up"}`}, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selectors) != 2 {
		t.Fatalf("want two selectors, got %v", selectors)
	}
	for _, selector := range selectors {
		if selector == "" || selector[0] != '{' {
			t.Errorf("the caller's __name__ matcher was dropped: %s", selector)
		}
	}
}
