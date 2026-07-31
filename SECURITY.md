# Security Policy

## Reporting a vulnerability

If your hosting forge supports private vulnerability reporting (for example GitHub's
**Security** tab -> **Report a vulnerability**), please use that channel. Otherwise, contact
`sckyzo` through a private channel instead of opening a public issue.

Do not open a public issue or pull request for a suspected vulnerability.

Include, where possible: the affected version, the flags/environment the exporter was running
with, and steps to reproduce. Reports are acknowledged and triaged on a best-effort basis.

## Supported versions

| Version | Supported |
|---|---|
| Latest release | yes |
| Older releases | no |

Only the latest release receives security fixes.

## Security practices

- `make vuln` (govulncheck) runs locally and reports only the vulnerabilities reachable from
  the code. Wire it into CI for continuous coverage.
- `make osv` scans every dependency against the OSV database.
- `gosec` findings (weak crypto, unsafe file inclusion, missing request timeouts, etc.) are
  part of the `make lint` gate, not an optional extra.
- This exporter never places secrets in a metric or a label: no passwords, tokens, API keys,
  certificate paths, or passphrases are exposed via `/metrics`: that endpoint is public and
  unauthenticated by default. A collector that touches sensitive data must filter it out at
  parse time, before it ever reaches a `prometheus.Desc`.
- TLS and Basic Authentication are available, opt-in, via `--web.config.file` (see
  [docs/configuration.md](docs/configuration.md)). The exporter itself does not force a
  particular hardening posture by default: the right one depends on your network, and an
  unannounced change to a security-relevant default would be a breaking change.
