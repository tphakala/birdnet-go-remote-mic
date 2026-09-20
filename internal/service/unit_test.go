//go:build linux

package service

import (
	"strings"
	"testing"
)

// wantLines asserts every expected line appears as a COMPLETE line in the
// rendered unit (split on newlines) and that no unexpanded template placeholder
// remains, so a regression that merges two directives onto one line or leaves a
// stray {{...}} fails rather than passing a loose substring match.
func wantLines(t *testing.T, got string, lines ...string) {
	t.Helper()
	if strings.Contains(got, "{{") {
		t.Errorf("rendered unit contains an unexpanded template placeholder:\n%s", got)
	}
	have := make(map[string]bool)
	for _, l := range strings.Split(got, "\n") {
		have[l] = true
	}
	for _, l := range lines {
		if !have[l] {
			t.Errorf("rendered unit missing exact line %q\n---\n%s", l, got)
		}
	}
}

func TestRenderDefaults(t *testing.T) {
	b, err := Render(ServiceSpec{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(b)
	wantLines(t, got,
		"Type=simple",
		"User=remote-mic",
		"Group=remote-mic",
		"SupplementaryGroups=audio",
		"After=network-online.target sound.target",
		"Wants=network-online.target",
		"Environment=REMOTEMIC_CONFIG=/etc/remote-mic/config.yaml",
		"ExecStartPre=-/usr/local/bin/remote-mic serve --check --cert-dir=/var/lib/remote-mic",
		"ExecStart=/usr/local/bin/remote-mic serve --cert-dir=/var/lib/remote-mic",
		"Restart=on-failure",
		"RestartSec=5",
		"WorkingDirectory=/var/lib/remote-mic",
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"ProtectHome=true",
		"PrivateTmp=true",
		"ReadWritePaths=/etc/remote-mic /var/lib/remote-mic",
		"WantedBy=multi-user.target",
	)
}

func TestRenderCustom(t *testing.T) {
	b, err := Render(ServiceSpec{
		User:       "birdmic",
		BinPath:    "/opt/remote-mic/bin/remote-mic",
		ConfigPath: "/etc/birdmic/config.yaml",
		StateDir:   "/var/lib/birdmic",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(b)
	wantLines(t, got,
		"User=birdmic",
		"Group=birdmic",
		"Environment=REMOTEMIC_CONFIG=/etc/birdmic/config.yaml",
		"ExecStart=/opt/remote-mic/bin/remote-mic serve --cert-dir=/var/lib/birdmic",
		"ReadWritePaths=/etc/birdmic /var/lib/birdmic",
	)
}

func TestRenderRejectsBadSpec(t *testing.T) {
	cases := map[string]ServiceSpec{
		"relative bin":    {BinPath: "bin/remote-mic"},
		"relative config": {ConfigPath: "config.yaml"},
		"relative state":  {StateDir: "state"},
		"bad user":        {User: "bad user"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(spec); err == nil {
				t.Fatalf("Render(%+v) = nil error, want rejection", spec)
			}
		})
	}
}
