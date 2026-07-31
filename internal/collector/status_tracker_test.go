package collector

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// statusTrackerSuccessMetricName is the collector_success metric family name
// StatusTracker emits, shared by every test below both as the
// testutil.CollectAndCompare filter and inside each expected-text literal.
// Filtering to just this family deliberately excludes
// collector_duration_seconds from every comparison: its value is a real
// elapsed time and therefore not deterministic.
//
// Deliberately NOT named statusTrackerSuccessMetric: this file is common
// (see scaffold.sh's code/<flavor>/ staging vs. plain internal/collector/
// path) and ships into every flavor's internal/collector package
// unconditionally, so its package-level identifiers must never collide with
// a flavor's own test file: code/http/collector_test.go.tmpl already
// defines a const named statusTrackerSuccessMetric for the same value.
const statusTrackerSuccessMetricName = "tapelibrary_exporter_collector_success"

// stubStatusTrackerCollector is a minimal, self-contained prometheus.Collector
// used only by this file's tests. It exists so StatusTracker's concurrency
// and failure-detection behavior can be locked down without depending on any
// flavor-specific collector (ExampleCollector, an HTTP client, fixtures,
// ...): status_tracker.go.tmpl has no flavor dependency, so its tests must
// not gain one either: this file has to build and pass identically no
// matter which --flavor was scaffolded.
type stubStatusTrackerCollector struct {
	desc   *prometheus.Desc
	panics bool
	emits  bool
}

// newStubStatusTrackerCollector's Describe always sends exactly one
// descriptor, whether or not Collect ends up emitting it: every stub here is
// a well-formed collector that *promises* a metric, so a Collect that emits
// nothing is a genuine broken scrape (the "log the error, then bare return"
// shape every real collector in this exporter follows on its error path, see
// status_tracker.go.tmpl's Collect doc comment) and not merely an
// intentionally metric-less collector (which Prometheus also allows).
func newStubStatusTrackerCollector(metricName string, panics, emits bool) *stubStatusTrackerCollector {
	return &stubStatusTrackerCollector{
		desc:   prometheus.NewDesc(metricName, "stub metric for StatusTracker tests", nil, nil),
		panics: panics,
		emits:  emits,
	}
}

func (s *stubStatusTrackerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.desc
}

func (s *stubStatusTrackerCollector) Collect(ch chan<- prometheus.Metric) {
	if s.panics {
		panic("stubStatusTrackerCollector: simulated panic")
	}
	if s.emits {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, 1)
	}
}

// TestStatusTracker_PanicMarksFailed wraps a collector whose Collect panics
// and asserts two things at once: the panic never escapes StatusTracker (a
// hung or crashed Gather would mean the defer-close-before-recover ordering
// documented on StatusTracker.Collect regressed), and the wrapped collector
// is reported failed even though nothing about "emitted metrics" said so:
// here the panic itself is the only failure signal.
func TestStatusTracker_PanicMarksFailed(t *testing.T) {
	log := logger.NewTextLogger("error")
	stub := newStubStatusTrackerCollector("statustrackertest_panic_metric", true, false)

	tracker := NewStatusTracker(log)
	tracker.Add("panicking", stub)

	expected := `
# HELP ` + statusTrackerSuccessMetricName + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetricName + ` gauge
` + statusTrackerSuccessMetricName + `{collector="panicking"} 0
`

	// Run the comparison in the background and bound the wait: if a panic
	// ever escaped the buffering goroutine inside StatusTracker.Collect (e.g.
	// a future edit breaks the close(sub)-before-recover ordering), that
	// goroutine blocks forever on `range sub` and this call never returns.
	// Bounding it here fails this one test in seconds instead of hanging the
	// whole `go test` run until its outer -timeout kills the binary.
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- testutil.CollectAndCompare(tracker, strings.NewReader(expected), statusTrackerSuccessMetricName)
	}()

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("unexpected collecting result:\n%s", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CollectAndCompare did not return within 5s after the wrapped collector panicked; StatusTracker.Collect likely hung")
	}
}

// TestStatusTracker_ZeroMetricsMarksFailed wraps a collector that returns
// normally from Collect but emits nothing: the same "log the error, then
// bare return" shape every collector in this exporter follows on its own
// error path. Nothing here panics or returns an error value, so this pins
// down that StatusTracker detects the failure purely by counting emitted
// metrics.
func TestStatusTracker_ZeroMetricsMarksFailed(t *testing.T) {
	log := logger.NewTextLogger("error")
	stub := newStubStatusTrackerCollector("statustrackertest_zero_metric", false, false)

	tracker := NewStatusTracker(log)
	tracker.Add("silent", stub)

	expected := `
# HELP ` + statusTrackerSuccessMetricName + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetricName + ` gauge
` + statusTrackerSuccessMetricName + `{collector="silent"} 0
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expected), statusTrackerSuccessMetricName); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestStatusTracker_SuccessWhenMetricsEmitted wraps a healthy collector and
// asserts both halves of the success path: collector_success reports 1,
// and (just as important) the wrapped collector's own metric is still
// forwarded through the tracker into the final gather output, rather than
// being swallowed by the per-entry buffering StatusTracker.Collect uses to
// count metrics.
func TestStatusTracker_SuccessWhenMetricsEmitted(t *testing.T) {
	const stubMetricName = "statustrackertest_emitted_metric"

	log := logger.NewTextLogger("error")
	stub := newStubStatusTrackerCollector(stubMetricName, false, true)

	tracker := NewStatusTracker(log)
	tracker.Add("healthy", stub)

	expected := `
# HELP ` + stubMetricName + ` stub metric for StatusTracker tests
# TYPE ` + stubMetricName + ` gauge
` + stubMetricName + ` 1
# HELP ` + statusTrackerSuccessMetricName + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetricName + ` gauge
` + statusTrackerSuccessMetricName + `{collector="healthy"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expected), stubMetricName, statusTrackerSuccessMetricName); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
