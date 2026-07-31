// docs_check_test.go implements `make docs-check`: it verifies docs/metrics.md
// never documents a Prometheus metric name or label that this package's own
// source code cannot produce. See docs/metrics.md's own header comment for
// the table format parseMetricsDoc below expects, and CONTRIBUTING.md's
// Definition of Done (step 4) for how this fits into the release workflow.
//
// Flavor-agnostic by construction: this file ships unmodified to both the
// http and cli flavors, exactly like status_tracker.go/status_tracker_test.go
// alongside it. It never imports or names a specific collector type
// (ExampleCollector, Client, ...). Instead, it parses whichever *.go files
// happen to live in this same directory at build time and extracts every
// prometheus.NewDesc(...) call and every Opts-based constructor call
// (NewHistogramVec/NewCounterVec/NewGaugeVec/NewSummaryVec and their non-Vec
// NewHistogram/NewCounter/NewGauge/NewSummary forms) it finds via go/ast, the
// same way for either flavor. A metric name built with
// prometheus.BuildFQName(namespace, subsystem, name), the standard,
// idiomatic way to compose a fqName (used throughout client_golang itself),
// and not a naming anti-pattern, is resolved exactly like a bare string literal,
// provided all three arguments are themselves string literals (see
// resolveNameExpr/buildFQName below).
//
// This is a source-extraction check, not a construct-and-Describe check:
// nothing here constructs a collector or mocks any I/O. A metric's name and
// label set are fixed at its call site in source, regardless of whether that
// collector's own Collect ever successfully runs: constructing collectors
// and calling Gather() instead would make this check depend on whether a
// fake client/Execute happens to succeed this run, which has nothing to do
// with whether the DOCS are truthful (see collector.go's own Collect doc
// comment: a collector emits zero metrics on error, so a Gather()-based
// check would misreport a merely-unreachable-target collector's real,
// well-documented metrics as "undocumented" or vice versa).
//
// Known, deliberate limitations of this static-analysis approach (documented
// here rather than silently assumed):
//   - A metric's fqName must be a plain double-quoted string literal, or a
//     prometheus.BuildFQName(ns, sub, name) call whose three arguments are
//     themselves string literals, at its NewDesc/Opts.Name call site. A
//     genuinely computed name (fmt.Sprintf, string concatenation, a
//     variable, ...) cannot be resolved, also a Prometheus naming
//     anti-pattern independent of this tool (metric names and label keys
//     are meant to be static; see CONTRIBUTING.md's "Adding a new
//     collector" section), so this is not expected to bite a
//     correctly-written collector. Unlike an earlier version of this
//     extractor, an unresolvable name is not silently dropped: it is
//     reported as a WARNING naming the offending file:line (visible under
//     `go test -v`/`make docs-check`), so an unverifiable metric stays
//     visible instead of becoming a silent hole a label-lie could slip
//     through.
//   - Recognized call shapes: prometheus.NewDesc(fqName, help,
//     variableLabels, constLabels); the four Vec constructors
//     (NewHistogramVec/NewCounterVec/NewGaugeVec/NewSummaryVec), each
//     taking a prometheus.<X>Opts{Namespace, Subsystem, Name, ...}
//     composite literal as their first argument and a variable-label slice
//     as their second; and their non-Vec, zero-variable-label forms
//     (NewHistogram/NewCounter/NewGauge/NewSummary), Opts only, no second
//     argument. A differently-shaped call to one of these same function
//     names (for example an Opts value built from a variable instead of a
//     composite literal) is reported as a WARNING, not silently skipped.
//     See extractSourceMetrics. A *different* constructor entirely
//     (promauto's auto-registering variants, NewGaugeFunc/NewCounterFunc/
//     NewUntypedFunc, a differently aliased or wrapped "prometheus"
//     import, ...) is still genuinely invisible to this extractor, out of
//     scope, not merely unresolved, since recognizing every possible
//     metric-constructing call shape in the client_golang API is not this
//     tool's goal.
//   - Only this package's own directory (".") is scanned, non-recursively:
//     correct for this scaffold's flat internal/collector/ layout; a future
//     reorganization into subpackages would need a wider walk.
//   - A label list that isn't a literal `nil` or `[]string{"a", "b"}` (for
//     example a shared package-level variable reused across several Desc
//     calls) cannot be resolved here. Rather than guessing wrong, such a
//     metric's labels are marked unknown (metricDesc.labelsKnown = false)
//     and TestDocsCheck skips label verification for it. This extractor
//     must never manufacture a false "docs lie" out of its own inability to
//     resolve a label list. This gap is also surfaced as a WARNING naming
//     the file:line, for the same reason as the name-resolution case above.
package collector

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docsMetricsPath is docs/metrics.md, expressed relative to this package's
// own directory (internal/collector/, always exactly two levels below repo
// root in every flavor this scaffold ships) since `go test` runs with the
// package's source directory as its working directory, the same
// convention every other test file here already relies on for its own
// testdata/ fixtures.
const docsMetricsPath = "../../docs/metrics.md"

