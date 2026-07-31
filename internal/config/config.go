// Package config loads this exporter's optional YAML configuration file,
// selected with --config.file.
//
// The file has two sections and one rule decides which a setting belongs to:
// anything a flag can express goes under "flags:", keyed by the flag's own
// name; anything a flag cannot express (authentication, TLS) gets its own
// section. Nothing is expressible in both, so no value ever has two sources.
//
// Resolution works by rendering "flags:" back into command-line arguments and
// letting kingpin parse them once, rather than writing values into flags after
// parsing. kingpin applies a flag's default only when that flag is absent from
// the parsed arguments, so a value written in afterwards would ADD to the
// default of a repeatable flag such as --web.listen-address instead of
// replacing it. Feeding kingpin arguments also means it performs every type
// conversion and validation itself.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	promconfig "github.com/prometheus/common/config"
	"go.yaml.in/yaml/v2"
)

// Config is the parsed content of --config.file.
type Config struct {
	// Flags maps a declared flag name to the value the file gives it. The
	// value stays untyped because YAML supplies three shapes: a scalar
	// (log.level: debug), a list for a repeatable flag
	// (web.listen-address: [":9341"]), and a bool (collector.example:
	// false). kingpin converts when it parses the rendered arguments.
	Flags map[string]interface{} `yaml:"flags,omitempty"`

	// HTTPClientConfig carries the authentication and TLS no flag can
	// express. It is a pointer so an absent section is distinguishable from
	// an empty one: with no section the flavor wiring keeps calling
	// NewClient, whose transport is the one every existing deployment
	// already runs.
	HTTPClientConfig *promconfig.HTTPClientConfig `yaml:"http_client_config,omitempty"`

	// Modules are named credential/TLS bundles, Blackbox-shaped. A
	// multi-instance instance references one by name for its transport. A nil
	// HTTPClientConfig means the default transport. (The multi /probe model
	// will select one per request in a later version.)
	Modules map[string]Module `yaml:"modules,omitempty"`

	// Instances is the list of machines a multi-instance exporter watches.
	// Meaningful only under the multi-instance target model; single and multi
	// reject it at boot.
	Instances []Instance `yaml:"instances,omitempty"`
}

// Module is a named bundle of an optional collector subset and an optional
// outbound HTTP client config, shaped like a Blackbox exporter module.
type Module struct {
	// Collectors narrows a probe to this subset. Honoured only under the multi
	// (/probe) model; multi-instance enables collectors globally via the
	// --collector.<name> flags and refuses this key at boot (see
	// ResolveInstances).
	Collectors []string `yaml:"collectors,omitempty"`

	// HTTPClientConfig carries the authentication and TLS this module applies,
	// the same promconfig.HTTPClientConfig the top-level section uses. A nil
	// value means the default transport.
	HTTPClientConfig *promconfig.HTTPClientConfig `yaml:"http_client_config,omitempty"`
}

