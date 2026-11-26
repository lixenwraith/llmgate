package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The jail's llmgate.toml shows every key at its default, but for the
// three the template fills
func TestTheSampleConfigIsTheDefaults(t *testing.T) {
	c, err := load("jail/usr/local/etc/llmgate/llmgate.toml")
	if err != nil {
		t.Fatal(err)
	}
	def := defaults()
	c.Listen, c.Proxies, c.Model.Name = def.Listen, def.Proxies, def.Model.Name
	if !reflect.DeepEqual(c, def) {
		t.Errorf("the sample\n%+v\nthe defaults\n%+v", c, def)
	}
}

func TestAMisspeltKeyOrVariableIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmgate.toml")
	os.WriteFile(path, []byte("[model]\nthread = 2\n[apps.lwdescribe.limits.client]\nburst = 1\n"), 0o600)
	t.Setenv("LLMGATE_MODEL_THREADS", "2")
	t.Setenv("LLMGATE_LINE_WAITNG", "2")
	_, err := load(path)
	if err == nil || !strings.Contains(err.Error(), "model.thread") || !strings.Contains(err.Error(), "LLMGATE_LINE_WAITNG") ||
		strings.Contains(err.Error(), "burst") || strings.Contains(err.Error(), "THREADS") {
		t.Errorf("load: %v, want model.thread and LLMGATE_LINE_WAITNG alone refused", err)
	}
}

// -check runs load: what start would refuse or panic on is refused there
func TestAValueStartRefusesIsRefused(t *testing.T) {
	for _, v := range []string{"[log]\nlevel = \"verbose\"", "[model]\nthink = \"true\"", "[line]\nheartbeat = \"0s\""} {
		path := filepath.Join(t.TempDir(), "llmgate.toml")
		os.WriteFile(path, []byte(v+"\n"), 0o600)
		if _, err := load(path); err == nil {
			t.Errorf("%q loads", v)
		}
	}
}