// metricDesc is one Prometheus metric's variable label keys, as either
// extracted from source (extractSourceMetrics) or parsed from
// docs/metrics.md (parseMetricsDoc). The metric's name is not stored here:
// it is always the key of the map this type is a value in.
type metricDesc struct {
	labels []string
	// labelsKnown is false when this metric's name was statically resolved
	// but its label list was not (see this file's own doc comment). A label
	// documented against such a metric must never be reported as a lie:
	// this checker genuinely does not know the real label set, so it must
	// not guess.
	labelsKnown bool
}

// TestDocsCheck is `make docs-check`'s implementation: docs/metrics.md must
// never document a metric or label this package's source cannot produce
// (docs subset-of code, a hard failure, since a lying doc is worse than no
// doc at all). The reverse gap, a real metric missing from docs/metrics.md,
// is only logged as a warning (visible under `go test -v`, which is how
// `make docs-check` always invokes this). See CONTRIBUTING.md's Definition
// of Done, step 4: undocumented is a quality gap, not a lie. Any call site
// extractSourceMetrics recognized by function name but could not fully
// resolve (an unresolvable name, Opts, or variable-label list) is also
// logged as a warning, naming the file:line. See extractSourceMetrics's own
// doc comment for why that must never be silent.
func TestDocsCheck(t *testing.T) {
	extracted, warnings, err := extractSourceMetrics(".")
	if err != nil {
		t.Fatalf("extract metrics from internal/collector/*.go: %v", err)
	}
	// A completely empty extraction almost certainly means this extractor
	// itself broke (a refactor changed the call shapes it recognizes)
	// rather than that this package genuinely defines zero metrics: every
	// flavor this scaffold ships defines at least the shared StatusTracker's
	// two. Left unchecked, an extractor that silently finds nothing would
	// make every future run of this test vacuously pass forever regardless
	// of what docs/metrics.md claims, exactly the kind of lie this test
	// exists to catch.
	if len(extracted) == 0 {
		t.Fatal("extractSourceMetrics found zero metrics in internal/collector/*.go: either this package defines none (unexpected) or the extractor's go/ast pattern matching no longer recognizes this code; see this file's own doc comment")
	}
	for _, w := range warnings {
		t.Logf("WARNING: %s", w)
	}

	documented, err := parseMetricsDoc(docsMetricsPath)
	if err != nil {
		t.Fatalf("parse %s: %v", docsMetricsPath, err)
	}

	lies, undocumented := diffDocs(extracted, documented)
	if len(lies) > 0 {
		t.Fatalf("%s documents %d metric(s)/label(s) that do not exist in code:\n  - %s", docsMetricsPath, len(lies), strings.Join(lies, "\n  - "))
	}
	if len(undocumented) > 0 {
		t.Logf("WARNING: %d metric(s) emitted by internal/collector/*.go are not documented in %s: %s (see CONTRIBUTING.md's Definition of Done, step 4)", len(undocumented), docsMetricsPath, strings.Join(undocumented, ", "))
	}
}

