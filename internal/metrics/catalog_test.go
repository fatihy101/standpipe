package metrics

import (
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Catalog() is the single source of the metric reference.
//
// docs/reference/metrics.md is generated from Catalog() (see internal/docgen),
// so the published table cannot drift from the code: a changed row changes
// the generated file, and internal/docgen's currency test fails until
// `go generate ./...` is run and the diff is reviewed.
//
// What emission does relative to Catalog() is a runtime fact and is owned by
// Drift(): recording an undeclared name, under the wrong kind, with a
// different label set, or under a NotEmitted row is a drift entry, surfaced as
// metrics_drift on /healthz. This file owns the shape of the catalog itself,
// which is what every row of the published table is read from.
// ---------------------------------------------------------------------------

var (
	metricNameRE = regexp.MustCompile(`^gateway_[a-z0-9_]+$`)
	labelNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

func TestCatalogRowsAreWellFormed(t *testing.T) {
	catalog := Catalog()
	if len(catalog) == 0 {
		t.Fatal("Catalog() returned no rows; every check below would pass vacuously")
	}

	seen := map[string]bool{}
	for _, d := range catalog {
		if seen[d.Name] {
			// New() maps by name and appends to its order unconditionally, so
			// a duplicate silently discards one Def and writes the survivor
			// twice into the exposition.
			t.Errorf("Catalog() lists %q twice; New() would keep one Def and export the metric twice", d.Name)
		}
		seen[d.Name] = true

		if !metricNameRE.MatchString(d.Name) {
			t.Errorf("%q is not a gateway_ metric name in lower snake case", d.Name)
		}
		switch d.Kind {
		case KindCounter:
			if !strings.HasSuffix(d.Name, "_total") {
				t.Errorf("counter %s does not end in _total", d.Name)
			}
		case KindGauge, KindHistogram:
			if strings.HasSuffix(d.Name, "_total") {
				t.Errorf("%s %s ends in _total, which Prometheus reserves for counters", d.Kind, d.Name)
			}
		default:
			t.Errorf("%s has kind %q, which is not one of %s/%s/%s", d.Name, d.Kind, KindCounter, KindGauge, KindHistogram)
		}
		if d.Kind != KindHistogram && len(d.Buckets) != 0 {
			t.Errorf("%s is a %s and carries histogram buckets", d.Name, d.Kind)
		}
		for i := 1; i < len(d.Buckets); i++ {
			if d.Buckets[i] <= d.Buckets[i-1] {
				t.Errorf("%s buckets are not strictly increasing: %v", d.Name, d.Buckets)
				break
			}
		}
		if strings.TrimSpace(d.Help) == "" {
			t.Errorf("%s has no Help; the published reference describes each metric from it", d.Name)
		}

		labels := map[string]bool{}
		for _, l := range d.Labels {
			if !labelNameRE.MatchString(l) {
				t.Errorf("%s carries label %q, which is not a lower snake case label name", d.Name, l)
			}
			if labels[l] {
				t.Errorf("%s carries label %q twice", d.Name, l)
			}
			labels[l] = true
			if l == "le" && d.Kind == KindHistogram {
				t.Errorf("histogram %s declares the reserved label le", d.Name)
			}
		}
	}
}

// TestNotEmittedRowsStayOffTheWire pins the meaning the reference gives a
// "declared, not emitted yet" row: New() seeds no series for it, so it does
// not appear on /metrics until a code path records it.
func TestNotEmittedRowsStayOffTheWire(t *testing.T) {
	r := New()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := b.String()
	for _, d := range Catalog() {
		if !d.NotEmitted {
			continue
		}
		if strings.Contains(out, d.Name) {
			t.Errorf("%s is declared NotEmitted and appears in a fresh registry's exposition", d.Name)
		}
	}
}
