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
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

func TestCloneExprReturnsADistinctTree(t *testing.T) {
	original, err := parseExpr(`up{job="x"}`)
	if err != nil {
		t.Fatalf("failed to parse the original: %v", err)
	}
	clone, err := cloneExpr(original)
	if err != nil {
		t.Fatalf("failed to clone: %v", err)
	}
	if clone == original {
		t.Fatal("the clone is the original tree")
	}
	if clone.String() != original.String() {
		t.Errorf("want %s, got %s", original.String(), clone.String())
	}
	clonedSelector, ok := clone.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("expected a vector selector, got %T", clone)
	}
	wantOriginal := original.String()
	clonedSelector.LabelMatchers = append(clonedSelector.LabelMatchers,
		labels.MustNewMatcher(labels.MatchEqual, "cluster", "c-a"))
	if original.String() != wantOriginal {
		t.Errorf("mutating the clone changed the original: %s", original.String())
	}
}

func TestCloneExprPreservesOffsetAndTimestamp(t *testing.T) {
	original, err := parseExpr(`up offset 1h @ 1234`)
	if err != nil {
		t.Fatalf("failed to parse the original: %v", err)
	}
	clone, err := cloneExpr(original)
	if err != nil {
		t.Fatalf("failed to clone: %v", err)
	}
	clonedSelector, ok := clone.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("expected a vector selector, got %T", clone)
	}
	if clone.String() != `up @ 1234.000 offset 1h` {
		t.Errorf("unexpected clone: %s", clone.String())
	}
	if clonedSelector.OriginalOffset == 0 {
		t.Error("the offset was lost")
	}
	if clonedSelector.Timestamp == nil {
		t.Error("the @ modifier was lost")
	}
}

func TestOrExprKeepsASingleExpressionAsIs(t *testing.T) {
	expr, err := parseExpr("up")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	joined := orExpr([]parser.Expr{expr})
	if joined != expr {
		t.Errorf("a single expression must be returned unchanged, got %T", joined)
	}
}

func TestOrExprJoinsTwoExpressions(t *testing.T) {
	left, _ := parseExpr("a")
	right, _ := parseExpr("b")
	joined := orExpr([]parser.Expr{left, right})
	want := "(a or b)"
	if joined.String() != want {
		t.Errorf("want %s, got %s", want, joined.String())
	}
}

func TestOrExprNestsLeftAssociativelyInOrder(t *testing.T) {
	var exprs []parser.Expr
	for _, input := range []string{"a", "b", "c"} {
		expr, err := parseExpr(input)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", input, err)
		}
		exprs = append(exprs, expr)
	}
	joined := orExpr(exprs)
	if joined.String() != "(a or b or c)" {
		t.Fatalf("unexpected rendering: %s", joined.String())
	}
	// The tree must stay left-associative and in declaration order: the first
	// branch is the outermost LHS, so a later branch can never shadow it.
	paren, ok := joined.(*parser.ParenExpr)
	if !ok {
		t.Fatalf("expected parentheses, got %T", joined)
	}
	outer, ok := paren.Expr.(*parser.BinaryExpr)
	if !ok || outer.Op != parser.LOR {
		t.Fatalf("expected a vector or, got %T", paren.Expr)
	}
	if outer.RHS.String() != "c" {
		t.Errorf("the last branch must be the RHS, got %s", outer.RHS.String())
	}
	inner, ok := outer.LHS.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected the earlier branches on the left, got %T", outer.LHS)
	}
	if inner.String() != "a or b" {
		t.Errorf("unexpected left side: %s", inner.String())
	}
}
