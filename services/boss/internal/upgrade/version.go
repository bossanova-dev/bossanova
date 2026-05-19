package upgrade

import (
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

var gitDescribeVersionRE = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?-[0-9]+-g[0-9a-fA-F]+(?:-dirty)?$`)

type CompareResult int

const (
	CompareInvalid CompareResult = iota
	CompareOlder
	CompareCurrent
	CompareNewer
)

func NormalizeVersion(in string) (version string, ok bool, dev bool) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", false, false
	}
	if in == "dev" {
		return "", false, true
	}

	version = strings.Fields(in)[0]
	if gitDescribeVersionRE.MatchString(version) {
		return "", false, false
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return "", false, false
	}

	return semver.Canonical(version), true, false
}

func CompareStableVersions(current, latest string) CompareResult {
	currentVersion, currentOK, _ := NormalizeVersion(current)
	latestVersion, latestOK, _ := NormalizeVersion(latest)
	if !currentOK || !latestOK || semver.Prerelease(currentVersion) != "" || semver.Prerelease(latestVersion) != "" {
		return CompareInvalid
	}

	switch semver.Compare(currentVersion, latestVersion) {
	case -1:
		return CompareOlder
	case 1:
		return CompareNewer
	default:
		return CompareCurrent
	}
}
