//go:build linux

package service

import (
	"slices"
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
	for l := range strings.SplitSeq(got, "\n") {
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
		"After=network-online.target sound.target remote-fs.target",
		"Wants=network-online.target",
		"Environment=REMOTEMIC_CONFIG=/etc/remote-mic/config.yaml",
		"ExecStartPre=-/usr/local/bin/remote-mic serve --check --cert-dir=/var/lib/remote-mic",
		"ExecStart=/usr/local/bin/remote-mic serve --cert-dir=/var/lib/remote-mic",
		"Restart=always",
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

func TestRenderUpdaterDefaults(t *testing.T) {
	pathUnit, serviceUnit, err := RenderUpdater(ServiceSpec{})
	if err != nil {
		t.Fatalf("RenderUpdater: %v", err)
	}
	wantLines(t, string(pathUnit),
		"[Path]",
		"PathExists=/var/lib/remote-mic/update/request.json",
		"PathExists=/usr/local/bin/remote-mic.pending",
		"Unit=remote-mic-update.service",
		"WantedBy=multi-user.target",
	)
	svc := string(serviceUnit)
	wantLines(t, svc,
		"Type=oneshot",
		"ExecStart=/usr/local/bin/remote-mic service apply-update --bin-path=/usr/local/bin/remote-mic --state-dir=/var/lib/remote-mic",
		"NoNewPrivileges=true",
		"ProtectSystem=strict",
		"PrivateNetwork=true",
		"ProtectKernelTunables=true",
		"ProtectKernelModules=true",
		"ProtectKernelLogs=true",
		"ProtectControlGroups=true",
		"ProtectClock=true",
		"LockPersonality=true",
		"RestrictRealtime=true",
		"RestrictSUIDSGID=true",
		"RestrictNamespaces=true",
		"RestrictAddressFamilies=AF_UNIX",
		"ReadWritePaths=/usr/local/bin /var/lib/remote-mic",
		"StartLimitBurst=10",
		"StartLimitIntervalSec=10min",
		"TimeoutStartSec=10min",
	)
	// The updater must run as root to replace a root-owned binary; a User=
	// line would silently make every update fail.
	if strings.Contains(svc, "User=") {
		t.Errorf("updater unit sets a user:\n%s", svc)
	}
	// SystemCallFilter= and CapabilityBoundingSet= were weighed and left out: the
	// updater swaps a root-owned binary, restarts a service and runs the new
	// binary, and a list that misses one call or capability it needs would fail
	// every update.
	for _, key := range []string{"SystemCallFilter=", "CapabilityBoundingSet="} {
		if strings.Contains(svc, key) {
			t.Errorf("updater unit sets %s, which was decided against:\n%s", key, svc)
		}
	}
}

func TestRenderUpdaterCustom(t *testing.T) {
	pathUnit, serviceUnit, err := RenderUpdater(ServiceSpec{BinPath: "/opt/remote-mic/bin/remote-mic", StateDir: "/srv/remote-mic"})
	if err != nil {
		t.Fatalf("RenderUpdater: %v", err)
	}
	wantLines(t, string(pathUnit), "PathExists=/srv/remote-mic/update/request.json", "PathExists=/opt/remote-mic/bin/remote-mic.pending")
	wantLines(t, string(serviceUnit),
		"ExecStart=/opt/remote-mic/bin/remote-mic service apply-update --bin-path=/opt/remote-mic/bin/remote-mic --state-dir=/srv/remote-mic",
		"ReadWritePaths=/opt/remote-mic/bin /srv/remote-mic",
	)
	if _, _, err := RenderUpdater(ServiceSpec{StateDir: "/var/lib"}); err == nil {
		t.Error("RenderUpdater accepted a shared state directory")
	}
}

// TestHiddenDir pins the lexical match serve --check uses to flag a file
// source path the installed service cannot see: a hidden directory itself or
// anything under it, never a sibling that only shares a name prefix, and
// /run/user without the rest of /run.
func TestHiddenDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		wantDir string
	}{
		{"/home", "/home"},
		{"/home/pi/a.flac", "/home"},
		{"/homework/a.flac", ""},
		{"/root/a.wav", "/root"},
		{"/run/user/1000/a.wav", "/run/user"},
		{"/run/media/usb/a.wav", ""},
		{"/tmp/a.opus", "/tmp"},
		{"/var/tmp", "/var/tmp"},
		{"/var/tmpfiles/a.wav", ""},
		{"/srv/audio", ""},
	}
	for _, tc := range tests {
		dir, ok := HiddenDir(tc.path)
		if dir != tc.wantDir || ok != (tc.wantDir != "") {
			t.Errorf("HiddenDir(%q) = %q, %v, want %q, %v", tc.path, dir, ok, tc.wantDir, tc.wantDir != "")
		}
	}
}

// TestHiddenDirsMatchReleasedUnits keeps hiddenDirs in step with the unit
// templates: every row is a setting some released unit has, every setting a
// released unit uses to hide paths from the service has a row (so a template
// that changes ProtectHome= or adds TemporaryFileSystem= fails here until the
// table covers it), and every hidden directory is one the CLI already treats as
// private (privateDirs).
func TestHiddenDirsMatchReleasedUnits(t *testing.T) {
	t.Parallel()
	watched := []string{"ProtectHome", "PrivateTmp", "InaccessiblePaths", "TemporaryFileSystem"}
	seen := make(map[string]bool)
	for _, text := range releasedUnitTexts {
		for line := range strings.SplitSeq(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if key, val, ok := entry(line); ok && slices.Contains(watched, key) {
				seen[key+"="+val] = true
			}
		}
	}
	rows := make(map[string]bool)
	for _, row := range hiddenDirs {
		rows[row.setting] = true
		if !seen[row.setting] {
			t.Errorf("hiddenDirs row %q is set by no released unit", row.setting)
		}
		for _, dir := range row.dirs {
			if !slices.ContainsFunc(privateDirs, func(p string) bool { return within(dir, p) }) {
				t.Errorf("hidden dir %q lies in no privateDirs entry %v", dir, privateDirs)
			}
		}
	}
	for setting := range seen {
		if !rows[setting] {
			t.Errorf("a released unit sets %q, which no hiddenDirs row covers", setting)
		}
	}
}
