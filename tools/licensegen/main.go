// Command licensegen writes THIRD_PARTY_LICENSES.md: the license of every
// third-party component shipped in the remote-mic binary. That is the Go
// modules linked into ./cmd/remotemic for each release target, the Go standard
// library and runtime, and the fonts bundled with the web UI. It also writes
// the same data, plus remote-mic's own LICENSE and NOTICE, as
// web/static/licenses.json, which the web UI's About page loads, so the page
// never drifts from the document.
//
// The module list comes from the build graph (go list -deps), never from a
// hand-kept list, so a dependency added or dropped by a go.mod change shows up
// the next time the file is generated. A module without a license file fails
// the run: shipping it would be a compliance gap, not a formatting problem.
//
// Usage, from the repository root:
//
//	go run ./tools/licensegen          # rewrite both generated files
//	go run ./tools/licensegen -check   # exit 1 if either is out of date
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// outFile is the generated document, relative to the repository root.
const outFile = "THIRD_PARTY_LICENSES.md"

// jsonFile is the About page's copy of the same data; web:build copies
// web/static into the embedded UI.
const jsonFile = "web/static/licenses.json"

// projectLicense is remote-mic's own license, shown first on the About page
// with its NOTICE; both sit at the repository root.
const projectLicense = "LICENSE"

// projectNotice is remote-mic's Apache NOTICE, read next to projectLicense.
const projectNotice = "NOTICE"

// mainPackage is the binary whose dependencies are listed.
const mainPackage = "./cmd/remotemic"

// unknownLicense is what classify returns for text it cannot name.
const unknownLicense = "see license text"

// target is one release build whose linked modules are collected; the union
// over every target is listed, since each architecture can link a different
// set (golang.org/x/sys, for one, varies by GOARCH).
type target struct{ goarch, goarm string }

// targets mirrors the release builds in .goreleaser.yaml.
var targets = []target{{"amd64", ""}, {"arm64", ""}, {"arm", "6"}}

// asset is a non-Go component bundled into the binary, with the license file
// kept beside it in the repository.
type asset struct{ name, license string }

// assets are the fonts the web UI embeds (web/static/fonts).
var assets = []asset{
	{"Inter (web UI font)", "web/static/fonts/Inter-LICENSE.txt"},
	{"JetBrains Mono (web UI font)", "web/static/fonts/JetBrainsMono-LICENSE.txt"},
}

// component is one entry of the document.
type component struct {
	name, version string
	files         []licenseFile
}

// licenseFile is one license or notice file of a component.
type licenseFile struct{ name, text string }

// ErrStale reports that a committed generated file differs from a fresh one.
var ErrStale = errors.New("is out of date; run: task licenses:generate")

func main() {
	check := flag.Bool("check", false, "exit 1 if "+outFile+" or "+jsonFile+" is out of date instead of rewriting them")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, "licensegen:", err)
		os.Exit(1)
	}
}

func run(check bool) error {
	comps, err := collect()
	if err != nil {
		return err
	}
	return generate(comps, check)
}

// generate writes both generated files for comps, or with check compares them,
// from the project's LICENSE and NOTICE in the current directory. It is run()
// without the module listing, so tests can drive it in a temporary directory.
func generate(comps []component, check bool) error {
	project, err := loadProject(".")
	if err != nil {
		return err
	}
	js, err := renderJSON(project, comps)
	if err != nil {
		return err
	}
	return syncOutputs(generatedOutputs(render(project, comps), js), check)
}

// generatedOutputs is every file the tool maintains, in write order: the
// markdown document and the About page's JSON copy.
func generatedOutputs(doc, js []byte) []output {
	return []output{{outFile, doc}, {jsonFile, js}}
}

// loadProject reads remote-mic's LICENSE and NOTICE from dir and holds them to
// the same bar as every dependency: a LICENSE the classifier cannot name would
// list remote-mic itself as "see license text" on the About page and in the
// document, so it fails instead.
func loadProject(dir string) (component, error) {
	project, err := readProject(dir)
	if err != nil {
		return component{}, err
	}
	if err := checkRecognized(project); err != nil {
		return component{}, err
	}
	return project, nil
}

// readProject reads exactly remote-mic's LICENSE and NOTICE from dir (Apache
// 2.0 section 4(d) wants the NOTICE carried along), both required; a stray
// LICENSE.orig or NOTICE~ beside them is never read.
func readProject(dir string) (component, error) {
	project := component{name: "remote-mic"}
	for _, name := range []string{projectLicense, projectNotice} {
		f, err := readLicense(filepath.Join(dir, name))
		if err != nil {
			return component{}, fmt.Errorf("remote-mic %s: %w", name, err)
		}
		project.files = append(project.files, f)
	}
	return project, nil
}

// output is one generated file and the bytes it must hold.
type output struct {
	path string
	data []byte
}

