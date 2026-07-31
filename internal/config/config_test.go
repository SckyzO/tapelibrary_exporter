package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"
	promconfig "github.com/prometheus/common/config"
)

// testFlags is a flag set shaped like a real exporter's: an enum, a bool, a
// duration and a repeatable flag, which is the combination ToArgs must render.
// The parsed targets are captured at declaration time, because calling
// .Strings() a second time through GetFlag would rebind the flag to a
// different target and silently observe nothing.
type testFlags struct {
	app     *kingpin.Application
	level   *string
	addrs   *[]string
	enabled *bool
	timeout *time.Duration
}

func newApp() testFlags {
	app := kingpin.New("test", "")
	return testFlags{
		app:     app,
		level:   app.Flag("log.level", "").Default("info").Enum("debug", "info", "warn", "error"),
		addrs:   app.Flag("web.listen-address", "").Default(":9999").Strings(),
		enabled: app.Flag("collector.example", "").Default("true").Bool(),
		timeout: app.Flag("collector.example.timeout", "").Default("5s").Duration(),
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadEmptyPathIsNoOp(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") returned an error: %v", err)
	}
	if len(c.Flags) != 0 {
		t.Errorf("Flags = %v, want empty", c.Flags)
	}
	if c.HTTPClientConfig != nil {
		t.Errorf("HTTPClientConfig = %v, want nil", c.HTTPClientConfig)
	}
	if args := c.ToArgs(nil); len(args) != 0 {
		t.Errorf("ToArgs = %v, want no arguments", args)
	}
}

