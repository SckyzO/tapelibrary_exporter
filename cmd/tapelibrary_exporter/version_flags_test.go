package main

import "testing"

// TestWantsVersionOrHelp is the regression test for a bug found on 2026-08-04
// by running the artefact `make build` actually produces, rather than a
// `go build` of the same package.
//
// This target model requires --config.file, and main enforced that BEFORE
// kingpin.Parse, which is where --version and --help are handled. So
// `tapelibrary_exporter --version` exited 1 with "the multi-instance target
// model requires --config.file", and so did --help. Both are wrong: packaging,
// CI and container healthchecks call --version on a binary they have no
// configuration for, and --help is the first thing an operator runs.
//
// Every earlier run this session passed --config.file, because it was starting
// the exporter rather than interrogating it, which is exactly why nothing
// caught this until the binary itself was asked to describe itself.
func TestWantsVersionOrHelp(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want bool
	}{
		"--version alone":                {[]string{"--version"}, true},
		"--help alone":                   {[]string{"--help"}, true},
		"-h alone":                       {[]string{"-h"}, true},
		"--version after other flags":    {[]string{"--log.level=debug", "--version"}, true},
		"no arguments at all":            {nil, false},
		"a normal run":                   {[]string{"--config.file=/etc/x.yml"}, false},
		"--web.listen-address only":      {[]string{"--web.listen-address=:9170"}, false},
		"a path merely containing it":    {[]string{"--config.file=/etc/version.yml"}, false},
		"--version=false is not a query": {[]string{"--version=false"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := wantsVersionOrHelp(tc.args); got != tc.want {
				t.Errorf("wantsVersionOrHelp(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
