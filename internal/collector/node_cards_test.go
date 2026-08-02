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

// nodeCardsFixtureServer serves testdata/node_cards.json on every request and
// returns a collector already pointed at it. Nothing is started: the caller
// decides whether to drive refresh directly (deterministic) or via Start
// (which is what the lifecycle tests below exercise).
func nodeCardsFixtureServer(t *testing.T) (*httptest.Server, *NodeCardsCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/node_cards.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewNodeCardsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// nodeCardsServing returns a collector fed by a server that answers every
// request with body. Used by the branch tests below, which need input shapes
// the real capture does not contain and which must therefore not be invented
// inside testdata/node_cards.json (that fixture stays faithful to the
// 2026-07-28 capture, in which every card is either online or unknown).
func nodeCardsServing(t *testing.T, body string) *NodeCardsCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewNodeCardsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseNodeCards exercises parseNodeCards (piece 2, the pure parser) with
// a static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseNodeCards(t *testing.T) {
	data, err := os.ReadFile("testdata/node_cards.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	cards, err := parseNodeCards(data)
	if err != nil {
		t.Fatalf("parseNodeCards: %v", err)
	}

	// Eight cards: four LCCs (one per frame that has one) and two cards on
	// each of the two accessors. The capture's full set, not a trim.
	if len(cards) != 8 {
		t.Fatalf("len(cards) = %d, want 8", len(cards))
	}

	first := cards[0]
	if first.Location != "accessor_Aa" {
		t.Errorf("Location = %q, want %q", first.Location, "accessor_Aa")
	}
	if first.Type != "MDA" {
		t.Errorf("Type = %q, want %q", first.Type, "MDA")
	}
	if first.State != "unknown" {
		t.Errorf("State = %q, want %q", first.State, "unknown")
	}

	// The reason every metric on this collector is keyed by location AND type
	// rather than by location alone. If a future fixture edit collapsed the
	// two cards on each accessor into one, the duplicate-key protection below
	// would still pass while silently no longer testing anything.
	t.Run("location alone is not a unique key on this endpoint", func(t *testing.T) {
		perLocation := map[string]int{}
		for _, n := range cards {
			perLocation[n.Location]++
		}
		shared := 0
		for _, n := range perLocation {
			if n > 1 {
				shared++
			}
		}
		if shared == 0 {
			t.Error("no location carries more than one card: the fixture no longer exercises the composite key")
		}
	})

	// Null is "this card is not an LCC", which is a different fact from "no".
	// The accessor cards must decode to nil pointers, not to empty strings.
	t.Run("non-LCC cards report the role fields as null", func(t *testing.T) {
		if first.PrimaryLCC != nil {
			t.Errorf("PrimaryLCC = %q, want nil on an MDA card", *first.PrimaryLCC)
		}
		if first.ReportingLCC != nil {
			t.Errorf("ReportingLCC = %q, want nil on an MDA card", *first.ReportingLCC)
		}
		if first.LastRestart != nil {
			t.Errorf("LastRestart = %q, want nil on this card", *first.LastRestart)
		}
	})

	// Exactly one LCC holds each role in the capture. Both rules the business
	// alerts read depend on this being true of a healthy library.
	t.Run("exactly one primary and one reporting LCC", func(t *testing.T) {
		primary, reporting := 0, 0
		for _, n := range cards {
			if n.PrimaryLCC != nil && *n.PrimaryLCC == "yes" {
				primary++
			}
			if n.ReportingLCC != nil && *n.ReportingLCC == "yes" {
				reporting++
			}
		}
		if primary != 1 {
			t.Errorf("primary LCC count = %d, want 1", primary)
		}
		if reporting != 1 {
			t.Errorf("reporting LCC count = %d, want 1", reporting)
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseNodeCards([]byte("not json")); err == nil {
			t.Error("parseNodeCards(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseNodeCards([]byte(`{"location":"frame_F1"}`)); err == nil {
			t.Error("parseNodeCards(object) returned a nil error, want non-nil")
		}
	})

	// Every TS4500 runs on at least one LCC, and an unreachable one is
	// reported as an `unknown` card rather than by omission, so an empty list
	// is a response that lost its content. Accepting it would replace a good
	// cache with nothing exactly when the hardware most needs watching.
	t.Run("empty array is an error, not a library with no node cards", func(t *testing.T) {
		if _, err := parseNodeCards([]byte(`[]`)); err == nil {
			t.Error("parseNodeCards(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseNodeCards(nil); err == nil {
			t.Error("parseNodeCards(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseNodeCards([]byte(`[{"type":"LCC","state":"online"}]`)); err == nil {
			t.Error("parseNodeCards(no location) returned a nil error, want non-nil")
		}
	})

	// Type is half the key here, unlike on every earlier collector, so an
	// empty one is rejected for the same reason an empty location is.
	t.Run("an entry with no type is an error", func(t *testing.T) {
		if _, err := parseNodeCards([]byte(`[{"location":"frame_F1","state":"online"}]`)); err == nil {
			t.Error("parseNodeCards(no type) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries sharing location AND type
	// would send two metrics sharing a descriptor AND a label set, and
	// Registry.Gather rejects the whole scrape when that happens, taking out
	// every other collector's metrics with it (CONTRIBUTING.md, "Common
	// Pitfalls").
	t.Run("a duplicate location and type pair is rejected", func(t *testing.T) {
		dup := `[{"type":"LCC","location":"frame_F1","state":"online"},{"type":"LCC","location":"frame_F1","state":"online"}]`
		if _, err := parseNodeCards([]byte(dup)); err == nil {
			t.Error("parseNodeCards(duplicate location+type) returned a nil error, want non-nil")
		}
	})

	// The other half of that rule, and the one that matters on real hardware:
	// two cards at one location differing in type are LEGAL and must survive
	// the parser. A key on location alone would reject the real capture.
	t.Run("one location with two card types is accepted", func(t *testing.T) {
		ok := `[{"type":"MDA","location":"accessor_Aa","state":"online"},{"type":"ACC","location":"accessor_Aa","state":"online"}]`
		got, err := parseNodeCards([]byte(ok))
		if err != nil {
			t.Fatalf("parseNodeCards(two types at one location) = %v, want nil: this is the real hardware's shape", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})
}

// TestNodeCardsCollector_Describe locks the descriptor count at exactly 6 (the
// stateset, the info series, the last-restart timestamp, the two LCC role
// gauges and the freshness gauge) so a future edit that silently adds or drops
// a metric is caught here rather than downstream in docs-check or a dashboard.
func TestNodeCardsCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 20)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 6 {
		t.Fatalf("Describe sent %d descriptors, want 6", count)
	}
}

// TestNodeCardsCollector_Collect pins the exact exposition text against the
// fixture. refresh is driven directly rather than through Start, so the
// assertion is deterministic and carries no sleep: Start's own scheduling is
// what the lifecycle tests below cover.
//
// All seven documented states are emitted per card, exactly one carrying 1,
// which is what keeps the severity classification in the alerting rules rather
// than in the value.
func TestNodeCardsCollector_Collect(t *testing.T) {
	_, c := nodeCardsFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_node_card_state Operational state of the node card, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_node_card_state gauge
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="noCAN"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="online"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="restarting"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="unknown"} 1
tapelibrary_node_card_state{card_type="ACC",location="accessor_Aa",state="unreachable"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="noCAN"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="online"} 1
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="restarting"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="unknown"} 0
tapelibrary_node_card_state{card_type="ACC",location="accessor_Ab",state="unreachable"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="online"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unreachable"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="online"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F11",state="unreachable"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="online"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F12",state="unreachable"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="online"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="unreachable"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="noCAN"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="online"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="restarting"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="unknown"} 1
tapelibrary_node_card_state{card_type="MDA",location="accessor_Aa",state="unreachable"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="noCAN"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="online"} 1
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="restarting"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="unknown"} 0
tapelibrary_node_card_state{card_type="MDA",location="accessor_Ab",state="unreachable"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_node_card_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// The identity series, and the two role gauges that only the LCCs report.
	// Emitted for every card regardless of state, so a card that has gone
	// `unknown` can still be identified by serial and part number.
	expectedIdentity := `
# HELP tapelibrary_node_card_info Node card identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade or a card swap changes this series alone instead of breaking the continuity of every other.
# TYPE tapelibrary_node_card_info gauge
tapelibrary_node_card_info{card_type="ACC",firmware="0073",location="accessor_Aa",part_number="46X5961",serial="SN00000003"} 1
tapelibrary_node_card_info{card_type="ACC",firmware="0073",location="accessor_Ab",part_number="02XX591",serial="SN00000004"} 1
tapelibrary_node_card_info{card_type="LCC",firmware="1.11.0.2-C00",location="frame_F1",part_number="02XW913",serial="SN00000005"} 1
tapelibrary_node_card_info{card_type="LCC",firmware="1.11.0.2-C00",location="frame_F11",part_number="02XW913",serial="SN00000007"} 1
tapelibrary_node_card_info{card_type="LCC",firmware="1.11.0.2-C00",location="frame_F12",part_number="38L6434",serial="SN00000008"} 1
tapelibrary_node_card_info{card_type="LCC",firmware="1.11.0.2-C00",location="frame_F2",part_number="02XW913",serial="SN00000006"} 1
tapelibrary_node_card_info{card_type="MDA",firmware="0072",location="accessor_Aa",part_number="95P8618",serial="SN00000001"} 1
tapelibrary_node_card_info{card_type="MDA",firmware="0072",location="accessor_Ab",part_number="38L7590",serial="SN00000002"} 1
# HELP tapelibrary_node_card_last_restart_timestamp_seconds Unix time at which this node card last restarted. A card the library reports no restart for produces no series at all, rather than a 0 that would place its last restart in 1970.
# TYPE tapelibrary_node_card_last_restart_timestamp_seconds gauge
tapelibrary_node_card_last_restart_timestamp_seconds{card_type="LCC",location="frame_F1"} 1.770385936e+09
tapelibrary_node_card_last_restart_timestamp_seconds{card_type="LCC",location="frame_F11"} 1.770385936e+09
tapelibrary_node_card_last_restart_timestamp_seconds{card_type="LCC",location="frame_F12"} 1.770385936e+09
tapelibrary_node_card_last_restart_timestamp_seconds{card_type="LCC",location="frame_F2"} 1.770630245e+09
# HELP tapelibrary_node_card_primary_lcc Whether this card is the library's primary LCC (1) or not (0). Emitted only for cards that report the role at all, which means the LCC cards: an accessor's MDA or ACC card produces no series rather than a 0 asserting it lost an election it never stood in.
# TYPE tapelibrary_node_card_primary_lcc gauge
tapelibrary_node_card_primary_lcc{card_type="LCC",location="frame_F1"} 1
tapelibrary_node_card_primary_lcc{card_type="LCC",location="frame_F11"} 0
tapelibrary_node_card_primary_lcc{card_type="LCC",location="frame_F12"} 0
tapelibrary_node_card_primary_lcc{card_type="LCC",location="frame_F2"} 0
# HELP tapelibrary_node_card_reporting_lcc Whether this card is the LCC currently answering for the library (1) or not (0). Orthogonal to the primary role: a failover moves this one first. Emitted only for cards that report the role at all.
# TYPE tapelibrary_node_card_reporting_lcc gauge
tapelibrary_node_card_reporting_lcc{card_type="LCC",location="frame_F1"} 0
tapelibrary_node_card_reporting_lcc{card_type="LCC",location="frame_F11"} 0
tapelibrary_node_card_reporting_lcc{card_type="LCC",location="frame_F12"} 0
tapelibrary_node_card_reporting_lcc{card_type="LCC",location="frame_F2"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expectedIdentity),
		"tapelibrary_node_card_info",
		"tapelibrary_node_card_last_restart_timestamp_seconds",
		"tapelibrary_node_card_primary_lcc",
		"tapelibrary_node_card_reporting_lcc",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 76 cached metrics — 8 cards x 7 states, 8 info series, and 4 each of the
	// last-restart timestamp and the two role gauges (the LCCs only) — plus the
	// freshness gauge Collect always appends. Recorded as the observed figure
	// against this collector's cardinality budget in docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 77 {
		t.Fatalf("GatherAndCount = %d, want 77 (8x7 states + 8 info + 3x4 LCC-only + freshness)", count)
	}
}

// TestNodeCardsCollector_DegradedCard covers the states the business alert
// reads. The 2026-07-28 capture shows no card in any of them, so the fixture
// cannot exercise this path and a synthetic body must: without it, the
// transitions this collector exists to detect would be untested.
func TestNodeCardsCollector_DegradedCard(t *testing.T) {
	c := nodeCardsServing(t, `[{"type":"LCC","location":"frame_F1","state":"unreachable"},{"type":"LCC","location":"frame_F2","state":"noCAN"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_node_card_state Operational state of the node card, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_node_card_state gauge
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="online"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unreachable"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="noCAN"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="online"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F2",state="unreachable"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_node_card_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestNodeCardsCollector_UndocumentedStateIsStillEmitted covers the manual's
// own incompleteness, and on this collector it covers a named, live
// disagreement rather than a hypothetical one: the legacy check scripts under
// samples/legacy/ expect a "normal" healthy state that R1.11.2 does not
// document (docs/exporter-journal.md, "Open questions / assumptions"). If any
// library ever reports it, it must surface as its own series at 1 rather than
// leaving all seven documented series at 0 and making the card look stateless.
func TestNodeCardsCollector_UndocumentedStateIsStillEmitted(t *testing.T) {
	c := nodeCardsServing(t, `[{"type":"LCC","location":"frame_F1","state":"normal"}]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_node_card_state Operational state of the node card, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_node_card_state gauge
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="inServiceMode"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noCAN"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="noEthernet"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="normal"} 1
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="online"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="restarting"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unknown"} 0
tapelibrary_node_card_state{card_type="LCC",location="frame_F1",state="unreachable"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "tapelibrary_node_card_state"); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestNodeCardsCollector_NullRoleFieldsEmitNoSeries proves the distinction the
// nullable role fields exist to preserve: a card that is not an LCC reports no
// role series at all, rather than a 0 that would put every MDA and ACC card
// into the denominator of an "exactly one primary LCC" query.
//
// An unparseable value takes the same branch, and is logged rather than guessed
// at in either direction — guessing 0 would hide a lost primary, guessing 1
// would invent a second one.
func TestNodeCardsCollector_NullRoleFieldsEmitNoSeries(t *testing.T) {
	c := nodeCardsServing(t, `[
		{"type":"MDA","location":"accessor_Aa","state":"online","primaryLCC":null,"reportingLCC":null},
		{"type":"ACC","location":"accessor_Aa","state":"online","primaryLCC":"maybe","reportingLCC":""},
		{"type":"LCC","location":"frame_F1","state":"online","primaryLCC":"yes","reportingLCC":"no"}
	]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_node_card_primary_lcc Whether this card is the library's primary LCC (1) or not (0). Emitted only for cards that report the role at all, which means the LCC cards: an accessor's MDA or ACC card produces no series rather than a 0 asserting it lost an election it never stood in.
# TYPE tapelibrary_node_card_primary_lcc gauge
tapelibrary_node_card_primary_lcc{card_type="LCC",location="frame_F1"} 1
# HELP tapelibrary_node_card_reporting_lcc Whether this card is the LCC currently answering for the library (1) or not (0). Orthogonal to the primary role: a failover moves this one first. Emitted only for cards that report the role at all.
# TYPE tapelibrary_node_card_reporting_lcc gauge
tapelibrary_node_card_reporting_lcc{card_type="LCC",location="frame_F1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_node_card_primary_lcc",
		"tapelibrary_node_card_reporting_lcc",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestNodeCardsCollector_UnparseableRestartEmitsNoSeries covers the other
// nullable field's failure branch. A timestamp this collector cannot read must
// produce no series rather than a 0: a restart placed in 1970 would make every
// "restarted within the last N days" query quietly answer about the wrong
// cards, which is worse than the question going unanswered.
func TestNodeCardsCollector_UnparseableRestartEmitsNoSeries(t *testing.T) {
	c := nodeCardsServing(t, `[
		{"type":"LCC","location":"frame_F1","state":"online","lastRestart":"not a timestamp"},
		{"type":"LCC","location":"frame_F2","state":"online","lastRestart":"2026-02-09T09:44:05+0000"}
	]`)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_node_card_last_restart_timestamp_seconds Unix time at which this node card last restarted. A card the library reports no restart for produces no series at all, rather than a 0 that would place its last restart in 1970.
# TYPE tapelibrary_node_card_last_restart_timestamp_seconds gauge
tapelibrary_node_card_last_restart_timestamp_seconds{card_type="LCC",location="frame_F2"} 1.770630245e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_node_card_last_restart_timestamp_seconds",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestNodeCardsCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestNodeCardsCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestNodeCardsCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval
// (1 hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestNodeCardsCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/node_cards.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestNodeCardsCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestNodeCardsCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestNodeCardsCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh via
// a short interval. The cache from the successful first refresh must survive
// the later failure (fail-open, per refresh's doc comment) rather than being
// cleared or replaced with nothing.
//
// A cleared cache would drop tapelibrary_node_card_state{state="online"}
// entirely, and an absent series does not satisfy the NodeCardDegraded rule's
// matchers, so a library that became unreachable would silently stop being
// watched rather than alerting.
func TestNodeCardsCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/node_cards.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 77 metrics: the 76 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 77 {
		t.Fatalf("GatherAndCount = %d, want 77: the previous cache must survive a later refresh error", count)
	}
}

// TestNodeCardsCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestNodeCardsCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewNodeCardsCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_node_cards_last_refresh_timestamp_seconds Unix time of the last successful node cards refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_node_cards_last_refresh_timestamp_seconds gauge
tapelibrary_node_cards_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("node_cards", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="node_cards"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
