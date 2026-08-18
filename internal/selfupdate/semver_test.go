package selfupdate

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.4.0", "v1.3.9", 1},
		{"v1.3.9", "v1.4.0", -1},
		{"1.4.0", "v1.4.0", 0},
		{"v1.4.0", "v1.4.0-rc.1", 1},
		{"v1.4.0-rc.1", "v1.4.0", -1},
		// The case a string compare gets wrong.
		{"v1.4.0-rc.10", "v1.4.0-rc.2", 1},
		{"v1.4.0-alpha.1", "v1.4.0-alpha", 1},
		{"v1.4.0-alpha.1", "v1.4.0-alpha.beta", -1},
		{"v1.4.0+build.1", "v1.4.0", 0},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.0.10", "v1.0.9", 1},
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if err != nil {
			t.Errorf("Compare(%q, %q) errored: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareRejectsNonSemver(t *testing.T) {
	for _, v := range []string{"dev", "", "v1.4", "v01.4.0", "1.2.3.4", "v1.2.x", "v1.2.3-"} {
		if _, err := Compare(v, "v1.0.0"); err == nil {
			t.Errorf("Compare(%q, ...) accepted a non-version", v)
		}
		if Valid(v) {
			t.Errorf("Valid(%q) = true, want false", v)
		}
	}
}

func TestIsRelease(t *testing.T) {
	if !IsRelease("v1.4.0") {
		t.Error("v1.4.0 should be a release")
	}
	if IsRelease("v1.4.0-rc.1") {
		t.Error("v1.4.0-rc.1 should not be a release")
	}
	if IsRelease("dev") {
		t.Error("dev should not be a release")
	}
}

// A git describe build parses as semver but must not be compared: it sits ahead
// of the tag it names, so an advisory would point backwards.
func TestGitDescribeBuildIsNotCheckable(t *testing.T) {
	const v = "v0.1.0-3-gabc1234"
	if !Valid(v) {
		t.Errorf("Valid(%q) = false; it is well-formed semver", v)
	}
	if Checkable(v) {
		t.Errorf("Checkable(%q) = true; a git describe build must not be compared", v)
	}
	if !Checkable("v1.4.0") || !Checkable("v1.4.0-rc.1") {
		t.Error("a release or a real pre-release should be checkable")
	}
}
