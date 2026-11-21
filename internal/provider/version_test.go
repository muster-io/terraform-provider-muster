// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

func TestParseRelease(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"0.2.0", [3]int{0, 2, 0}, true},
		{"v0.9.3", [3]int{0, 9, 3}, true},
		{" 1.10.12 ", [3]int{1, 10, 12}, true},
		{"0.0.0-dev", [3]int{}, false},
		{"0.2.1-5-gabc1234", [3]int{}, false},
		{"0.2.1-SNAPSHOT-abc1234", [3]int{}, false},
		{"0.2.1+build.5", [3]int{}, false},
		{"dev", [3]int{}, false},
		{"", [3]int{}, false},
		{"0.2", [3]int{}, false},
		{"01.2.3", [3]int{}, false},
		{"99999999999999999999.0.0", [3]int{}, false},
	} {
		got, ok := parseRelease(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseRelease(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		reported string
		errors   []string
		warnings []string
	}{
		{"0.1.4", []string{"Muster 0.1.4 is older than 0.2.0, the oldest version this provider works with"}, nil},
		{"v0.1.4", []string{"Muster 0.1.4 is older than 0.2.0, the oldest version this provider works with"}, nil},
		{"0.0.9", []string{"Muster 0.0.9 is older than 0.2.0, the oldest version this provider works with"}, nil},
		{"0.2.0", nil, nil},
		{"0.9.3", nil, nil},
		{"1.0.0", nil, nil},
		{"0.2.1-5-gabc1234", nil, []string{"Muster's version cannot be compared"}},
		{"0.0.0-dev", nil, []string{"Muster's version cannot be compared"}},
		{"dev", nil, []string{"Muster's version cannot be compared"}},
		{"", nil, []string{"Muster's version cannot be compared"}},
	} {
		diags := checkVersion("http://127.0.0.1:8080", tc.reported)
		if got := summaries(diags.Errors()); strings.Join(got, "|") != strings.Join(tc.errors, "|") {
			t.Errorf("%q: errors %q, want %q", tc.reported, got, tc.errors)
		}
		if got := summaries(diags.Warnings()); strings.Join(got, "|") != strings.Join(tc.warnings, "|") {
			t.Errorf("%q: warnings %q, want %q", tc.reported, got, tc.warnings)
		}
	}
}

func summaries(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Summary())
	}
	return out
}