// diffDocs compares extracted (from source, via extractSourceMetrics) against
// documented (from docs/metrics.md, via parseMetricsDoc) and reports both
// directions of disagreement. lies are documented metrics/labels the code
// cannot produce: TestDocsCheck fails the build on these, since a lying doc
// is worse than no doc at all. undocumented are metrics the code emits that
// docs/metrics.md never mentions: TestDocsCheck only warns on these (see
// CONTRIBUTING.md's Definition of Done, step 4). Split out from TestDocsCheck
// as a pure function so the comparison logic itself is directly unit
// testable against hand-built maps, without needing a real docs/metrics.md
// or a real package directory on disk (see TestDocsCheck_BuildFQName).
func diffDocs(extracted, documented map[string]metricDesc) (lies []string, undocumented []string) {
	for name, doc := range documented {
		src, ok := extracted[name]
		if !ok {
			lies = append(lies, fmt.Sprintf("%s documents metric `%s`, which does not exist in internal/collector/*.go", docsMetricsPath, name))
			continue
		}
		if !src.labelsKnown {
			continue
		}
		for _, label := range doc.labels {
			if !containsString(src.labels, label) {
				lies = append(lies, fmt.Sprintf("%s documents label `%s` on metric `%s`, which the code does not emit (code labels: %v)", docsMetricsPath, label, name, src.labels))
			}
		}
	}
	sort.Strings(lies)

	for name := range extracted {
		if _, ok := documented[name]; !ok {
			undocumented = append(undocumented, name)
		}
	}
	sort.Strings(undocumented)

	return lies, undocumented
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// extractSourceMetrics parses every non-test *.go file directly under dir
// and returns every prometheus.NewDesc / Opts-based constructor call site
// (NewHistogramVec, NewCounterVec, NewGaugeVec, NewSummaryVec, and their
// non-Vec NewHistogram/NewCounter/NewGauge/NewSummary forms) it can
// statically resolve, keyed by metric name: resolving a
// prometheus.BuildFQName(...) fqName argument or an Opts.Name/Namespace/
// Subsystem field exactly like a bare string literal (see
// resolveNameExpr/optsFQName). Test files are skipped deliberately:
// status_tracker_test.go's stub collectors build throwaway prometheus.NewDesc
// calls from a variable name, for StatusTracker's own tests, never a metric
// this exporter actually ships on /metrics.
//
// The second return value collects one human-readable "file:line: ..."
// warning per call site that extractSourceMetrics recognized by function
// name but could not fully resolve (an unresolvable name, Opts, or
// variable-label list). The caller (TestDocsCheck) surfaces each via t.Logf
// so an unverifiable metric is visible under `go test -v`/`make docs-check`
// instead of silently vanishing from the extracted map. A metric this
// extractor cannot vouch for must never look, to a reader of test output,
// indistinguishable from a metric that simply doesn't exist.
func extractSourceMetrics(dir string) (map[string]metricDesc, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	result := make(map[string]metricDesc)
	var warnings []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Assumes the conventional, unaliased
			// `"github.com/prometheus/client_golang/prometheus"` import
			// every template file in this scaffold actually uses. See
			// this file's own doc comment for why a differently-aliased
			// import is out of scope.
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "prometheus" {
				return true
			}

			loc := fmt.Sprintf("%s:%d", fset.Position(call.Pos()).Filename, fset.Position(call.Pos()).Line)
			fn := sel.Sel.Name

			switch fn {
			case "NewDesc":
				// prometheus.NewDesc(fqName, help, variableLabels, constLabels)
				if len(call.Args) < 3 {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.NewDesc call does not have the expected argument shape (fqName, help, variableLabels, constLabels), skipped", loc))
					return true
				}
				fqName, ok := resolveNameExpr(call.Args[0])
				if !ok {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.NewDesc's metric name could not be statically resolved (not a string literal or prometheus.BuildFQName(...) with literal arguments), this metric is invisible to docs-check", loc))
					return true
				}
				labels, labelsKnown := stringSliceLit(call.Args[2])
				if !labelsKnown {
					warnings = append(warnings, fmt.Sprintf("%s: metric `%s`'s variable-label list could not be statically resolved (not `nil` or a []string{...} literal), docs-check cannot verify its documented labels", loc, fqName))
				}
				result[fqName] = metricDesc{labels: labels, labelsKnown: labelsKnown}

			case "NewHistogramVec", "NewCounterVec", "NewGaugeVec", "NewSummaryVec":
				// prometheus.New<X>Vec(prometheus.<X>Opts{Namespace, Subsystem, Name, ...}, variableLabels)
				if len(call.Args) < 2 {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.%s call does not have the expected argument shape (Opts, variableLabels), skipped", loc, fn))
					return true
				}
				fqName, ok := optsFQName(call.Args[0])
				if !ok {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.%s's Opts (Namespace/Subsystem/Name) could not be statically resolved (expected a prometheus.Opts{...}-shaped composite literal with string-literal fields), this metric is invisible to docs-check", loc, fn))
					return true
				}
				labels, labelsKnown := stringSliceLit(call.Args[1])
				if !labelsKnown {
					warnings = append(warnings, fmt.Sprintf("%s: metric `%s`'s variable-label list could not be statically resolved (not `nil` or a []string{...} literal), docs-check cannot verify its documented labels", loc, fqName))
				}
				result[fqName] = metricDesc{labels: labels, labelsKnown: labelsKnown}

			case "NewHistogram", "NewCounter", "NewGauge", "NewSummary":
				// prometheus.New<X>(prometheus.<X>Opts{Namespace, Subsystem, Name, ...}):
				// a single, non-Vec metric has no variable-label argument
				// at all (its ConstLabels are constant, not "variable" in
				// the sense this checker tracks), so a resolved name always
				// pairs with a known, empty label set.
				if len(call.Args) < 1 {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.%s call does not have the expected argument shape (Opts), skipped", loc, fn))
					return true
				}
				fqName, ok := optsFQName(call.Args[0])
				if !ok {
					warnings = append(warnings, fmt.Sprintf("%s: prometheus.%s's Opts (Namespace/Subsystem/Name) could not be statically resolved (expected a prometheus.Opts{...}-shaped composite literal with string-literal fields), this metric is invisible to docs-check", loc, fn))
					return true
				}
				result[fqName] = metricDesc{labels: nil, labelsKnown: true}
			}
			return true
		})
	}
	return result, warnings, nil
}

