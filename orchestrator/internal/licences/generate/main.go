// Command generate writes internal/licences/components.json: the open source
// this product carries, with the full text of every licence.
//
// It is generated rather than kept by hand, because a list kept by hand is a
// list that is wrong. A dependency added on a Tuesday does not remind anybody to
// write it down, and the screen it feeds is the one place a person looks to find
// out what is really inside the thing they installed.
//
//	cd orchestrator && go run ./internal/licences/generate
//
// WHAT IT COUNTS, and why that is narrower than everything.
//
// An attribution obligation attaches to what is DISTRIBUTED. A linter that runs
// on a developer's machine is not distributed, and listing it is not extra
// honesty: it is noise that buries the entries that matter. So this counts the
// Go modules actually compiled into the binary (79, where the module graph
// reports 213) and the RUNTIME dependencies of the two front ends, not their
// build tools.
//
// Two things it cannot see, and which licences.go writes down instead because
// they are facts about the product rather than about a manifest: the source held
// in lib/, and MariaDB, which the desktop applications carry as a program.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// component mirrors licences.Component. It is declared again here rather than
// imported, so that generating the file cannot depend on the file it generates.
type component struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Part    string `json:"part"`
	Licence string `json:"licence,omitempty"`
	Text    string `json:"text,omitempty"`
	URL     string `json:"url,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "licences:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	all, err := goModules(root)
	if err != nil {
		return fmt.Errorf("the server's modules: %w", err)
	}
	for _, ui := range []struct{ dir, part string }{
		{"chat-ui", "Chat"},
		{"admin-ui", "Console"},
	} {
		packages, err := npmPackages(filepath.Join(root, ui.dir), ui.part)
		if err != nil {
			return fmt.Errorf("%s: %w", ui.dir, err)
		}
		all = append(all, packages...)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Part != all[j].Part {
			return all[i].Part < all[j].Part
		}
		return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
	})

	body, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(root, "orchestrator", "internal", "licences", "components.json")
	if err := os.WriteFile(out, append(body, '\n'), 0o600); err != nil {
		return err
	}

	// A component whose licence file could not be found is reported rather than
	// quietly written as an empty string. It is the one failure of this generator
	// that would otherwise look exactly like success.
	var missing []string
	for _, c := range all {
		if c.Text == "" {
			missing = append(missing, c.Part+"/"+c.Name)
		}
	}
	fmt.Printf("wrote %d components\n", len(all))
	if len(missing) > 0 {
		fmt.Printf("%d with no licence file found:\n", len(missing))
		for _, m := range missing {
			fmt.Println("  ", m)
		}
	}
	return nil
}

// repoRoot walks up until it finds the repository.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "COPYRIGHT")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no repository root above %s", dir)
		}
		dir = parent
	}
}

// goModules is every module compiled into the server binary.
//
// `go list -deps` over the command names the PACKAGES linked in, and each
// carries the module it came from. That is much narrower than the module graph
// and it is the right set: a module in go.mod that nothing imports is not in the
// binary, so it is not distributed and nobody is owed attribution for it.
func goModules(root string) ([]component, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", "./cmd/sag")
	cmd.Dir = filepath.Join(root, "orchestrator")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}

	type module struct{ Path, Version, Dir string }
	type pkg struct {
		Standard bool
		Module   *module
	}

	seen := map[string]bool{}
	var found []component
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for decoder.More() {
		var p pkg
		if err := decoder.Decode(&p); err != nil {
			return nil, err
		}
		// The standard library travels under Go's own licence, and this product
		// is not the one redistributing it.
		if p.Standard || p.Module == nil || p.Module.Path == "flexie.io/sag" {
			continue
		}
		if seen[p.Module.Path] {
			continue
		}
		seen[p.Module.Path] = true
		text, name := licenceIn(p.Module.Dir)
		found = append(found, component{
			Name:    p.Module.Path,
			Version: p.Module.Version,
			Part:    "Server",
			Licence: name,
			Text:    text,
			URL:     "https://" + p.Module.Path,
		})
	}
	return found, nil
}

// npmPackages is one front end's runtime dependencies: what ends up in the
// bundle a person downloads. devDependencies are build tools and stay here.
func npmPackages(dir, part string) ([]component, error) {
	manifest, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var declared struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifest, &declared); err != nil {
		return nil, err
	}

	out := make([]component, 0, len(declared.Dependencies))
	for name := range declared.Dependencies {
		installed := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		c := component{Name: name, Part: part, URL: "https://www.npmjs.com/package/" + name}
		// The INSTALLED copy is the authority on the version: package.json's
		// range says what was asked for, not what is here.
		if body, err := os.ReadFile(filepath.Join(installed, "package.json")); err == nil {
			var meta struct {
				Version string `json:"version"`
				License any    `json:"license"`
			}
			if json.Unmarshal(body, &meta) == nil {
				c.Version = meta.Version
				c.Licence = licenceName(meta.License)
			}
		}
		text, declaredName := licenceIn(installed)
		c.Text = text
		if c.Licence == "" {
			c.Licence = declaredName
		}
		out = append(out, c)
	}
	return out, nil
}

// licenceName reads npm's license field, a string in anything written this
// decade and an object in packages old enough to predate that.
func licenceName(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["type"].(string); ok {
			return s
		}
	}
	return ""
}

// licenceIn finds the licence file in a directory and reads it, with a guess at
// its name from its own first line.
func licenceIn(dir string) (text, name string) {
	if dir == "" {
		return "", ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	// A package carrying several is offering a choice, and the plainly named one
	// is the one it means.
	best, rank := "", 99
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		upper := strings.ToUpper(e.Name())
		var r int
		switch {
		case upper == "LICENSE", upper == "LICENCE", upper == "LICENSE.MD", upper == "LICENSE.TXT":
			r = 0
		case strings.HasPrefix(upper, "LICENSE"), strings.HasPrefix(upper, "LICENCE"):
			r = 1
		case upper == "COPYING", upper == "COPYING.MD":
			r = 2
		default:
			continue
		}
		if r < rank {
			best, rank = e.Name(), r
		}
	}
	if best == "" {
		return "", ""
	}
	body, err := os.ReadFile(filepath.Join(dir, best))
	if err != nil {
		return "", ""
	}
	return string(body), detect(string(body))
}

// detect names a licence from what the text says, for the label beside it.
//
// It reads the wording rather than the first line, because most of these files
// open with a copyright notice and say which licence they are several lines
// later: reading the top line left a third of the list unlabelled.
//
// The label is a convenience for scanning. The full text sits beside it and is
// the authority, so a licence this does not recognise comes back unlabelled
// rather than guessed at.
func detect(text string) string {
	has := func(phrases ...string) bool {
		for _, p := range phrases {
			if !strings.Contains(text, p) {
				return false
			}
		}
		return true
	}
	switch {
	case has("Apache License", "Version 2.0"):
		return "Apache-2.0"
	case has("Permission is hereby granted, free of charge"):
		return "MIT"
	case has("Permission to use, copy, modify"):
		// ISC, whose own text is published with and without the "and/or" in
		// "copy, modify, and/or distribute". Matching the longer phrase left a
		// real package unlabelled.
		return "ISC"
	case has("Redistribution and use in source and binary forms"):
		// The third clause is the no-endorsement one, and it is the whole
		// difference between the two BSD licences that are still in use.
		if has("Neither the name") || has("nor the names of its") {
			return "BSD-3-Clause"
		}
		return "BSD-2-Clause"
	case has("Mozilla Public License", "2.0"):
		return "MPL-2.0"
	case has("GNU GENERAL PUBLIC LICENSE", "Version 2"):
		return "GPL-2.0"
	case has("GNU GENERAL PUBLIC LICENSE", "Version 3"):
		return "GPL-3.0"
	case has("GNU LESSER GENERAL PUBLIC LICENSE"):
		return "LGPL"
	case has("This is free and unencumbered software released into the public domain"):
		return "Unlicense"
	}
	return ""
}