- Container images run as a dedicated non-root user; the minimal image variant additionally
  runs on a distroless base with no shell and no package manager (see
  [Security & supply chain](README.md#security--supply-chain) in the README).
- If you use the GitHub Actions layer (`--forge github` at scaffold time), this repository
  also ships: a weekly Trivy scan of published images (CVE regressions fail the workflow), a
  `govulncheck` run on every pull request, and Dependabot-managed dependency updates across
  Go modules, GitHub Actions, and container base images.

## Configuration file

`--config.file` is optional and empty by default: pass no such flag and this
exporter reads nothing from disk. If you do use it, its `http_client_config:`
section is the one place in a generated repository that can hold credentials,
so treat that file as a secret.

- **Prefer the `_file` variants to inline values.** `password_file`,
  `credentials_file`, `bearer_token_file`, `client_secret_file`,
  `client_certificate_key_file`, `ca_file`, `cert_file` and `key_file` keep
  the secret in its own file, which you can permission and rotate
  separately, and keep it out of the configuration you might paste into an
  issue. `password`, `credentials`, `bearer_token`, `client_secret`,
  `client_certificate_key` and `key` accept the value inline; that is
  supported, not recommended.
- **Restrict the file's permissions.** `0600` owned by the user running the
  exporter is enough, whether it is read once at startup only (a
  single-target build, or `multi`/`multi-instance` before their first
  reload) or re-read later by SIGHUP/`POST /-/reload` (see below): every
  read, at startup or at reload, is done by that same process running as
  that same user, so the permission bound does not change. The same goes
  for any file a `_file` key points at.
- **Paths inside the file resolve relative to the file itself**, not to the
  working directory the exporter was started from. A unit started from `/` and
  a shell started from the repository therefore read the same certificate.
- Nothing from this section reaches `/metrics`: this exporter never places a
  configuration value in a metric or a label, and it never logs the parsed
  configuration. Note that `prometheus/common`'s `Secret` type redacts itself
  when the configuration is marshalled back to YAML or JSON, not when it is
  formatted with `%v`, so a collector you add must not print one either.
- `config.example.yml` ships at the repository root as documentation. It is
  never loaded unless you point `--config.file` at it, and its
  `http_client_config:` block is commented out, so it holds no credentials as
  shipped.

Credentials declared in `--config.file`, whether in the top-level
`http_client_config:` or in a `modules:` entry, are read at startup. On a
multi-target build (`--target-model multi`) or a multi-instance build
(`--target-model multi-instance`), SIGHUP (always) and `POST /-/reload`
(once `--web.enable-lifecycle` is set) re-read them without restarting the
process. A single-target build (`--target-model single`, the default) has no
reload at all: its configuration file holds only a `flags:` section, applied
once at startup and never revisited, and an `http_client_config:` section
whose file-backed secrets and TLS material `prometheus/common` already
re-reads from disk on every outbound request, so there is nothing left for a
reload to apply there. That covers every `_file` variant the section
accepts: `username_file`, `password_file`, `credentials_file`,
`bearer_token_file`, `client_secret_file`, `client_certificate_key_file`,
`ca_file`, `cert_file` and `key_file`. Rotating a `_file`-backed credential on a
single-target build therefore takes effect on its own, with no restart and
no reload; rotating an inline value (`password`, `bearer_token`, `key`, none
of which are recommended, see above) still means restarting the process,
since nothing re-parses the configuration file to pick it up. A module is
selected per request (`?module=` on a multi-target build's `/probe`) or once
at boot (an instance's `module:` key on a multi-instance build); either way
it is never a metric label and never reaches any series this exporter
exposes.

`POST /-/reload` is a mutating endpoint. It is gated behind
`--web.enable-lifecycle`, default `false`: the same conservative-default
reasoning as everything else in this file, since shipping every generated
exporter an unauthenticated way to force a configuration reload would be the
one change here that degrades the default posture of an operator who
configured nothing. SIGHUP needs no flag: sending it already requires being
on the machine. See [docs/configuration.md](docs/configuration.md) for the
full reload surface (what SIGHUP and `POST /-/reload` do, and what a refused
reload leaves running).

See [docs/configuration.md](docs/configuration.md) for the file's format and
the precedence rules.

## Multi-target `/probe` (if applicable)

Multi-target builds (`--target-model multi`) also expose a
`/probe?target=…` endpoint (see [docs/configuration.md](docs/configuration.md))
that makes this exporter issue outbound HTTP/HTTPS requests to whatever
`target=` a caller supplies. That is an SSRF primitive by construction, the
same shape as the Blackbox, SNMP, and IPMI exporters, and the same
considerations apply here:

- **Default = allow-any.** With `--probe.target-allowlist` unset (the
  default), `/probe` accepts any `target` that clears the always-on
  `http`/`https` floor (`file://`, `gopher://`, and scheme-less targets are
  rejected `400` regardless of the allowlist). This mirrors the ecosystem
  default and is what lets `/probe` work out of the box with Prometheus
  service discovery: a static allowlist fights dynamic SD, which is exactly
  why the ecosystem defaults to allow-any rather than the other way round.
- **An outbound probe carries the credentials of the module it selected.**
  Once `--config.file` declares a `modules:` section with
  `http_client_config:` entries, the connection a probe opens presents
  whatever that module declares: a basic-auth password, a bearer token, a
  client certificate. The module is chosen by the request itself
  (`&module=`), falling back to a `default` module and then to the top-level
  `http_client_config:`. So a caller who reaches an unrestricted `/probe`
  picks both the host this exporter connects to **and** which credentials it
  presents there. That makes an unrestricted `/probe` a
  credential-exfiltration primitive, not only an SSRF one, and the more
  modules one exporter concentrates (prod, staging, one per tenant), the
  more there is to pick from. `--probe.target-allowlist` is what bounds
  which hosts can ever receive them: on a build that carries credentials,
  treat it as containment rather than hygiene.
- **Harden with `--probe.target-allowlist <host>` (repeatable).** When set,
  a target's host must match an entry or the probe returns `403`. Matching is
  **exact-string, case- and trailing-dot-sensitive** against `target=`'s
  parsed host (or `host:port`): an entry `node1` will **not** match `NODE1`
  or `node1.`; list every allowed host exactly as it appears in your
  `target=` values.
- **Startup warning.** With no allowlist configured, the exporter logs one
  visible, non-fatal warning at startup: anyone who can reach this exporter
  can make it issue requests to arbitrary HTTP/HTTPS hosts. It never refuses
  to start over this (same posture as the exposed-and-unauthenticated
  warning above).
- **Two module-related refusals, both `400`**, documented in
  [docs/configuration.md](docs/configuration.md) under "Selecting a module":
  a request naming an unknown module is refused rather than silently probed
  with everything, and a request that resolves no credentials at all against
  a configuration that declares some is refused rather than probed in the
  clear. The second one is what keeps a forgotten `&module=` from quietly
  downgrading an authenticated probe into an unauthenticated one that still
  answers `200`.
- **Isolate the exporter's network regardless of the allowlist.** As with any
  allow-any-by-default prober, the real hardening boundary is network
  placement: run `/probe` somewhere that cannot reach anything you don't
  want probed, the same posture recommended for the Blackbox/SNMP exporters.

Released binaries and images are signed. Binaries carry a CycloneDX software bill of materials;
container images get their own CycloneDX SBOM via `make sbom-image`, plus a supplementary SPDX
attestation embedded in the image manifest: see
[Security & supply chain](README.md#security--supply-chain) in the README and
[docs/release-process.md](docs/release-process.md) for the release pipeline that produces
them.
