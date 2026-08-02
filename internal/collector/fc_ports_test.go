package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sckyzo/tapelibrary_exporter/internal/logger"
)

// fcPortsMetricNames is every family this collector emits except the freshness
// gauge, whose value is a wall-clock timestamp and therefore cannot be pinned
// in an exposition block. Listed in the order Registry.Gather sorts them (by
// family name), which is the order the expected blocks below use.
var fcPortsMetricNames = []string{
	"tapelibrary_fc_port_info",
	"tapelibrary_fc_port_speed_bytes_per_second",
	"tapelibrary_fc_port_state",
}

// fcPortsFixtureServer serves testdata/fc_ports.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start (which
// is what the lifecycle tests below exercise).
func fcPortsFixtureServer(t *testing.T) (*httptest.Server, *FCPortsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/fc_ports.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewFCPortsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// fcPortsServing returns a collector fed by a server that answers every request
// with body. Used by the branch tests below, which need input shapes the real
// capture does not contain and which must therefore not be invented inside
// testdata/fc_ports.json (that fixture stays faithful to the 2026-07-28
// capture, in which every port is either communicationEstablished or
// noLightDetected).
func fcPortsServing(t *testing.T, body string) *FCPortsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewFCPortsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseFCPorts exercises parseFCPorts (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine involved.
func TestParseFCPorts(t *testing.T) {
	data, err := os.ReadFile("testdata/fc_ports.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	ports, err := parseFCPorts(data)
	if err != nil {
		t.Fatalf("parseFCPorts: %v", err)
	}

	// Four ports: a trim of the capture's 80, chosen to carry every distinct
	// shape it contains rather than its volume.
	if len(ports) != 4 {
		t.Fatalf("len(ports) = %d, want 4", len(ports))
	}

	first := ports[0]
	if first.Location != "fcPort_F1C4R1P0" {
		t.Errorf("Location = %q, want %q", first.Location, "fcPort_F1C4R1P0")
	}
	if first.State != "communicationEstablished" {
		t.Errorf("State = %q, want %q", first.State, "communicationEstablished")
	}
	if first.DriveLocation != "drive_F1C4R1" {
		t.Errorf("DriveLocation = %q, want %q", first.DriveLocation, "drive_F1C4R1")
	}
	if first.SpeedActual != "16Gbps" {
		t.Errorf("SpeedActual = %q, want %q", first.SpeedActual, "16Gbps")
	}
	if first.LoopID != 13 {
		t.Errorf("LoopID = %d, want 13", first.LoopID)
	}

	// The join key to the drives collector. If it ever stops being decoded,
	// FCPortNoLight silently matches nothing rather than failing loudly:
	// its `on (job, library, location)` join would find no right-hand side.
	t.Run("every port carries the drive location it belongs to", func(t *testing.T) {
		for _, p := range ports {
			if p.DriveLocation == "" {
				t.Errorf("port %q has an empty DriveLocation: the drives join key is missing", p.Location)
			}
		}
	})

	// Two ports per drive is what makes location, not driveLocation, the key
	// on this endpoint — the port number is baked into the location string
	// (...P0 / ...P1). Verified against the capture rather than inherited
	// from another collector, per the key rule in docs/exporter-journal.md.
	t.Run("one drive carries two ports at distinct locations", func(t *testing.T) {
		body := `[{"location":"fcPort_F1C4R1P0","driveLocation":"drive_F1C4R1","state":"communicationEstablished"},` +
			`{"location":"fcPort_F1C4R1P1","driveLocation":"drive_F1C4R1","state":"communicationEstablished"}]`
		got, err := parseFCPorts([]byte(body))
		if err != nil {
			t.Fatalf("parseFCPorts(two ports on one drive): %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2: a drive's two ports must not collide", len(got))
		}
	})

	// R1.11.2 types portNumber "(string)" while every entry in the capture
	// sends a bare JSON number. The field is deliberately not declared on
	// fcPortStats, so neither shape can break the decode; this pins that.
	t.Run("the undeclared portNumber does not break the decode in either shape", func(t *testing.T) {
		for _, body := range []string{
			`[{"location":"fcPort_F1C4R1P0","state":"unknown","portNumber":0}]`,
			`[{"location":"fcPort_F1C4R1P0","state":"unknown","portNumber":"0"}]`,
		} {
			if _, err := parseFCPorts([]byte(body)); err != nil {
				t.Errorf("parseFCPorts(%s): %v", body, err)
			}
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseFCPorts([]byte("not json")); err == nil {
			t.Error("parseFCPorts(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseFCPorts([]byte(`{"location":"fcPort_F1C4R1P0"}`)); err == nil {
			t.Error("parseFCPorts(object) returned a nil error, want non-nil")
		}
	})

	// Every TS4500 drive ships with Fibre Channel ports and a library has
	// drives by definition, so an empty list is a response that lost its
	// content rather than a library with none. Accepting it would replace a
	// good cache with nothing. (/v1/sasPorts legitimately returns [] on this
	// hardware; this endpoint is not that one.)
	t.Run("empty array is an error, not a library with no FC ports", func(t *testing.T) {
		if _, err := parseFCPorts([]byte(`[]`)); err == nil {
			t.Error("parseFCPorts(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseFCPorts(nil); err == nil {
			t.Error("parseFCPorts(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseFCPorts([]byte(`[{"state":"communicationEstablished"}]`)); err == nil {
			t.Error("parseFCPorts(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"fcPort_F1C4R1P0","state":"unknown"},{"location":"fcPort_F1C4R1P0","state":"unknown"}]`
		if _, err := parseFCPorts([]byte(dup)); err == nil {
			t.Error("parseFCPorts(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestFCPortsCollector_Describe locks the descriptor count at exactly 4 (the
// stateset, the speed gauge, the info series and the freshness gauge) so a
// future edit that silently adds or drops a metric is caught here rather than
// downstream in docs-check or a dashboard.
func TestFCPortsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 4 {
		t.Fatalf("Describe sent %d descriptors, want 4", count)
	}
}

// TestFCPortsCollector_Collect pins the exact exposition text of every family
// against the fixture. refresh is driven directly rather than through Start, so
// the assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
//
// All four documented states are emitted per port, exactly one carrying 1,
// which is what keeps the severity classification in the alerting rules rather
// than in the value. The speed family carries only three of the four ports: the
// dark one reporting "unknown" produces no speed series at all.
func TestFCPortsCollector_Collect(t *testing.T) {
	_, c := fcPortsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_fc_port_info Fibre Channel port identity and configuration, always 1. Identity strings live here rather than on a measurement series, so a drive swap or a re-negotiated topology changes this series alone instead of breaking the continuity of the link state and speed.
# TYPE tapelibrary_fc_port_info gauge
tapelibrary_fc_port_info{drive_location="drive_F11C2R3",drive_serial="SN00000004",location="fcPort_F11C2R3P0",loop_id="55",speed_setting="auto",topology_actual="unknown",topology_setting="auto-L",wwpn="5005076400000401"} 1
tapelibrary_fc_port_info{drive_location="drive_F11C3R2",drive_serial="SN00000005",location="fcPort_F11C3R2P0",loop_id="58",speed_setting="8Gbps",topology_actual="N-Port",topology_setting="auto-L",wwpn="5005076400000501"} 1
tapelibrary_fc_port_info{drive_location="drive_F1C4R1",drive_serial="SN00000001",location="fcPort_F1C4R1P0",loop_id="13",speed_setting="auto",topology_actual="N-Port",topology_setting="auto-L",wwpn="5005076400000101"} 1
tapelibrary_fc_port_info{drive_location="drive_F1C4R2",drive_serial="SN00000002",location="fcPort_F1C4R2P1",loop_id="78",speed_setting="auto",topology_actual="unknown",topology_setting="auto-L",wwpn="5005076400000202"} 1
# HELP tapelibrary_fc_port_speed_bytes_per_second Negotiated link rate of the Fibre Channel port, in bytes per second, converted from the library's own Gbps figure at 8 bits per byte. This is the nominal signalling rate, not achievable payload throughput. A port reporting a rate this exporter cannot map (the capture's undocumented "unknown" on every dark port) produces no series rather than a 0 asserting a link running at zero.
# TYPE tapelibrary_fc_port_speed_bytes_per_second gauge
tapelibrary_fc_port_speed_bytes_per_second{drive_location="drive_F11C2R3",location="fcPort_F11C2R3P0"} 1.25e+08
tapelibrary_fc_port_speed_bytes_per_second{drive_location="drive_F11C3R2",location="fcPort_F11C3R2P0"} 1e+09
tapelibrary_fc_port_speed_bytes_per_second{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0"} 2e+09
# HELP tapelibrary_fc_port_state Link state of the Fibre Channel port, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_fc_port_state gauge
tapelibrary_fc_port_state{drive_location="drive_F11C2R3",location="fcPort_F11C2R3P0",state="communicationEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F11C2R3",location="fcPort_F11C2R3P0",state="communicationNotEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F11C2R3",location="fcPort_F11C2R3P0",state="noLightDetected"} 1
tapelibrary_fc_port_state{drive_location="drive_F11C2R3",location="fcPort_F11C2R3P0",state="unknown"} 0
tapelibrary_fc_port_state{drive_location="drive_F11C3R2",location="fcPort_F11C3R2P0",state="communicationEstablished"} 1
tapelibrary_fc_port_state{drive_location="drive_F11C3R2",location="fcPort_F11C3R2P0",state="communicationNotEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F11C3R2",location="fcPort_F11C3R2P0",state="noLightDetected"} 0
tapelibrary_fc_port_state{drive_location="drive_F11C3R2",location="fcPort_F11C3R2P0",state="unknown"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="communicationEstablished"} 1
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="communicationNotEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="noLightDetected"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="unknown"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R2",location="fcPort_F1C4R2P1",state="communicationEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R2",location="fcPort_F1C4R2P1",state="communicationNotEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R2",location="fcPort_F1C4R2P1",state="noLightDetected"} 1
tapelibrary_fc_port_state{drive_location="drive_F1C4R2",location="fcPort_F1C4R2P1",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), fcPortsMetricNames...); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 23 cached metrics (4 ports x (4 documented states + info) = 20, plus a
	// speed gauge on the 3 ports that report a mappable rate) plus the
	// freshness gauge Collect always appends. Unlike power_supplies and
	// node_cards, this fixture IS a trim: the real library reports 80 ports,
	// projecting to 445 series, against the ~480 planned. That gap is the 36
	// dark ports whose "unknown" speed emits nothing, and it is recorded
	// against this collector's cardinality budget in docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 24 {
		t.Fatalf("GatherAndCount = %d, want 24 (4 ports x 5 + 3 speeds + freshness)", count)
	}
}

// TestFCPortsCollector_UnmappableSpeedEmitsNoSeries covers the two inputs for
// which a speed gauge must not exist at all, rather than reporting 0.
//
// A 0 asserts a link that is up and running at zero bytes per second, which is
// not a state Fibre Channel has; it would also drag the library's mean link
// rate down and, worse, make FCPortSpeedBelowPeers fire on every dark port
// forever instead of on a genuinely renegotiated one.
//
// The first case is the ordinary one on real hardware — 36 of the capture's 80
// ports report the literal "unknown", a value R1.11.2 tabulates nowhere — so it
// is deliberately not logged as a warning. The second is a rate this exporter
// has never heard of (32GFC hardware, say), which is logged.
func TestFCPortsCollector_UnmappableSpeedEmitsNoSeries(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "the undocumented unknown every dark port reports",
			body: `[{"location":"fcPort_F1C4R1P0","driveLocation":"drive_F1C4R1","state":"noLightDetected","speedActual":"unknown"}]`,
		},
		{
			name: "a rate outside the documented set",
			body: `[{"location":"fcPort_F1C4R1P0","driveLocation":"drive_F1C4R1","state":"communicationEstablished","speedActual":"32Gbps"}]`,
		},
		{
			name: "no speed reported at all",
			body: `[{"location":"fcPort_F1C4R1P0","driveLocation":"drive_F1C4R1","state":"unknown"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fcPortsServing(t, tc.body)
			c.refresh(context.Background())

			reg := prometheus.NewRegistry()
			if err := reg.Register(c); err != nil {
				t.Fatalf("Register: %v", err)
			}
			count, err := testutil.GatherAndCount(reg, "tapelibrary_fc_port_speed_bytes_per_second")
			if err != nil {
				t.Fatalf("GatherAndCount: %v", err)
			}
			if count != 0 {
				t.Fatalf("GatherAndCount(speed) = %d, want 0: a 0 would assert a link running at zero bytes per second", count)
			}

			// The port itself is still reported: only the speed series is
			// withheld, never the whole entry.
			stateCount, err := testutil.GatherAndCount(reg, "tapelibrary_fc_port_state")
			if err != nil {
				t.Fatalf("GatherAndCount: %v", err)
			}
			if stateCount != len(fcPortStates) {
				t.Fatalf("GatherAndCount(state) = %d, want %d: withholding the speed must not drop the port",
					stateCount, len(fcPortStates))
			}

			// _info must survive too: it is what FCPortSpeedBelowPeers
			// joins against to find the auto-negotiating ports, so a port
			// dropping out of it drops out of the rule.
			infoCount, err := testutil.GatherAndCount(reg, "tapelibrary_fc_port_info")
			if err != nil {
				t.Fatalf("GatherAndCount: %v", err)
			}
			if infoCount != 1 {
				t.Fatalf("GatherAndCount(info) = %d, want 1: the _info series must be emitted in every state", infoCount)
			}
		})
	}
}

// TestFCPortsCollector_SpeedConversion pins the Gbps-to-bytes-per-second
// mapping for every rate R1.11.2 documents. The conversion is the one place
// this collector transforms a value rather than passing it through, and getting
// the factor wrong by 8 would be invisible in every other test here: the
// stateset would still be right, the series would still exist, and only the
// number would silently be off by an order of magnitude.
func TestFCPortsCollector_SpeedConversion(t *testing.T) {
	for rate, want := range map[string]float64{
		"1Gbps":  125000000,
		"2Gbps":  250000000,
		"4Gbps":  500000000,
		"8Gbps":  1000000000,
		"16Gbps": 2000000000,
	} {
		t.Run(rate, func(t *testing.T) {
			log := logger.NewTextLogger("error")
			c := NewFCPortsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

			got, ok := c.speedBytesPerSecond(&fcPortStats{Location: "fcPort_F1C4R1P0", SpeedActual: rate})
			if !ok {
				t.Fatalf("speedBytesPerSecond(%s) not emitted, want a series", rate)
			}
			if got != want {
				t.Errorf("speedBytesPerSecond(%s) = %v, want %v (the Gbps figure divided by 8, not multiplied)", rate, got, want)
			}
		})
	}
}

// TestFCPortsCollector_UndocumentedStateIsStillEmitted covers the manual's own
// incompleteness, which this endpoint demonstrates twice over: it names states
// in prose that appear in none of its tables, and its own speedActual returns
// an "unknown" tabulated nowhere. A state outside the documented set must
// surface as its own series at 1, rather than leaving all four documented
// series at 0 and making the port look stateless — which would also silently
// satisfy nothing at all, so the signal would be lost rather than merely
// mislabelled.
func TestFCPortsCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	c := fcPortsServing(t, `[{"location":"fcPort_F1C4R1P0","driveLocation":"drive_F1C4R1","state":"someUndocumentedState","speedActual":"unknown"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_fc_port_state Link state of the Fibre Channel port, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_fc_port_state gauge
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="communicationEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="communicationNotEstablished"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="noLightDetected"} 0
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="someUndocumentedState"} 1
tapelibrary_fc_port_state{drive_location="drive_F1C4R1",location="fcPort_F1C4R1P0",state="unknown"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_fc_port_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestFCPortsCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism main.go's
// shutdown seam relies on (see registry.Wait after web.ListenAndServe).
func TestFCPortsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	c.Start(ctx)
	cancel()

	select {
	case <-c.Done():
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close within 2s after cancel: graceful shutdown broken")
	}
}

// TestFCPortsCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval (1
// hour) guarantees the ticker cannot fire again during this test, so any further
// request the server receives could only come from Collect itself calling out,
// which the design forbids.
func TestFCPortsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/fc_ports.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)
	time.Sleep(100 * time.Millisecond) // let Start's immediate refresh land

	afterStart := calls.Load()
	if afterStart == 0 {
		t.Fatal("calls = 0 after Start, want >= 1: Start must run an immediate refresh")
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := testutil.GatherAndCount(reg); err != nil {
			t.Fatalf("GatherAndCount (scrape %d): %v", i, err)
		}
	}

	if got := calls.Load(); got != afterStart {
		t.Fatalf("calls after 3 scrapes = %d, want %d unchanged: Collect must never call the library", got, afterStart)
	}
}

// TestFCPortsCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestFCPortsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
	c.refresh(context.Background())

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("GatherAndCount = %d, want 1 (freshness gauge only): a failed first refresh must cache nothing", count)
	}
}

// TestFCPortsCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh via
// a short interval. The cache from the successful first refresh must survive the
// later failure (fail-open, per refresh's doc comment) rather than being cleared
// or replaced with nothing.
//
// A cleared cache here would drop tapelibrary_fc_port_state entirely, and an
// absent series cannot satisfy FCPortNoLight's `== 1`, so a library that became
// unreachable would silently stop being watched for a port that had gone dark
// rather than alerting.
func TestFCPortsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/fc_ports.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-c.Done()
	}()

	c.Start(ctx)

	// Wait for backend call 3 to start, rather than sleeping a fixed
	// duration: refresh runs synchronously on Start's single goroutine, so
	// the ticker cannot dispatch call 3 until call 2 (the first failing, 500
	// refresh) has fully returned, its error branch included. Seeing calls
	// reach 3 is therefore a deterministic proof that call 2's whole error
	// path, not merely the server's response, has already completed.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("calls = %d after 2s, want >= 3: a failing refresh's error path never completed", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	// A surviving cache emits 24 metrics: the 23 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 24 {
		t.Fatalf("GatherAndCount = %d, want 24: the previous cache must survive a later refresh error", count)
	}
}

// TestFCPortsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately never
// called here). Collect must still emit exactly the freshness gauge, valued 0
// (not a zero time.Time's large-negative Unix()), and StatusTracker must still
// report this collector as successful: "Collect ran and returned data" and "the
// data is fresh" are different questions.
func TestFCPortsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewFCPortsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_fc_ports_last_refresh_timestamp_seconds Unix time of the last successful fc ports refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_fc_ports_last_refresh_timestamp_seconds gauge
tapelibrary_fc_ports_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("fc_ports", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="fc_ports"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
