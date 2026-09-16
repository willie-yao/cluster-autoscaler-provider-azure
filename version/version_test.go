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

package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakefileVersion(t *testing.T) {
	makefile, err := filepath.Abs("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "make"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("Makefile version regression requires %s: %v", name, err)
		}
	}
	for _, key := range []string{
		"VERSION", "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
	} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name      string
		tag       string
		dirty     bool
		staged    bool
		untracked bool
		noGit     bool
		override  string
		env       bool
	}{
		{name: "clean SHA"},
		{name: "dirty SHA", dirty: true},
		{name: "clean tag", tag: "cluster-autoscaler-v1.37.0"},
		{name: "dirty tag", tag: "cluster-autoscaler-v1.37.0", dirty: true},
		{name: "staged only matches upstream", staged: true},
		{name: "untracked only matches upstream", untracked: true},
		{name: "command-line override", dirty: true, override: "operator-version"},
		{name: "environment override", dirty: true, override: "operator-version", env: true},
		{name: "outside Git", noGit: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			want := "dev"
			if !tt.noGit {
				git("init", "--quiet")
				write("source", "baseline\n")
				git("add", "source")
				git(
					"-c", "user.name=Version test", "-c", "user.email=version-test@example.invalid",
					"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
					"commit", "--quiet", "-m", "fixture",
				)
				want = git("rev-parse", "HEAD")
				if tt.tag != "" {
					git("-c", "tag.gpgsign=false", "tag", tt.tag)
					want = strings.TrimPrefix(tt.tag, "cluster-autoscaler-")
				}
				if tt.dirty || tt.staged {
					write("source", "changed\n")
				}
				if tt.staged {
					git("add", "source")
				}
				if tt.untracked {
					write("untracked", "not included by upstream diff\n")
				}
				if tt.dirty {
					want += "-dirty"
				}
			}
			args := []string{"--no-print-directory", "-f", makefile, "-f", "-", "print-version"}
			if tt.override != "" {
				want = tt.override
				if tt.env {
					t.Setenv("VERSION", tt.override)
				} else {
					args = append(args, "VERSION="+tt.override)
				}
			}
			cmd := exec.Command("make", args...)
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader("print-version:\n\t@printf '%s\\n' '$(VERSION_LDFLAG)'\n")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make: %v\n%s", err, output)
			}
			want = "-X k8s.io/autoscaler/cluster-autoscaler/version.ClusterAutoscalerVersion=" + want
			if got := strings.TrimSpace(string(output)); got != want {
				t.Fatalf("version linker value = %q, want %q", got, want)
			}
		})
	}
}