// syncOutputs writes each output, or with check compares it with the file on
// disk instead and returns an error wrapping ErrStale that names the first
// file missing or different.
func syncOutputs(outs []output, check bool) error {
	for _, o := range outs {
		if !check {
			if err := os.WriteFile(o.path, o.data, 0o644); err != nil { //nolint:gosec // public documents in the repository, meant to be world-readable
				return err
			}
			continue
		}
		cur, err := os.ReadFile(o.path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !bytes.Equal(cur, o.data) {
			return fmt.Errorf("%s %w", o.path, ErrStale)
		}
	}
	return nil
}

// collect gathers every component: the linked modules (sorted by path), then
// the Go standard library, then the bundled assets. It fails on a component
// with no license file or with one the classifier cannot name.
func collect() ([]component, error) {
	mods := map[string]module{}
	for _, t := range targets {
		if err := listModules(t, mods); err != nil {
			return nil, err
		}
	}
	paths := slices.Sorted(func(yield func(string) bool) {
		for p := range mods {
			if !yield(p) {
				return
			}
		}
	})
	comps := make([]component, 0, len(paths)+1+len(assets))
	for _, p := range paths {
		m := mods[p]
		files, err := licenseFiles(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("module %s %s: %w", m.Path, m.Version, err)
		}
		comps = append(comps, component{name: m.Path, version: m.Version, files: files})
	}

	goroot, err := goEnv("GOROOT")
	if err != nil {
		return nil, err
	}
	// The Go distribution's LICENSE and PATENTS grant sit at the top of GOROOT,
	// the same layout licenseFiles reads for a module.
	std, err := licenseFiles(goroot)
	if err != nil {
		return nil, fmt.Errorf("go standard library: %w", err)
	}
	// No version: it would follow whichever toolchain generated the file, and
	// the license does not change between releases.
	comps = append(comps, component{name: "Go standard library and runtime", files: std})

	for _, a := range assets {
		f, err := readLicense(a.license)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.name, err)
		}
		comps = append(comps, component{name: a.name, files: []licenseFile{f}})
	}
	for _, c := range comps {
		if err := checkRecognized(c); err != nil {
			return nil, err
		}
	}
	return comps, nil
}

// module is the part of go list's JSON this tool reads.
type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

// listModules adds the non-main modules linked into mainPackage for t to mods.
func listModules(t target, mods map[string]module) error {
	// skipfrontend selects the web embed stub, so listing needs no built web/dist;
	// it does not change which modules are linked.
	cmd := exec.Command("go", "list", "-deps", "-json=Module", "-tags", "skipfrontend", mainPackage)
	// GOWORK=off: under a go.work every workspace module reports Main and would
	// be left out, though the released binary links it from the module cache.
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0", "GOARCH="+t.goarch, "GOARM="+t.goarm, "GOWORK=off")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list for linux/%s: %w", t.goarch, err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct{ Module *module }
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode go list output: %w", err)
		}
		m := pkg.Module
		if m == nil || m.Main { // the standard library has no module
			continue
		}
		if m.Replace != nil {
			m.Version, m.Dir = m.Replace.Version, m.Replace.Dir
		}
		mods[m.Path] = *m
	}
}

// licenseFiles returns the license, notice and patent files at the top of a module
// directory, sorted by name. None is an error.
func licenseFiles(dir string) ([]licenseFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []licenseFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !isLicenseName(e.Name()) {
			continue
		}
		f, err := readLicense(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, errors.New("no LICENSE, COPYING, NOTICE or PATENTS file found")
	}
	return files, nil
}

