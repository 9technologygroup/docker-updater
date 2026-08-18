package selfupdate

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// gitDescribeRe matches the suffix `git describe --tags` appends when the build
// is not exactly on a tag. That parses as a pre-release of the tag, but it means
// the build is AHEAD of it, so comparing would advise a downgrade.
var gitDescribeRe = regexp.MustCompile(`-\d+-g[0-9a-f]{7,}$`)

var ErrNotSemver = errors.New("not a semantic version")

type semver struct {
	major, minor, patch int
	pre                 []string
}

// Compare returns -1 if a sorts below b, +1 if above, 0 if equal. Build metadata
// is ignored, per semver 2.0.0 section 10.
func Compare(a, b string) (int, error) {
	va, err := parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := parse(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

// IsRelease reports whether v parses and carries no pre-release identifiers.
func IsRelease(v string) bool {
	p, err := parse(v)
	return err == nil && len(p.pre) == 0
}

// Valid reports whether v parses at all.
func Valid(v string) bool {
	_, err := parse(v)
	return err == nil
}

// Checkable reports whether v is a version worth comparing against a release.
func Checkable(v string) bool {
	return Valid(v) && !gitDescribeRe.MatchString(strings.TrimSpace(v))
}

func parse(v string) (semver, error) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}

	core := v
	var pre []string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core = v[:i]
		rest := v[i+1:]
		if rest == "" {
			return semver{}, ErrNotSemver
		}
		pre = strings.Split(rest, ".")
		for _, id := range pre {
			if id == "" || !validIdent(id) {
				return semver{}, ErrNotSemver
			}
		}
	}

	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return semver{}, ErrNotSemver
	}
	nums := make([]int, 3)
	for i, f := range fields {
		n, err := numericField(f)
		if err != nil {
			return semver{}, err
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

func numericField(s string) (int, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, ErrNotSemver
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrNotSemver
		}
	}
	return strconv.Atoi(s)
}

func validIdent(s string) bool {
	for i := range len(s) {
		c := s[i]
		alnum := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !alnum && c != '-' {
			return false
		}
	}
	return true
}

func (v semver) compare(o semver) int {
	if c := cmpInt(v.major, o.major); c != 0 {
		return c
	}
	if c := cmpInt(v.minor, o.minor); c != 0 {
		return c
	}
	if c := cmpInt(v.patch, o.patch); c != 0 {
		return c
	}
	return comparePre(v.pre, o.pre)
}

func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

func compareIdent(a, b string) int {
	an, aNumeric := asNumber(a)
	bn, bNumeric := asNumber(b)
	switch {
	case aNumeric && bNumeric:
		return cmpInt(an, bn)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func asNumber(s string) (int, bool) {
	n, err := numericField(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
