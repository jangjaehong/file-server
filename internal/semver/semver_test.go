package semver

import "testing"

func TestParseAndString(t *testing.T) {
	for _, s := range []string{"1.2.3", "0.0.1", "1.0.0.57", "2.1.0.0", "10.20.30-beta.1", "1.0.0-rc.1+build.7"} {
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if v.String() != s {
			t.Errorf("String() = %q, want %q", v.String(), s)
		}
	}
	if v := MustParse("v2.0.0"); v.String() != "2.0.0" {
		t.Errorf("leading v not stripped: %s", v)
	}
	if v := MustParse("1.2"); v.String() != "1.2.0" {
		t.Errorf("1.2 should become 1.2.0, got %s", v)
	}
	for _, s := range []string{"", "1", "1.2.3.4.5", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-01", "1.2.3-a..b", "1.2.3+"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) should fail", s)
		}
	}
}

func TestCompareOrder(t *testing.T) {
	// Ordered example from semver.org section 11.
	order := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "1.10.0", "2.0.0",
	}
	for i := 0; i+1 < len(order); i++ {
		a, b := MustParse(order[i]), MustParse(order[i+1])
		if Compare(a, b) != -1 || Compare(b, a) != 1 {
			t.Errorf("expected %s < %s", a, b)
		}
	}
	// 4-part (Windows file version) ordering.
	for _, p := range [][2]string{{"1.0.0.9", "1.0.0.10"}, {"1.0.0", "1.0.0.1"}, {"1.0.0.99", "1.0.1"}, {"1.0.0.5-beta", "1.0.0.5"}} {
		if Compare(MustParse(p[0]), MustParse(p[1])) != -1 {
			t.Errorf("expected %s < %s", p[0], p[1])
		}
	}
	if Compare(MustParse("1.0.0+a"), MustParse("1.0.0+b")) != 0 {
		t.Error("build metadata must be ignored")
	}
}
