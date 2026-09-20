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
		"config in /etc":        {ConfigPath: "/etc/remote-mic.yaml"}, // ConfigDir == /etc
		"state is /var/lib":     {StateDir: "/var/lib"},               // state dir shared
		"config parent is root": {ConfigPath: "/config.yaml"},         // ConfigDir == /
		"state is /usr/local":   {StateDir: "/usr/local"},             //
		"config in /var/lib":    {ConfigPath: "/var/lib/remote.yaml"}, // ConfigDir == /var/lib
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
	cases := []string{"", "-o", "Bad", "user name", "root/../x", "a%b", "toolongtoolongtoolongtoolongtoolongx"}
	for _, u := range cases {
		spec := ServiceSpec{User: u}.withDefaults()
		spec.User = u // withDefaults would fill an empty user; force the tested value
		if err := spec.Validate(); err == nil {
			t.Errorf("Validate accepted invalid user %q", u)
		}
	}
}
