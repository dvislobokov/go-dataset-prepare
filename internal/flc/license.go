package flc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// License detection from the root LICENSE/COPYING text. Signatures are conservative: an unrecognized text yields nil
// (unknown), never a guess. A declared SPDX id (from the selection manifest / GitHub metadata) that conflicts with a
// detected one blocks the repository (license_conflict).
var licenseSignatures = []struct {
	id  string
	all []*regexp.Regexp
	not []*regexp.Regexp
}{
	{"Apache-2.0", rx(`(?i)apache license`, `(?i)version 2\.0`), nil},
	{"MPL-2.0", rx(`(?i)mozilla public license,? (version|v\.?) ?2\.0`), nil},
	{"AGPL-3.0", rx(`(?i)gnu affero general public license`), nil},
	{"LGPL-3.0", rx(`(?i)gnu lesser general public license`), nil},
	{"GPL-3.0", rx(`(?i)gnu general public license`, `(?i)version 3`), nil},
	{"GPL-2.0", rx(`(?i)gnu general public license`, `(?i)version 2`), nil},
	{"Unlicense", rx(`(?i)this is free and unencumbered software released into the public domain`), nil},
	{"0BSD", rx(`(?i)permission to use, copy, modify, and/or distribute this software for any purpose with or without fee`, `(?i)zero-clause|0bsd`), nil},
	{"ISC", rx(`(?i)permission to use, copy, modify, and(/or)? distribute this software for any purpose with or without fee`), nil},
	{"MIT", rx(`(?i)permission is hereby granted, free of charge`, `(?i)the above copyright notice and this permission notice shall be\s+included`), nil},
	{"BSD-3-Clause", rx(`(?i)redistribution and use in source and binary forms`, `(?i)(neither the name|names of (its|the) contributors)`), nil},
	{"BSD-2-Clause", rx(`(?i)redistribution and use in source and binary forms`), rx(`(?i)(neither the name|names of (its|the) contributors)`)},
	{"Zlib", rx(`(?i)this software is provided 'as-is', without any express or implied`, `(?i)altered source versions must be plainly marked`), nil},
}

func rx(ps ...string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range ps {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}

// DetectLicense returns the SPDX id detected from root license files, the file used, or nil.
func DetectLicense(root string) (*string, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, ""
	}
	var names []string
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if e.Type().IsRegular() && (strings.HasPrefix(n, "license") || strings.HasPrefix(n, "licence") || strings.HasPrefix(n, "copying")) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(root, n))
		if err != nil || len(b) > 256*1024 {
			continue
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, sig := range licenseSignatures {
			ok := true
			for _, r := range sig.all {
				if !r.MatchString(text) {
					ok = false
					break
				}
			}
			for _, r := range sig.not {
				if r.MatchString(text) {
					ok = false
				}
			}
			if ok {
				id := sig.id
				return &id, n
			}
		}
	}
	return nil, ""
}

// ResolveLicense combines detection with the declared id and the allowlist.
func ResolveLicense(cfg *Config, root string, declared *string) (lic *string, reason string, allowed bool) {
	if declared == nil {
		declared = cfg.License.Declared
	}
	detected, file := DetectLicense(root)
	switch {
	case detected != nil && declared != nil && *declared != "" && !strings.EqualFold(*detected, *declared):
		return detected, "license_conflict: declared " + *declared + ", detected " + *detected + " in " + file, false
	case detected != nil:
		lic, reason = detected, "detected:"+file
	case declared != nil && *declared != "":
		lic, reason = declared, "declared_only"
	default:
		return nil, "license_unknown", cfg.License.AllowUnknown
	}
	for _, a := range cfg.License.Allowlist {
		if a == *lic {
			return lic, reason, true
		}
	}
	return lic, reason + " (not allowlisted)", false
}
