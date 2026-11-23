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

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// testScope is the two-branch organization-shaped scope used by the golden
// cases: one branch constrains the management cluster to a namespace, the
// other admits a hosted cluster by name.
func testScope() Scope {
	return Scope{Branches: []BranchSpec{
		{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "cluster", "management"),
			labels.MustNewMatcher(labels.MatchEqual, "namespace", "ns-a"),
		}},
		{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "cluster", "c-a"),
		}},
	}}
}

func oneBranchScope() Scope {
	return Scope{Branches: []BranchSpec{{Matchers: []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "cluster", "management"),
		labels.MustNewMatcher(labels.MatchEqual, "namespace", "ns-a"),
	}}}}
}

func TestRewriteQuery(t *testing.T) {
	a := `cluster="management",namespace="ns-a"`
	b := `cluster="c-a"`
	cases := []struct {
		name  string
		input string
		scope Scope
		want  string
	}{
		{
			name:  "bare selector",
			input: "up",
			scope: testScope(),
			want:  "(up{" + a + "} or up{" + b + "})",
		},
		{
			name:  "caller matcher is kept",
			input: `up{job="x"}`,
			scope: testScope(),
			want:  `(up{cluster="management",job="x",namespace="ns-a"} or up{cluster="c-a",job="x"})`,
		},
		{
			name:  "metric name is kept",
			input: "up",
			scope: oneBranchScope(),
			want:  "up{" + a + "}",
		},
		{
			name:  "duplicate scope label is appended, not dropped",
			input: `x{a="1"}`,
			scope: Scope{Branches: []BranchSpec{{Matchers: []*labels.Matcher{
				labels.MustNewMatcher(labels.MatchEqual, "a", "2"),
			}}}},
			want: `x{a="1",a="2"}`,
		},
		{
			name:  "matrix call",
			input: "rate(x_total[5m])",
			scope: testScope(),
			want:  "(rate(x_total{" + a + "}[5m]) or rate(x_total{" + b + "}[5m]))",
		},
		{
			name:  "histogram_quantile rewrites the inner rate",
			input: "histogram_quantile(0.9, rate(x_bucket[5m]))",
			scope: testScope(),
			want: "histogram_quantile(0.9, (rate(x_bucket{" + a + "}[5m]) or " +
				"rate(x_bucket{" + b + "}[5m])))",
		},
		{
			name:  "subquery keeps range and step",
			input: "(rate(x[5m]))[10m:1m]",
			scope: testScope(),
			want:  "((rate(x{" + a + "}[5m]) or rate(x{" + b + "}[5m])))[10m:1m]",
		},
		{
			name:  "aggregate over a matrix call",
			input: "sum by (namespace) (rate(x[5m]))",
			scope: testScope(),
			want: "sum by (namespace) ((rate(x{" + a + "}[5m]) or " +
				"rate(x{" + b + "}[5m])))",
		},
		{
			name:  "binary sides are independent",
			input: "x / y",
			scope: testScope(),
			want:  "(x{" + a + "} or x{" + b + "}) / (y{" + a + "} or y{" + b + "})",
		},
		{
			name:  "binary with vector matching",
			input: "x and on(a) y",
			scope: testScope(),
			want: "(x{" + a + "} or x{" + b + "}) and on (a) (y{" + a + "} or " +
				"y{" + b + "})",
		},
		{
			name:  "vector is left alone",
			input: "vector(1)",
			scope: testScope(),
			want:  "vector(1)",
		},
		{
			name:  "number literals are left alone",
			input: "1 + 2",
			scope: testScope(),
			want:  "1 + 2",
		},
		{
			name:  "topk scopes its argument",
			input: "topk(3, x)",
			scope: testScope(),
			want:  "topk(3, (x{" + a + "} or x{" + b + "}))",
		},
		{
			name:  "offset and at survive every clone",
			input: "x offset 1h @ 1234",
			scope: testScope(),
			want: "(x{" + a + "} @ 1234.000 offset 1h or x{" + b +
				"} @ 1234.000 offset 1h)",
		},
		{
			name:  "label_replace and its binary sibling",
			input: `label_replace(rate(a[5m]), "n", "$1", "m", "(.*)") and rate(b[5m])`,
			scope: testScope(),
			want: `label_replace((rate(a{` + a + `}[5m]) or rate(a{` + b +
				`}[5m])), "n", "$1", "m", "(.*)") and (rate(b{` + a +
				`}[5m]) or rate(b{` + b + `}[5m]))`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			expr, err := parseExpr(testCase.input)
			if err != nil {
				t.Fatalf("failed to parse %s: %v", testCase.input, err)
			}
			rewritten, err := Rewrite(expr, testCase.scope)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := rewritten.String(); got != testCase.want {
				t.Errorf("want %s\ngot  %s", testCase.want, got)
			}
		})
	}
}

