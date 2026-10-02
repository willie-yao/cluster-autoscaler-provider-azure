#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail
cd "$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"

python3 - <<'PY'
import json
from pathlib import Path
import re
import subprocess
import tempfile


def git(*args, cwd=None):
    command = ["git"]
    if cwd is not None:
        command.append(f"--git-dir={cwd}")
    return subprocess.check_output([*command, *args])


def tree(ref, cwd=None):
    result = {}
    for entry in git("ls-tree", "-rz", ref, cwd=cwd).split(b"\0"):
        if entry:
            meta, path = entry.split(b"\t")
            mode, kind, blob = meta.decode().split()
            if kind != "blob":
                raise SystemExit(f"Unexpected object type: {path!r}")
            result[path.decode()] = (mode, blob)
    return result


def fail(message):
    raise SystemExit(message)


commits = git("rev-list", "--reverse", "HEAD").decode().splitlines()
if len(commits) < 3:
    fail("Import verification requires the first three commits.")
for commit in commits:
    if len(git("rev-list", "--parents", "-n", "1", commit).split()) > 2:
        fail("Import verification requires linear history.")
first, second, third = commits[:3]
document = git("show", f"{first}:docs/provenance.md").decode()
match = re.search(r"<!-- import-manifest\n(.*?)\n-->", document, re.S)
if not match:
    fail("The first commit has no import manifest.")
manifest = json.loads(match.group(1))
files = manifest["files"]
sources = {entry["source"] for entry in files}
targets = {entry["target"] for entry in files}
extras = {"docs/provenance.md", "hack/verify-upstream.sh"}
if len(sources) != len(files) or len(targets) != len(files):
    fail("Import paths are not unique.")
one, two, three = (tree(commit) for commit in commits[:3])
if set(one) != sources | extras or set(two) != targets | extras or set(three) != set(two):
    fail("Unexpected added or removed files in the import commits.")
with tempfile.TemporaryDirectory(prefix="azure-autoscaler-upstream-") as upstream:
    subprocess.run(["git", "init", "--bare", "--quiet", upstream], check=True)
    subprocess.run(
        ["git", f"--git-dir={upstream}", "fetch", "--quiet", "--depth=1",
         "--filter=blob:none", manifest["url"], manifest["upstream"]],
        check=True,
    )
    original = tree(manifest["upstream"], cwd=upstream)
    for entry in files:
        source, target = entry["source"], entry["target"]
        identity = (entry["mode"], entry["blob"])
        if original.get(source) != identity or one[source] != identity:
            fail(f"Upstream import mismatch: {source}")
        if two[target] != identity:
            fail(f"Rename changed content or mode: {source} -> {target}")
        data = git("show", f"{second}:{target}")
        expected = data
        try:
            text = data.decode()
        except UnicodeDecodeError:
            pass
        else:
            for before, after in manifest["substitutions"]:
                text = text.replace(before, after)
            expected = text.encode()
            if target.endswith(".go") and expected != data:
                expected = subprocess.check_output(["gofmt"], input=expected)
        actual = git("show", f"{third}:{target}")
        if three[target][0] != identity[0] or actual != expected:
            fail(f"Module substitution changed unexpected content: {target}")
for path in extras:
    if not (one[path] == two[path] == three[path]):
        fail(f"Import metadata changed during rename commits: {path}")
renames = git("diff", "--name-status", "-M100%", first, second).decode().splitlines()
if any(not line.startswith("R100\t") for line in renames):
    fail("The second commit includes a change other than a pure rename.")
print(f"PASS upstream {manifest['upstream']}: {len(files)} identical files and modes")
print(f"PASS layout {second}: {len(renames)} pure renames")
print(f"PASS module {third}: only recorded substitutions and Go formatting")
later = git("rev-list", "--count", f"{third}..HEAD").decode().strip()
print(f"REPORT later commits: {later}")
subprocess.run(["git", "--no-pager", "diff", "--stat", f"{third}..HEAD"], check=True)
PY
