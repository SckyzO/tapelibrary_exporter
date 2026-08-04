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

// reportsDrivesFixtureServer serves testdata/reports_drives.json on every
// request and returns a collector already pointed at it. Nothing is started:
// the caller decides whether to drive refresh directly (deterministic) or via
// Start (which is what the lifecycle tests below exercise).
func reportsDrivesFixtureServer(t *testing.T) (*httptest.Server, *ReportsDrivesCollector) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/reports_drives.json")
	}))
	t.Cleanup(srv.Close)

	log := logger.NewTextLogger("error")
	return srv, NewReportsDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
}

// TestParseReportsDrives exercises parseReportsDrives (piece 2, the pure
// parser) with a static byte fixture: no HTTP, no collector, no logger, no
// goroutine involved.
//
// The fixture carries three drives x three hourly windows, and the assertions
// below pin each drive's newest one (09:05). Getting drive_F1C4R1's 08:05
// figures (0 mounts, 3795 corrected read errors) would mean the selection
// picked a window that is an hour stale.
func TestParseReportsDrives(t *testing.T) {
	data, err := os.ReadFile("testdata/reports_drives.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	drives, err := parseReportsDrives(data)
	if err != nil {
		t.Fatalf("parseReportsDrives: %v", err)
	}

	if got, want := len(drives), 3; got != want {
		t.Fatalf("len(drives) = %d, want %d: one selected window per drive", got, want)
	}

	// Sorted by location, which is what parseReportsDrives promises so the
	// cached slice is deterministic for a given response.
	if got, want := drives[0].window.Location, "drive_F11C1R3"; got != want {
		t.Errorf("drives[0].Location = %q, want %q: the result must be sorted by location", got, want)
	}
	if got, want := drives[2].window.Location, "drive_F1C4R1"; got != want {
		t.Errorf("drives[2].Location = %q, want %q: the result must be sorted by location", got, want)
	}

	byLocation := make(map[string]reportsDriveStats, len(drives))
	for _, d := range drives {
		byLocation[d.window.Location] = d
	}

	// Every drive's newest window is the same 09:05 hour in this capture, but
	// that is a property of the data, not something the parser may assume:
	// the per-drive selection is what the mixed-timestamp sub-test below pins.
	for loc, d := range byLocation {
		if got, want := d.at.Unix(), int64(1785229500); got != want {
			t.Errorf("%s: at.Unix() = %d, want %d (the 09:05 window)", loc, got, want)
		}
		if d.window.Duration != 3600 {
			t.Errorf("%s: Duration = %v, want 3600", loc, d.window.Duration)
		}
		if d.window.Cleans != 0 {
			t.Errorf("%s: Cleans = %v, want 0", loc, d.window.Cleans)
		}
	}

	a := byLocation["drive_F1C4R1"].window
	if a.Mounts != 2 {
		t.Errorf("drive_F1C4R1: Mounts = %v, want 2 (the 09:05 window, not 08:05's 0)", a.Mounts)
	}
	if a.DataReadByHosts != 154 {
		t.Errorf("drive_F1C4R1: DataReadByHosts = %v, want 154", a.DataReadByHosts)
	}
	if a.ErrorsCorrectedRead != 252 {
		t.Errorf("drive_F1C4R1: ErrorsCorrectedRead = %v, want 252 (the 09:05 window, not 08:05's 3795)", a.ErrorsCorrectedRead)
	}
	if a.ErrorsCorrectedWrite != 0 {
		t.Errorf("drive_F1C4R1: ErrorsCorrectedWrite = %v, want 0", a.ErrorsCorrectedWrite)
	}
	if a.ErrorsUncorrected != 0 {
		t.Errorf("drive_F1C4R1: ErrorsUncorrected = %v, want 0", a.ErrorsUncorrected)
	}
	if a.TemperatureAverage == nil || *a.TemperatureAverage != 24.0 {
		t.Errorf("drive_F1C4R1: TemperatureAverage = %v, want 24", a.TemperatureAverage)
	}
	if a.TemperatureMax == nil || *a.TemperatureMax != 25.0 {
		t.Errorf("drive_F1C4R1: TemperatureMax = %v, want 25", a.TemperatureMax)
	}
	if a.HumidityAverage == nil || *a.HumidityAverage != 33.0 {
		t.Errorf("drive_F1C4R1: HumidityAverage = %v, want 33", a.HumidityAverage)
	}

	// The one drive in the fixture reporting an uncorrected error in its
	// newest window: the value DriveReportUncorrectedErrors reads.
	c := byLocation["drive_F11C3R4"].window
	if c.ErrorsUncorrected != 1 {
		t.Errorf("drive_F11C3R4: ErrorsUncorrected = %v, want 1", c.ErrorsUncorrected)
	}
	if c.ErrorsCorrectedWrite != 2 {
		t.Errorf("drive_F11C3R4: ErrorsCorrectedWrite = %v, want 2", c.ErrorsCorrectedWrite)
	}
	// Its 07:05 window carries 65535 corrected write errors, which is a
	// saturated 16-bit counter rather than a reading. Selecting the newest
	// window is what keeps it off the wire here; nothing in this collector
	// detects saturation, and nothing should pretend to.
	if c.ErrorsCorrectedWrite == 65535 {
		t.Error("drive_F11C3R4: selection returned the 07:05 window, whose 65535 is a saturated counter")
	}

	// R1.11.2 documents no ordering for this endpoint. A parser that took the
	// first entry per drive would pass every assertion above while being one
	// firmware release away from reporting week-old activity as current.
	t.Run("the newest window wins per drive regardless of array order", func(t *testing.T) {
		shuffled := []byte(`[
			{"location":"drive_F1C4R1","time":"2026-07-28T06:05:00+0000","mounts":29},
			{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","mounts":36},
			{"location":"drive_F1C4R1","time":"2026-07-28T07:05:00+0000","mounts":34}
		]`)
		drives, err := parseReportsDrives(shuffled)
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		if len(drives) != 1 {
			t.Fatalf("len(drives) = %d, want 1", len(drives))
		}
		if drives[0].window.Mounts != 36 {
			t.Errorf("Mounts = %v, want 36: selection must be by timestamp, not by position", drives[0].window.Mounts)
		}
	})

	// The selection is per drive, not library-wide. A drive that went offline
	// part-way through the week keeps its own last reported hour rather than
	// being dropped because a healthier sibling reported more recently.
	t.Run("each drive keeps its own newest window", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","mounts":36},
			{"location":"drive_F1C4R1","time":"2026-07-28T08:05:00+0000","mounts":12},
			{"location":"drive_F2C4R1","time":"2026-07-28T06:05:00+0000","mounts":7}
		]`)
		drives, err := parseReportsDrives(mixed)
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		if len(drives) != 2 {
			t.Fatalf("len(drives) = %d, want 2", len(drives))
		}
		byLoc := map[string]reportsDriveStats{}
		for _, d := range drives {
			byLoc[d.window.Location] = d
		}
		if got := byLoc["drive_F1C4R1"].window.Mounts; got != 36 {
			t.Errorf("drive_F1C4R1: Mounts = %v, want 36", got)
		}
		if got := byLoc["drive_F2C4R1"].window.Mounts; got != 7 {
			t.Errorf("drive_F2C4R1: Mounts = %v, want 7: a drive whose newest window is older must keep it", got)
		}
		if got, want := byLoc["drive_F2C4R1"].at.Unix(), int64(1785218700); got != want {
			t.Errorf("drive_F2C4R1: at.Unix() = %d, want %d (its own 06:05 window)", got, want)
		}
	})

	// The wire format's zone offset carries no colon, so a parser reaching
	// for time.RFC3339 rejects every window the library ever sends.
	t.Run("the offset carries no colon and must still parse", func(t *testing.T) {
		drives, err := parseReportsDrives([]byte(`[{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","mounts":36}]`))
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		if got, want := drives[0].at.Unix(), int64(1785229500); got != want {
			t.Errorf("at.Unix() = %d, want %d", got, want)
		}
	})

	// One unusable hour out of a week must not discard the other 167 for
	// every drive, so an entry missing a timestamp or a location is skipped
	// rather than failing the whole response. Here the only parseable window
	// for the drive is also its oldest.
	t.Run("an entry with an unparseable timestamp is skipped, not ranked", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"drive_F1C4R1","time":"not a timestamp","mounts":99},
			{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00","mounts":98},
			{"location":"drive_F1C4R1","time":"2026-07-28T06:05:00+0000","mounts":29}
		]`)
		drives, err := parseReportsDrives(mixed)
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		if len(drives) != 1 {
			t.Fatalf("len(drives) = %d, want 1", len(drives))
		}
		if drives[0].window.Mounts != 29 {
			t.Errorf("Mounts = %v, want 29: only a parseable window may be selected", drives[0].window.Mounts)
		}
	})

	// A series labelled location="" traces back to no hardware, so the entry
	// is skipped. Its siblings still ship.
	t.Run("an entry with an empty location is skipped, not emitted", func(t *testing.T) {
		mixed := []byte(`[
			{"location":"","time":"2026-07-28T09:05:00+0000","mounts":99},
			{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","mounts":36}
		]`)
		drives, err := parseReportsDrives(mixed)
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		if len(drives) != 1 {
			t.Fatalf("len(drives) = %d, want 1: an entry with no location must not become a series", len(drives))
		}
		if drives[0].window.Location != "drive_F1C4R1" {
			t.Errorf("Location = %q, want drive_F1C4R1", drives[0].window.Location)
		}
	})

	// Rejected rather than served with a fabricated timestamp: refresh then
	// keeps the previous good cache, and the per-drive window timestamps stay
	// honest.
	t.Run("nothing selectable anywhere is an error", func(t *testing.T) {
		if _, err := parseReportsDrives([]byte(`[{"location":"","time":""},{"location":"drive_F1C4R1","time":"nope"}]`)); err == nil {
			t.Error("parseReportsDrives(nothing selectable) returned a nil error, want non-nil")
		}
	})

	t.Run("malformed input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsDrives([]byte("not json")); err == nil {
			t.Error("parseReportsDrives(malformed) returned a nil error, want non-nil")
		}
	})

	t.Run("an object rather than the documented array is an error", func(t *testing.T) {
		if _, err := parseReportsDrives([]byte(`{"mounts":36}`)); err == nil {
			t.Error("parseReportsDrives(object) returned a nil error, want non-nil")
		}
	})

	// An empty array must not decode to no drives at all: a library with no
	// drive cannot serve any host, and accepting it would silently replace a
	// good cache with nothing.
	t.Run("empty array is an error, not an empty fleet", func(t *testing.T) {
		if _, err := parseReportsDrives([]byte(`[]`)); err == nil {
			t.Error("parseReportsDrives(empty array) returned a nil error, want non-nil")
		}
	})

	t.Run("empty input yields an error, not a panic", func(t *testing.T) {
		if _, err := parseReportsDrives(nil); err == nil {
			t.Error("parseReportsDrives(nil) returned a nil error, want non-nil")
		}
	})

	// Absent environmental readings must survive parsing as nil rather than
	// decoding to 0: refresh emits no series for them, and the operating
	// envelope rules would otherwise page on a freezing, bone dry drive.
	t.Run("null environmental readings parse as nil, not zero", func(t *testing.T) {
		drives, err := parseReportsDrives([]byte(`[{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","mounts":36,"temperatureAverage":null,"humidityMax":null}]`))
		if err != nil {
			t.Fatalf("parseReportsDrives: %v", err)
		}
		w := drives[0].window
		if w.TemperatureAverage != nil {
			t.Errorf("TemperatureAverage = %v, want nil", *w.TemperatureAverage)
		}
		if w.HumidityMax != nil {
			t.Errorf("HumidityMax = %v, want nil", *w.HumidityMax)
		}
		if w.Mounts != 36 {
			t.Errorf("Mounts = %v, want 36: a null sensor must not discard the activity figures", w.Mounts)
		}
	})
}

// TestReportsDrivesCollector_Describe locks the descriptor count at exactly 17
// (five activity gauges, three error gauges, six environmental, the per-drive
// window timestamp and duration, and the freshness gauge) so a future edit
// that silently adds or drops a metric is caught here rather than downstream
// in docs-check or a dashboard.
func TestReportsDrivesCollector_Describe(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)

	ch := make(chan *prometheus.Desc, 30)
	c.Describe(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 17 {
		t.Fatalf("Describe sent %d descriptors, want 17", count)
	}
}

// TestReportsDrivesCollector_Collect pins the exact exposition text of every
// business metric against the fixture. refresh is driven directly rather than
// through Start, so the assertion is deterministic and carries no sleep:
// Start's own scheduling is what the lifecycle tests below cover.
//
// Every value here belongs to each drive's NEWEST window (09:05). Registry
// .Gather sorts metric families by name and, within a family, by label value,
// which is why drive_F11C1R3 and drive_F11C3R4 precede drive_F1C4R1
// throughout: that ordering is deterministic, not incidental.
func TestReportsDrivesCollector_Collect(t *testing.T) {
	_, c := reportsDrivesFixtureServer(t)
	c.refresh(context.Background())

	expected := `
# HELP tapelibrary_drive_report_cleans Number of times this drive was cleaned during the reporting window. A per-window figure, not a cumulative counter. Zero in every window of the reference capture: a drive requests cleaning rarely, so a window recording one is the event worth looking at.
# TYPE tapelibrary_drive_report_cleans gauge
tapelibrary_drive_report_cleans{location="drive_F11C1R3"} 0
tapelibrary_drive_report_cleans{location="drive_F11C3R4"} 0
tapelibrary_drive_report_cleans{location="drive_F1C4R1"} 0
# HELP tapelibrary_drive_report_errors_corrected_read Read errors this drive corrected during the reporting window. A corrected error cost throughput but lost no data; a drive whose corrected count runs far above its peers is the classic early signature of a failing head or a dirty tape path. Compare against tapelibrary_drive_report_read_by_hosts_bytes before reading a high count as a fault, since a busy drive corrects more.
# TYPE tapelibrary_drive_report_errors_corrected_read gauge
tapelibrary_drive_report_errors_corrected_read{location="drive_F11C1R3"} 255
tapelibrary_drive_report_errors_corrected_read{location="drive_F11C3R4"} 559
tapelibrary_drive_report_errors_corrected_read{location="drive_F1C4R1"} 252
# HELP tapelibrary_drive_report_errors_corrected_write Write errors this drive corrected during the reporting window, typically by rewriting the affected block further along the tape. Compare against tapelibrary_drive_report_written_by_hosts_bytes before reading a high count as a fault.
# TYPE tapelibrary_drive_report_errors_corrected_write gauge
tapelibrary_drive_report_errors_corrected_write{location="drive_F11C1R3"} 1
tapelibrary_drive_report_errors_corrected_write{location="drive_F11C3R4"} 2
tapelibrary_drive_report_errors_corrected_write{location="drive_F1C4R1"} 0
# HELP tapelibrary_drive_report_errors_uncorrected Errors this drive could not correct during the reporting window, read and write together: R1.11.2 reports no direction breakdown for these, unlike the corrected pair. Any non-zero value is data the drive failed to move, and is what DriveReportUncorrectedErrors reads.
# TYPE tapelibrary_drive_report_errors_uncorrected gauge
tapelibrary_drive_report_errors_uncorrected{location="drive_F11C1R3"} 0
tapelibrary_drive_report_errors_uncorrected{location="drive_F11C3R4"} 1
tapelibrary_drive_report_errors_uncorrected{location="drive_F1C4R1"} 0
# HELP tapelibrary_drive_report_humidity_average_ratio Average relative humidity this drive measured over the reporting window, as a ratio from 0 to 1 (the API reports a 0-100 percentage). Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_humidity_average_ratio gauge
tapelibrary_drive_report_humidity_average_ratio{location="drive_F11C1R3"} 0.35000000000000003
tapelibrary_drive_report_humidity_average_ratio{location="drive_F11C3R4"} 0.28800000000000003
tapelibrary_drive_report_humidity_average_ratio{location="drive_F1C4R1"} 0.33
# HELP tapelibrary_drive_report_humidity_max_ratio Highest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_humidity_max_ratio gauge
tapelibrary_drive_report_humidity_max_ratio{location="drive_F11C1R3"} 0.35000000000000003
tapelibrary_drive_report_humidity_max_ratio{location="drive_F11C3R4"} 0.29
tapelibrary_drive_report_humidity_max_ratio{location="drive_F1C4R1"} 0.33
# HELP tapelibrary_drive_report_humidity_min_ratio Lowest relative humidity this drive measured over the reporting window, as a ratio from 0 to 1. Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_humidity_min_ratio gauge
tapelibrary_drive_report_humidity_min_ratio{location="drive_F11C1R3"} 0.35000000000000003
tapelibrary_drive_report_humidity_min_ratio{location="drive_F11C3R4"} 0.28
tapelibrary_drive_report_humidity_min_ratio{location="drive_F1C4R1"} 0.33
# HELP tapelibrary_drive_report_mounts Number of cartridges mounted into this drive during the reporting window. A per-window figure, not a cumulative counter: the next window restarts from zero.
# TYPE tapelibrary_drive_report_mounts gauge
tapelibrary_drive_report_mounts{location="drive_F11C1R3"} 0
tapelibrary_drive_report_mounts{location="drive_F11C3R4"} 3
tapelibrary_drive_report_mounts{location="drive_F1C4R1"} 2
# HELP tapelibrary_drive_report_read_by_hosts_bytes Bytes read from cartridges by this drive during the reporting window. Converted from the API's megabytes, read decimally (1 MB = 1e6 bytes) to match how this exporter already converts the library report and the cartridge lifetime counters.
# TYPE tapelibrary_drive_report_read_by_hosts_bytes gauge
tapelibrary_drive_report_read_by_hosts_bytes{location="drive_F11C1R3"} 1e+06
tapelibrary_drive_report_read_by_hosts_bytes{location="drive_F11C3R4"} 1.5e+08
tapelibrary_drive_report_read_by_hosts_bytes{location="drive_F1C4R1"} 1.54e+08
# HELP tapelibrary_drive_report_temperature_average_celsius Average temperature in Celsius this drive measured over the reporting window. Measured inside the library at the drive, so it reads above the ambient figure R1.11.2's operating envelope is written against. Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_temperature_average_celsius gauge
tapelibrary_drive_report_temperature_average_celsius{location="drive_F11C1R3"} 24
tapelibrary_drive_report_temperature_average_celsius{location="drive_F11C3R4"} 27
tapelibrary_drive_report_temperature_average_celsius{location="drive_F1C4R1"} 24
# HELP tapelibrary_drive_report_temperature_max_celsius Highest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_temperature_max_celsius gauge
tapelibrary_drive_report_temperature_max_celsius{location="drive_F11C1R3"} 24
tapelibrary_drive_report_temperature_max_celsius{location="drive_F11C3R4"} 27
tapelibrary_drive_report_temperature_max_celsius{location="drive_F1C4R1"} 25
# HELP tapelibrary_drive_report_temperature_min_celsius Lowest temperature in Celsius this drive measured over the reporting window. Absent, never zero, when the drive reported no reading.
# TYPE tapelibrary_drive_report_temperature_min_celsius gauge
tapelibrary_drive_report_temperature_min_celsius{location="drive_F11C1R3"} 24
tapelibrary_drive_report_temperature_min_celsius{location="drive_F11C3R4"} 27
tapelibrary_drive_report_temperature_min_celsius{location="drive_F1C4R1"} 24
# HELP tapelibrary_drive_report_window_duration_seconds Number of seconds this drive's reporting window covers, as the library reports it. 3600 on every window in the reference capture. Exposed so that a window covering less than a full hour is visible rather than assumed away: its activity figures would be proportionally low through no fault of the drive.
# TYPE tapelibrary_drive_report_window_duration_seconds gauge
tapelibrary_drive_report_window_duration_seconds{location="drive_F11C1R3"} 3600
tapelibrary_drive_report_window_duration_seconds{location="drive_F11C3R4"} 3600
tapelibrary_drive_report_window_duration_seconds{location="drive_F1C4R1"} 3600
# HELP tapelibrary_drive_report_window_timestamp_seconds Unix time the library stamped on the reporting window these metrics describe, for this drive. Per drive rather than library-wide so that a single drive dropping out of the report is visible: the library publishes one window per completed hour, so alert if time() - this exceeds a few hours. Every other metric in this family would otherwise keep serving a stale window's values indefinitely, looking healthy.
# TYPE tapelibrary_drive_report_window_timestamp_seconds gauge
tapelibrary_drive_report_window_timestamp_seconds{location="drive_F11C1R3"} 1.7852295e+09
tapelibrary_drive_report_window_timestamp_seconds{location="drive_F11C3R4"} 1.7852295e+09
tapelibrary_drive_report_window_timestamp_seconds{location="drive_F1C4R1"} 1.7852295e+09
# HELP tapelibrary_drive_report_written_by_hosts_bytes Bytes written to cartridges by this drive during the reporting window, measured before compression. Divide by tapelibrary_drive_report_written_to_cartridges_bytes for this drive's average compression ratio over the window. Converted from the API's decimal megabytes.
# TYPE tapelibrary_drive_report_written_by_hosts_bytes gauge
tapelibrary_drive_report_written_by_hosts_bytes{location="drive_F11C1R3"} 0
tapelibrary_drive_report_written_by_hosts_bytes{location="drive_F11C3R4"} 0
tapelibrary_drive_report_written_by_hosts_bytes{location="drive_F1C4R1"} 0
# HELP tapelibrary_drive_report_written_to_cartridges_bytes Bytes this drive actually wrote onto the media during the reporting window, after compression. Converted from the API's decimal megabytes.
# TYPE tapelibrary_drive_report_written_to_cartridges_bytes gauge
tapelibrary_drive_report_written_to_cartridges_bytes{location="drive_F11C1R3"} 0
tapelibrary_drive_report_written_to_cartridges_bytes{location="drive_F11C3R4"} 0
tapelibrary_drive_report_written_to_cartridges_bytes{location="drive_F1C4R1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"tapelibrary_drive_report_cleans",
		"tapelibrary_drive_report_errors_corrected_read",
		"tapelibrary_drive_report_errors_corrected_write",
		"tapelibrary_drive_report_errors_uncorrected",
		"tapelibrary_drive_report_humidity_average_ratio",
		"tapelibrary_drive_report_humidity_max_ratio",
		"tapelibrary_drive_report_humidity_min_ratio",
		"tapelibrary_drive_report_mounts",
		"tapelibrary_drive_report_read_by_hosts_bytes",
		"tapelibrary_drive_report_temperature_average_celsius",
		"tapelibrary_drive_report_temperature_max_celsius",
		"tapelibrary_drive_report_temperature_min_celsius",
		"tapelibrary_drive_report_window_duration_seconds",
		"tapelibrary_drive_report_window_timestamp_seconds",
		"tapelibrary_drive_report_written_by_hosts_bytes",
		"tapelibrary_drive_report_written_to_cartridges_bytes",
	); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	// 48 cached metrics (3 drives x 16 series) plus the freshness gauge
	// Collect always appends. Scaled to the reference fleet's 40 drives this
	// is the 641 recorded in docs/exporter-journal.md's budget for this
	// collector.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 49 {
		t.Fatalf("GatherAndCount = %d, want 49 (3 drives x 16 series + freshness)", count)
	}
}

// TestReportsDrivesCollector_AbsentSensorsEmitNoSeries covers the null branch:
// a drive that reported no temperature or humidity must emit its ten
// always-present series and nothing in their place. A 0 °C / 0% RH would sit
// outside R1.11.2's operating envelope in both directions and page on a drive
// that is merely quiet about its sensors.
//
// The sensorless drive sits beside a healthy one, so this also pins that the
// absence is per drive rather than collapsing the whole family.
func TestReportsDrivesCollector_AbsentSensorsEmitNoSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"location":"drive_F1C4R1","time":"2026-07-28T09:05:00+0000","duration":3600,"mounts":36,
			 "temperatureAverage":null,"temperatureMin":null,"temperatureMax":null,
			 "humidityAverage":null,"humidityMin":null,"humidityMax":null},
			{"location":"drive_F2C4R1","time":"2026-07-28T09:05:00+0000","duration":3600,"mounts":4,
			 "temperatureAverage":24.0,"temperatureMin":24.0,"temperatureMax":25.0,
			 "humidityAverage":33.0,"humidityMin":33.0,"humidityMax":33.0}
		]`))
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
	c.refresh(context.Background())

	// One series each, from the healthy drive only: never two, and never a
	// zero standing in for the sensorless one.
	for _, name := range []string{
		"tapelibrary_drive_report_temperature_average_celsius",
		"tapelibrary_drive_report_temperature_min_celsius",
		"tapelibrary_drive_report_temperature_max_celsius",
		"tapelibrary_drive_report_humidity_average_ratio",
		"tapelibrary_drive_report_humidity_min_ratio",
		"tapelibrary_drive_report_humidity_max_ratio",
	} {
		if got := testutil.CollectAndCount(c, name); got != 1 {
			t.Errorf("%s produced %d series, want 1: an absent reading must emit nothing, never a zero", name, got)
		}
	}

	// The activity figures and each window's own identity are unaffected: 10
	// always-present series per drive, 6 environmental from the healthy drive
	// only, plus the freshness gauge.
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	count, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	if count != 27 {
		t.Fatalf("GatherAndCount = %d, want 27 (2 drives x 10 + 6 environmental + freshness)", count)
	}
}