func TestToArgsRendersEachShape(t *testing.T) {
	path := writeConfig(t, `
flags:
  log.level: debug
  collector.example.timeout: 10s
  collector.example: false
  web.listen-address:
    - ":9341"
    - ":9342"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := strings.Join(c.ToArgs(nil), " ")
	for _, want := range []string{
		"--log.level=debug",
		"--collector.example.timeout=10s",
		"--no-collector.example",
		"--web.listen-address=:9341",
		"--web.listen-address=:9342",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ToArgs = %q, missing %q", got, want)
		}
	}
}

// A repeatable flag must end up holding exactly what the file asked for. This
// is the behaviour that writing values in after Parse would have broken: the
// default would already have been applied and the file's values appended.
func TestRepeatableFlagReplacesDefault(t *testing.T) {
	path := writeConfig(t, "flags:\n  web.listen-address: [\":9341\", \":9342\"]\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	f := newApp()
	if _, err := f.app.Parse(c.ToArgs(nil)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(*f.addrs) != 2 || (*f.addrs)[0] != ":9341" || (*f.addrs)[1] != ":9342" {
		t.Errorf("addresses = %v, want exactly [:9341 :9342] (the default must be replaced, not extended)", *f.addrs)
	}
}

func TestCommandLineWins(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.level: debug\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cli := []string{"--log.level=warn"}
	args := append(c.ToArgs(CLIFlagNames(cli)), cli...)

	f := newApp()
	if _, err := f.app.Parse(args); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *f.level != "warn" {
		t.Errorf("log.level = %q, want warn (the command line must win)", *f.level)
	}
}

func TestValidateRejectsUnknownFlagKey(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.levl: debug\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate(newApp().app)
	if err == nil {
		t.Fatal("Validate accepted an unknown flag key")
	}
	if !strings.Contains(err.Error(), "log.levl") {
		t.Errorf("error = %q, want it to name the offending key", err)
	}
}

func TestValidateRejectsNullValue(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.level:\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate(newApp().app)
	if err == nil {
		t.Fatal("Validate accepted a null value")
	}
	if !strings.Contains(err.Error(), "no value") {
		t.Errorf("error does not say the value is missing: %v", err)
	}
}

// config.file names the file being read, so its value was consumed from
// os.Args long before Validate runs. It IS a declared flag, so the
// unknown-key check accepts it and ToArgs would render an argument that
// changes nothing. Rejecting it beats letting the key sit there doing
// nothing an operator can see.
func TestValidateRejectsConfigFileNamingItself(t *testing.T) {
	path := writeConfig(t, "flags:\n  config.file: /somewhere/else.yml\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate(newApp().app)
	if err == nil {
		t.Fatal("Validate accepted config.file set from inside the config file")
	}
	if !strings.Contains(err.Error(), "command line") {
		t.Errorf("error does not tell the operator where to pass it: %v", err)
	}
}

// A nested block under a flag name decodes to a Go map, which renderFlag's
// default case would format with %v into --name=map[a:1 b:2]. A string flag
// accepts that without complaint, so the exporter would start carrying a
// value nobody wrote. Same silent-corruption class as the YAML 1.1 boolean.
func TestValidateRejectsNestedBlockForAFlag(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.level:\n    a: 1\n    b: 2\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate(newApp().app)
	if err == nil {
		t.Fatal("Validate accepted a nested block where a flag expects a value")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

// YAML 1.1 (the pinned parser's dialect) resolves an unquoted "on" to the Go
// bool true regardless of the destination type, so log.level (a string enum
// flag) must not silently accept it: renderFlag would then emit the bare
// --log.level instead of --log.level=on.
func TestValidateRejectsYAML11BooleanOnNonBooleanFlag(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.level: on\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = c.Validate(newApp().app)
	if err == nil {
		t.Fatal("Validate accepted an unquoted YAML 1.1 boolean word on a non-boolean flag")
	}
	if !strings.Contains(err.Error(), "log.level") {
		t.Errorf("error = %q, want it to name the offending flag", err)
	}
	if !strings.Contains(err.Error(), "quote") {
		t.Errorf("error = %q, want it to tell the operator to quote the value", err)
	}
}

// Quoting the same word keeps it a YAML string, so it must keep working end
// to end: Validate accepts it and ToArgs renders it as a plain string flag.
func TestValidateAcceptsQuotedYAML11BooleanWord(t *testing.T) {
	path := writeConfig(t, "flags:\n  log.level: \"on\"\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(newApp().app); err != nil {
		t.Fatalf("Validate rejected a quoted value: %v", err)
	}
	got := strings.Join(c.ToArgs(nil), " ")
	if !strings.Contains(got, "--log.level=on") {
		t.Errorf("ToArgs = %q, want --log.level=on", got)
	}
}

// collector.example is a genuine boolean flag: a Go bool is the correct,
// expected decoding for it and must keep passing Validate and rendering the
// --no-<name> negation form. This is the non-regression case for the fix
// above: the new check must not reject every bool, only a bool landing on a
// flag that is not itself boolean.
func TestValidateAcceptsRealBooleanFlagValue(t *testing.T) {
	path := writeConfig(t, "flags:\n  collector.example: false\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(newApp().app); err != nil {
		t.Fatalf("Validate rejected a real boolean flag's boolean value: %v", err)
	}
	got := strings.Join(c.ToArgs(nil), " ")
	if !strings.Contains(got, "--no-collector.example") {
		t.Errorf("ToArgs = %q, want --no-collector.example", got)
	}
}

func TestLoadRejectsUnknownTopLevelKey(t *testing.T) {
	path := writeConfig(t, "nonsense_section: true\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an unknown top-level key; parsing must be strict")
	}
}

func TestLoadParsesModulesAndInstances(t *testing.T) {
	path := writeConfig(t, `
modules:
  default:
    http_client_config:
      basic_auth: { username: monitor, password: hunter2 }
instances:
  - { name: lib-a, address: https://a.example.net, labels: { site: paris } }
  - { name: lib-b, address: https://b.example.net, module: default }
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Modules) != 1 {
		t.Fatalf("Modules = %v, want exactly one", c.Modules)
	}
	if len(c.Instances) != 2 {
		t.Fatalf("Instances = %d, want 2", len(c.Instances))
	}
	if c.Instances[0].Name != "lib-a" || c.Instances[0].Labels["site"] != "paris" {
		t.Errorf("instance 0 = %+v, want name lib-a with site=paris", c.Instances[0])
	}
	if c.Instances[1].Module != "default" {
		t.Errorf("instance 1 module = %q, want default", c.Instances[1].Module)
	}
}

func TestLoadValidatesModuleHTTPClientConfig(t *testing.T) {
	path := writeConfig(t, `
modules:
  broken:
    http_client_config:
      basic_auth: { username: monitor, password: inline, password_file: /etc/pw }
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a module whose basic_auth sets both password and password_file")
	}
}

func TestLoadResolvesModulePathsAgainstTheFile(t *testing.T) {
	path := writeConfig(t, `
modules:
  tls:
    http_client_config:
      tls_config: { ca_file: certs/corp.pem }
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(filepath.Dir(path), "certs/corp.pem")
	if got := c.Modules["tls"].HTTPClientConfig.TLSConfig.CAFile; got != want {
		t.Errorf("module ca_file = %q, want %q (relative to the config file)", got, want)
	}
}

func TestLoadResolvesRelativePathsAgainstTheFile(t *testing.T) {
	path := writeConfig(t, "http_client_config:\n  tls_config:\n    ca_file: certs/corp.pem\n")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(filepath.Dir(path), "certs/corp.pem")
	if got := c.HTTPClientConfig.TLSConfig.CAFile; got != want {
		t.Errorf("ca_file = %q, want %q (relative to the config file, not the working directory)", got, want)
	}
}

func TestLoadRejectsInvalidHTTPClientConfig(t *testing.T) {
	path := writeConfig(t, `
http_client_config:
  basic_auth:
    username: monitor
    password: inline
    password_file: /etc/pw
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted basic_auth with both password and password_file")
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	path := writeConfig(t, `
http_client_config:
  basic_auth:
    username: monitor
    password: hunter2
`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := c.HTTPClientConfig.String()
	if strings.Contains(rendered, "hunter2") {
		t.Error("the rendered config leaked the password")
	}
	if !strings.Contains(rendered, "<secret>") {
		t.Errorf("rendered = %q, want the redaction token", rendered)
	}
}

func TestCLIFlagNames(t *testing.T) {
	got := CLIFlagNames([]string{
		"--log.level=debug",
		"--web.listen-address", ":9341",
		"--no-collector.example",
		"--", "--not-a-flag",
	})
	for _, want := range []string{"log.level", "web.listen-address", "collector.example"} {
		if !got[want] {
			t.Errorf("CLIFlagNames missing %q, got %v", want, got)
		}
	}
	if got["not-a-flag"] {
		t.Error("a token after the -- terminator was treated as a flag name")
	}
}

func TestExtractFlagValue(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"equals form", []string{"--config.file=/etc/c.yml"}, "/etc/c.yml"},
		{"separate form", []string{"--config.file", "/etc/c.yml"}, "/etc/c.yml"},
		{"absent", []string{"--log.level=debug"}, ""},
		{"after terminator", []string{"--", "--config.file=/etc/c.yml"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractFlagValue(tc.argv, "config.file"); got != tc.want {
				t.Errorf("ExtractFlagValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// Validate reads the flag model before Parse has run. Assert that is legal
// rather than trusting it: the whole layer depends on it.
func TestModelIsReadableBeforeParse(t *testing.T) {
	f := newApp()
	if len(f.app.Model().Flags) == 0 {
		t.Fatal("Model() returned no flags before Parse")
	}
	if _, err := f.app.Parse([]string{"--log.level=debug"}); err != nil {
		t.Fatalf("Parse after Model failed: %v", err)
	}
	if *f.level != "debug" {
		t.Errorf("log.level = %q, want debug: reading Model() must not disturb parsing", *f.level)
	}
}

func TestResolveInstancesRejectsEmptyList(t *testing.T) {
	if _, err := (&Config{}).ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted an empty instance list")
	}
}

func TestResolveInstancesRejectsDuplicateNames(t *testing.T) {
	c := &Config{Instances: []Instance{
		{Name: "a", Address: "https://a.example.net"},
		{Name: "a", Address: "https://b.example.net"},
	}}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted two instances named the same")
	}
}

func TestResolveInstancesRejectsNonHTTPAddress(t *testing.T) {
	c := &Config{Instances: []Instance{{Name: "a", Address: "ftp://a.example.net"}}}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted a non-http address")
	}
}

func TestResolveInstancesRejectsLabelCollidingWithIdentifier(t *testing.T) {
	c := &Config{Instances: []Instance{
		{Name: "a", Address: "https://a.example.net", Labels: map[string]string{"target": "x"}},
	}}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted an instance label reusing the identifying label")
	}
}

func TestResolveInstancesRejectsLabelCollidingWithCollectorLabel(t *testing.T) {
	c := &Config{Instances: []Instance{
		{Name: "a", Address: "https://a.example.net", Labels: map[string]string{"collector": "x"}},
	}}
	if _, err := c.ResolveInstances("target", "collector"); err == nil {
		t.Fatal("ResolveInstances accepted an instance label reusing a collector-emitted label")
	}
}

func TestResolveInstancesResolvesDefaultModuleFromTopLevel(t *testing.T) {
	// v0.4 compat: no modules: section, top-level http_client_config is default.
	hc := &promconfig.HTTPClientConfig{}
	c := &Config{
		HTTPClientConfig: hc,
		Instances:        []Instance{{Name: "a", Address: "https://a.example.net"}},
	}
	got, err := c.ResolveInstances("target")
	if err != nil {
		t.Fatalf("ResolveInstances: %v", err)
	}
	if got[0].ClientConfig != hc {
		t.Error("the default module did not resolve to the top-level http_client_config")
	}
}

func TestResolveInstancesRejectsUnknownModule(t *testing.T) {
	c := &Config{
		Modules:   map[string]Module{"known": {}},
		Instances: []Instance{{Name: "a", Address: "https://a.example.net", Module: "ghost"}},
	}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted a reference to an undeclared module")
	}
}

func TestResolveInstancesRejectsModuleCollectorsSubset(t *testing.T) {
	c := &Config{
		Modules:   map[string]Module{"m": {Collectors: []string{"example"}}},
		Instances: []Instance{{Name: "a", Address: "https://a.example.net", Module: "m"}},
	}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted a module with a collectors: subset under multi-instance")
	}
}

func TestResolveInstancesRejectsHeterogeneousLabelKeys(t *testing.T) {
	c := &Config{Instances: []Instance{
		{Name: "a", Address: "https://a.example.net", Labels: map[string]string{"rack": "r1"}},
		{Name: "b", Address: "https://b.example.net"},
	}}
	if _, err := c.ResolveInstances("target"); err == nil {
		t.Fatal("ResolveInstances accepted instances with different label key sets (would panic at registry registration)")
	}
}

func TestResolveInstancesAcceptsUniformLabelKeys(t *testing.T) {
	c := &Config{Instances: []Instance{
		{Name: "a", Address: "https://a.example.net", Labels: map[string]string{"rack": "r1"}},
		{Name: "b", Address: "https://b.example.net", Labels: map[string]string{"rack": "r2"}},
	}}
	if _, err := c.ResolveInstances("target"); err != nil {
		t.Fatalf("ResolveInstances rejected uniform label keys: %v", err)
	}
}

func TestRejectModulesAndInstances(t *testing.T) {
	if err := (&Config{}).RejectModulesAndInstances(); err != nil {
		t.Errorf("errored on an empty config: %v", err)
	}
	if err := (&Config{Modules: map[string]Module{"x": {}}}).RejectModulesAndInstances(); err == nil {
		t.Error("accepted a modules: section")
	}
	if err := (&Config{Instances: []Instance{{Name: "a"}}}).RejectModulesAndInstances(); err == nil {
		t.Error("accepted an instances: section")
	}
}

func TestResolveModulesReturnsNilWithNoSection(t *testing.T) {
	got, err := (&Config{}).ResolveModules()
	if err != nil {
		t.Fatalf("ResolveModules: %v", err)
	}
	if got != nil {
		t.Errorf("ResolveModules() = %v, want nil when no modules: section is declared", got)
	}
}

func TestResolveModulesKeepsCollectorsAndClientConfig(t *testing.T) {
	hc := &promconfig.HTTPClientConfig{}
	c := &Config{Modules: map[string]Module{
		"prod":  {HTTPClientConfig: hc},
		"disks": {Collectors: []string{"disks"}},
	}}
	got, err := c.ResolveModules()
	if err != nil {
		t.Fatalf("ResolveModules: %v", err)
	}
	if got["prod"].ClientConfig != hc {
		t.Errorf("module prod lost its client config")
	}
	if len(got["disks"].Collectors) != 1 || got["disks"].Collectors[0] != "disks" {
		t.Errorf("module disks collectors = %v, want [disks]", got["disks"].Collectors)
	}
	if got["disks"].ClientConfig != nil {
		t.Errorf("module disks invented a client config")
	}
}

// Rule 9: with modules declared, nothing reads the top-level section, so
// accepting both would silently ignore one of two places an operator wrote
// credentials.
func TestResolveModulesRejectsTopLevelClientConfigAlongsideModules(t *testing.T) {
	c := &Config{
		HTTPClientConfig: &promconfig.HTTPClientConfig{},
		Modules:          map[string]Module{"prod": {}},
	}
	_, err := c.ResolveModules()
	if err == nil {
		t.Fatal("ResolveModules accepted a modules: section alongside a top-level http_client_config:")
	}
	if !strings.Contains(err.Error(), "http_client_config") {
		t.Errorf("error does not name the offending section: %v", err)
	}
}

func TestDiffFlagsNamesEveryChangedKey(t *testing.T) {
	boot := &Config{Flags: map[string]interface{}{
		"log.level":          "info",
		"web.listen-address": []interface{}{":9999"},
	}}
	next := &Config{Flags: map[string]interface{}{
		"log.level":         "debug", // changed
		"collector.example": false,   // added
		// web.listen-address removed
	}}

	got := DiffFlags(boot, next)
	want := []string{"collector.example", "log.level", "web.listen-address"}
	if len(got) != len(want) {
		t.Fatalf("DiffFlags returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DiffFlags returned %v, want %v (sorted)", got, want)
		}
	}
}

func TestDiffFlagsIsEmptyForIdenticalSections(t *testing.T) {
	a := &Config{Flags: map[string]interface{}{"log.level": "info", "x": []interface{}{1, 2}}}
	b := &Config{Flags: map[string]interface{}{"x": []interface{}{1, 2}, "log.level": "info"}}
	if got := DiffFlags(a, b); len(got) != 0 {
		t.Fatalf("DiffFlags reported %v for two identical sections", got)
	}
}

func TestDiffFlagsHandlesAbsentSections(t *testing.T) {
	if got := DiffFlags(&Config{}, &Config{}); len(got) != 0 {
		t.Fatalf("DiffFlags reported %v for two configs with no flags section", got)
	}
	got := DiffFlags(&Config{}, &Config{Flags: map[string]interface{}{"log.level": "debug"}})
	if len(got) != 1 || got[0] != "log.level" {
		t.Fatalf("DiffFlags returned %v; adding a flags section must be reported", got)
	}
}
