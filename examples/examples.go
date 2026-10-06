// Package examples embeds the example sandboxes, so `box new --from`
// works from an installed binary, not only from a checkout of the repo.
package examples

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed */*
var files embed.FS

// Names lists the examples, sorted.
func Names() []string {
	entries, _ := files.ReadDir(".")
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Load returns an example's Boxfile and any other files that come with it,
// keyed by file name. Only names from Names are accepted, so a name can never
// reach outside the embedded tree.
func Load(name string) (boxfile []byte, extras map[string][]byte, err error) {
	known := false
	for _, n := range Names() {
		known = known || n == name
	}
	if !known {
		return nil, nil, fmt.Errorf("no example %q; choose one of: %s", name, strings.Join(Names(), ", "))
	}
	entries, err := fs.ReadDir(files, name)
	if err != nil {
		return nil, nil, err
	}
	extras = map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := files.ReadFile(path.Join(name, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		if e.Name() == "Boxfile" {
			boxfile = b
		} else {
			extras[e.Name()] = b
		}
	}
	if boxfile == nil {
		return nil, nil, fmt.Errorf("example %q has no Boxfile", name)
	}
	return boxfile, extras, nil
}
