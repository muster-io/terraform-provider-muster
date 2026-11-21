// SPDX-License-Identifier: MIT
// Copyright The Muster Authors

package provider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// MinimumMusterVersion is the oldest Muster this provider works with: the release that carries managed_by.
const MinimumMusterVersion = "0.2.0"

var releaseVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// parseRelease returns the numbers of a release version MAJOR.MINOR.PATCH, with an optional leading v; anything else,
// such as a pre-release, a build suffix or the version of a development build, is not a release.
func parseRelease(version string) ([3]int, bool) {
	m := releaseVersion.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func olderThan(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// checkVersion compares the version Muster reports with MinimumMusterVersion: an older release is an error, a version
// that is not a release passes with a warning.
func checkVersion(endpoint, reported string) diag.Diagnostics {
	var diags diag.Diagnostics
	minimum, _ := parseRelease(MinimumMusterVersion)
	version, ok := parseRelease(reported)
	if !ok {
		diags.AddWarning("Muster's version cannot be compared",
			fmt.Sprintf("Muster at %s reports the version %q, which is not a release version MAJOR.MINOR.PATCH, as a "+
				"development build reports. The provider works with Muster %s or newer and cannot tell whether this "+
				"build is; it goes on.", endpoint, reported, MinimumMusterVersion))
		return diags
	}
	if olderThan(version, minimum) {
		normalized := strings.TrimPrefix(strings.TrimSpace(reported), "v")
		diags.AddError(
			fmt.Sprintf("Muster %s is older than %s, the oldest version this provider works with", normalized,
				MinimumMusterVersion),
			fmt.Sprintf("Upgrade Muster at %s to %s or newer.", endpoint, MinimumMusterVersion))
	}
	return diags
}
