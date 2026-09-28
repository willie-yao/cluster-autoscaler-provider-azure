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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
)

var fields = []struct {
	name string
	kind token.Token
}{
	{"InstanceType", token.STRING},
	{"SkuFamily", token.STRING},
	{"VCPU", token.INT},
	{"MemoryMb", token.INT},
	{"GPU", token.INT},
	{"Architecture", token.STRING},
}

type instanceTypes map[string]map[string]string

type fieldChange struct {
	name string
	old  string
	new  string
}

type skuChange struct {
	name   string
	fields []fieldChange
}

type comparison struct {
	added   []string
	removed []string
	changed []skuChange
}

func main() {
	different, err := verify()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if different {
		os.Exit(1)
	}
}

func verify() (bool, error) {
	dir, err := os.MkdirTemp("", "azure-instance-types-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)

	livePath := filepath.Join(dir, "azure_instance_types.go")
	cmd := exec.Command("go", "run", "./cloudprovider/azure/azure_instance_types/gen.go", "-output", livePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("generate Azure instance types: %w", err)
	}

	committed, err := readInstanceTypes("cloudprovider/azure/azure_instance_types.go")
	if err != nil {
		return false, err
	}
	live, err := readInstanceTypes(livePath)
	if err != nil {
		return false, err
	}
	if len(live) == 0 {
		return false, fmt.Errorf("Azure returned no VM SKUs")
	}

	diff := compareInstanceTypes(committed, live)
	diff.write(os.Stdout)
	return diff.hasChanges(), nil
}

func readInstanceTypes(path string) (instanceTypes, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}

	var entries *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if name.Name != "InstanceTypes" {
					continue
				}
				if entries != nil || i >= len(value.Values) {
					return nil, fmt.Errorf("%s: invalid InstanceTypes declaration", path)
				}
				entries, ok = value.Values[i].(*ast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("%s: InstanceTypes is not a map literal", path)
				}
			}
		}
	}
	if entries == nil {
		return nil, fmt.Errorf("%s: InstanceTypes map not found", path)
	}

	result := make(instanceTypes, len(entries.Elts))
	for _, element := range entries.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("%s: invalid SKU entry", path)
		}
		key, ok := entry.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			return nil, fmt.Errorf("%s: invalid SKU name", path)
		}
		name, err := strconv.Unquote(key.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid SKU name: %w", path, err)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("%s: duplicate SKU %q", path, name)
		}
		value, ok := entry.Value.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("%s: invalid fields for SKU %q", path, name)
		}
		values := make(map[string]string, len(fields))
		for _, element := range value.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("%s: invalid field for SKU %q", path, name)
			}
			key, ok := field.Key.(*ast.Ident)
			if !ok {
				return nil, fmt.Errorf("%s: invalid field name for SKU %q", path, name)
			}
			var kind token.Token
			for _, expected := range fields {
				if key.Name == expected.name {
					kind = expected.kind
					break
				}
			}
			if kind == token.ILLEGAL {
				return nil, fmt.Errorf("%s: unknown field %s for SKU %q", path, key.Name, name)
			}
			if _, exists := values[key.Name]; exists {
				return nil, fmt.Errorf("%s: duplicate field %s for SKU %q", path, key.Name, name)
			}
			lit, ok := field.Value.(*ast.BasicLit)
			if !ok || lit.Kind != kind {
				return nil, fmt.Errorf("%s: invalid %s for SKU %q", path, key.Name, name)
			}
			if kind == token.STRING {
				values[key.Name], err = strconv.Unquote(lit.Value)
			} else {
				var n int64
				n, err = strconv.ParseInt(lit.Value, 0, 64)
				values[key.Name] = strconv.FormatInt(n, 10)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: invalid %s for SKU %q: %w", path, key.Name, name, err)
			}
		}
		for _, field := range fields {
			if _, exists := values[field.name]; !exists {
				return nil, fmt.Errorf("%s: missing %s for SKU %q", path, field.name, name)
			}
		}
		result[name] = values
	}
	return result, nil
}

func compareInstanceTypes(committed, live instanceTypes) comparison {
	var diff comparison
	for name, current := range committed {
		latest, exists := live[name]
		if !exists {
			diff.removed = append(diff.removed, name)
			continue
		}
		var changed skuChange
		changed.name = name
		for _, field := range fields {
			if current[field.name] != latest[field.name] {
				changed.fields = append(changed.fields, fieldChange{field.name, current[field.name], latest[field.name]})
			}
		}
		if len(changed.fields) > 0 {
			diff.changed = append(diff.changed, changed)
		}
	}
	for name := range live {
		if _, exists := committed[name]; !exists {
			diff.added = append(diff.added, name)
		}
	}
	sort.Strings(diff.added)
	sort.Strings(diff.removed)
	sort.Slice(diff.changed, func(i, j int) bool { return diff.changed[i].name < diff.changed[j].name })
	return diff
}

func (diff comparison) hasChanges() bool {
	return len(diff.added)+len(diff.removed)+len(diff.changed) > 0
}

func (diff comparison) write(w io.Writer) {
	fmt.Fprintf(w, "Summary: %d added, %d removed, %d changed\n", len(diff.added), len(diff.removed), len(diff.changed))
	for _, name := range diff.added {
		fmt.Fprintf(w, "Added: %s\n", name)
	}
	for _, name := range diff.removed {
		fmt.Fprintf(w, "Removed: %s\n", name)
	}
	for _, change := range diff.changed {
		fmt.Fprintf(w, "Changed: %s\n", change.name)
		for _, field := range change.fields {
			fmt.Fprintf(w, "  %s: %q -> %q\n", field.name, field.old, field.new)
		}
	}
}