// stringLit resolves expr as a plain double-quoted string literal.
func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// resolveNameExpr resolves expr as a metric name: either a plain string
// literal, or a prometheus.BuildFQName(namespace, subsystem, name) call
// whose three arguments are themselves string literals, the standard,
// idiomatic way many real-world exporters compose a fqName without a
// separate Opts struct (see NewDesc's own call shape). Returns ok=false for
// anything else (a variable, string concatenation, a differently-shaped
// call, ...): the caller must then treat this metric's name as unresolvable
// and surface a warning rather than silently dropping it or guessing at a
// name.
func resolveNameExpr(expr ast.Expr) (string, bool) {
	if v, ok := stringLit(expr); ok {
		return v, true
	}

	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok || pkgIdent.Name != "prometheus" || sel.Sel.Name != "BuildFQName" {
		return "", false
	}
	if len(call.Args) != 3 {
		return "", false
	}
	namespace, ok := stringLit(call.Args[0])
	if !ok {
		return "", false
	}
	subsystem, ok := stringLit(call.Args[1])
	if !ok {
		return "", false
	}
	name, ok := stringLit(call.Args[2])
	if !ok {
		return "", false
	}
	return buildFQName(namespace, subsystem, name), true
}

// buildFQName mirrors prometheus.BuildFQName's own join logic exactly
// (client_golang's prometheus/metric.go): namespace and subsystem are each
// joined with "_" only when non-empty, and the whole result collapses to ""
// when name itself is empty, regardless of namespace/subsystem: this
// extractor cannot import and call the real function on values it only
// knows as parsed source text, never live strings the real function would
// receive at runtime, so the join logic is deliberately kept in lockstep
// with upstream here rather than reimplemented differently by accident.
func buildFQName(namespace, subsystem, name string) string {
	if name == "" {
		return ""
	}
	var sb strings.Builder
	if namespace != "" {
		sb.WriteString(namespace)
		sb.WriteString("_")
	}
	if subsystem != "" {
		sb.WriteString(subsystem)
		sb.WriteString("_")
	}
	sb.WriteString(name)
	return sb.String()
}