func TestRewriteSingleBranchMergesInPlace(t *testing.T) {
	expr, err := parseExpr("up")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, oneBranchScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rewritten != expr {
		t.Error("a single branch must rewrite the existing selector, not clone it")
	}
}

func TestRewriteEmptyScope(t *testing.T) {
	expr, err := parseExpr("up")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, err := Rewrite(expr, Scope{}); !errors.Is(err, ErrNoScope) {
		t.Errorf("want ErrNoScope, got %v", err)
	}
}

func TestRewriteMatrixSelectorAtRootIsUnrewritable(t *testing.T) {
	expr, err := parseExpr("x[5m]")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, err := Rewrite(expr, testScope()); !errors.Is(err, ErrUnrewritable) {
		t.Errorf("want ErrUnrewritable, got %v", err)
	}
}

func TestRewriteTwoMatrixLeavesInOneCallIsUnrewritable(t *testing.T) {
	first, err := parseExpr("x[5m]")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	second, err := parseExpr("y[5m]")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	call := &parser.Call{
		Func: parser.Functions["abs"],
		Args: []parser.Expr{first, second},
	}
	if _, err := Rewrite(call, testScope()); !errors.Is(err, ErrUnrewritable) {
		t.Errorf("want ErrUnrewritable, got %v", err)
	}
}

func TestRewriteMatrixCallPreservesRangeAndOffset(t *testing.T) {
	expr, err := parseExpr("rate(x[5m] offset 1h)")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	paren, ok := rewritten.(*parser.ParenExpr)
	if !ok {
		t.Fatalf("expected parentheses, got %T", rewritten)
	}
	disjunction, ok := paren.Expr.(*parser.BinaryExpr)
	if !ok || disjunction.Op != parser.LOR {
		t.Fatalf("expected a vector or, got %T", paren.Expr)
	}
	for _, side := range []parser.Expr{disjunction.LHS, disjunction.RHS} {
		call, ok := side.(*parser.Call)
		if !ok {
			t.Fatalf("expected a call, got %T", side)
		}
		matrix, ok := call.Args[0].(*parser.MatrixSelector)
		if !ok {
			t.Fatalf("expected a matrix argument, got %T", call.Args[0])
		}
		if matrix.Range.String() != "5m0s" {
			t.Errorf("the range changed: %s", matrix.Range)
		}
		selector, ok := matrixSelector(matrix)
		if !ok {
			t.Fatalf("expected a selector inside the matrix, got %T", matrix.VectorSelector)
		}
		if selector.OriginalOffset.String() != "1h0m0s" {
			t.Errorf("the offset changed: %s", selector.OriginalOffset)
		}
	}
}

func TestRewritePreservesGroupingLabels(t *testing.T) {
	expr, err := parseExpr("sum by (namespace, job) (rate(x[5m]))")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	aggregate, ok := rewritten.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("expected an aggregate, got %T", rewritten)
	}
	if len(aggregate.Grouping) != 2 || aggregate.Grouping[0] != "namespace" ||
		aggregate.Grouping[1] != "job" || aggregate.Without {
		t.Errorf("grouping labels changed: %v without=%v", aggregate.Grouping, aggregate.Without)
	}
}

