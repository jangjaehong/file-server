// Package semver implements a small subset of Semantic Versioning 2.0.0
// (https://semver.org) that is sufficient for ordering program releases.
//
// Accepted forms: "1.2.3", "v1.2.3", "1.2.3-beta.1", "1.2.3+build.5".
// Build metadata is kept but ignored when comparing.
//
// For Windows programs the .NET / file-version style is accepted as well:
// "1.2" (= 1.2.0) and "1.0.0.57" (a fourth "revision" number, typically the CI
// build number). The revision is compared after PATCH and before pre-release.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch uint64
	Rev                 uint64   // optional 4th number (1.0.0.57)
	HasRev              bool     // true if the 4th number was given
	Pre                 []string // pre-release identifiers, e.g. ["beta", "1"]
	Build               string
}

// Parse parses s into a Version.
func Parse(s string) (Version, error) {
	var v Version
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return v, fmt.Errorf("invalid version %q", orig)
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Build = s[i+1:]
		s = s[:i]
		if v.Build == "" || !validIdents(v.Build) {
			return v, fmt.Errorf("invalid build metadata in %q", orig)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" || !validIdents(pre) {
			return v, fmt.Errorf("invalid pre-release in %q", orig)
		}
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if isNum(id) && len(id) > 1 && id[0] == '0' {
				return v, fmt.Errorf("numeric pre-release identifier with leading zero in %q", orig)
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return v, fmt.Errorf("version %q must have the form MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH.BUILD", orig)
	}
	nums := make([]uint64, 4)
	for i, p := range parts {
		if p == "" || !isNum(p) || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("invalid number %q in version %q", p, orig)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, fmt.Errorf("invalid number %q in version %q", p, orig)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch, v.Rev = nums[0], nums[1], nums[2], nums[3]
	v.HasRev = len(parts) == 4
	return v, nil
}

// MustParse is like Parse but panics on error.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String returns the canonical form without a leading "v".
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.HasRev {
		s += fmt.Sprintf(".%d", v.Rev)
	}
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// IsNumeric reports whether v is a plain dotted number without pre-release
// part, i.e. something .NET's System.Version can parse ("1.0.0.57").
func (v Version) IsNumeric() bool { return len(v.Pre) == 0 && v.Build == "" }

// IsPrerelease reports whether v has pre-release identifiers.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or +1 depending on whether a < b, a == b or a > b.
func Compare(a, b Version) int {
	if c := cmpU(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpU(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpU(a.Patch, b.Patch); c != 0 {
		return c
	}
	if c := cmpU(a.Rev, b.Rev); c != 0 {
		return c
	}
	// A version without pre-release has higher precedence.
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		x, y := a.Pre[i], b.Pre[i]
		xn, yn := isNum(x), isNum(y)
		switch {
		case xn && yn:
			xi, _ := strconv.ParseUint(x, 10, 64)
			yi, _ := strconv.ParseUint(y, 10, 64)
			if c := cmpU(xi, yi); c != 0 {
				return c
			}
		case xn:
			return -1 // numeric identifiers have lower precedence
		case yn:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a.Pre), len(b.Pre))
}

// Less reports whether a < b.
func Less(a, b Version) bool { return Compare(a, b) < 0 }

func cmpU(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt(a, b int) int { return cmpU(uint64(a), uint64(b)) }

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validIdents(s string) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
		}
	}
	return true
}