// stringSliceLit resolves expr as either the bare identifier nil (no
// labels, ok=true, a known-empty label set) or a []string{"a", "b", ...}
// composite literal of plain string literals (ok=true). Anything else (a
// variable, a function call, a slice built from a non-literal element, ...)
// reports ok=false: the caller must then treat that metric's label set as
// unknown, not as empty. See metricDesc's labelsKnown field.
func stringSliceLit(expr ast.Expr) ([]string, bool) {
	if ident, ok := expr.(*ast.Ident); ok && ident.Name == "nil" {
		return nil, true
	}
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	labels := make([]string, 0, len(comp.Elts))
	for _, elt := range comp.Elts {
		s, ok := stringLit(elt)
		if !ok {
			return nil, false
		}
		labels = append(labels, s)
	}
	return labels, true
}

// optsFQName resolves expr as a prometheus Opts-shaped composite literal
// (Opts, HistogramOpts, SummaryOpts, GaugeOpts, CounterOpts, this doesn't
// care which one, it only looks at field names) and returns its
// fully-qualified metric name, composed from whichever of its
// Namespace/Subsystem/Name fields are present exactly the way
// prometheus.BuildFQName (called internally by every one of these
// constructors at registration time) would. See buildFQName. A Name field
// is mandatory (ok=false if absent, matching client_golang's own "It is
// mandatory to set Name" contract); Namespace/Subsystem default to "" when
// absent, matching Go's own zero-value struct-literal semantics. A field
// that is present but not a plain string literal (a variable, a
// prometheus.BuildFQName(...) call, a computed expression, ...) makes the
// whole call unresolvable (ok=false): this must never guess at a
// namespace/subsystem/name it cannot prove statically.
func optsFQName(expr ast.Expr) (string, bool) {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return "", false
	}

	var namespace, subsystem, name string
	nameSet := false
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Namespace":
			v, ok := stringLit(kv.Value)
			if !ok {
				return "", false
			}
			namespace = v
		case "Subsystem":
			v, ok := stringLit(kv.Value)
			if !ok {
				return "", false
			}
			subsystem = v
		case "Name":
			v, ok := stringLit(kv.Value)
			if !ok {
				return "", false
			}
			name = v
			nameSet = true
		}
	}
	if !nameSet {
		return "", false
	}
	return buildFQName(namespace, subsystem, name), true
}

// metricsDocNameRE matches a markdown table cell that is exactly one
// backtick-quoted name, nothing else: this is what excludes
// docs/metrics.md's header row (`| Metric | Type | ... |`, no backticks)
// and separator row (`|---|---|...|`) from being misread as metric rows.
var metricsDocNameRE = regexp.MustCompile("^`([A-Za-z_][A-Za-z0-9_]*)`$")

