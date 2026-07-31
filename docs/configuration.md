# Configuration Reference

> Back to [README](../README.md)

## Usage

The exporter is configured through command-line flags. Optionally, a YAML file
passed via `--config.file` can set some of those same flags and add settings no
flag can express, such as authentication for outbound requests; see
[Configuration file](#configuration-file) below. Nothing changes for a
deployment that never passes `--config.file`.

**Basic execution, flags only:**

```bash
./tapelibrary_exporter --web.listen-address=":9170"
```

**Using a configuration file for web settings (TLS/Basic Auth):**

```bash
./tapelibrary_exporter --web.config.file=/path/to/web-config.yml
```

For the `web-config.yml` format, see
[Prometheus's TLS/Basic Auth configuration reference](https://prometheus.io/docs/prometheus/latest/configuration/https/).
`tapelibrary_exporter` parses this file with the same `prometheus/exporter-toolkit` library
Prometheus itself uses.

**View help and every available flag:**

```bash
./tapelibrary_exporter --help
```

### Command-line options

| Flag | Description | Default |
|------|-------------|---------|
| `--web.listen-address` | Address to listen on for web interface and telemetry | `:9170` |
| `--web.config.file` | Path to a configuration file for TLS/Basic Auth | (none) |
| `--config.file` | Path to this exporter's own YAML configuration file (see [Configuration file](#configuration-file)). Not the same thing as `--web.config.file`. **Required** for a multi-instance build (`--target-model multi-instance`); optional and empty by default everywhere else | (none) |
| `--log.level` | Log level: `debug`, `info`, `warn`, `error` | `info` |
| `--log.format` | Log format: `json`, `text` | `text` |
| `--web.disable-exporter-metrics` | Exclude Go runtime and process metrics from `/metrics` | `false` |
| `--[no-]collector.<name>` | *(single-target and multi-instance builds)* Enable or disable a collector (kingpin boolean flag) | see below |
| `--exporter.max-requests-per-target` | *(single-target and multi-instance builds only, absent on `multi`)* Bounds how many outbound requests/commands this exporter has in flight at once. `0` means unlimited. Scope differs by build; see [Bounding outbound concurrency](#bounding-outbound-concurrency) below | `0` |
| `--web.enable-lifecycle` | *(multi-target and multi-instance builds only)* Expose `POST /-/reload`, which reloads `--config.file`. `SIGHUP` always works and needs no flag; see [Configuration reload](#configuration-reload-multi-and-multi-instance-builds-only) below | `false` |
| `--probe.target-allowlist` | *(multi-target builds only, `--target-model multi`)* Restrict `/probe` to these target hosts (repeatable; host or host:port, exact match) | (empty = allow-any) |
| `--probe.timeout` | *(multi-target builds only)* Ceiling on each `/probe` request's own deadline | `5s` |
| `--probe.timeout-offset` | *(multi-target builds only)* Subtracted from Prometheus's scrape timeout when computing a probe's deadline, so the exporter answers before Prometheus abandons the scrape | `0.5s` |

In **single-target builds**, individual collectors may also expose their own flags. The bundled `example` collector does:

| Flag | Description | Default | Flavor |
|------|-------------|---------|--------|
| `--collector.example.timeout` | Per-request/per-command timeout for the `example` collector | `5s` | HTTP and CLI |
| `--collector.example.target` | Base URL the `example` collector fetches | `https://<library-address>/web/api/v1` | HTTP only |

In **multi-target builds** (`--target-model multi`), the `example` collector is built fresh per `/probe` request: its target and timeout come from the request (bounded by the `--probe.*` flags above), so it exposes no `--collector.example.*` flags, and multi has no `--[no-]collector.<name>` toggle.

In **multi-instance builds** (`--target-model multi-instance`), the `example` collector is the background-refresh variant: `--collector.example.timeout` and `--collector.example.interval` (background refresh period, default `5m`) apply the same way to every watched instance, but there is no `--collector.example.target`, since each instance's address comes from `instances:` in the configuration file (see below), not a flag. `--[no-]collector.example` still toggles it on or off, same as single-target.

This build's own collectors follow that same multi-instance pattern:

| Flag | Description | Default |
|------|-------------|---------|
| `--collector.library.timeout` | Per-request timeout for the `library` collector | `5s` |
| `--collector.library.interval` | Background refresh interval for the `library` collector | `5m` |
| `--[no-]collector.library` | Enable or disable the `library` collector | enabled |

There is no single global command/request timeout: each collector owns its own, following
the `example` collector's pattern above. Run `--help` after adding your own collectors to see
the full, current flag list.

### Available collectors

| Collector | Default | Description |
|-----------|---------|-------------|
| `example` | enabled | Starter collector: replace with your real data source (see `CONTRIBUTING.md`) |
| `library` | enabled | Library status, capacity and cartridge counters, and identity, from `GET /v1/library` |
| `http_client_requests` *(HTTP flavor)* | enabled | Self-instrumentation: HTTP request duration by outcome |
| `command_exec` *(CLI flavor)* | enabled | Self-instrumentation: command execution duration by outcome |

Both the `example` collector and the self-instrumentation histogram are registered through the
same `--[no-]collector.<name>` mechanism: there is nothing special about self-instrumentation
from the flag's point of view.

### Enabling and disabling collectors

Use `--[no-]collector.<name>` (kingpin boolean syntax) to enable or disable a collector.

**Example: disable the self-instrumentation collector**

```bash
./tapelibrary_exporter --no-collector.http_client_requests
```

**Example: custom timeout and logging**

```bash
./tapelibrary_exporter \
  --collector.example.timeout=10s \
  --log.level=debug \
  --log.format=json
```

## Configuration file

`--config.file` optionally points at a YAML file that sets flag values and,
for an HTTP-flavor exporter, the outbound authentication and TLS settings no
flag can express. It is **empty by default**, so nothing in this section
changes anything for a deployment that never passes `--config.file`.

```bash
./tapelibrary_exporter --config.file=config.example.yml
```

A commented, ready-to-copy example ships at the repo root:
[`config.example.yml`](../config.example.yml).

**`--config.file` is not `--web.config.file`.** `--config.file` configures
what this exporter does: its own flags, and how it authenticates to whatever
it queries. `--web.config.file` is a separate flag, from
`prometheus/exporter-toolkit`, that configures the TLS/Basic Auth the
exporter's own HTTP server presents to Prometheus. They govern opposite
directions of the same deployment and neither substitutes for the other.

The file has two sections, and one rule decides which a setting belongs to:
anything a command-line flag can already express goes under `flags:`, keyed
by the flag's own long name (without the leading `--`). Anything a flag
cannot express, such as `basic_auth` or `tls_config` for an HTTP-flavor
exporter's outbound requests, gets its own section instead,
`http_client_config:`. No setting is ever expressible in both places, so a
value never has two conflicting sources.

`http_client_config:` is HTTP-flavor only. A CLI-flavor exporter runs a local
command rather than issuing HTTP requests, so it has nothing to authenticate.
Given that section it exits with an error instead of starting, because a
setting that looks applied but does nothing is worse than a refusal.

**Precedence, highest wins:**

1. Command line
2. `--config.file`
3. Process environment
4. Each flag's own built-in default

A value set on the command line always overrides the same key set in the
file. The file itself sits above the process environment, not below it: for
a flag that also reads an environment variable, the file wins if both are
set.

Paths written inside the file (`password_file`, `ca_file`, `cert_file`,
`key_file`, and similar) resolve relative to the file's own location, not to
the exporter's working directory. Prefer a `*_file` form over an inline
secret, `password_file` over `password` for example, so the secret itself
never has to live in the file.

## Bounding outbound concurrency

`--exporter.max-requests-per-target` (default `0`, unlimited) bounds how
many outbound requests (HTTP flavor) or command invocations (CLI flavor)
this exporter has in flight at once. It exists because a slow or throttled
target can otherwise receive as many concurrent requests as this exporter
has collectors and background pollers willing to issue at the same moment,
turning one slow target into a source of starvation for its own siblings.
It is absent on multi-target builds (`--target-model multi`): `/probe`
builds a client per incoming request from a caller-controlled target, so
there is no fixed, finite key space to index a ceiling by the way the other
two models have.

**What the ceiling actually bounds differs by build, and the difference
matters before you set a number:**

- **Single-target, HTTP flavor**: one ceiling per distinct
  `--collector.<name>.target` address, via a shared `LimiterSet`. Two
  collectors pointed at the same address share one ceiling; two collectors
  pointed at different addresses are bounded independently.
- **Single-target, CLI flavor**: one ceiling for the whole process. There is
  no per-target `Client` to index by, only the single package-level command
  boundary every collector calls through, so this flag caps total concurrent
  command invocations across every collector regardless of which machine or
  binary each one targets.
- **Multi-instance builds**: one ceiling per watched *instance* (the
  `instances:` entry, from `--config.file`), not per physical address. Two
  `instances:` entries that happen to name the same physical machine are
  bounded **independently**, not jointly, so this exporter's aggregate
  concurrency against that one machine can exceed the configured value. This
  is a deliberate trade-off, not an oversight: a per-address index would
  either need to be pre-populated to cover an instance a later reload adds,
  or rebuilt on every reload, and per-instance does neither.

A request that has to wait for a slot is not silent: `tapelibrary_exporter_request_wait_seconds`
(see [docs/metrics.md](metrics.md)) records how long, so queuing shows up in
your monitoring instead of just quietly lengthening scrape times. The wait
is bounded by the collector's own `--collector.<name>.timeout`, which is why
a positive timeout is required once a ceiling is configured: without one,
the wait would have no deadline of its own to honor.

## Configuration reload (`multi` and `multi-instance` builds only)

A single-target build has no reload at all: its `--config.file`, if any,
holds only a `flags:` section (applied once at startup, and a running
process cannot adopt a new value for a flag) and an `http_client_config:`
section whose file-backed secrets and TLS material `prometheus/common`
already re-reads from disk on every outbound request, so there is nothing
left for a reload to apply. That covers every `_file` variant the section
accepts: `username_file`, `password_file`, `credentials_file`,
`bearer_token_file`, `client_secret_file`, `client_certificate_key_file`,
`ca_file`, `cert_file` and `key_file`. See `SECURITY.md` for the full
reasoning.

Multi-target and multi-instance builds both reload `--config.file` in
place, two ways:

- **`SIGHUP`**: always available, no flag needed. Sending it already
  requires being on the machine, which is why it needs no separate opt-in.
- **`POST /-/reload`**: gated behind `--web.enable-lifecycle` (default
  `false`), the same conservative-default posture as everywhere else in this
  scaffold: an unauthenticated exporter that also exposed an unauthenticated
  way to force a reload would be a real change to the default security
  posture. With the flag unset, this route answers `404` for every method,
  exactly as if it were never registered: the flag is checked, and the
  request refused, before it ever reaches the code that would distinguish
  `POST` from anything else. With the flag set, `GET /-/reload` (and any
  method other than `POST`) instead answers `405 Method Not Allowed` with
  an `Allow: POST` header.

**On a `multi` build with no `--config.file` set** (optional and empty by
default there, see above), `SIGHUP` and `POST /-/reload` are a no-op
success, not a failure: there is nothing on disk to re-read, so
`tapelibrary_exporter_config_last_reload_successful` stays at `1` rather
than dropping to `0`. A `multi-instance` build never hits this case:
`--config.file` is required for that target model.

**A reload either fully succeeds or changes nothing.** Everything that can
fail (re-reading the file, parsing it, validating it, building any new
transport a credential change needs) happens before anything currently
running is touched. If any of it fails, the process keeps serving exactly
the configuration it had before, `tapelibrary_exporter_config_last_reload_successful`
drops to `0`, and the failure is logged at `error` with the specific reason.
A successful reload sets that same gauge back to `1` and advances
`tapelibrary_exporter_config_last_reload_success_timestamp_seconds`; see
[docs/metrics.md](metrics.md).

**A changed `flags:` section refuses the whole reload**, naming the keys
that changed: those are applied once at startup by rendering them into
kingpin arguments, and a running process has no mechanism to re-parse them,
so accepting the rest of a file while silently ignoring a `flags:` edit
would leave the process describing neither the old configuration nor the
new one. Restart the process to pick up a `flags:` change.

**On a multi-instance build specifically, a reload cannot change which
label *keys* an instance's series carry**, though it can freely change
their *values*: adding an instance label, removing one, or renaming one
across every instance is refused with a restart-required error, because a
Prometheus registry never releases a metric family's label-name dimension
once it has registered a series under it, even after every series of that
family is later unregistered, so silently allowing it would panic rather
than reload. Everything else on this build reloads with no restart: which
instances exist, an instance's address, its credentials, and the values of
its labels.

## The `/probe?target=` endpoint (multi-target builds only)

Multi-target builds (scaffolded with `--target-model multi`, http flavor
only) also expose `/probe?target=<url>`: Prometheus's own
[multi-target exporter pattern](https://prometheus.io/docs/guides/multi-target-exporter/):
each request builds a fresh registry and collector set scoped to `target`,
instead of the fixed, process-lifetime registry `/metrics` reports on. A
single-target build (`--target-model single`, the default) has no `/probe`
endpoint at all; everything below only applies once `--target-model multi`
was chosen at scaffold time.

```bash
curl 'http://localhost:9170/probe?target=http://some-host:9100'
```

- **Always-on floor:** `target` must parse as an `http`/`https` URL:
  a missing scheme, `file://`, or any other scheme gets a `400`, regardless of
  the allowlist below.
- **`--probe.target-allowlist`** (repeatable, empty by default): when set, a
  target's host must match one of these entries exactly (case- and
  trailing-dot-sensitive; matches the parsed host or `host:port` verbatim:
  `node1` will not match `NODE1` or `node1.`) or the probe returns `403`.
  Empty (the default) accepts any target that clears the floor above.
- **`X-Prometheus-Scrape-Timeout-Seconds`**: Prometheus sends this header on
  every scrape; the handler computes the probe's own deadline as
  `min(--probe.timeout, X-Prometheus-Scrape-Timeout-Seconds - --probe.timeout-offset)`,
  so a probe never outruns Prometheus's deadline or the operator's configured
  ceiling. `--probe.timeout-offset` (default `0.5s`) is subtracted from
  Prometheus's own scrape timeout so the exporter answers before Prometheus
  abandons the scrape: a probe that uses its full budget and replies a
  moment too late is a wasted scrape. `--probe.timeout` (default `5s`) is
  the ceiling applied when the header is missing, unparseable, or would
  leave no budget once the offset is subtracted.
- **SSRF posture**: `/probe` making this exporter fetch an operator-supplied
  URL is a deliberate, ecosystem-standard trade-off (Blackbox/SNMP/IPMI all
  work this way): see [SECURITY.md](../SECURITY.md) before exposing this
  endpoint beyond a trusted network.

### Selecting a module

`?module=` names one or more modules declared in `--config.file`'s `modules:`
section. It is repeatable and comma-separated, so `?module=a&module=b,c`
selects a, b and c. For the YAML shape of that section, see the commented
`modules:` block in `config.example.yml` at the repository root, which spells
out both conventions (one complete bundle per group of targets, or
credentials and collector subsets as independent axes); the `modules:` sample
further down this page is the multi-instance variant, where a module is
resolved once at boot rather than per request.

- **Collectors combine.** The probe runs the union of the selected modules'
  `collectors:` lists, always in the exporter's own declared order. A module
  that lists none contributes none, and if the union ends up empty, for
  example because the only module selected was a credentials-only one such as
  `default`, every registered collector runs: the same fallback an absent
  `module=` parameter takes.
- **Credentials do not combine.** A request makes one connection, so it needs
  one set of credentials. They resolve in this order, first hit wins: the
  unique selected module carrying an `http_client_config:`, then a module
  named `default`, then the top-level `http_client_config:`. Selecting two
  modules that both carry credentials returns **400**.
- **A probe that resolves no credentials against a file that declares some
  returns 400.** Probing in the clear would return 200 with series nobody
  questions; the 400 makes `up` go to 0 and shows in your monitoring.
- **An unknown module returns 400**, and a request naming no module at all
  runs every collector.

A matching scrape config carries the module as a label and relabels it:

```yaml
  - job_name: 'tapelibrary_exporter-probe'
    metrics_path: /probe
    file_sd_configs: [{ files: ['targets/*.yml'] }]   # each entry carries a `module` label
    relabel_configs:
      - { source_labels: [__address__],    target_label: __param_target }
      - { source_labels: [module],         target_label: __param_module }
      - { source_labels: [__param_target], target_label: instance }
      - { target_label: __address__, replacement: 'exporter-host:9170' }
```

## The multi-instance target model (multi-instance builds only)

A multi-instance build (scaffolded with `--target-model multi-instance`, http
flavor only) watches a fixed list of machines declared in a configuration
file, instead of one target baked in at scaffold time (single-target) or one
target per `/probe` request (multi-target). `--config.file` is **required**
for this build: without it there is no instance list to load, and the
exporter refuses to start. Single-target and multi-target builds keep
`--config.file` optional, unchanged.

```bash
./tapelibrary_exporter --config.file=config.yml
```

### The `modules:` and `instances:` sections

On top of the `flags:` and `http_client_config:` sections described above, a
multi-instance config file adds two more:

```yaml
# modules: named credential/TLS bundles, one exporter-wide set an instance
# can reference by name.
modules:
  default:
    http_client_config:
      basic_auth:
        username: monitor
        password_file: /etc/tapelibrary_exporter/default.pass
  privileged:
    http_client_config:
      tls_config:
        ca_file: /etc/tapelibrary_exporter/internal-ca.pem

# instances: the machines this process watches. Required: at least one.
instances:
  - name: machine-a
    address: https://machine-a.example.net
    labels:
      site: paris
  - name: machine-b
    address: https://machine-b.example.net
    module: privileged
```

- **`modules:`** is optional. Each entry names an `http_client_config:`
  bundle; an instance picks one by name in its own `module:` key. Leaving
  `module:` off an instance uses the `default` module; with no `modules:`
  section at all, a top-level `http_client_config:` (if present) is used
  instead, so a plain pre-multi-instance config file keeps working
  unchanged.
- **`instances:`** is required and needs at least one entry. Each instance
  needs a unique `name` and an `address` that parses as an `http`/`https`
  URL; `module` is optional (`default` if omitted); `labels` are optional
  extra key/value pairs added to every series that instance emits. All
  instances must declare the same set of label keys.
- A module's `collectors:` key (used on multi-target builds to narrow a
  `/probe` request) is meaningless here, since collector enablement is
  global via `--[no-]collector.<name>` under multi-instance; setting it on
  any module in the file is refused at boot rather than silently ignored.
- An instance's `labels:` may not reuse the identifying label (see below).

Any problem here (a missing instance list, a duplicate name, an address that
is not `http`/`https`, an unknown module, a label collision) fails at boot
with a clear message, not at the first scrape.

### Instance label

Every per-collector series this exporter emits, across every instance and every
collector including the per-collector health metrics, carries one identifying
label: `library` by default, for example
`tapelibrary_exporter_collector_success{collector="example", library="machine-a"}`.
The two exceptions are `tapelibrary_exporter_request_duration_seconds` and
`tapelibrary_exporter_request_wait_seconds`: both single, process-wide
histograms shared across all instances, so neither carries
`library`. Both are labeled only by `outcome`.
Its name is fixed at scaffold time via `scaffold.sh --instance-label`, never
a runtime flag. It is applied by the exporter itself, per instance, rather
than by any collector's own descriptor, so `make docs-check` cannot see or
verify it; keeping the name fixed at scaffold time is what lets
`docs/metrics.md` state it as fact instead of describing a value that could
drift at runtime with nothing left to catch it.

### Prometheus configuration for a multi-instance build

A multi-instance build is scraped exactly like a single-target one: one
static target, no relabeling.

```yaml
scrape_configs:
  - job_name: 'tapelibrary_exporter'
    static_configs:
      - targets: ['tapelibrary_exporter.example.internal:9170']
```

`scrape_timeout` only has to cover this one process answering from its own
caches, not N live fetches against N instances: `/metrics` never talks to an
instance directly. Each instance's collectors are background pollers that
refresh a cache on their own schedule (`--collector.<name>.interval`); a
scrape just reads whatever is already cached.

---

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: 'tapelibrary_exporter'
    scrape_interval: 30s
    scrape_timeout: 30s
    static_configs:
      - targets: ['tapelibrary_exporter.example.internal:9170']
```

- **scrape_interval**: pick a value that comfortably exceeds how long a full scrape takes
  against your real target: start at 30s and tighten only once you've measured
  `tapelibrary_exporter_collector_duration_seconds` under load.
- **scrape_timeout**: should be equal to or less than `scrape_interval`, to avoid
  `context_deadline_exceeded` errors piling up in Prometheus itself.

Check the config:

```bash
promtool check-config prometheus.yml
```

### Internal exporter metrics

Every collector, including the bundled `example` one, is wrapped by a shared status tracker
that emits two self-monitoring metrics regardless of what the collector itself reports:

| Metric | Description | Labels |
|---|---|---|
| `tapelibrary_exporter_collector_success` | `1` if the last scrape emitted at least one metric and did not panic, `0` otherwise | `collector` |
| `tapelibrary_exporter_collector_duration_seconds` | Wall time of the last `Collect()` call | `collector` |

These let you alert per-collector, independently of Prometheus's own global `up` metric.

In addition, each I/O flavor exposes a histogram timing its own outbound calls:

| Metric | Description | Labels | Flavor |
|---|---|---|---|
| `tapelibrary_exporter_request_duration_seconds` | Duration of HTTP requests issued by collectors | `outcome` (`success`/`error`) | HTTP |
| `tapelibrary_exporter_command_duration_seconds` | Duration of external commands executed by collectors | `outcome` (`success`/`error`) | CLI |
| `tapelibrary_exporter_request_wait_seconds` | Duration a request or command waited for a concurrency slot, by outcome. Both series stay at a permanent zero once `--exporter.max-requests-per-target` is unset (the default); see [Bounding outbound concurrency](#bounding-outbound-concurrency) above | `outcome` (`success`/`error`) | HTTP and CLI |

On a multi-target or multi-instance build, two further gauges report the
health of the configuration-reload mechanism itself (absent on
single-target builds, which have no reload; see
[Configuration reload](#configuration-reload-multi-and-multi-instance-builds-only)
above):

| Metric | Description | Labels |
|---|---|---|
| `tapelibrary_exporter_config_last_reload_successful` | Whether the last configuration reload attempt succeeded (`1`) or failed (`0`) | - |
| `tapelibrary_exporter_config_last_reload_success_timestamp_seconds` | Unix time of the last successful configuration reload | - |

---

### Performance considerations

- **Per-collector timeouts**: raise a collector's own `--collector.<name>.timeout` flag if
  your target is slow to respond, but remember Prometheus's `scrape_timeout` still applies
  across *all* collectors combined, so a single slow collector can still starve the others.
- **Scrape interval**: use an interval that comfortably exceeds your measured
  `tapelibrary_exporter_collector_duration_seconds` to avoid overlapping scrapes.
- **Collector selection**: disable collectors you don't need to reduce load:

  ```bash
  ./tapelibrary_exporter --no-collector.http_client_requests
  ```
