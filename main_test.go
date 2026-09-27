/*
Copyright The Kubernetes Authors.

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
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestAKSCompatibilityFlags(t *testing.T) {
	fs := pflag.NewFlagSet("aks", pflag.ContinueOnError)
	addAKSCompatibilityFlags(fs)

	for _, name := range []string{"enable-force-delete", "enable-dynamic-instance-list", "enable-detailed-cse-message"} {
		flag := fs.Lookup(name)
		if flag == nil || flag.Value.Type() != "bool" || flag.DefValue != "false" {
			t.Errorf("%s should be a false-by-default boolean flag: %v", name, flag)
		}
		if flag != nil && !strings.Contains(flag.Usage, "No effect") {
			t.Errorf("%s should say it has no effect", name)
		}
	}
	if flag := fs.Lookup("config-path"); flag == nil || flag.Value.Type() != "string" || flag.DefValue != "" {
		t.Errorf("config-path should be an empty-by-default string flag: %v", flag)
	}

	if err := fs.Parse([]string{
		"--config-path=/opt/conf/autoscaler/settings.json",
		"--enable-force-delete",
		"--enable-dynamic-instance-list=true",
		"--enable-detailed-cse-message=false",
	}); err != nil {
		t.Fatal(err)
	}
	if path, err := fs.GetString("config-path"); err != nil || path != "/opt/conf/autoscaler/settings.json" {
		t.Errorf("config-path = %q, %v", path, err)
	}
	for name, want := range map[string]bool{
		"enable-force-delete":          true,
		"enable-dynamic-instance-list": true,
		"enable-detailed-cse-message":  false,
	} {
		if got, err := fs.GetBool(name); err != nil || got != want {
			t.Errorf("%s = %t, %v; want %t", name, got, err, want)
		}
	}
}

func TestAKSCompatibilityFlagsRejectInvalidBool(t *testing.T) {
	fs := pflag.NewFlagSet("aks", pflag.ContinueOnError)
	addAKSCompatibilityFlags(fs)
	if err := fs.Parse([]string{"--enable-force-delete=invalid"}); err == nil {
		t.Fatal("invalid boolean must not be accepted")
	}
}