// TestReportsDrivesCollector_DoneClosesOnCancel verifies the Done() channel
// closes when the context passed to Start is cancelled. This is the mechanism
// main.go's shutdown seam relies on (see registry.Wait after
// web.ListenAndServe).
func TestReportsDrivesCollector_DoneClosesOnCancel(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
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

// TestReportsDrivesCollector_CollectServesCacheWithoutIO proves Collect never
// calls the library: after Start's own immediate refresh completes, a long
// interval (1 hour) guarantees the ticker cannot fire again during this test,
// so any further request the server receives could only come from Collect
// itself calling out, which the design forbids. It matters more here than on
// most collectors, since this endpoint's unbounded week is ~3.3 MB against a
// concurrency ceiling of 1.
func TestReportsDrivesCollector_CollectServesCacheWithoutIO(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.ServeFile(w, r, "testdata/reports_drives.json")
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestReportsDrivesCollector_ErrorHandling drives a refresh against a library
// that only ever fails. No cache was ever filled, so the scrape must carry
// exactly the freshness gauge, and must neither panic nor emit a partial
// window.
func TestReportsDrivesCollector_ErrorHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient(srv.URL, time.Second), time.Hour)
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

// TestReportsDrivesCollector_ErrorKeepsPreviousCache scripts the backend to
// succeed once, then fail on every later call, and drives at least one more
// refresh via a short interval. The cache from the successful first refresh
// must survive the later failure (fail-open, per refresh's doc comment)
// rather than being cleared or replaced with nothing.
func TestReportsDrivesCollector_ErrorKeepsPreviousCache(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.ServeFile(w, r, "testdata/reports_drives.json")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient(srv.URL, time.Second), 20*time.Millisecond)
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
	// A surviving cache emits 49 metrics: the 48 cached from the successful
	// first refresh plus the freshness gauge. A wrongly-cleared cache would
	// emit only 1 (the gauge alone), which "> 0" cannot tell apart.
	if count != 49 {
		t.Fatalf("GatherAndCount = %d, want 49: the previous cache must survive a later refresh error", count)
	}
}