// isLicenseName reports whether a file name is a license, notice or patent
// grant file (LICENSE, LICENSE.md, LICENCE, COPYING, NOTICE, PATENTS and the
// like). Go source files such as license.go are code, not license text.
func isLicenseName(name string) bool {
	if strings.HasSuffix(name, ".go") {
		return false
	}
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// readLicense reads a license file with CRLF line endings normalized.
func readLicense(path string) (licenseFile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // paths come from the module cache, GOROOT, the fixed asset table, or the repository LICENSE and NOTICE
	if err != nil {
		return licenseFile{}, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	return licenseFile{name: filepath.Base(path), text: strings.TrimRight(text, "\n")}, nil
}

func goEnv(key string) (string, error) {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// classify names the licenses a text carries, recognizing the common ones by
// their wording; anything else is left to the reader of the full text.
func classify(text string) string {
	var ids []string
	has := func(s string) bool { return strings.Contains(text, s) }
	if has("Apache License") && has("Version 2.0") {
		ids = append(ids, "Apache-2.0")
	}
	ofl := has("SIL OPEN FONT LICENSE") || has("SIL Open Font License")
	// The OFL grant opens with the same sentence as MIT's, so it is not MIT.
	if has("Permission is hereby granted, free of charge") && !ofl {
		ids = append(ids, "MIT")
	}
	if has("Redistribution and use in source and binary forms") {
		if has("Neither the name") || has("names of its contributors") {
			ids = append(ids, "BSD-3-Clause")
		} else {
			ids = append(ids, "BSD-2-Clause")
		}
	}
	if has("Mozilla Public License") {
		ids = append(ids, "MPL-2.0")
	}
	if has("Permission to use, copy, modify, and/or distribute this software") {
		ids = append(ids, "ISC")
	}
	if ofl {
		ids = append(ids, "OFL-1.1")
	}
	if len(ids) == 0 {
		return unknownLicense
	}
	return strings.Join(ids, ", ")
}

// componentLicense summarizes a component's licenses over all its files.
// Notice and patent files carry attributions or grants, not a license, so
// they are listed in full but do not name one.
func componentLicense(c component) string {
	var ids []string
	for _, f := range c.files {
		if isNoticeName(f.name) {
			continue
		}
		for id := range strings.SplitSeq(classify(f.text), ", ") {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.SortFunc(ids, cmp.Compare)
	return strings.Join(ids, ", ")
}

// isNoticeName reports whether a license file is a notice or patent grant
// rather than a license.
func isNoticeName(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "NOTICE") || strings.HasPrefix(upper, "PATENTS")
}

// ErrUnrecognized reports a license file the classifier cannot name. A new or
// unusual license needs a human look (and a case in classify) before it ships,
// rather than a quiet "see license text" row.
var ErrUnrecognized = errors.New("unrecognized license; review it and teach classify to name it")

// ErrNoLicense reports a component whose only files are notices or patent
// grants: listed, but carrying no license to name.
var ErrNoLicense = errors.New("only notice or patent files, no license")

// checkRecognized fails when any license file of c is unrecognized, or when c
// has no license file at all besides notices and patent grants.
func checkRecognized(c component) error {
	licensed := false
	for _, f := range c.files {
		if isNoticeName(f.name) {
			continue
		}
		licensed = true
		if classify(f.text) == unknownLicense {
			return fmt.Errorf("%s: %s: %w", c.name, f.name, ErrUnrecognized)
		}
	}
	if !licensed {
		return fmt.Errorf("%s: %w", c.name, ErrNoLicense)
	}
	return nil
}

// fence wraps license text in a code fence longer than any backtick run inside
// it, so no text can close the fence early.
func fence(text string) string {
	n := 3
	for run, i := 0, 0; i < len(text); i++ {
		if text[i] == '`' {
			run++
			n = max(n, run+1)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", n)
	return f + "text\n" + text + "\n" + f + "\n"
}

func render(project component, comps []component) []byte {
	var b strings.Builder
	b.WriteString("# Third-party licenses\n\n")
	b.WriteString("Generated by `task licenses:generate` (tools/licensegen); do not edit by hand. ")
	b.WriteString("It covers every third-party component shipped in the remote-mic binary: the Go modules linked into it for linux/amd64, linux/arm64 and linux/arm, the Go standard library and runtime, and the fonts bundled with the web UI. ")
	fmt.Fprintf(&b, "remote-mic itself is licensed under %s; see LICENSE and NOTICE.\n\n", componentLicense(project))
	b.WriteString("| Component | Version | License |\n|---|---|---|\n")
	for _, c := range comps {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.name, cmp.Or(c.version, "-"), componentLicense(c))
	}
	for _, c := range comps {
		fmt.Fprintf(&b, "\n## %s", c.name)
		if c.version != "" {
			fmt.Fprintf(&b, " %s", c.version)
		}
		b.WriteString("\n")
		for _, f := range c.files {
			fmt.Fprintf(&b, "\n%s:\n\n%s", f.name, fence(f.text))
		}
	}
	return []byte(b.String())
}

// licenseDoc is the JSON the web UI's About page reads (web/src/lib/about-core.ts
// validates the same shape).
type licenseDoc struct {
	Project    licenseEntry   `json:"project"`
	Components []licenseEntry `json:"components"`
}

type licenseEntry struct {
	Name    string      `json:"name"`
	Version string      `json:"version,omitempty"`
	License string      `json:"license"`
	Files   []fileEntry `json:"files"`
}

type fileEntry struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// renderJSON encodes remote-mic's own entry and every component, in the
// document's order, indented so a diff of the committed file stays readable.
// HTML escaping is off so license texts keep <, > and & literally rather than
// as \u003c and the like; the About page renders them as text, never as HTML.
func renderJSON(project component, comps []component) ([]byte, error) {
	entry := func(c component) licenseEntry {
		files := make([]fileEntry, 0, len(c.files))
		for _, f := range c.files {
			files = append(files, fileEntry{Name: f.name, Text: f.text})
		}
		return licenseEntry{Name: c.name, Version: c.version, License: componentLicense(c), Files: files}
	}
	doc := licenseDoc{
		Project:    entry(project),
		Components: make([]licenseEntry, 0, len(comps)),
	}
	for _, c := range comps {
		doc.Components = append(doc.Components, entry(c))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	// Encode ends the document with a newline, as the committed file does.
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
