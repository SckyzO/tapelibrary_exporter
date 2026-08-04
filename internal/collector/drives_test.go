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

// drivesFixtureServer serves testdata/drives.json on every request and returns
// a collector already pointed at it, with --collector.drives.per-volser in the
// state the caller asks for. Nothing is started: the caller decides whether to
// drive refresh directly (deterministic) or via Start (which is what the
// lifecycle tests below exercise).
func drivesFixtureServer(t *testing.T, perVolser bool) (*httptest.Server, *DrivesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/drives.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour, perVolser)
}

// drivesServing returns a collector fed by a one-shot server that answers
// every request with body. Used by the branch tests below, which need input
// shapes the real capture does not contain — an undocumented state, a drive
// that has never been cleaned, an unparseable timestamp — and which must
// therefore not be invented inside testdata/drives.json (that fixture stays
// faithful to the 2026-07-28 capture, where every drive is reachable and every
// drive has been cleaned).
func drivesServing(t *testing.T, body string, perVolser bool) *DrivesCollector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return NewDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour, perVolser)
}

// TestParseDrives exercises parseDrives (piece 2, the pure parser) with a
// static byte fixture: no HTTP, no collector, no logger, no goroutine
// involved.
func TestParseDrives(t *testing.T) {
	data, err := os.ReadFile("testdata/drives.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	drives, err := parseDrives(data)
	if err != nil {
		t.Fatalf("parseDrives: %v", err)
	}

	if len(drives) != 4 {
		t.Fatalf("len(drives) = %d, want 4", len(drives))
	}

	// The first control-path drive, which pins every non-nullable field the
	// collector reads.
	d := drives[0]
	if d.Location != "drive_F1C4R1" {
		t.Errorf("Location = %q, want %q", d.Location, "drive_F1C4R1")
	}
	if d.State != "online" {
		t.Errorf("State = %q, want %q", d.State, "online")
	}
	if d.Accessible != "normal" {
		t.Errorf("Accessible = %q, want %q", d.Accessible, "normal")
	}
	if d.LogicalLibrary != "Library-5" {
		t.Errorf("LogicalLibrary = %q, want %q", d.LogicalLibrary, "Library-5")
	}
	if d.SerialNumber != "SN00000001" {
		t.Errorf("SerialNumber = %q, want %q", d.SerialNumber, "SN00000001")
	}
	if d.Firmware != "5A9D" {
		t.Errorf("Firmware = %q, want %q", d.Firmware, "5A9D")
	}
	if d.MTM != "3592-60F" {
		t.Errorf("MTM = %q, want %q", d.MTM, "3592-60F")
	}
	if d.MediaType != "3592" {
		t.Errorf("MediaType = %q, want %q", d.MediaType, "3592")
	}
	// A control-path drive is how the host talks to the library at all;
	// losing one is materially different from losing a data drive, and
	// nothing else on this endpoint says which is which.
	if d.Use != "controlPath" {
		t.Errorf("Use = %q, want %q", d.Use, "controlPath")
	}
	if d.Encryption != "disabled" {
		t.Errorf("Encryption = %q, want %q", d.Encryption, "disabled")
	}
	if d.Interface != "fibreChannel" {
		t.Errorf("Interface = %q, want %q", d.Interface, "fibreChannel")
	}
	if d.WWNN != "5005076400000001" {
		t.Errorf("WWNN = %q, want %q", d.WWNN, "5005076400000001")
	}

	// The nullable two. A pointer is the whole point on both: an idle drive
	// and a drive holding no cartridge both report null, and a plain string
	// would decode either to "" — which for volser is a barcode nobody can
	// read, and for operation is a state the manual does not define.
	t.Run("a present nullable field decodes to a non-nil pointer", func(t *testing.T) {
		if d.Operation == nil {
			t.Fatal("Operation = nil, want a value: the capture reports empty for this drive")
		}
		if *d.Operation != "empty" {
			t.Errorf("*Operation = %q, want %q", *d.Operation, "empty")
		}
		if d.LastCleaned == nil {
			t.Fatal("LastCleaned = nil, want a value: the capture reports one for this drive")
		}
		if *d.LastCleaned != "2026-07-23T20:43:36+0000" {
			t.Errorf("*LastCleaned = %q, want %q", *d.LastCleaned, "2026-07-23T20:43:36+0000")
		}
	})

	t.Run("an empty drive decodes volser to nil, not to the empty string", func(t *testing.T) {
		if d.Volser != nil {
			t.Errorf("Volser = %q, want nil (an empty drive holds no cartridge)", *d.Volser)
		}
		loaded := drives[1]
		if loaded.Volser == nil {
			t.Fatal("Volser = nil on the loaded drive, want a value")
		}
		if *loaded.Volser != "TST001JD" {
			t.Errorf("*Volser = %q, want %q", *loaded.Volser, "TST001JD")
		}
	})

	// The in-service drive is the only one in the fixture with a null
	// operation, which is the manual's own way of saying "no operation is in
	// progress" rather than missing data.
	t.Run("a null operation decodes to nil and renders as none", func(t *testing.T) {
		inService := drives[3]
		if inService.State != "inServiceMode" {
			t.Fatalf("State = %q, want %q", inService.State, "inServiceMode")
		}
		if inService.Operation != nil {
			t.Errorf("Operation = %q, want nil", *inService.Operation)
		}
		if got := driveOperation(&inService); got != "none" {
			t.Errorf("driveOperation(null) = %q, want %q", got, "none")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseDrives([]byte("not json")); err == nil {
			t.Error("parseDrives(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseDrives([]byte(`{"location":"drive_F1C4R1"}`)); err == nil {
			t.Error("parseDrives(object) returned a nil error, want non-nil")
		}
	})

	// A library with no drive cannot serve any host, so an empty list is a
	// response that lost its content rather than a real inventory. Accepting
	// it would replace a good cache with no drives at all.
	t.Run("empty array is an error, not a library with no drives", func(t *testing.T) {
		if _, err := parseDrives([]byte(`[]`)); err == nil {
			t.Error("parseDrives(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseDrives(nil); err == nil {
			t.Error("parseDrives(nil) returned a nil error, want non-nil")
		}
	})

	t.Run("an entry with no location is an error", func(t *testing.T) {
		if _, err := parseDrives([]byte(`[{"state":"online"}]`)); err == nil {
			t.Error("parseDrives(no location) returned a nil error, want non-nil")
		}
	})

	// Fail closed, in the pure step: two entries at one location would send
	// two metrics sharing a descriptor AND a label set, and Registry.Gather
	// rejects the whole scrape when that happens, taking out every other
	// collector's metrics with it (CONTRIBUTING.md, "Common Pitfalls").
	t.Run("duplicate locations are rejected rather than reaching Collect", func(t *testing.T) {
		dup := `[{"location":"drive_F1C4R1","state":"online"},{"location":"drive_F1C4R1","state":"online"}]`
		if _, err := parseDrives([]byte(dup)); err == nil {
			t.Error("parseDrives(duplicate location) returned a nil error, want non-nil")
		}
	})
}

// TestDrivesCollector_Describe locks the descriptor count at exactly 7 (three
// statesets, the last-cleaned timestamp, the identity series, the gated
// loaded-cartridge series, and the freshness gauge) so a future edit that
// silently adds or drops a metric is caught here rather than downstream in
// docs-check or a dashboard.
//
// Describing all 7 when per-volser is off, so that only 6 of them can emit, is
// deliberate: a descriptor states what this collector CAN emit, not what its
// last refresh happened to contain, and docs/metrics.md documents it on that
// basis.
func TestDrivesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 7 {
		t.Fatalf("Describe sent %d descriptors, want 7", count)
	}
}

// TestDrivesCollector_Collect pins the exact exposition text of every business
// metric against the fixture, at default settings (per-volser off). refresh is
// driven directly rather than through Start, so the assertion is deterministic
// and carries no sleep: Start's own scheduling is what the lifecycle tests
// below cover.
//
// Four things matter beyond the raw numbers. The state stateset emits all 9
// known states per drive, exactly one carrying 1, which keeps the severity
// classification in the alerting rules rather than in the value. operation is
// a separate stateset rather than a label on state: a drive can be online and
// idle or online and mid-unload, and only operation tells those apart.
// logical_library rides on all three statesets, which is what lets the planned
// "too few online drives" rule be a plain count by () rather than a group_left
// join. And tapelibrary_drive_loaded_cartridge_info does not appear at all:
// per-volser is off by default.
func TestDrivesCollector_Collect(t *testing.T) {
	_, c := drivesFixtureServer(t, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_drive_access Whether the accessor can reach this drive, as a stateset: 1 on the active value and 0 on every other known value. A drive can be online and still be unreachable, in which case no cartridge can be mounted in it.
# TYPE tapelibrary_drive_access gauge
tapelibrary_drive_access{access="limited",location="drive_F11C2R3",logical_library="Library-6"} 0
tapelibrary_drive_access{access="limited",location="drive_F1C4R1",logical_library="Library-5"} 0
tapelibrary_drive_access{access="limited",location="drive_F1C4R2",logical_library="Library-5"} 0
tapelibrary_drive_access{access="limited",location="drive_F2C4R3",logical_library="Library-5"} 0
tapelibrary_drive_access{access="no",location="drive_F11C2R3",logical_library="Library-6"} 0
tapelibrary_drive_access{access="no",location="drive_F1C4R1",logical_library="Library-5"} 0
tapelibrary_drive_access{access="no",location="drive_F1C4R2",logical_library="Library-5"} 0
tapelibrary_drive_access{access="no",location="drive_F2C4R3",logical_library="Library-5"} 0
tapelibrary_drive_access{access="normal",location="drive_F11C2R3",logical_library="Library-6"} 1
tapelibrary_drive_access{access="normal",location="drive_F1C4R1",logical_library="Library-5"} 1
tapelibrary_drive_access{access="normal",location="drive_F1C4R2",logical_library="Library-5"} 1
tapelibrary_drive_access{access="normal",location="drive_F2C4R3",logical_library="Library-5"} 1
# HELP tapelibrary_drive_info Drive identity, always 1. Identity strings live here rather than on a measurement series, so a firmware upgrade or a drive swap changes this series alone instead of breaking the continuity of every other. use=controlPath marks the drives the host talks to the library through.
# TYPE tapelibrary_drive_info gauge
tapelibrary_drive_info{encryption="disabled",firmware="5A9D",interface="fibreChannel",location="drive_F11C2R3",logical_library="Library-6",media_type="3592",mtm="3592-60F",serial="SN00000004",use="access",wwnn="5005076400000004"} 1
tapelibrary_drive_info{encryption="disabled",firmware="5A9D",interface="fibreChannel",location="drive_F1C4R1",logical_library="Library-5",media_type="3592",mtm="3592-60F",serial="SN00000001",use="controlPath",wwnn="5005076400000001"} 1
tapelibrary_drive_info{encryption="disabled",firmware="5A9D",interface="fibreChannel",location="drive_F1C4R2",logical_library="Library-5",media_type="3592",mtm="3592-60F",serial="SN00000002",use="controlPath",wwnn="5005076400000002"} 1
tapelibrary_drive_info{encryption="disabled",firmware="5B14",interface="fibreChannel",location="drive_F2C4R3",logical_library="Library-5",media_type="3592",mtm="3592-60F",serial="SN00000003",use="access",wwnn="5005076400000003"} 1
# HELP tapelibrary_drive_last_cleaned_timestamp_seconds Unix time at which this drive was last cleaned. A drive that has never been cleaned reports no series at all, rather than a 0 that would place its last cleaning in 1970.
# TYPE tapelibrary_drive_last_cleaned_timestamp_seconds gauge
tapelibrary_drive_last_cleaned_timestamp_seconds{location="drive_F11C2R3",logical_library="Library-6"} 1.774755476e+09
tapelibrary_drive_last_cleaned_timestamp_seconds{location="drive_F1C4R1",logical_library="Library-5"} 1.784839416e+09
tapelibrary_drive_last_cleaned_timestamp_seconds{location="drive_F1C4R2",logical_library="Library-5"} 1.784983751e+09
tapelibrary_drive_last_cleaned_timestamp_seconds{location="drive_F2C4R3",logical_library="Library-5"} 1.784812158e+09
# HELP tapelibrary_drive_operation Operation the tape drive is currently performing, as a stateset: 1 on the active operation and 0 on every other known operation. Orthogonal to the drive's state, which says whether the drive is healthy rather than what it is doing. The API's null is reported here as none.
# TYPE tapelibrary_drive_operation gauge
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="empty"} 0
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="loading"} 0
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="none"} 1
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="ready"} 0
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="unloaded"} 0
tapelibrary_drive_operation{location="drive_F11C2R3",logical_library="Library-6",operation="unloading"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="empty"} 1
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="loading"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="none"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="ready"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="unloaded"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="unloading"} 0
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="empty"} 0
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="loading"} 0
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="none"} 0
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="ready"} 1
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="unloaded"} 0
tapelibrary_drive_operation{location="drive_F1C4R2",logical_library="Library-5",operation="unloading"} 0
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="empty"} 1
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="loading"} 0
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="none"} 0
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="ready"} 0
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="unloaded"} 0
tapelibrary_drive_operation{location="drive_F2C4R3",logical_library="Library-5",operation="unloading"} 0
# HELP tapelibrary_drive_state Operational state of the tape drive, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_drive_state gauge
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="cleaning"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="inServiceMode"} 1
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="initializing"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="online"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="resetRequired"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="restarting"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="unknown"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="unreachable"} 0
tapelibrary_drive_state{location="drive_F11C2R3",logical_library="Library-6",state="updating"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="cleaning"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="inServiceMode"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="initializing"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="online"} 1
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="resetRequired"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="restarting"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="unknown"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="unreachable"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="updating"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="cleaning"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="inServiceMode"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="initializing"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="online"} 1
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="resetRequired"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="restarting"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="unknown"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="unreachable"} 0
tapelibrary_drive_state{location="drive_F1C4R2",logical_library="Library-5",state="updating"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="cleaning"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="inServiceMode"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="initializing"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="online"} 1
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="resetRequired"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="restarting"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="unknown"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="unreachable"} 0
tapelibrary_drive_state{location="drive_F2C4R3",logical_library="Library-5",state="updating"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_drive_access",
		"tapelibrary_drive_info",
		"tapelibrary_drive_last_cleaned_timestamp_seconds",
		"tapelibrary_drive_loaded_cartridge_info",
		"tapelibrary_drive_operation",
		"tapelibrary_drive_state",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 80 cached metrics (4 drives x (9 states + 6 operations + 3 access + 1
	// last-cleaned + 1 info)) plus the freshness gauge Collect always appends.
	// The same 20 per drive over the capture's real 40 drives is the ~800
	// figure recorded against this collector's cardinality budget in
	// docs/exporter-journal.md.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 81 {
		t.Fatalf("GatherAndCount = %d, want 81 (4 drives x 20 + freshness)", count)
	}
}

// TestDrivesCollector_PerVolserGate covers both sides of
// --collector.drives.per-volser against the same fixture. Off, the
// loaded-cartridge series is absent entirely — not present at 0 — because a
// series that exists is a series Prometheus indexes, and the whole point of
// the flag is that this pairing accumulates index entries far beyond the 40
// that are ever live. On, exactly the two drives that actually hold a
// cartridge report one; the two empty drives still report nothing, so an empty
// drive never claims to hold a tape with an unreadable barcode.
func TestDrivesCollector_PerVolserGate(t *testing.T) {
	t.Run("off by default: no loaded-cartridge series at all", func(t *testing.T) {
		_, c := drivesFixtureServer(t, false)
		c.refresh(context.Background())

		if err := testutil.CollectAndCompare(c, strings.NewReader(""),
			"tapelibrary_drive_loaded_cartridge_info",
		); err != nil {
			t.Fatalf("unexpected collecting result:\n%s", err)
		}
	})

	t.Run("on: one series per loaded drive, none per empty drive", func(t *testing.T) {
		_, c := drivesFixtureServer(t, true)
		c.refresh(context.Background())

		expected := `
# HELP tapelibrary_drive_loaded_cartridge_info The cartridge currently loaded in this drive, always 1. Emitted only when --collector.drives.per-volser is set, and only for drives that actually hold a cartridge: the volser churns, so this pairing accumulates index entries in Prometheus far beyond the 40 that are ever active at once.
# TYPE tapelibrary_drive_loaded_cartridge_info gauge
tapelibrary_drive_loaded_cartridge_info{location="drive_F11C2R3",logical_library="Library-6",volser="TST011JL"} 1
tapelibrary_drive_loaded_cartridge_info{location="drive_F1C4R2",logical_library="Library-5",volser="TST001JD"} 1
`
		if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
			"tapelibrary_drive_loaded_cartridge_info",
		); err != nil {
			t.Fatalf("unexpected collecting result:\n%s", err)
		}

		// 81 as in the default case, plus one series for each of the two
		// loaded drives.
		reg := prometheus.NewRegistry()
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register: %v", err)
		}
		count, err := testutil.GatherAndCount(reg)
		if err != nil {
			t.Fatalf("GatherAndCount: %v", err)
		}
		if count != 83 {
			t.Fatalf("GatherAndCount = %d, want 83 (81 + one per loaded drive)", count)
		}
	})

	// An empty-string volser is not a cartridge, it is a barcode nobody could
	// read. Emitting volser="" would put a series in the index that no
	// operator can trace back to any tape.
	t.Run("on: an empty-string volser emits nothing", func(t *testing.T) {
		c := drivesServing(t, `[{"location":"drive_F1C4R1","state":"online","operation":"empty","accessible":"normal","logicalLibrary":"Library-5","volser":""}]`, true)
		c.refresh(context.Background())

		if err := testutil.CollectAndCompare(c, strings.NewReader(""),
			"tapelibrary_drive_loaded_cartridge_info",
		); err != nil {
			t.Fatalf("unexpected collecting result:\n%s", err)
		}
	})
}