// TestReportsDrivesCollector_StatusTrackerSuccessOnFirstScrape covers the
// startup window before Start's first refresh has completed (Start is
// deliberately never called here). Collect must still emit exactly the
// freshness gauge, valued 0 (not a zero time.Time's large-negative Unix()),
// and StatusTracker must still report this collector as successful: "Collect
// ran and returned data" and "the data is fresh" are different questions.
func TestReportsDrivesCollector_StatusTrackerSuccessOnFirstScrape(t *testing.T) {
	log := logger.NewTextLogger("error")
	c := NewReportsDrivesCollector(log, NewClient("http://example.invalid", time.Second), time.Hour)
	// Do NOT call Start(): cache is empty, no refresh has ever run.

	expected := `
# HELP tapelibrary_drive_report_last_refresh_timestamp_seconds Unix time of the last successful reports/drives refresh. Alert if time() - this > 2 x the collector's configured interval. Named for this collector's metric subsystem rather than its registered name, so a subsystem sweep finds the freshness of the data it is reading. Distinct from tapelibrary_drive_report_window_timestamp_seconds, which ages even while this one stays current.
# TYPE tapelibrary_drive_report_last_refresh_timestamp_seconds gauge
tapelibrary_drive_report_last_refresh_timestamp_seconds 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}

	tracker := NewStatusTracker(log)
	tracker.Add("reports_drives", c)

	expectedSuccess := `
# HELP ` + statusTrackerSuccessMetric + ` Whether the last scrape of the collector succeeded (1=success, 0=failure)
# TYPE ` + statusTrackerSuccessMetric + ` gauge
` + statusTrackerSuccessMetric + `{collector="reports_drives"} 1
`
	if err := testutil.CollectAndCompare(tracker, strings.NewReader(expectedSuccess), statusTrackerSuccessMetric); err != nil {
		t.Fatalf("unexpected collecting result:\n%s", err)
	}
}