// metricsDocLabelRE finds every backtick-quoted token inside a table row's
// Labels cell (see docs/metrics.md's own header comment for the table
// format): comma/space-separated doesn't matter, this just extracts each
// `name` in turn. A cell of "-" (no labels) simply yields zero matches.
var metricsDocLabelRE = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)`")

// parseMetricsDoc parses path (docs/metrics.md) as a sequence of
// "| `metric` | Type | `label`, ... | Description |" table rows. See this
// file's own header comment and docs/metrics.md's header comment for the
// exact format. Any line that isn't a well-formed, backtick-first-cell row
// is ignored, which is what lets prose and headings coexist in the same
// file without special-casing them here. Lines inside an HTML comment
// (`<!-- ... -->`, on their own lines, docs/metrics.md's own format-guide
// block is written exactly this way) are skipped outright: that block's own
// example row, `| \`metric_name\` | Type | \`label1\`, \`label2\` |
// Description |`, is itself well-formed table syntax and would otherwise be
// misread as documenting real metrics literally named "metric_name" with
// labels "label1"/"label2", a false lie this checker would report against
// itself on every single run.
func parseMetricsDoc(path string) (map[string]metricDesc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	result := make(map[string]metricDesc)
	inComment := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		if strings.Contains(line, "<!--") && !strings.Contains(line, "-->") {
			inComment = true
			continue
		}

		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}

		nameMatch := metricsDocNameRE.FindStringSubmatch(strings.TrimSpace(cells[0]))
		if nameMatch == nil {
			continue // header row, separator row, or some other non-metric table row
		}
		name := nameMatch[1]

		var labels []string
		for _, m := range metricsDocLabelRE.FindAllStringSubmatch(strings.TrimSpace(cells[2]), -1) {
			labels = append(labels, m[1])
		}
		result[name] = metricDesc{labels: labels, labelsKnown: true}
	}
	return result, nil
}

// TestDocsCheck_BuildFQName is a regression test for a false-FAIL the
// pre-fix extractor produced: prometheus.BuildFQName(ns, sub, name) is the
// standard, idiomatic way many real-world exporters compose a metric's
// fqName, not a naming anti-pattern, yet the pre-fix extractor only
// recognized a bare string literal as NewDesc's first argument, so a
// collector written this way was silently invisible to docs-check: a
// truthful docs/metrics.md row for such a metric then failed with
// "documents metric `x`, which does not exist in internal/collector/*.go",
// a false accusation against a correct doc. See resolveNameExpr/buildFQName.
func TestDocsCheck_BuildFQName(t *testing.T) {
	dir := t.TempDir()
	src := `package collector

import "github.com/prometheus/client_golang/prometheus"

func newDemoDesc() *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName("demo", "", "items"),
		"help text",
		nil, nil,
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	extracted, warnings, err := extractSourceMetrics(dir)
	if err != nil {
		t.Fatalf("extractSourceMetrics: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected zero warnings for a fully-resolvable BuildFQName call, got: %v", warnings)
	}
	if _, ok := extracted["demo_items"]; !ok {
		t.Fatalf(`expected prometheus.BuildFQName("demo", "", "items") to resolve to metric `+"`demo_items`"+`, got extracted=%v`, extracted)
	}

	// PASS case: docs/metrics.md truthfully documents the BuildFQName-composed name.
	truthful := map[string]metricDesc{"demo_items": {labelsKnown: true}}
	if lies, _ := diffDocs(extracted, truthful); len(lies) != 0 {
		t.Errorf("expected docs-check to PASS when docs truthfully document a BuildFQName-composed metric, got lies: %v", lies)
	}

	// FAIL case: docs/metrics.md documents a name this code does not produce.
	lying := map[string]metricDesc{"demo_items_wrong": {labelsKnown: true}}
	if lies, _ := diffDocs(extracted, lying); len(lies) == 0 {
		t.Error("expected docs-check to FAIL when docs document a metric the BuildFQName-composed code does not actually produce")
	}
}

// TestDocsCheck_OptsBasedConstructors covers the Opts-based constructor
// family beyond NewHistogramVec (already exercised by this scaffold's own
// shipped self-instrumentation): NewCounterVec/NewGaugeVec/NewSummaryVec,
// Namespace+Subsystem+Name composition, and a non-Vec form (NewGauge, zero
// variable labels by construction).
func TestDocsCheck_OptsBasedConstructors(t *testing.T) {
	dir := t.TempDir()
	src := `package collector

import "github.com/prometheus/client_golang/prometheus"

var (
	c = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "demo",
		Subsystem: "sub",
		Name:      "counter_total",
	}, []string{"a", "b"})
	g = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "demo",
		Name:      "gauge",
	}, nil)
	s = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "summary",
	}, []string{"x"})
	single = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "demo",
		Name:      "single_gauge",
	})
)
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	extracted, warnings, err := extractSourceMetrics(dir)
	if err != nil {
		t.Fatalf("extractSourceMetrics: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	cases := []struct {
		name       string
		wantLabels []string
	}{
		{"demo_sub_counter_total", []string{"a", "b"}},
		{"demo_gauge", nil},
		{"summary", []string{"x"}},
		{"demo_single_gauge", nil},
	}
	for _, c := range cases {
		desc, ok := extracted[c.name]
		if !ok {
			t.Errorf("expected metric `%s` extracted, got %v", c.name, extracted)
			continue
		}
		if !desc.labelsKnown {
			t.Errorf("metric `%s`: expected labelsKnown=true", c.name)
			continue
		}
		if strings.Join(desc.labels, ",") != strings.Join(c.wantLabels, ",") {
			t.Errorf("metric `%s`: got labels %v, want %v", c.name, desc.labels, c.wantLabels)
		}
	}
}

