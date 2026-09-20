//go:build linux

package service

import (
	"strings"
	"testing"
)

// wantLines asserts every expected line is present verbatim in the rendered
// unit, so a template change that drops or mangles a critical directive fails.
func wantLines(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(got, l) {
			t.Errorf("rendered unit missing line %q\n---\n%s", l, got)
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
		"User=remote-mic",
		"Group=remote-mic",
		"SupplementaryGroups=audio",
		"Environment=REMOTEMIC_CONFIG=/etc/remote-mic/config.yaml",
		"ExecStartPre=-/usr/local/bin/remote-mic serve --check",
		"ExecStart=/usr/local/bin/remote-mic serve --cert-dir=/var/lib/remote-mic",
		"Restart=on-failure",
		"WorkingDirectory=/var/lib/remote-mic",
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
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
