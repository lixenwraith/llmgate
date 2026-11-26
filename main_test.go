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

func TestAMisspeltKeyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmgate.toml")
	os.WriteFile(path, []byte("[model]\nthread = 2\n[apps.lwdescribe.limits.client]\nburst = 1\n"), 0o600)
	if _, err := load(path); err == nil || !strings.Contains(err.Error(), "model.thread") || strings.Contains(err.Error(), "burst") {
		t.Errorf("load: %v, want model.thread alone refused", err)
	}
}