// TestDocsCheck_WarnsOnUnresolvableName is a regression test for the
// "silent hole" this checker used to leave open: a metric name built from a
// package-level var (or any other non-literal, non-BuildFQName expression)
// used to vanish from the extracted set with no trace at all. It must now
// surface as a WARNING naming the file:line, not disappear silently.
func TestDocsCheck_WarnsOnUnresolvableName(t *testing.T) {
	dir := t.TempDir()
	src := `package collector

import "github.com/prometheus/client_golang/prometheus"

var dynamicName = "not_a_literal_call_site"

var d = prometheus.NewDesc(dynamicName, "help", nil, nil)
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	extracted, warnings, err := extractSourceMetrics(dir)
	if err != nil {
		t.Fatalf("extractSourceMetrics: %v", err)
	}
	if len(extracted) != 0 {
		t.Errorf("expected zero metrics extracted for an unresolvable name, got %v", extracted)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning for the unresolvable NewDesc name, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "fixture.go:") {
		t.Errorf("expected warning to name the file:line, got: %s", warnings[0])
	}
}

// TestDocsCheck_WarnsOnUnresolvableLabels covers the other half of the same
// "silent hole": a label list built from a shared package-level var (this
// file's own doc comment's example) resolved the metric's name fine but used
// to skip label verification with no visible trace. It must now also warn.
func TestDocsCheck_WarnsOnUnresolvableLabels(t *testing.T) {
	dir := t.TempDir()
	src := `package collector

import "github.com/prometheus/client_golang/prometheus"

var sharedLabels = []string{"a", "b"}

var d = prometheus.NewDesc("demo_thing", "help", sharedLabels, nil)
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	extracted, warnings, err := extractSourceMetrics(dir)
	if err != nil {
		t.Fatalf("extractSourceMetrics: %v", err)
	}
	desc, ok := extracted["demo_thing"]
	if !ok {
		t.Fatalf("expected metric `demo_thing` extracted despite unresolvable labels, got %v", extracted)
	}
	if desc.labelsKnown {
		t.Error("expected labelsKnown=false for a label list built from a package-level var")
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning for the unresolvable label list, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "fixture.go:") || !strings.Contains(warnings[0], "demo_thing") {
		t.Errorf("expected warning to name the file:line and the metric name, got: %s", warnings[0])
	}
}
