package main

import (
	"errors"
	"os"
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
	const license = "LICENSE"
	odd := component{name: "example.com/odd", files: []licenseFile{{name: license, text: "All rights reserved."}}}
	if err := checkRecognized(odd); !errors.Is(err, ErrUnrecognized) {
		t.Errorf("unrecognized LICENSE: err = %v, want ErrUnrecognized", err)
	}
	withNotice := component{name: "example.com/ok", files: []licenseFile{
		{name: license, text: "Permission is hereby granted, free of charge"},
		{name: "NOTICE", text: "This product includes software developed by someone."},
		{name: "PATENTS", text: "Additional IP Rights Grant (Patents)"},
	}}
	if err := checkRecognized(withNotice); err != nil {
		t.Errorf("recognized LICENSE with NOTICE and PATENTS: err = %v, want nil", err)
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
