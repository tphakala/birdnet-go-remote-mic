//go:build linux

package service

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Test fixtures shared by the unit reader tests.
const (
	emptyService = "[Service]\n" // a unit file with an empty [Service] section
	devNullLink  = "->/dev/null" // a unitTree link to /dev/null
	errEdited    = "installer writes"
	errNotFollow = "does not follow"
	errNotPlain  = "not plain"
	errHidden    = "may see differently"
	errMayHold   = "may hold only"
	errBOM       = "byte order mark"
	errMasked    = "masked"
	errHides     = "may see differently"
	lineA        = "A=1"
	bYAML        = "/b.yaml"
)

// unitTree points unitRoot at a fresh temporary tree holding files, keyed by
// absolute host path. A value "->target" makes that path a symbolic link to
// target. It returns the tree's root.
func unitTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	prev := unitRoot
	unitRoot = root
	t.Cleanup(func() { unitRoot = prev })
	for p, text := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(text, "->"); ok {
			if err := os.Symlink(target, full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// mustRender renders the appliance unit for s with releasedUnitTmpls[i].
func mustRender(t *testing.T, i int, s ServiceSpec) string {
	t.Helper()
	b, err := render(releasedUnitTmpls[i], s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mustRenderUnchecked renders the current appliance unit for s without
// Validate, as an older release with looser rules could have written it.
func mustRenderUnchecked(t *testing.T, s ServiceSpec) string {
	t.Helper()
	var b strings.Builder
	if err := unitTmpl.Execute(&b, unitData{
		User: s.User, Group: s.User, BinPath: s.BinPath, BinDir: filepath.Dir(s.BinPath),
		ConfigPath: s.ConfigPath, ConfigDir: s.ConfigDir(), StateDir: s.StateDir,
	}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestReadLines(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
		wantErr    string
	}{
		{name: "comments and blanks dropped, CRLF", text: "# c\r\n; c\r\n  A=1  \r\n\r\nB=2\r\n", want: []string{lineA, "B=2"}},
		{name: "continuation joins with a space", text: "A=1 \\\nB=2\n", want: []string{"A=1  B=2"}},
		{name: "comment inside a continuation skipped", text: "A=1 \\\n# inner\n B=2\n", want: []string{"A=1   B=2"}},
		{name: "a comment ending in a backslash does not continue", text: "# note \\\nA=1\n", want: []string{lineA}},
		{name: "even trailing backslashes do not continue", text: "A=x\\\\\nB=2\n", want: []string{`A=x\\`, "B=2"}},
		{name: "continuation pending at end of file", text: "A=1 \\", want: []string{lineA}},
		{name: "byte order mark", text: "\uFEFF[Service]\n", wantErr: errBOM},
		{name: "NUL", text: "A=1\x00B=2\n", wantErr: "NUL"},
		{name: "bare carriage return", text: "A=1\rB=2\n", wantErr: "carriage return"},
		{name: "continuation backslash with trailing blanks", text: "A=1 \\  \nB=2\n", wantErr: "followed by blanks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unitTree(t, map[string]string{"/f": tc.text})
			_, got, err := readLines("/f")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestInstalledBinPaths(t *testing.T) {
	unit := applianceUnitPath()
	upd := filepath.Join(unitDir, UpdateServiceUnit)
	r := mustRender(t, 0, ServiceSpec{})
	_, updater, err := RenderUpdater(ServiceSpec{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		files         map[string]string
		wantApp, want string
		wantErr       string
	}{
		{name: "no units"},
		{name: "appliance unit only", files: map[string]string{unit: r}, wantApp: DefaultBinPath},
		{name: "both units", files: map[string]string{unit: r, upd: string(updater)}, wantApp: DefaultBinPath, want: DefaultBinPath},
		{name: "special-executable prefix", files: map[string]string{unit: "[Service]\nExecStart=-/opt/rm serve\n"}, wantApp: "/opt/rm"},
		{name: "drop-in replaces ExecStart", files: map[string]string{
			unit: r,
			"/run/systemd/system/remote-mic.service.d/override.conf": "[Service]\nExecStart=\nExecStart=/opt/rm serve\n",
		}, wantErr: "replaces ExecStart="},
		{name: "quoted program", files: map[string]string{unit: "[Service]\nExecStart=\"/opt/my dir/rm\" serve\n"}, wantErr: "not a plain absolute path"},
		{name: "no ExecStart", files: map[string]string{unit: "[Service]\nType=simple\n"}, wantErr: "no ExecStart="},
		{name: "masked", files: map[string]string{unit: devNullLink}, wantErr: errMasked},
		{name: "zero-byte unit", files: map[string]string{unit: ""}, wantErr: errMasked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unitTree(t, tc.files)
			app, u, err := InstalledBinPaths()
			if app != tc.wantApp || u != tc.want {
				t.Errorf("got %q, %q; want %q, %q", app, u, tc.wantApp, tc.want)
			}
			if (tc.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstalledSpec(t *testing.T) {
	unit := applianceUnitPath()
	custom := ServiceSpec{User: "mic", ConfigPath: "/srv/rm/config.yaml", StateDir: "/srv/rm-state", BinPath: "/opt/bin/remote-mic"}
	for _, tc := range []struct {
		name    string
		files   map[string]string
		want    ServiceSpec
		wantErr string
	}{
		{name: "no unit"},
		{name: "default install", files: map[string]string{unit: mustRender(t, 0, ServiceSpec{})}, want: ServiceSpec{}.withDefaults()},
		{name: "custom install", files: map[string]string{unit: mustRender(t, 0, custom)}, want: custom},
		{name: "v0.1.0 template", files: map[string]string{unit: mustRender(t, 1, custom)}, want: custom},
		{name: "drop-ins do not count", files: map[string]string{
			unit: mustRender(t, 0, custom),
			"/etc/systemd/system/remote-mic.service.d/x.conf": "[Service]\nEnvironment=REMOTEMIC_CONFIG=/b.yaml\n",
		}, want: custom},
		{name: "edited by hand", files: map[string]string{unit: mustRender(t, 0, custom) + "Nice=5\n"}, wantErr: errEdited},
		{name: "path edited in place", files: map[string]string{unit: strings.Replace(mustRender(t, 0, custom), "REMOTEMIC_CONFIG=/srv/rm/config.yaml", "REMOTEMIC_CONFIG=/srv/other/c.yaml", 1)}, wantErr: errEdited},
		{name: "masked", files: map[string]string{unit: devNullLink}, wantErr: errMasked},
		{name: "zero-byte unit", files: map[string]string{unit: ""}, wantErr: errMasked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unitTree(t, tc.files)
			got, err := InstalledSpec()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %+v, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestInstalledConfig(t *testing.T) {
	const (
		userMic = "mic"
		srvCfg  = "/srv/rm/config.yaml"
		etcD    = "/etc/systemd/system/remote-mic.service.d/"
		runD    = "/run/systemd/system/remote-mic.service.d/"
		prefixD = "/etc/systemd/system/remote-.service.d/"
		typeD   = "/etc/systemd/system/service.d/"
	)
	unit := applianceUnitPath()
	r := mustRender(t, 0, ServiceSpec{ConfigPath: srvCfg, User: userMic})
	drop := func(lines ...string) string { return "[Service]\n" + strings.Join(lines, "\n") + "\n" }
	set := func(p string) string { return "Environment=REMOTEMIC_CONFIG=" + p }

	accepted := []struct {
		name               string
		files              map[string]string
		wantPath, wantUser string
	}{
		{"rendered unit", map[string]string{unit: r}, srvCfg, userMic},
		{"v0.1.0 unit", map[string]string{unit: mustRender(t, 1, ServiceSpec{ConfigPath: srvCfg, User: userMic})}, srvCfg, userMic},
		{"plain override", map[string]string{unit: r, etcD + "x.conf": drop(set("/etc/rm2/c.yaml"))}, "/etc/rm2/c.yaml", userMic},
		{"several assignments and a tab", map[string]string{unit: r, etcD + "x.conf": drop("Environment=A=1\tREMOTEMIC_CONFIG=/b.yaml B=2")}, bYAML, userMic},
		{"look-alike names are not the variable", map[string]string{unit: r, etcD + "x.conf": drop("Environment=REMOTEMIC_CONFIG_X=/x FOO=REMOTEMIC_CONFIG=/y")}, srvCfg, userMic},
		{"reset then set", map[string]string{unit: r, etcD + "x.conf": drop("Environment=", set(bYAML))}, bYAML, userMic},
		{"user override", map[string]string{unit: r, etcD + "x.conf": drop("User=other")}, srvCfg, "other"},
		{"empty user", map[string]string{unit: r, etcD + "x.conf": drop("User=")}, srvCfg, ""},
		{"CRLF drop-in", map[string]string{unit: r, etcD + "x.conf": "[Service]\r\n" + set("/crlf.yaml") + "\r\n"}, "/crlf.yaml", userMic},
		{"continuation in a drop-in", map[string]string{unit: r, etcD + "x.conf": "[Service]\nEnvironment=A=1 \\\n# inner\n REMOTEMIC_CONFIG=/cont.yaml\n"}, "/cont.yaml", userMic},
		{"continuation at end of file", map[string]string{unit: r, etcD + "x.conf": "[Service]\n" + set("/eof.yaml") + ` \`}, "/eof.yaml", userMic},
		{"drop-ins apply in file name order across directories", map[string]string{
			unit:               r,
			etcD + "20-b.conf": drop(set(bYAML)),
			runD + "10-a.conf": drop(set("/a.yaml"), "User=other"),
		}, bYAML, "other"},
		{"a higher-priority irrelevant drop-in masks a same-named one", map[string]string{
			unit:            r,
			etcD + "x.conf": drop("Nice=5"),
			runD + "x.conf": drop(set("/masked.yaml")),
		}, srvCfg, userMic},
		{"/dev/null link masks", map[string]string{unit: r, etcD + "x.conf": devNullLink, runD + "x.conf": drop(set("/masked.yaml"))}, srvCfg, userMic},
		{"empty file masks", map[string]string{unit: r, etcD + "x.conf": "", runD + "x.conf": drop(set("/masked.yaml"))}, srvCfg, userMic},
		{"hidden and non-conf files skipped", map[string]string{
			unit:                  r,
			etcD + ".hidden.conf": drop(set("/hidden.yaml")),
			etcD + "notes.conf~":  drop(set("/backup.yaml")),
		}, srvCfg, userMic},
		{"irrelevant keys anywhere are ignored", map[string]string{
			unit:               r,
			prefixD + "x.conf": drop("Nice=5", "ExecStartPre=/usr/bin/", "Description=x\\\\"),
			typeD + "y.conf":   drop("TimeoutStopFailureMode=abort"),
			etcD + "z.conf":    "[Unit]\nAfter=x.target\n",
		}, srvCfg, userMic},
		{"a lowercase key is ignored, as by systemd", map[string]string{unit: r, etcD + "x.conf": drop("environment=REMOTEMIC_CONFIG=/lower.yaml")}, srvCfg, userMic},
		{"a header continued onto the assignment is invalid, so systemd stops reading the file", map[string]string{unit: r, etcD + "x.conf": "[Service] \\\n" + set(bYAML) + "\n"}, srvCfg, userMic},
		{"a line without = is ignored, as by systemd", map[string]string{unit: r, etcD + "x.conf": drop("EnvironmentFile")}, srvCfg, userMic},
		{"unit linked from /etc to its own name", map[string]string{unit: "->/usr/lib/systemd/system/remote-mic.service", "/usr/lib/systemd/system/remote-mic.service": r}, srvCfg, userMic},
		{"unit linked by a relative target", map[string]string{unit: "->../../../usr/lib/systemd/system/remote-mic.service", "/usr/lib/systemd/system/remote-mic.service": r}, srvCfg, userMic},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			unitTree(t, tc.files)
			path, user, err := InstalledConfig()
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if path != tc.wantPath || user != tc.wantUser {
				t.Errorf("got %q, %q; want %q, %q", path, user, tc.wantPath, tc.wantUser)
			}
		})
	}

	refused := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{"unit edited by hand", map[string]string{unit: r + "Nice=5\n"}, errEdited},
		{"quoted value", map[string]string{unit: r, etcD + "x.conf": drop(`Environment="REMOTEMIC_CONFIG=/q/c.yaml"`)}, errNotPlain},
		{"escaped name", map[string]string{unit: r, etcD + "x.conf": drop(`Environment=REMOTEMIC\x5fCONFIG=/b.yaml`)}, errNotPlain},
		{"escaped value", map[string]string{unit: r, etcD + "x.conf": drop(`Environment=REMOTEMIC_CONFIG=/a\x2fb.yaml`)}, errNotPlain},
		{"specifier", map[string]string{unit: r, etcD + "x.conf": drop(set("/srv/%i/c.yaml"))}, errNotPlain},
		{"non-ASCII", map[string]string{unit: r, etcD + "x.conf": drop(set("/a b.yaml"))}, errNotPlain},
		{"invalid UTF-8", map[string]string{unit: r, etcD + "x.conf": drop(set("/b\xff.yaml"))}, errNotPlain},
		{"relative value", map[string]string{unit: r, etcD + "x.conf": drop(set("config.yaml"))}, "clean absolute"},
		{"unclean value", map[string]string{unit: r, etcD + "x.conf": drop(set("/a/../b.yaml"))}, "clean absolute"},
		{"an unfollowed line before the assignment", map[string]string{unit: r, etcD + "x.conf": drop("ExecStartPre=/usr/bin/", set(bYAML))}, errNotFollow},
		{"user with a group", map[string]string{unit: r, etcD + "x.conf": drop("User=a:b", set(bYAML))}, errNotFollow},
		{"numeric user", map[string]string{unit: r, etcD + "x.conf": drop("User=65535", set(bYAML))}, errNotFollow},
		{"assignment before any section", map[string]string{unit: r, etcD + "x.conf": set(bYAML) + "\n"}, errNotFollow},
		{"oversized drop-in", map[string]string{unit: r, etcD + "x.conf": drop("# "+strings.Repeat("x", maxDropIn), set(bYAML))}, "larger than"},
		{"journal namespace path", map[string]string{unit: r, etcD + "x.conf": drop(set("/run/systemd/journal/rm/config.yaml"))}, errHidden},
		{"device path", map[string]string{unit: r, etcD + "x.conf": drop(set("/dev/shm/rm/config.yaml"))}, errHidden},
		{"script at the program path", map[string]string{unit: r, "/usr/local/bin/remote-mic": "#!/bin/sh\nexec /opt/rm \"$@\"\n"}, "a script"},
		{"unclosed section header", map[string]string{unit: r, etcD + "x.conf": "[Service\n" + set(bYAML) + "\n"}, errNotFollow},
		{"lowercase section", map[string]string{unit: r, etcD + "x.conf": "[service]\n" + set(bYAML) + "\n"}, errNotFollow},
		{"bare key beside an assignment", map[string]string{unit: r, etcD + "x.conf": drop("EnvironmentFile", set(bYAML))}, errNotFollow},
		{"environment file", map[string]string{unit: r, etcD + "x.conf": drop("EnvironmentFile=-/etc/default/rm")}, errNotFollow},
		{"unset environment", map[string]string{unit: r, etcD + "x.conf": drop("UnsetEnvironment=REMOTEMIC_CONFIG")}, errNotFollow},
		{"pass environment", map[string]string{unit: r, etcD + "x.conf": drop("PassEnvironment=REMOTEMIC_CONFIG")}, errNotFollow},
		{"PAM", map[string]string{unit: r, etcD + "x.conf": drop("PAMName=login")}, errNotFollow},
		{"ExecStart", map[string]string{unit: r, etcD + "x.conf": drop("ExecStart=", "ExecStart=/opt/wrapper serve")}, errNotFollow},
		{"dynamic user", map[string]string{unit: r, etcD + "x.conf": drop("DynamicUser=yes")}, errNotFollow},
		{"bind mount", map[string]string{unit: r, etcD + "x.conf": drop("BindPaths=/srv:/etc/remote-mic")}, errNotFollow},
		{"protect home off", map[string]string{unit: r, etcD + "x.conf": drop("ProtectHome=no")}, errNotFollow},
		{"relevant key in a prefix drop-in", map[string]string{unit: r, prefixD + "x.conf": drop(set("/p.yaml"))}, "prefix or type-level"},
		{"relevant key in a type-level drop-in", map[string]string{unit: r, typeD + "x.conf": drop("EnvironmentFile=-/etc/default/all")}, "prefix or type-level"},
		{"relevant key outside [Service]", map[string]string{unit: r, etcD + "x.conf": "[Unit]\n" + set("/u.yaml") + "\n"}, errNotFollow},
		{"reset leaves none", map[string]string{unit: r, etcD + "x.conf": drop("Environment=")}, "not set"},
		{"private /tmp", map[string]string{unit: r, etcD + "x.conf": drop(set("/tmp/rm/config.yaml"))}, errHidden},
		{"private /var/tmp", map[string]string{unit: r, etcD + "x.conf": drop(set("/var/tmp/config.yaml"))}, errHidden},
		{"protected /home", map[string]string{unit: r, etcD + "x.conf": drop(set("/home/pi/rm/config.yaml"))}, errHidden},
		{"protected /root", map[string]string{unit: r, etcD + "x.conf": drop(set("/root/config.yaml"))}, errHidden},
		{"protected /run/user", map[string]string{unit: r, etcD + "x.conf": drop(set("/run/user/1000/config.yaml"))}, errHidden},
		{"proc path", map[string]string{unit: r, etcD + "x.conf": drop(set("/proc/1/root/c.yaml"))}, errHidden},
		{"private /tmp through a link", map[string]string{unit: r, etcD + "x.conf": drop(set("/etc/rmlink/config.yaml")), "/etc/rmlink": "->../tmp/rm", "/tmp/rm/keep": ""}, errHidden},
		{"byte order mark", map[string]string{unit: r, etcD + "x.conf": "\uFEFF" + drop(set(bYAML))}, errBOM},
		{"masked unit", map[string]string{unit: devNullLink}, errMasked},
		{"empty unit", map[string]string{unit: "", etcD + "x.conf": drop("ExecStart=/x serve", set(bYAML))}, errMasked},
		{"unit linked to another name", map[string]string{unit: "->/usr/lib/systemd/system/other.service", "/usr/lib/systemd/system/other.service": r}, "is a link to"},
		{"unit shadowed by a higher-priority directory", map[string]string{unit: r, "/run/systemd/transient/remote-mic.service": r}, "not where the installer writes it"},
		{"unit only below /etc", map[string]string{"/usr/local/lib/systemd/system/remote-mic.service": r}, "not where the installer writes it"},
		{"alias link", map[string]string{unit: r, "/etc/systemd/system/rmic.service": "->" + unit}, "is an alias of"},
		{"linked drop-in directory", map[string]string{unit: r, "/etc/systemd/system/remote-mic.service.d": "->rm-overrides", "/etc/systemd/system/rm-overrides/o.conf": drop(set(bYAML))}, "is a link"},
		{"config linked into /tmp", map[string]string{unit: r, "/etc/remote-mic/c.yaml": "->../../tmp/rm.yaml", "/tmp/rm.yaml": "", etcD + "x.conf": drop(set("/etc/remote-mic/c.yaml"))}, errHidden},
		{"older install with a path this version refuses", map[string]string{unit: mustRenderUnchecked(t, ServiceSpec{User: "remote-mic", ConfigPath: "/etc/r(m)/config.yaml", StateDir: DefaultStateDir, BinPath: DefaultBinPath})}, "this version does not accept"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			unitTree(t, tc.files)
			path, user, err := InstalledConfig()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %q, %q, %v; want an error containing %q", path, user, err, tc.wantErr)
			}
		})
	}

	for _, k := range relevantKeys {
		if k == keyEnvironment || k == keyUser {
			continue // followed with plain values; pinned by the rows above
		}
		t.Run("relevant key "+k, func(t *testing.T) {
			unitTree(t, map[string]string{unit: r, etcD + "x.conf": drop(k + "=x")})
			if _, _, err := InstalledConfig(); err == nil || !strings.Contains(err.Error(), errNotFollow) {
				t.Errorf("err = %v, want one containing %q", err, errNotFollow)
			}
			unitTree(t, map[string]string{unit: r, typeD + "x.conf": drop(k + "=x")})
			if _, _, err := InstalledConfig(); err == nil || !strings.Contains(err.Error(), "prefix or type-level") {
				t.Errorf("type-level: err = %v, want a refusal", err)
			}
		})
	}
	for _, d := range privateDirs {
		t.Run("private directory "+d, func(t *testing.T) {
			unitTree(t, map[string]string{unit: r, etcD + "x.conf": drop(set(d + "/rm/config.yaml"))})
			if _, _, err := InstalledConfig(); err == nil || !strings.Contains(err.Error(), errHidden) {
				t.Errorf("err = %v, want one containing %q", err, errHidden)
			}
		})
	}

	t.Run("no unit", func(t *testing.T) {
		unitTree(t, map[string]string{etcD + "a.conf": drop(set("/a.yaml"))})
		if path, user, err := InstalledConfig(); path != "" || user != "" || err != nil {
			t.Errorf("got %q, %q, %v; want no unit", path, user, err)
		}
	})
}

// TestInstalledConfigUnreadable asserts an installed unit that cannot be read,
// or whose drop-in directory cannot be listed, is an error, not "no unit".
func TestInstalledConfigUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	unit := applianceUnitPath()
	r := mustRender(t, 0, ServiceSpec{})
	root := unitTree(t, map[string]string{unit: r})
	if err := os.Chmod(root+unit, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstalledConfig(); err == nil {
		t.Error("unreadable unit: err = nil, want an error")
	}

	dir := unit + ".d"
	root = unitTree(t, map[string]string{unit: r, dir + "/x.conf": emptyService})
	if err := os.Chmod(root+dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root+dir, 0o755) })
	if _, _, err := InstalledConfig(); err == nil {
		t.Error("unreadable drop-in directory: err = nil, want an error")
	}
	if app, _, err := InstalledBinPaths(); app != "" || err == nil {
		t.Errorf("InstalledBinPaths with an unreadable drop-in directory: got %q, %v; want \"\" and an error", app, err)
	}
}

// TestDropInPathsOrder pins the application order and precedence: by file
// name, whatever directory each file came from (the highest-priority
// directory holds the last name, so an order taken from the directory walk
// would be wrong), and for one name, directory priority first, then the more
// specific directory name, with type-level only when nothing else has it.
func TestDropInPathsOrder(t *testing.T) {
	unitTree(t, map[string]string{
		"/etc/systemd/system.control/remote-mic.service.d/90-z.conf": "",
		"/usr/lib/systemd/system/remote-mic.service.d/10-a.conf":     "",
		"/etc/systemd/system/remote-.service.d/50-m.conf":            "",
		"/etc/systemd/system/service.d/70-t.conf":                    "",
		"/etc/systemd/system/remote-.service.d/20-p.conf":            "",
		"/usr/lib/systemd/system/remote-mic.service.d/20-p.conf":     "",
		"/etc/systemd/system/remote-mic.service.d/30-s.conf":         "",
		"/etc/systemd/system/remote-.service.d/30-s.conf":            "",
		"/etc/systemd/system/service.d/40-t.conf":                    "",
		"/usr/lib/systemd/system/remote-mic.service.d/40-t.conf":     "",
	})
	got, err := dropInPaths(DefaultUnitName)
	if err != nil {
		t.Fatal(err)
	}
	want := []dropIn{
		{"/usr/lib/systemd/system/remote-mic.service.d/10-a.conf", true},
		{"/etc/systemd/system/remote-.service.d/20-p.conf", false},       // /etc prefix beats /usr/lib name-specific
		{"/etc/systemd/system/remote-mic.service.d/30-s.conf", true},     // name-specific beats prefix in one directory
		{"/usr/lib/systemd/system/remote-mic.service.d/40-t.conf", true}, // name-specific beats type-level
		{"/etc/systemd/system/remote-.service.d/50-m.conf", false},
		{"/etc/systemd/system/service.d/70-t.conf", false},
		{"/etc/systemd/system.control/remote-mic.service.d/90-z.conf", true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestValidateRejectsUnplainAndHiddenPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, want string }{
		{`/etc/r\m/config.yaml`, errMayHold},
		{`/etc/"rm"/config.yaml`, errMayHold},
		{"/etc/r$m/config.yaml", errMayHold},
		{"/etc/rém/config.yaml", errMayHold},
		{"/tmp/rm/config.yaml", errHides},
		{"/var/tmp/rm/config.yaml", errHides},
		{"/home/pi/rm/config.yaml", errHides},
		{"/run/rm/config.yaml", errHides},
	} {
		s := ServiceSpec{ConfigPath: tc.path}.withDefaults()
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%q) = %v, want an error containing %q", tc.path, err, tc.want)
		}
	}
}

// TestReleasedUnitTemplates pins every appliance unit template a release has
// installed: InstalledSpec matches installed units against them byte for
// byte, so an edit to a released one, or a change to the current one that
// does not keep the old text in the list, would refuse existing installs.
func TestReleasedUnitTemplates(t *testing.T) {
	t.Parallel()
	want := []string{
		"d1144c60096d39e0483646cfc9c738bfb7cf529639ff0d661b6dab5e4de6f535", // current (v0.3.0)
		"caf62128459cc56043490f772cf999d770b198aee1069c4f4819b671ec904089", // v0.1.0 and v0.2.0
	}
	texts := releasedUnitTexts
	if len(texts) != len(want) || texts[0] != unitTemplate || len(releasedUnitTmpls) != len(texts) {
		t.Fatalf("releasedUnitTexts has %d entries (current first: %t), releasedUnitTmpls %d; want %d with the current template first",
			len(texts), texts[0] == unitTemplate, len(releasedUnitTmpls), len(want))
	}
	for i, text := range texts {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		if sum != want[i] {
			t.Errorf("template %d sha256 %s, want %s (a released template must never change)", i, sum, want[i])
		}
		if _, err := render(releasedUnitTmpls[i], ServiceSpec{}); err != nil {
			t.Errorf("template %d does not render: %v", i, err)
		}
	}
}

// TestExecStartDropIn asserts a drop-in that sets ExecStart= is reported, and
// one that only sets the environment, or no drop-in at all, is not.
func TestExecStartDropIn(t *testing.T) {
	base := "/etc/systemd/system/" + DefaultUnitName
	unit := "[Service]\nExecStart=/usr/bin/remote-mic serve\n"
	unitTree(t, map[string]string{base: unit})
	if got, err := execStartDropIn(DefaultUnitName); err != nil || got != "" {
		t.Fatalf("no drop-in: %q, %v", got, err)
	}
	unitTree(t, map[string]string{base: unit, base + ".d/env.conf": "[Service]\nEnvironment=A=b\n"})
	if got, err := execStartDropIn(DefaultUnitName); err != nil || got != "" {
		t.Fatalf("environment-only drop-in: %q, %v", got, err)
	}
	unitTree(t, map[string]string{base: unit, base + ".d/exec.conf": "[Service]\nExecStart=\nExecStart=/opt/wrap\n"})
	if got, err := execStartDropIn(DefaultUnitName); err != nil || !strings.HasSuffix(got, "exec.conf") {
		t.Fatalf("overriding drop-in: %q, %v, want its path", got, err)
	}
	unitTree(t, nil)
	if got, err := execStartDropIn(DefaultUnitName); err != nil || got != "" {
		t.Fatalf("no unit: %q, %v", got, err)
	}
	unitTree(t, map[string]string{base: "\ufeff" + unit})
	if _, err := execStartDropIn(DefaultUnitName); err == nil {
		t.Error("a unit that cannot be read reported no error")
	}
}