func TestRewriteProjectScopeGolden(t *testing.T) {
	expr, err := parseExpr("rate(kubevirt_vmi_cpu_usage_seconds_total[5m])")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, ProjectScope(projectFixture()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	metric := "kubevirt_vmi_cpu_usage_seconds_total"
	want := "(rate(" + metric + `{cluster="management",name=~"vm-1",namespace=~"vms"}[5m]) or ` +
		"rate(" + metric + `{cluster="management",namespace=~"cp"}[5m]) or ` +
		"rate(" + metric + `{cluster=~"c-1"}[5m]))`
	if got := rewritten.String(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

// orSides unwraps one disjunction into its instant-vector leaves.
func orSides(t *testing.T, expr parser.Expr) []*parser.VectorSelector {
	t.Helper()
	paren, ok := expr.(*parser.ParenExpr)
	if !ok {
		t.Fatalf("expected a parenthesized disjunction, got %T", expr)
	}
	var sides []*parser.VectorSelector
	var walk func(parser.Expr)
	walk = func(node parser.Expr) {
		switch typed := node.(type) {
		case *parser.VectorSelector:
			sides = append(sides, typed)
		case *parser.BinaryExpr:
			if typed.Op != parser.LOR {
				t.Fatalf("expected a vector or, got %s", typed.Op)
			}
			walk(typed.LHS)
			walk(typed.RHS)
		default:
			t.Fatalf("unexpected node %T", node)
		}
	}
	walk(paren.Expr)
	return sides
}

// matcherValues returns every value the selector matches for one label.
func matcherValues(selector *parser.VectorSelector, name string) []string {
	var values []string
	for _, matcher := range selector.LabelMatchers {
		if matcher.Name == name {
			values = append(values, matcher.Value)
		}
	}
	return values
}

func TestRewriteProjectScopeBinary(t *testing.T) {
	expr, err := parseExpr("kubevirt_vmi_x / kube_persistentvolumeclaim_y")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, ProjectScope(projectFixture()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	binary, ok := rewritten.(*parser.BinaryExpr)
	if !ok || binary.Op != parser.DIV {
		t.Fatalf("expected the division, got %T", rewritten)
	}
	left, right := orSides(t, binary.LHS), orSides(t, binary.RHS)
	if len(left) != 3 || len(right) != 3 {
		t.Fatalf("each side needs its own three branches: left=%d right=%d",
			len(left), len(right))
	}
	names := 0
	for _, selector := range left {
		if selector.Name != "kubevirt_vmi_x" {
			t.Errorf("the left metric changed: %q", selector.Name)
		}
		if values := matcherValues(selector, "persistentvolumeclaim"); len(values) != 0 {
			t.Errorf("the VM side must not carry the claim matcher: %v", values)
		}
		if values := matcherValues(selector, "name"); len(values) != 0 {
			names += len(values)
			if values[0] != "vm-1" {
				t.Errorf("unexpected VM name matcher: %v", values)
			}
		}
	}
	if names != 1 {
		t.Errorf("exactly the VM branch carries name=~vm-1, got %d", names)
	}
	claims := 0
	for _, selector := range right {
		if selector.Name != "kube_persistentvolumeclaim_y" {
			t.Errorf("the right metric changed: %q", selector.Name)
		}
		if values := matcherValues(selector, "name"); len(values) != 0 {
			t.Errorf("the claim side must not carry the VM matcher: %v", values)
		}
		if values := matcherValues(selector, "persistentvolumeclaim"); len(values) != 0 {
			claims += len(values)
			if values[0] != "vol-1" {
				t.Errorf("unexpected claim matcher: %v", values)
			}
		}
	}
	if claims != 1 {
		t.Errorf("exactly the claim branch carries persistentvolumeclaim=~vol-1, got %d", claims)
	}
}

func TestRewriteProjectScopeUnconstrainedPreservesNameMatcher(t *testing.T) {
	expr, err := parseExpr(`{__name__="kubevirt_vmi_info"}`)
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, ProjectScope(projectFixture()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	name := `__name__="kubevirt_vmi_info"`
	want := "({" + name + `,cluster="management",name=~"vm-1",namespace=~"vms"} or {` +
		name + `,cluster="management",namespace=~"cp"} or {` + name +
		`,cluster=~"c-1"})`
	if got := rewritten.String(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestRewriteProjectScopeUnconstrainedAddsFamilyName(t *testing.T) {
	expr, err := parseExpr(`{cluster="anything"}`)
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	rewritten, err := Rewrite(expr, ProjectScope(projectFixture()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vmName := `__name__=~"kubevirt_vmi_.*|kubevirt_vm_.*"`
	claimName := `__name__=~"kube_persistentvolumeclaim_.*|` +
		`kube_pod_spec_volumes_persistentvolumeclaims_info"`
	caller := `cluster="anything"`
	want := "({" + vmName + "," + caller + `,cluster="management",name=~"vm-1",` +
		`namespace=~"vms"} or {` + claimName + "," + caller +
		`,cluster="management",namespace=~"tenant-ns|vms",persistentvolumeclaim=~"vol-1"} or {` +
		caller + `,cluster="management",namespace=~"cp"} or {` + caller +
		`,cluster=~"c-1"})`
	if got := rewritten.String(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestRewriteProjectScopeExplicitPersistentVolumeIsEmpty(t *testing.T) {
	expr, err := parseExpr("kube_persistentvolume_capacity_bytes")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if _, err := Rewrite(expr, ProjectScope(projectFixture())); !errors.Is(err, ErrNoScope) {
		t.Errorf("want ErrNoScope, got %v", err)
	}
}

func TestRewriteIsDeterministic(t *testing.T) {
	expr, err := parseExpr(`sum by (namespace) (rate(x{job="a"}[5m])) / y`)
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	first, err := Rewrite(expr, testScope())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := first.String()
	for i := 0; i < 50; i++ {
		fresh, err := parseExpr(`sum by (namespace) (rate(x{job="a"}[5m])) / y`)
		if err != nil {
			t.Fatalf("failed to parse: %v", err)
		}
		rewritten, err := Rewrite(fresh, testScope())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := rewritten.String(); got != want {
			t.Fatalf("iteration %d differs: want %s got %s", i, want, got)
		}
	}
}
