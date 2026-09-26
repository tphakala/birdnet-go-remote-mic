package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture texts shared by several tests.
const (
	apacheText  = "Apache License\nVersion 2.0, January 2004"
	unknownText = "All rights reserved."
	projectName = "remote-mic"
	apacheID    = "Apache-2.0"
)

// TestClassify pins the license names recognized from each license's own
// wording, including the OFL, whose grant sentence would otherwise read as MIT.
func TestClassify(t *testing.T) {
	t.Parallel()
	ofl, err := os.ReadFile("../../web/static/fonts/Inter-LICENSE.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text, want string
	}{
		{"mit", "Permission is hereby granted, free of charge, to any person", "MIT"},
		{"apache", apacheText, apacheID},
		{"bsd3", "Redistribution and use in source and binary forms ... Neither the name of", "BSD-3-Clause"},
		{"bsd2", "Redistribution and use in source and binary forms, with or without", "BSD-2-Clause"},
		{"isc", "Permission to use, copy, modify, and/or distribute this software for any purpose", "ISC"},
		{"mpl", "Mozilla Public License Version 2.0", "MPL-2.0"},
		{"ofl is not mit", string(ofl), "OFL-1.1"},
		{"dual", "MIT: Permission is hereby granted, free of charge ... Apache License Version 2.0", "Apache-2.0, MIT"},
		{"unknown", unknownText, "see license text"},
	} {
		if got := classify(tc.text); got != tc.want {
			t.Errorf("%s: classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUnrecognizedLicenseFails pins that a license file the classifier cannot
// name fails generation, while an unrecognized notice or patent file does not.
func TestUnrecognizedLicenseFails(t *testing.T) {
	t.Parallel()
	const license, notice = "LICENSE", "NOTICE"
	odd := component{name: "example.com/odd", files: []licenseFile{{name: license, text: unknownText}}}
	if err := checkRecognized(odd); !errors.Is(err, ErrUnrecognized) {
		t.Errorf("unrecognized LICENSE: err = %v, want ErrUnrecognized", err)
	}
	withNotice := component{name: "example.com/ok", files: []licenseFile{
		{name: license, text: "Permission is hereby granted, free of charge"},
		{name: notice, text: "This product includes software developed by someone."},
		{name: "PATENTS", text: "Additional IP Rights Grant (Patents)"},
	}}
	if err := checkRecognized(withNotice); err != nil {
		t.Errorf("recognized LICENSE with NOTICE and PATENTS: err = %v, want nil", err)
	}
	noticeOnly := component{name: "example.com/bare", files: []licenseFile{{name: notice, text: "Copyright someone."}}}
	if err := checkRecognized(noticeOnly); !errors.Is(err, ErrNoLicense) {
		t.Errorf("NOTICE only: err = %v, want ErrNoLicense", err)
	}
	if got := componentLicense(withNotice); got != "MIT" {
		t.Errorf("componentLicense = %q, want MIT (notice and patent files name no license)", got)
	}
}

// TestLicenseFilesRequiresOne pins the compliance gate: a module directory
// with no license or notice file is an error, not an empty entry.
func TestLicenseFilesRequiresOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/README.md", []byte("no license here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if files, err := licenseFiles(dir); err == nil {
		t.Fatalf("licenseFiles = %v, want an error for a directory with no license file", files)
	}
	if err := os.WriteFile(dir+"/COPYING", []byte("Permission is hereby granted, free of charge"), 0o600); err != nil {
		t.Fatal(err)
	}
	if files, err := licenseFiles(dir); err != nil || len(files) != 1 || files[0].name != "COPYING" {
		t.Fatalf("licenseFiles = %v, %v, want the COPYING file", files, err)
	}
}

// TestFenceOutlastsBackticks pins that license text containing a backtick run
// cannot close its code fence early.
func TestFenceOutlastsBackticks(t *testing.T) {
	t.Parallel()
	got := fence("a ```` b")
	if !strings.HasPrefix(got, "`````text\n") || !strings.HasSuffix(got, "\n`````\n") {
		t.Errorf("fence = %q, want a five-backtick fence around a four-backtick run", got)
	}
}

// TestIsLicenseName pins which top-level files count as license or notice files.
func TestIsLicenseName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"LICENSE": true, "LICENSE.md": true, "license.txt": true, "LICENCE": true,
		"COPYING": true, "NOTICE": true, "PATENTS": true, "README.md": false, "go.mod": false,
		"license.go": false, "notice.go": false,
	} {
		if got := isLicenseName(name); got != want {
			t.Errorf("isLicenseName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestRenderJSON pins the About page's contract: the project entry first, then
// every component in order with its summarized license and full texts, and an
// omitted version where the document shows "-". It reads the raw JSON, so the
// key names web/src/lib/about-core.ts parses are pinned too, not only a round
// trip through the same struct.
func TestRenderJSON(t *testing.T) {
	t.Parallel()
	project := component{name: projectName, files: []licenseFile{{name: projectLicense, text: "MIT License\n\nPermission is hereby granted, free of charge"}}}
	comps := []component{
		{name: "example.com/a", version: "v1.0.0", files: []licenseFile{{name: "LICENSE.txt", text: "Apache License\nVersion 2.0"}}},
		{name: "Go standard library and runtime", files: []licenseFile{
			{name: "LICENSE.md", text: "Redistribution and use in source and binary forms ... Neither the name of"},
			{name: "NOTICE.txt", text: "Portions copyright the authors"},
		}},
	}
	b, err := renderJSON(project, comps)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	type entry = map[string]any
	proj, ok := doc["project"].(entry)
	if !ok {
		t.Fatalf("project = %v, want an object", doc["project"])
	}
	const wantProjectLicense = "MIT"
	if proj["name"] != projectName || proj["license"] != wantProjectLicense {
		t.Errorf("project = %v, want name remote-mic, license MIT", proj)
	}
	if _, has := proj["version"]; has {
		t.Error("project has a version key; want it omitted")
	}
	files, ok := proj["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("project files = %v, want one file", proj["files"])
	}
	if f, _ := files[0].(entry); f["name"] != projectLicense || f["text"] != project.files[0].text {
		t.Errorf("project file = %v, want LICENSE and its text", files[0])
	}
	list, ok := doc["components"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("components = %v, want 2", doc["components"])
	}
	a, _ := list[0].(entry)
	if a["name"] != "example.com/a" || a["version"] != "v1.0.0" || a["license"] != apacheID {
		t.Errorf("component 0 = %v, want example.com/a v1.0.0 Apache-2.0", a)
	}
	std, _ := list[1].(entry)
	if _, has := std["version"]; has {
		t.Error("component 1 has a version key; want it omitted")
	}
	if std["license"] != "BSD-3-Clause" {
		t.Errorf("component 1 license = %v, want BSD-3-Clause", std["license"])
	}
	stdFiles, _ := std["files"].([]any)
	if len(stdFiles) != 2 {
		t.Fatalf("component 1 files = %v, want 2", std["files"])
	}
	if f, _ := stdFiles[1].(entry); f["name"] != "NOTICE.txt" || f["text"] != "Portions copyright the authors" {
		t.Errorf("component 1 file 1 = %v, want the NOTICE and its text", stdFiles[1])
	}
	if !strings.HasSuffix(string(b), "}\n") || strings.HasSuffix(string(b), "\n\n") {
		t.Error("output does not end in exactly one newline")
	}
}

// TestRenderJSONKeepsHTMLCharacters pins that <, > and & in a license text are
// written literally, not as \u003c, \u003e and \u0026, so the committed file
// reads like its source texts.
func TestRenderJSONKeepsHTMLCharacters(t *testing.T) {
	t.Parallel()
	const text = "Copyright <someone@example.com> & others"
	project := component{name: projectName, files: []licenseFile{{name: projectLicense, text: text}}}
	b, err := renderJSON(project, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"text": "`+text+`"`) {
		t.Errorf("got %s, want the text with <, > and & unescaped", got)
	}
}

// TestSyncOutputs pins the write and -check behaviour for every generated
// file: write mode writes each one, and check mode fails, naming the path,
// when any of them (not only the first) is missing or differs.
func TestSyncOutputs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	md, js := filepath.Join(dir, "a.md"), filepath.Join(dir, "b.json")
	outs := []output{{md, []byte("doc\n")}, {js, []byte("{}\n")}}

	if err := syncOutputs(outs, true); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), md) {
		t.Fatalf("check with nothing written: got %v, want ErrStale naming %s", err, md)
	}
	if err := syncOutputs(outs, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, o := range outs {
		got, err := os.ReadFile(o.path)
		if err != nil || !bytes.Equal(got, o.data) {
			t.Errorf("%s = %q, %v; want %q", o.path, got, err, o.data)
		}
	}
	if err := syncOutputs(outs, true); err != nil {
		t.Fatalf("check after write: got %v, want nil", err)
	}
	if err := os.WriteFile(js, []byte("{\"stale\": true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncOutputs(outs, true); !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), js) {
		t.Fatalf("check with the second file stale: got %v, want ErrStale naming %s", err, js)
	}
}

// TestGeneratedOutputs pins that both generated files are maintained, so
// dropping one from the list cannot pass unnoticed (-check would then stop
// comparing it).
func TestGeneratedOutputs(t *testing.T) {
	t.Parallel()
	outs := generatedOutputs([]byte("doc"), []byte("js"))
	if len(outs) != 2 || outs[0].path != outFile || outs[1].path != jsonFile {
		t.Fatalf("got %+v, want %s then %s", outs, outFile, jsonFile)
	}
	if string(outs[0].data) != "doc" || string(outs[1].data) != "js" {
		t.Errorf("got data %q, %q; want doc, js", outs[0].data, outs[1].data)
	}
}

// TestRenderNamesProjectLicense pins that the document names remote-mic's own
// license from its LICENSE text rather than a hardcoded one.
func TestRenderNamesProjectLicense(t *testing.T) {
	t.Parallel()
	// render takes the license from the files; the name is not printed.
	project := component{files: []licenseFile{{name: projectLicense, text: apacheText}}}
	doc := string(render(project, nil))
	if want := "remote-mic itself is licensed under Apache-2.0; see LICENSE and NOTICE."; !strings.Contains(doc, want) {
		t.Errorf("document lacks %q", want)
	}
}

// TestReadProject pins that remote-mic's LICENSE and NOTICE are both read, in
// that order, that a missing NOTICE fails, and that a stray file beside them
// is not picked up.
func TestReadProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(projectLicense, "Apache License\nVersion 2.0")
	write("LICENSE.orig", "stale")
	if _, err := readProject(dir); err == nil {
		t.Fatal("readProject without a NOTICE succeeded; want an error")
	}
	write(projectNotice, "Copyright holder")
	p, err := readProject(dir)
	if err != nil {
		t.Fatalf("readProject: %v", err)
	}
	if len(p.files) != 2 || p.files[0].name != projectLicense || p.files[1].name != projectNotice {
		t.Errorf("files = %+v, want LICENSE then NOTICE only", p.files)
	}
}

// TestLoadProject pins the gate on remote-mic's own LICENSE: a recognized one
// loads, while an unrecognized or missing one fails with an error naming the
// cause, so the document can never call remote-mic "see license text".
func TestLoadProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		license string // "" leaves LICENSE out
		wantErr error  // nil means success
		wantMsg string // substring the error must carry
	}{
		{name: "recognized", license: apacheText},
		{name: "unrecognized", license: unknownText, wantErr: ErrUnrecognized, wantMsg: "remote-mic: LICENSE"},
		{name: "missing", wantErr: os.ErrNotExist, wantMsg: "remote-mic LICENSE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.license != "" {
				if err := os.WriteFile(filepath.Join(dir, projectLicense), []byte(tc.license), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, projectNotice), []byte("Copyright holder"), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := loadProject(dir)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("got error %v, want nil", err)
				}
				if got := componentLicense(p); got != apacheID {
					t.Errorf("got license %q, want Apache-2.0", got)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("got error %v, want %v mentioning %q", err, tc.wantErr, tc.wantMsg)
			}
		})
	}
}

// TestGenerate drives the generator end to end in a temporary tree: a write
// produces both files, a check of them passes, a changed NOTICE makes the check
// fail, and an unrecognized project LICENSE is refused. It changes the working
// directory, so it cannot run in parallel.
func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Dir(jsonFile), 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(projectLicense, apacheText)
	write(projectNotice, "Copyright holder")
	comps := []component{{name: "example.com/gen", version: "v2.3.4", files: []licenseFile{{name: "LICENCE", text: "Permission is hereby granted, free of charge"}}}}

	if err := generate(comps, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, name := range []string{outFile, jsonFile} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
	if err := generate(comps, true); err != nil {
		t.Fatalf("check after write: got %v, want nil", err)
	}
	write(projectNotice, "A different holder")
	if err := generate(comps, true); !errors.Is(err, ErrStale) {
		t.Fatalf("check after a NOTICE change: got %v, want ErrStale", err)
	}
	write(projectLicense, unknownText)
	if err := generate(comps, false); !errors.Is(err, ErrUnrecognized) {
		t.Fatalf("unrecognized LICENSE: got %v, want ErrUnrecognized", err)
	}
}

// TestSyncOutputsIOErrors pins that a file that cannot be written or read is an
// error of its own, not a silent pass or a misleading ErrStale.
func TestSyncOutputsIOErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missingDir := []output{{filepath.Join(dir, "no", "such", "dir", "a.md"), []byte("x")}}
	if err := syncOutputs(missingDir, false); err == nil {
		t.Error("write into a missing directory succeeded; want an error")
	}
	aDir := []output{{dir, []byte("x")}}
	if err := syncOutputs(aDir, true); err == nil || errors.Is(err, ErrStale) {
		t.Errorf("check of a directory: got %v, want a read error that is not ErrStale", err)
	}
}
