//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseMountinfoDecodesEscapes(t *testing.T) {
	t.Parallel()
	in := "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		`40 22 0:35 / /mnt/my\040disk rw - tmpfs tmpfs rw` + "\n" +
		`41 22 0:36 / /mnt/back\134slash rw - tmpfs tmpfs rw` + "\n" +
		`42 22 0:37 /a\040b /mnt/c rw - tmpfs tmpfs rw` + "\n"
	got, err := parseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []mountEntry{
		{id: "22", parent: "1", root: "/", point: "/"},
		{id: "40", parent: "22", root: "/", point: "/mnt/my disk"},
		{id: "41", parent: "22", root: "/", point: `/mnt/back\slash`},
		{id: "42", parent: "22", root: "/a b", point: "/mnt/c"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseMountinfoRefusesAShortLine(t *testing.T) {
	t.Parallel()
	if _, err := parseMountinfo(strings.NewReader("22 1 8:1\n")); err == nil {
		t.Error("a line with fewer than five fields was accepted")
	}
}

// TestSandboxHiddenDir pins how SandboxHiddenDir reads a mount table: the
// mount a path resolves to, followed by parent links as the kernel walks it,
// must be a systemd sandbox mount at the directory HiddenDir names. Lines are
// verbatim from captured tables unless marked derived.
func TestSandboxHiddenDir(t *testing.T) {
	t.Parallel()
	const (
		homeAudio   = "/home/pi/audio"
		tmpFile     = "/tmp/a.flac"
		runUser     = "/run/user"
		runUserFile = "/run/user/1000/a.flac"
	)
	// The installed unit on a Raspberry Pi (Debian 13, systemd 257), with
	// ProtectHome=true and PrivateTmp=true; /tmp is the host's own tmpfs.
	const (
		svcRoot    = "190 52 179:2 / / ro,nosuid,noatime shared:124 master:1 - ext4 /dev/mmcblk0p2 rw"
		svcRun     = "195 190 0:22 / /run ro,nosuid,nodev,noexec,relatime shared:141 master:5 - tmpfs tmpfs rw,size=777368k,mode=755"
		svcHostTmp = "289 190 0:35 / /tmp rw,nosuid,nodev shared:232 master:79 - tmpfs tmpfs rw,nr_inodes=1048576"
		svcHome    = "291 190 0:22 /systemd/inaccessible/dir /home ro,nosuid,nodev,noexec,relatime shared:235 master:5 - tmpfs tmpfs rw,size=777368k,mode=755"
		svcRootDir = "292 190 0:22 /systemd/inaccessible/dir /root ro,nosuid,nodev,noexec,relatime shared:236 master:5 - tmpfs tmpfs rw,size=777368k,mode=755"
		svcRunUser = "198 195 0:22 /systemd/inaccessible/dir /run/user ro,nosuid,nodev,noexec,relatime shared:145 master:5 - tmpfs tmpfs rw,size=777368k,mode=755"
		svcPrivTmp = "200 289 0:35 /systemd-private-b0debf5e106542878cba642025437640-remote-mic.service-f9H7YH/tmp /tmp rw,nosuid,nodev shared:233 master:79 - tmpfs tmpfs rw,nr_inodes=1048576"
		svcVarTmp  = "294 190 179:2 /var/tmp/systemd-private-b0debf5e106542878cba642025437640-remote-mic.service-dremne/tmp /var/tmp rw,nosuid,noatime shared:238 master:1 - ext4 /dev/mmcblk0p2 rw"
	)
	installed := []string{svcRoot, svcRun, svcHostTmp, svcHome, svcRootDir, svcRunUser, svcPrivTmp, svcVarTmp}
	// The same host without a sandbox.
	host := []string{
		"24 25 0:22 / /run rw,nosuid,nodev,noexec,relatime shared:5 - tmpfs tmpfs rw,size=777368k,mode=755",
		"25 1 179:2 / / rw,noatime shared:1 - ext4 /dev/mmcblk0p2 rw",
		"44 25 0:35 / /tmp rw,nosuid,nodev shared:79 - tmpfs tmpfs rw,nr_inodes=1048576",
		"56 24 0:40 / /run/user/1000 rw,nosuid,nodev,relatime shared:199 - tmpfs tmpfs rw,size=388684k,nr_inodes=97171,mode=700,uid=1000,gid=1000",
	}
	// systemd-resolved on Ubuntu (ProtectHome=yes): /run/user did not exist
	// when it started, so nothing covers it, and a later login's
	// /run/user/1000 propagated in.
	resolved := []string{
		"97 42 259:2 / / ro,nosuid,relatime shared:47 master:1 - ext4 /dev/nvme0n1p2 rw",
		"103 97 0:26 / /run rw,nosuid,nodev,noexec,relatime shared:48 master:5 - tmpfs tmpfs rw,size=3193716k,mode=755,inode64",
		"98 97 0:26 /systemd/inaccessible/dir /home ro,nosuid,nodev,noexec,relatime shared:70 master:5 - tmpfs tmpfs rw,size=3193716k,mode=755,inode64",
		"177 103 0:56 / /run/user/1000 rw,nosuid,nodev,relatime shared:334 master:305 - tmpfs tmpfs rw,size=3193712k,nr_inodes=798428,mode=700,uid=1000,gid=1000,inode64",
	}
	table := func(lines ...string) func() ([]mountEntry, error) {
		return func() ([]mountEntry, error) {
			return parseMountinfo(strings.NewReader(strings.Join(lines, "\n") + "\n"))
		}
	}
	with := func(extra ...string) func() ([]mountEntry, error) {
		return table(append(slices.Clone(installed), extra...)...)
	}
	// Derived: ProtectHome= relaxed by a drop-in, PrivateTmp= kept.
	relaxed := table(svcRoot, svcRun, svcHostTmp, svcPrivTmp, svcVarTmp)
	// Derived: a login after start attaches /run/user/1000 to /run, beneath
	// the mount covering /run/user, where no path walk reaches it.
	shadowed := with("400 195 0:56 / /run/user/1000 rw,nosuid,nodev,relatime shared:334 master:305 - tmpfs tmpfs rw,mode=700,uid=1000,gid=1000")
	// Derived: a /home mounted on the host after start propagates beneath the
	// inaccessible mount, which becomes its child yet is listed first.
	tucked := table(svcRoot, svcRun,
		"291 300 0:22 /systemd/inaccessible/dir /home ro,nosuid,nodev,noexec,relatime shared:235 master:5 - tmpfs tmpfs rw,size=777368k,mode=755",
		"300 190 0:50 / /home rw,relatime shared:400 master:401 - nfs4 srv:/home rw")
	// Derived: BindPaths=/srv/audio:/tmp/audio on top of the private /tmp.
	bound := with("201 200 179:2 /srv/audio /tmp/audio rw,nosuid,noatime shared:239 master:1 - ext4 /dev/mmcblk0p2 rw")
	// Derived: PrivateTmp='s read-only fallback in place of the private /tmp.
	empty := table(svcRoot, svcRun, svcHostTmp, svcHome, svcRootDir, svcRunUser,
		"200 289 0:22 /systemd/empty /tmp ro,nosuid,nodev,noexec,relatime shared:233 master:5 - tmpfs tmpfs rw,size=777368k,mode=755", svcVarTmp)
	// Malformed: a duplicate id 2 makes 2 -> 3 -> 2 a cycle.
	looping := table("1 0 0:1 / / rw - x x rw", "2 1 0:1 / /home rw - x x rw", "3 2 0:1 / /home rw - x x rw", "2 3 0:1 / /home rw - x x rw")

	tests := []struct {
		name, path, want string
		mounts           func() ([]mountEntry, error)
	}{
		{"inaccessible /home", homeAudio, dirHome, table(installed...)},
		{"inaccessible /run/user, two levels down", runUserFile, runUser, table(installed...)},
		{"private /tmp stacked on a tmpfs /tmp", tmpFile, dirTmp, table(installed...)},
		{"private /var/tmp on the root filesystem", "/var/tmp/a.flac", dirVarTmp, table(installed...)},
		{"host tmpfs /tmp, no sandbox", tmpFile, "", table(host...)},
		{"ProtectHome relaxed", homeAudio, "", relaxed},
		{"ProtectHome relaxed, PrivateTmp kept", tmpFile, dirTmp, relaxed},
		// Derived: InaccessiblePaths=/home/pi hides the path, not /home.
		{"sandbox mount below the hidden directory", homeAudio, "", table(svcRoot, svcRun,
			"300 190 0:22 /systemd/inaccessible/dir /home/pi ro,nosuid,nodev,noexec,relatime shared:240 master:5 - tmpfs tmpfs rw,size=777368k,mode=755")},
		{"/run/user missing at start", runUserFile, "", table(resolved...)},
		{"propagated mount shadowed by the sandbox", runUserFile, runUser, shadowed},
		{"propagated mount tucked beneath the sandbox", homeAudio, dirHome, tucked},
		{"bind on top of the private /tmp", "/tmp/audio/a.flac", "", bound},
		{"beside a bind on the private /tmp", "/tmp/other.flac", dirTmp, bound},
		{"read-only /tmp fallback", tmpFile, dirTmp, empty},
		{"malformed table", homeAudio, "", with("7 1 0:1")},
		{"path under no hidden directory", "/srv/audio", "", func() ([]mountEntry, error) {
			t.Error("the mount table was read for a path under no hidden directory")
			return nil, errors.New("not read")
		}},
		{"looping table", homeAudio, "", looping},
	}
	for _, tt := range tests {
		dir, ok := sandboxHiddenDir(tt.path, tt.mounts)
		if dir != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: sandboxHiddenDir(%q) = %q, %v, want %q", tt.name, tt.path, dir, ok, tt.want)
		}
	}
}

func TestMountPointsListsTheRoot(t *testing.T) {
	t.Parallel()
	got, err := mountPoints()
	if err != nil {
		t.Skipf("no mount list on this host: %v", err)
	}
	if !slices.Contains(got, "/") {
		t.Errorf("mount list %q does not contain /", got)
	}
}

func TestPurgeDirsOK(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf, state := filepath.Join(root, "etc"), filepath.Join(root, "state")
	for _, d := range []string{conf, state, filepath.Join(root, "state-other")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	spec := func(cfgDir, stateDir string) ServiceSpec {
		return ServiceSpec{ConfigPath: filepath.Join(cfgDir, "config.yaml"), StateDir: stateDir}
	}
	list := func(m ...string) func() ([]string, error) {
		return func() ([]string, error) { return m, nil }
	}
	tests := []struct {
		name    string
		spec    ServiceSpec
		mounts  func() ([]string, error)
		wantErr string
	}{
		{"mount inside the state directory", spec(conf, state), list("/", filepath.Join(state, "bin")), "state directory"},
		{"mount inside the config directory", spec(conf, state), list(filepath.Join(conf, "deep", "er")), "config directory"},
		{"unreadable mount list", spec(conf, state), func() ([]string, error) { return nil, errors.New("boom") }, "mount list"},
		{"mount at the directory itself", spec(conf, state), list(state, conf), ""},
		{"sibling sharing a prefix", spec(conf, state), list(filepath.Join(root, "state-other")), ""},
		{"symlinked directory", spec(conf, link), list(filepath.Join(state, "bin")), ""},
		{"missing directories", spec(filepath.Join(root, "none"), filepath.Join(root, "gone")), func() ([]string, error) { return nil, errors.New("not read") }, ""},
	}
	for _, tt := range tests {
		err := purgeDirsOK(tt.spec, tt.mounts)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: got %v, want an error containing %q", tt.name, err, tt.wantErr)
		}
	}
}

// TestPurgeDirsOKResolvesALinkedParent pins that a directory reached through a
// symlinked parent is compared by its target path, the form mountinfo lists.
func TestPurgeDirsOKResolvesALinkedParent(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(root, "via")
	if err := os.Symlink(target, via); err != nil {
		t.Fatal(err)
	}
	s := ServiceSpec{ConfigPath: filepath.Join(root, "missing", "config.yaml"), StateDir: filepath.Join(via, "state")}
	mounts := func() ([]string, error) { return []string{filepath.Join(target, "state", "bin")}, nil }
	if err := purgeDirsOK(s, mounts); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Errorf("got %v, want the refusal for a mount inside the state directory", err)
	}
}

func TestCheckPurgeDirsStillRefusesADirectoryOverTheBinDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.Symlink(bin, state); err != nil {
		t.Fatal(err)
	}
	s := ServiceSpec{BinPath: filepath.Join(bin, "remote-mic"), ConfigPath: filepath.Join(root, "etc", "config.yaml"), StateDir: state}
	if err := checkPurgeDirs(s); err == nil || !strings.Contains(err.Error(), "bin directory") {
		t.Errorf("got %v, want the bin directory refusal", err)
	}
}
