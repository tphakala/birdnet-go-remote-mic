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
		{"apache", "Apache License\nVersion 2.0, January 2004", "Apache-2.0"},
		{"bsd3", "Redistribution and use in source and binary forms ... Neither the name of", "BSD-3-Clause"},
		{"bsd2", "Redistribution and use in source and binary forms, with or without", "BSD-2-Clause"},
		{"isc", "Permission to use, copy, modify, and/or distribute this software for any purpose", "ISC"},
		{"mpl", "Mozilla Public License Version 2.0", "MPL-2.0"},
		{"ofl is not mit", string(ofl), "OFL-1.1"},
		{"dual", "MIT: Permission is hereby granted, free of charge ... Apache License Version 2.0", "Apache-2.0, MIT"},
		{"unknown", "All rights reserved.", "see license text"},
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
	odd := component{name: "example.com/odd", files: []licenseFile{{name: license, text: "All rights reserved."}}}
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
	project := component{name: "remote-mic", files: []licenseFile{{name: projectLicense, text: "MIT License\n\nPermission is hereby granted, free of charge"}}}
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
	if proj["name"] != "remote-mic" || proj["license"] != wantProjectLicense {
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
	if a["name"] != "example.com/a" || a["version"] != "v1.0.0" || a["license"] != "Apache-2.0" {
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
	if !strings.HasSuffix(string(b), "}\n") {
		t.Error("output does not end in a newline")
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
	project := component{files: []licenseFile{{name: projectLicense, text: "Apache License\nVersion 2.0, January 2004"}}}
	doc := string(render(project, nil))
	if want := "remote-mic itself is licensed under Apache-2.0; see LICENSE and NOTICE."; !strings.Contains(doc, want) {
		t.Errorf("document lacks %q", want)
	}
}
