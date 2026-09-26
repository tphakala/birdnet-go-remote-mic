package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUpdateCheckEnabled(t *testing.T) {
	t.Parallel()
	c := Default()
	if !c.UpdateCheckEnabled() {
		t.Error("absent updates.check: want on")
	}
	c.Updates.Check = new(false)
	if c.UpdateCheckEnabled() {
		t.Error("updates.check false: want off")
	}
	out, err := yaml.Marshal(&c)
	if err != nil || !strings.Contains(string(out), "check: false") {
		t.Errorf("saved YAML %q, %v; want updates.check false", out, err)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil || back.UpdateCheckEnabled() {
		t.Errorf("reloaded updates.check = %t, %v; want off", back.UpdateCheckEnabled(), err)
	}
}

// TestCloneDeepCopiesUpdatesCheck pins that a clone's updates.check flag has
// its own storage.
func TestCloneDeepCopiesUpdatesCheck(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Updates.Check = new(true)
	cl := c.Clone()
	*cl.Updates.Check = false
	if !*c.Updates.Check {
		t.Error("changing the clone's updates.check changed the original")
	}
}
