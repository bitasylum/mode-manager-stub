/*
Copyright 2026 Bitasylum

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"
	"time"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
)

func TestValidate(t *testing.T) {
	good := manager.Config{
		ClusterMode: "default", Definition: "standard", WriterLabel: "mode-manager-stub",
		BuiltinDefault: "standby", Namespace: "mode-manager", FingerprintConfigMap: "mode-manager-fingerprint-default",
		DiscoveryInterval: time.Minute, Debounce: 2 * time.Second, MoveStepTimeout: 10 * time.Minute,
	}
	if err := validate(good); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	cases := map[string]func(*manager.Config){
		"an empty ClusterMode name":            func(c *manager.Config) { c.ClusterMode = "" },
		"a ClusterMode name over 63":           func(c *manager.Config) { c.ClusterMode = string(make([]byte, 64)) },
		"an empty definition":                  func(c *manager.Config) { c.Definition = "" },
		"a writer label with a space":          func(c *manager.Config) { c.WriterLabel = "bad value" },
		"a built-in default that isn't a mode": func(c *manager.Config) { c.BuiltinDefault = "Standby" },
		"a built-in default not in known modes": func(c *manager.Config) {
			c.KnownModes = []string{"normal", "peak"}
		},
		"a known mode that isn't a mode": func(c *manager.Config) { c.KnownModes = []string{"standby", "a b"} },
		"no namespace":                   func(c *manager.Config) { c.Namespace = "" },
		"a zero discovery interval":      func(c *manager.Config) { c.DiscoveryInterval = 0 },
		"a negative debounce":            func(c *manager.Config) { c.Debounce = -time.Second },
	}
	for name, change := range cases {
		cfg := good
		change(&cfg)
		if err := validate(cfg); err == nil {
			t.Errorf("%s: validate accepted it", name)
		}
	}
	dotted := good
	dotted.ClusterMode, dotted.WriterLabel = "eu.prod", ""
	if err := validate(dotted); err != nil {
		t.Errorf("a dotted ClusterMode name and no writer label: %v", err)
	}
}