// Instance is one machine a multi-instance exporter watches. It references a
// module for its credentials by name and never carries inline authentication,
// keeping the "no value has two sources" rule the flags layer relies on.
type Instance struct {
	Name    string            `yaml:"name"`
	Address string            `yaml:"address"`
	Module  string            `yaml:"module,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

// Load reads and validates path. An empty path yields a zero Config that
// renders no arguments: that is the "no --config.file" path, and it stays
// behaviourally identical to this package not existing.
func Load(path string) (*Config, error) {
	if path == "" {
		return &Config{}, nil
	}

	// #nosec G304 -- reading an operator-supplied path is the entire purpose of
	// --config.file. The path comes from the command line of the process being
	// started, so whoever supplies it already controls the process; there is no
	// privilege boundary to cross. This is the same posture exporter-toolkit
	// takes for --web.config.file.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var c Config
	// UnmarshalStrict, not Unmarshal: prometheus/common/config's own package
	// documentation instructs callers to use it, and an unknown key is a typo
	// the operator wants to hear about rather than a setting silently dropped.
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Paths inside the file (ca_file, password_file, ...) are relative to the
	// file, not the process working directory. An exporter started by systemd
	// runs from /, so the difference decides whether it works. Resolve the
	// directory once and apply it to the top-level client config and to every
	// module's.
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve the directory of %s: %w", path, err)
	}

	if c.HTTPClientConfig != nil {
		c.HTTPClientConfig.SetDirectory(dir)
		if err := c.HTTPClientConfig.Validate(); err != nil {
			return nil, fmt.Errorf("invalid http_client_config in %s: %w", path, err)
		}
	}

	// Sorted so a config with two broken modules always fails on the same one.
	moduleNames := make([]string, 0, len(c.Modules))
	for name := range c.Modules {
		moduleNames = append(moduleNames, name)
	}
	sort.Strings(moduleNames)
	for _, name := range moduleNames {
		hc := c.Modules[name].HTTPClientConfig
		if hc == nil {
			continue
		}
		hc.SetDirectory(dir)
		if err := hc.Validate(); err != nil {
			return nil, fmt.Errorf("invalid http_client_config in module %q of %s: %w", name, path, err)
		}
	}

	return &c, nil
}

// Validate reports any key under "flags:" that names no declared flag, that
// carries no value, or whose value the YAML parser decoded to a Go bool for a
// flag that is not itself boolean.
//
// That last check exists because the pinned parser, go.yaml.in/yaml/v2, is a
// YAML 1.1 implementation: it resolves the unquoted scalars y, Y, yes, Yes,
// YES, n, N, no, No, NO, on, On, ON, off, Off, OFF, true and false (in every
// case variant) to a Go bool before this package, or the destination flag's
// type, ever enters the picture. A legitimate string value that happens to be
// one of those words, for example log.level: on, would otherwise decode to
// true and renderFlag would emit the bare --log.level instead of
// --log.level=on, silently corrupting the argument list. Quoting the value
// (log.level: "on") keeps it a YAML string and sidesteps the resolver
// entirely. A bool decoded for a flag that IS boolean, such as
// collector.example: false, is exactly what is expected and is accepted.
//
// It runs before the arguments are rendered so the operator reads "unknown
// flag in the config file" instead of kingpin's bare "unknown long flag",
// which would not say where the name came from.
//
// Every flag is declared by the time this runs, whichever target model this
// exporter was scaffolded with. A single-target main declares its own in a var
// block, the flavor wiring declares the collector's at // @@CLIENT_INIT@@, and
// register declares one --[no-]collector.<name> per collector. A multi-target
// main has neither of the last two: it declares its own flags, in the var block
// and in main itself, and its collectors take their target per request rather
// than from a flag. Either way the declarations all precede the call site.
func (c *Config) Validate(app *kingpin.Application) error {
	if len(c.Flags) == 0 {
		return nil
	}

	declared := make(map[string]*kingpin.FlagModel)
	for _, f := range app.Model().Flags {
		declared[f.Name] = f
	}

	var unknown, empty, notBool, notScalar, selfRef []string
	for name, v := range c.Flags {
		// config.file names this very file. It is a declared flag, so the
		// unknown-key check below would accept it, and ToArgs would render it
		// into arguments that arrive far too late to matter: the path was
		// already read from os.Args before parsing. Rejecting it says so,
		// rather than letting the key sit there doing nothing.
		if name == "config.file" {
			selfRef = append(selfRef, name)
			continue
		}
		f, ok := declared[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		if v == nil {
			empty = append(empty, name)
			continue
		}
		if _, isBool := v.(bool); isBool && !f.IsBoolFlag() {
			notBool = append(notBool, name)
			continue
		}
		// A nested block under a flag name is a shape no flag can take.
		// Without this, renderFlag's default case would format the map with
		// %v and hand kingpin --name=map[a:1 b:2], which a string flag
		// accepts without complaint: the same silent corruption the boolean
		// check above exists to stop.
		switch v.(type) {
		case map[interface{}]interface{}, map[string]interface{}:
			notScalar = append(notScalar, name)
		}
	}

	// Map iteration order is randomised; sort so the message is reproducible.
	sort.Strings(unknown)
	sort.Strings(empty)
	sort.Strings(notBool)
	sort.Strings(notScalar)
	sort.Strings(selfRef)

	var parts []string
	switch {
	case len(unknown) > 0 && len(empty) > 0:
		parts = append(parts, fmt.Sprintf("unknown flag(s) under \"flags:\": %s; flag(s) with no value: %s",
			strings.Join(unknown, ", "), strings.Join(empty, ", ")))
	case len(unknown) > 0:
		parts = append(parts, fmt.Sprintf("unknown flag(s) under \"flags:\": %s", strings.Join(unknown, ", ")))
	case len(empty) > 0:
		parts = append(parts, fmt.Sprintf("flag(s) with no value under \"flags:\": %s", strings.Join(empty, ", ")))
	}
	if len(notScalar) > 0 {
		parts = append(parts, fmt.Sprintf(
			"flag(s) given a nested block instead of a value: %s; a flag takes a scalar, or a list if it is repeatable",
			strings.Join(notScalar, ", ")))
	}
	if len(selfRef) > 0 {
		parts = append(parts, "config.file cannot be set from inside the configuration file; pass it on the command line")
	}
	if len(notBool) > 0 {
		parts = append(parts, fmt.Sprintf(
			"flag(s) read as boolean by YAML 1.1 (unquoted on, off, yes and no all resolve to true/false) but not declared as boolean flags: %s; quote the value to keep it a string, for example %s: \"on\"",
			strings.Join(notBool, ", "), notBool[0]))
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("config file: %s", strings.Join(parts, "; "))
}

// ToArgs renders "flags:" as command-line arguments, omitting any flag already
// present on the real command line so a value never has two sources. The
// result is meant to be prepended to os.Args[1:], which is what makes the
// command line win without any precedence being computed.
//
// Call Validate first: ToArgs assumes every key names a declared flag and
// carries a value.
func (c *Config) ToArgs(setOnCLI map[string]bool) []string {
	if len(c.Flags) == 0 {
		return nil
	}

	names := make([]string, 0, len(c.Flags))
	for name := range c.Flags {
		names = append(names, name)
	}
	// Deterministic output: a reproducible argument list makes a failure
	// reproducible too.
	sort.Strings(names)

	var args []string
	for _, name := range names {
		if setOnCLI[name] {
			continue
		}
		args = append(args, renderFlag(name, c.Flags[name])...)
	}
	return args
}

// renderFlag turns one key and value into zero or more arguments. A bool
// becomes --name or --no-name, kingpin's own negation form. A list repeats the
// flag, which is exactly how a repeatable flag accumulates on a real command
// line.
func renderFlag(name string, v interface{}) []string {
	switch t := v.(type) {
	case bool:
		if t {
			return []string{"--" + name}
		}
		return []string{"--no-" + name}
	case []interface{}:
		args := make([]string, 0, len(t))
		for _, item := range t {
			args = append(args, fmt.Sprintf("--%s=%v", name, item))
		}
		return args
	default:
		return []string{fmt.Sprintf("--%s=%v", name, v)}
	}
}

// CLIFlagNames returns the set of long flag names present in argv, which must
// not include the program name (pass os.Args[1:]). Only presence matters, so
// values are ignored and no type information is needed.
//
// kingpin's own IsSetByUser cannot serve here. It has to be attached when a
// flag is declared, so every template and fragment would have to change; it is
// impossible for the flags exporter-toolkit declares through webflag.AddFlags,
// which does not return its clauses; and it only answers after Parse, which is
// too late to decide what Parse is given.
//
// Known limit: a flag VALUE that itself begins with "--" is counted as a flag
// name, which would drop a key the file legitimately sets. kingpin rejects such
// a command line on the following token, so the failure is loud, not silent.
func CLIFlagNames(argv []string) map[string]bool {
	names := make(map[string]bool)
	for _, tok := range argv {
		if tok == "--" {
			break // everything after the terminator is positional
		}
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		name := strings.TrimPrefix(tok, "--")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			continue
		}
		names[name] = true
		// --no-collector.example negates the flag named collector.example.
		// Record both spellings rather than guessing which one is declared:
		// this set is only ever used to omit a key, so recording one name too
		// many is safe, while missing one would let a value have two sources.
		names[strings.TrimPrefix(name, "no-")] = true
	}
	return names
}

// ExtractFlagValue returns the value of one long flag from argv (again without
// the program name), handling both --name=value and --name value. It exists
// because --config.file has to be read before kingpin parses anything: its
// value decides which arguments kingpin is given.
func ExtractFlagValue(argv []string, name string) string {
	for i, tok := range argv {
		if tok == "--" {
			return ""
		}
		switch {
		case tok == "--"+name:
			if i+1 < len(argv) {
				return argv[i+1]
			}
			return ""
		case strings.HasPrefix(tok, "--"+name+"="):
			return strings.TrimPrefix(tok, "--"+name+"=")
		}
	}
	return ""
}

// ResolvedInstance is one validated instance ready to wire: its module
// reference resolved to a client config (nil meaning the default transport)
// and its extra labels checked against the identifying label.
type ResolvedInstance struct {
	Name         string
	Address      string
	Labels       map[string]string
	ClientConfig *promconfig.HTTPClientConfig
}

// ResolveInstances validates the instances section for the multi-instance
// target model and resolves each instance's module reference. instanceLabel is
// the identifying label scaffold.sh baked in (default "target"); an instance's
// own labels may not reuse it. It fails on the first problem, the boot posture
// the design mandates.
func (c *Config) ResolveInstances(instanceLabel string, reservedLabels ...string) ([]ResolvedInstance, error) {
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("the multi-instance model requires at least one instance under \"instances:\"")
	}

	// Label keys an instance may not reuse: the identifying label this exporter
	// applies itself, plus any label a collector already emits (passed by the
	// caller, which knows its collectors). Reusing one collides at registration.
	forbidden := map[string]string{instanceLabel: "the identifying label this exporter applies itself"}
	for _, r := range reservedLabels {
		forbidden[r] = "a label a collector already emits"
	}

	// A module carrying "collectors:" is meaningless under multi-instance
	// (collector enablement is global via --collector.<name>); refuse it rather
	// than silently ignore it. Sorted for a reproducible failure.
	moduleNames := make([]string, 0, len(c.Modules))
	for name := range c.Modules {
		moduleNames = append(moduleNames, name)
	}
	sort.Strings(moduleNames)
	for _, name := range moduleNames {
		if len(c.Modules[name].Collectors) > 0 {
			return nil, fmt.Errorf("module %q sets \"collectors:\", which multi-instance does not honour (collector enablement is global via --collector.<name>); remove it", name)
		}
	}

	seen := make(map[string]bool, len(c.Instances))
	out := make([]ResolvedInstance, 0, len(c.Instances))
	for i, inst := range c.Instances {
		if inst.Name == "" {
			return nil, fmt.Errorf("instance %d has no name", i)
		}
		if seen[inst.Name] {
			return nil, fmt.Errorf("instance name %q declared twice", inst.Name)
		}
		seen[inst.Name] = true

		if err := validateInstanceAddress(inst.Address); err != nil {
			return nil, fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		for k := range inst.Labels {
			if reason, bad := forbidden[k]; bad {
				return nil, fmt.Errorf("instance %q sets label %q, which is %s; rename it", inst.Name, k, reason)
			}
		}

		hc, err := c.resolveModule(inst.Module)
		if err != nil {
			return nil, fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		out = append(out, ResolvedInstance{
			Name:         inst.Name,
			Address:      inst.Address,
			Labels:       inst.Labels,
			ClientConfig: hc,
		})
	}

	// Every instance must declare the same set of label keys. WrapRegistererWith
	// folds them into each metric's ConstLabels, and the shared registry rejects
	// two collectors that share a metric name but differ in label-key dimension,
	// which would otherwise surface as an opaque panic at registration.
	if len(c.Instances) > 1 {
		refKeys := sortedLabelKeys(c.Instances[0].Labels)
		for _, inst := range c.Instances[1:] {
			if keys := sortedLabelKeys(inst.Labels); !equalStrings(keys, refKeys) {
				return nil, fmt.Errorf("instance %q declares label keys %v but instance %q declares %v; every instance must declare the same label keys", inst.Name, keys, c.Instances[0].Name, refKeys)
			}
		}
	}
	return out, nil
}

// resolveModule resolves an instance's module reference to a client config. An
// empty reference means the "default" module. The default module is either an
// explicit modules.default or, when no modules: section is present, the
// top-level http_client_config (the v0.4.0 compatibility rule). A missing
// default resolves to nil: the default transport.
func (c *Config) resolveModule(name string) (*promconfig.HTTPClientConfig, error) {
	if name == "" {
		name = "default"
	}
	if len(c.Modules) == 0 {
		if name == "default" {
			return c.HTTPClientConfig, nil // nil when no top-level section either
		}
		return nil, fmt.Errorf("references module %q but the config declares no modules", name)
	}
	m, ok := c.Modules[name]
	if !ok {
		return nil, fmt.Errorf("references unknown module %q", name)
	}
	return m.HTTPClientConfig, nil
}

// ResolvedModule is one validated module: the collector subset it selects and
// the client config its probes authenticate with, nil meaning the default
// transport.
type ResolvedModule struct {
	Collectors   []string
	ClientConfig *promconfig.HTTPClientConfig
}

// ResolveModules validates the modules section for the multi target model.
// Unlike multi-instance, multi HONOURS a module's "collectors:" key: there it
// selects the probe's collector subset.
//
// A modules: section alongside a top-level http_client_config: is refused.
// With modules declared, nothing reads the top-level section, so accepting
// both would silently ignore one of the two places an operator wrote
// credentials. That refusal is also what makes credential resolution
// unambiguous in internal/probe: the top-level client is reachable only when
// no module is.
//
// It builds no *http.Client. This package stays a parsing and validation
// layer with no I/O, exactly as ResolveInstances leaves it; the caller builds
// the clients so a failure there can name the module it came from.
func (c *Config) ResolveModules() (map[string]ResolvedModule, error) {
	if len(c.Modules) == 0 {
		return nil, nil
	}
	if c.HTTPClientConfig != nil {
		return nil, fmt.Errorf("config file: \"modules:\" and a top-level \"http_client_config:\" cannot both be set; with modules declared the top-level section has no reader, so move it into a module (a module named \"default\" is the one a probe gets when it names none)")
	}

	out := make(map[string]ResolvedModule, len(c.Modules))
	for name, m := range c.Modules {
		out[name] = ResolvedModule{
			Collectors:   m.Collectors,
			ClientConfig: m.HTTPClientConfig,
		}
	}
	return out, nil
}

// validateInstanceAddress enforces that an address parses as an http/https URL,
// the same floor a /probe target must clear.
func validateInstanceAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("has no address")
	}
	u, err := url.Parse(addr)
	if err != nil {
		return fmt.Errorf("address %q is not a valid URL: %w", addr, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("address %q scheme %q not allowed (only http/https)", addr, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("address %q has no host", addr)
	}
	return nil
}

// sortedLabelKeys returns m's keys sorted, for a deterministic comparison and
// error message.
func sortedLabelKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// RejectModulesAndInstances refuses a modules: or instances: section, which a
// single-target exporter cannot act on. A single-target main calls this so a
// misconfiguration fails loudly rather than being silently ignored.
func (c *Config) RejectModulesAndInstances() error {
	if len(c.Modules) > 0 {
		return fmt.Errorf("config file: \"modules:\" is only used by multi-target exporters; this is a single-target exporter")
	}
	if len(c.Instances) > 0 {
		return fmt.Errorf("config file: \"instances:\" is only used by multi-instance exporters; this is a single-target exporter")
	}
	return nil
}

// DiffFlags returns the sorted names of every "flags:" key whose value differs
// between a and b, including keys present in only one of them.
//
// It exists because "flags:" cannot be reloaded. This package renders that
// section back into command-line arguments and lets kingpin parse them exactly
// once (see the package comment for why writing into flags after Parse is
// wrong), so a running process cannot adopt a new value for one. A reload that
// finds this section changed therefore refuses the whole reload and names these
// keys, rather than applying the other sections and leaving the process
// describing neither the old file nor the new one.
//
// reflect.DeepEqual is the right comparison here: the values are whatever YAML
// produced (scalars, bools, and []interface{} for repeatable flags), never
// functions or channels.
func DiffFlags(a, b *Config) []string {
	seen := make(map[string]bool, len(a.Flags)+len(b.Flags))
	for k := range a.Flags {
		seen[k] = true
	}
	for k := range b.Flags {
		seen[k] = true
	}

	var changed []string
	for k := range seen {
		av, aok := a.Flags[k]
		bv, bok := b.Flags[k]
		if aok != bok || !reflect.DeepEqual(av, bv) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}
