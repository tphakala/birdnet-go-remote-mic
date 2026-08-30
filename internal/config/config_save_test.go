package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSaveRoundTrip(t *testing.T) {
	c := validBase()
	c.ApplyDefaults()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(c, loaded) {
		t.Errorf("round trip mismatch:\n saved  = %+v\n loaded = %+v", c, loaded)
	}
}

func TestSaveOmitsNilEnabled(t *testing.T) {
	c := validBase() // Discovery.Enabled and Management.Enabled are nil
	c.ApplyDefaults()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(data), "null") {
		t.Errorf("persisted config contains a null:\n%s", data)
	}
}

func TestSavePreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "config.yaml")
	c := validBase()
	c.ApplyDefaults()
	if err := Save(realPath, &c); err != nil {
		t.Fatalf("seed real file: %v", err)
	}
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	c.Devices[0].Name = "renamed"
	if err := Save(link, &c); err != nil {
		t.Fatalf("Save via symlink: %v", err)
	}
	// The symlink must survive as a symlink, and the real file must carry the change.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("Save replaced the symlink with a regular file")
	}
	loaded, err := Load(realPath)
	if err != nil {
		t.Fatalf("Load real: %v", err)
	}
	if loaded.Devices[0].Name != "renamed" {
		t.Errorf("real file name = %q, want renamed", loaded.Devices[0].Name)
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := validBase()
	c.ApplyDefaults()
	on := true
	mgmtOn := true
	c.Discovery.Enabled = &on
	c.Management.Enabled = &mgmtOn
	clone := c.Clone()

	clone.Devices[0].Name = "mutated"
	*clone.Discovery.Enabled = false
	*clone.Management.Enabled = false
	clone.Devices = append(clone.Devices, Device{Name: "extra"})

	if c.Devices[0].Name == "mutated" {
		t.Error("mutating clone device leaked into original")
	}
	if !*c.Discovery.Enabled {
		t.Error("mutating clone discovery flag leaked into original")
	}
	if !*c.Management.Enabled {
		t.Error("mutating clone management flag leaked into original")
	}
	if len(c.Devices) != 2 {
		t.Errorf("original device count changed to %d", len(c.Devices))
	}
}

func TestValidateDeviceCountBoundary(t *testing.T) {
	mkDevices := func(n int) []Device {
		devs := make([]Device, 0, n)
		for i := 0; i < n; i++ {
			devs = append(devs, Device{
				Name:   "dev" + strconv.Itoa(i),
				Device: "hw:" + strconv.Itoa(i) + ",0",
				Path:   "/dev" + strconv.Itoa(i),
				Mode:   ModePCM, Rate: 48000, Channels: 1, Format: formatS16,
			})
		}
		return devs
	}

	// The cap is 32: exactly 32 devices is accepted, 33 is rejected.
	at := validBase()
	at.Devices = mkDevices(32)
	if err := at.Validate(); err != nil {
		t.Errorf("Validate() with 32 devices = %v, want nil (boundary accepted)", err)
	}

	over := validBase()
	over.Devices = mkDevices(33)
	err := over.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "devices" {
		t.Fatalf("Validate() with 33 devices = %v, want a devices ValidationError", err)
	}
}
