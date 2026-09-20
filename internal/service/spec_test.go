//go:build linux

package service

import "testing"

func TestValidateAcceptsDefaults(t *testing.T) {
	if err := (ServiceSpec{}).withDefaults().Validate(); err != nil {
		t.Fatalf("default spec rejected: %v", err)
	}
	// A dedicated custom layout is fine.
	ok := ServiceSpec{User: "birdmic", BinPath: "/opt/birdmic/remote-mic", ConfigPath: "/etc/birdmic/config.yaml", StateDir: "/var/lib/birdmic"}
	if err := ok.withDefaults().Validate(); err != nil {
		t.Fatalf("dedicated custom spec rejected: %v", err)
	}
}

func TestValidateRejectsSharedDirs(t *testing.T) {
	// The installer chowns the config dir recursively and --purge removes it, so
	// a config whose parent is a shared system dir must be refused.
	cases := map[string]ServiceSpec{
		"config in /etc":           {ConfigPath: "/etc/remote-mic.yaml"},       // ConfigDir == /etc
		"state is /var/lib":        {StateDir: "/var/lib"},                     // state dir shared
		"config parent is root":    {ConfigPath: "/config.yaml"},               // ConfigDir == /
		"state is /usr/local":      {StateDir: "/usr/local"},                   //
		"config in /var/lib":       {ConfigPath: "/var/lib/remote.yaml"},       // ConfigDir == /var/lib
		"state is /usr/local/bin":  {StateDir: "/usr/local/bin"},               // the default bin dir
		"config in /usr/local/bin": {ConfigPath: "/usr/local/bin/config.yaml"}, // ConfigDir == /usr/local/bin
		"state is /var/tmp":        {StateDir: "/var/tmp"},                     //
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if err := spec.withDefaults().Validate(); err == nil {
				t.Fatalf("Validate accepted a shared system dir: %+v", spec)
			}
		})
	}
}

func TestValidateRejectsBadUser(t *testing.T) {
	// "root" is charset-valid but explicitly refused (least privilege).
	cases := []string{"", "-o", "Bad", "user name", "root/../x", "a%b", "root", "toolongtoolongtoolongtoolongtoolongx"}
	for _, u := range cases {
		spec := ServiceSpec{User: u}.withDefaults()
		spec.User = u // withDefaults would fill an empty user; force the tested value
		if err := spec.Validate(); err == nil {
			t.Errorf("Validate accepted invalid user %q", u)
		}
	}
}

func TestValidateRejectsBadPaths(t *testing.T) {
	// systemd splits on spaces and reads '%' as a specifier, so such paths must
	// be refused rather than silently misparsed in the unit.
	cases := map[string]ServiceSpec{
		"space in state dir":  {StateDir: "/var/lib/remote mic"},
		"percent in bin path": {BinPath: "/usr/local/bin/remote%mic"},
		"newline in config":   {ConfigPath: "/etc/remote-mic/co\nnfig.yaml"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if err := spec.withDefaults().Validate(); err == nil {
				t.Fatalf("Validate accepted a malformed path: %+v", spec)
			}
		})
	}
}