// TestDrivesCollector_UndocumentedValueIsStillEmitted covers the manual's own
// incompleteness on the enumerated fields this collector emits. The state
// tables are demonstrably a floor rather than a ceiling — the manual names
// states in prose and tabulates them nowhere, which is how AccessorsCollector
// ended up carrying failedToInitialize — so a value outside the documented set
// must surface as its own series at 1, rather than leaving every documented
// series at 0 and making the drive look stateless.
//
// Both state and accessible are checked because refresh drives all three
// statesets through one shared helper: a regression there would hit every one
// of them equally.
func TestDrivesCollector_UndocumentedValueIsStillEmitted(t *testing.T) {
	c := drivesServing(t, `[{"location":"drive_F1C4R1","state":"someUndocumentedState","operation":"empty","accessible":"blocked","logicalLibrary":"Library-5","volser":null,"lastCleaned":null}]`, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_drive_access Whether the accessor can reach this drive, as a stateset: 1 on the active value and 0 on every other known value. A drive can be online and still be unreachable, in which case no cartridge can be mounted in it.
# TYPE tapelibrary_drive_access gauge
tapelibrary_drive_access{access="blocked",location="drive_F1C4R1",logical_library="Library-5"} 1
tapelibrary_drive_access{access="limited",location="drive_F1C4R1",logical_library="Library-5"} 0
tapelibrary_drive_access{access="no",location="drive_F1C4R1",logical_library="Library-5"} 0
tapelibrary_drive_access{access="normal",location="drive_F1C4R1",logical_library="Library-5"} 0
# HELP tapelibrary_drive_state Operational state of the tape drive, as a stateset: 1 on the active state and 0 on every other known state.
# TYPE tapelibrary_drive_state gauge
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="cleaning"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="inServiceMode"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="initializing"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="online"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="resetRequired"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="restarting"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="someUndocumentedState"} 1
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="unknown"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="unreachable"} 0
tapelibrary_drive_state{location="drive_F1C4R1",logical_library="Library-5",state="updating"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_drive_access",
		"tapelibrary_drive_state",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDrivesCollector_NullOperationIsReportedAsNone pins the one place in this
// exporter where a JSON null becomes a series rather than suppressing one. On
// this field null is a documented VALUE — the manual's "no operation is in
// progress" — rather than the absence of a reading, so an idle drive must name
// itself idle instead of leaving all six operation series at 0 and looking
// like a drive whose operation nobody can determine.
//
// The empty string takes the same branch: the manual documents no such value,
// and a stateset with every member at 0 says less than one that names the
// drive idle. The fixture covers null; only "" needs its own input here.
func TestDrivesCollector_NullOperationIsReportedAsNone(t *testing.T) {
	c := drivesServing(t, `[{"location":"drive_F1C4R1","state":"online","operation":"","accessible":"normal","logicalLibrary":"Library-5","volser":null,"lastCleaned":null}]`, false)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_drive_operation Operation the tape drive is currently performing, as a stateset: 1 on the active operation and 0 on every other known operation. Orthogonal to the drive's state, which says whether the drive is healthy rather than what it is doing. The API's null is reported here as none.
# TYPE tapelibrary_drive_operation gauge
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="empty"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="loading"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="none"} 1
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="ready"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="unloaded"} 0
tapelibrary_drive_operation{location="drive_F1C4R1",logical_library="Library-5",operation="unloading"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_drive_operation",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}

// TestDrivesCollector_LastCleanedEmitsNoSeriesWhenAbsent pins the other half
// of the nullable story: a timestamp the library cannot report must produce no
// series at all rather than a 0. A 0 here places the last cleaning at the Unix
// epoch, which is not merely wrong but actively misleading — every "cleaned
// within the last N days" query would silently answer about the wrong drives,
// and a dashboard would show a drive overdue for cleaning by 56 years.
//
// The unparseable case takes the same branch rather than being guessed at, and
// is logged: a response this collector does not understand is a fact worth
// surfacing, not one worth interpolating.
func TestDrivesCollector_LastCleanedEmitsNoSeriesWhenAbsent(t *testing.T) {
	body := `[
		{"location":"drive_F1C4R1","state":"online","operation":"empty","accessible":"normal","logicalLibrary":"Library-5","volser":null,"lastCleaned":null},
		{"location":"drive_F1C4R2","state":"online","operation":"empty","accessible":"normal","logicalLibrary":"Library-5","volser":null,"lastCleaned":"not a timestamp"},
		{"location":"drive_F1C4R3","state":"online","operation":"empty","accessible":"normal","logicalLibrary":"Library-5","volser":null,"lastCleaned":"2026-07-23T20:43:36+0000"}
	]`
	c := drivesServing(t, body, false)
	c.refresh(context.Background())

	// Only the drive with a parseable timestamp reports one.
	expected := `
# HELP tapelibrary_drive_last_cleaned_timestamp_seconds Unix time at which this drive was last cleaned. A drive that has never been cleaned reports no series at all, rather than a 0 that would place its last cleaning in 1970.
# TYPE tapelibrary_drive_last_cleaned_timestamp_seconds gauge
tapelibrary_drive_last_cleaned_timestamp_seconds{location="drive_F1C4R3",logical_library="Library-5"} 1.784839416e+09
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_drive_last_cleaned_timestamp_seconds",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 3 drives x (9 states + 6 operations + 3 access + 1 info) = 57, plus the
	// one last-cleaned series and the freshness gauge. Pinned so that a future
	// change emitting a 0 for either missing timestamp is caught here even if
	// it somehow satisfied the comparison above.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 59 {
		t.Fatalf("GatherAndCount = %d, want 59 (only one drive reports a last-cleaned timestamp)", count)
	}
}

// TestDrivesCollector_DoneClosesOnCancel verifies the Done() channel closes
// when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestDrivesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
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

// TestDrivesCollector_CollectServesCacheWithoutIO proves Collect never calls
// the library: after Start's own immediate refresh completes, a long interval
// (1 hour) guarantees the ticker cannot fire again during this test, so any
// further request the server receives could only come from Collect itself
// calling out, which the design forbids.
func TestDrivesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/drives.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDrivesCollector_ErrorHandling drives a refresh against a library that
// only ever fails. No cache was ever filled, so the scrape must carry exactly
// the freshness gauge, and must neither panic nor emit a partial stateset.
func TestDrivesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour, false)
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

// TestDrivesCollector_ErrorKeepsPreviousCache scripts the backend to succeed
// once, then fail on every later call, and drives at least one more refresh
// via a short interval. The cache from the successful first refresh must
// survive the later failure (fail-open, per refresh's doc comment) rather than
// being cleared or replaced with nothing.
func TestDrivesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/drives.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond, false)
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
	// A surviving cache emits 81 metrics: the 80 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 81 {
		t.Fatalf("GatherAndCount = %d, want 81: the previous cache must survive a later refresh error", count)
	}
}

// TestDrivesCollector_StatusTrackerSuccessOnFirstScrape covers the startup
// window before Start's first refresh has completed (Start is deliberately
// never called here). Collect must still emit exactly the freshness gauge,
// valued 0 (not a zero time.Time's large-negative Unix()), and StatusTracker
// must still report this collector as successful: "Collect ran and returned
// data" and "the data is fresh" are different questions.
func TestDrivesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour, false)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_drives_last_refresh_timestamp_seconds Unix time of the last successful drives refresh. Alert if time() - this > 2 x the collector's configured interval.
# TYPE tapelibrary_drives_last_refresh_timestamp_seconds gauge
tapelibrary_drives_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("drives", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="drives"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
