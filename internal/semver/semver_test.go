package semver

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v2.6.2", "v2.11.6", -1},
		{"v2.11.6", "v2.6.2", 1},
		{"2.6.2", "v2.6.2", 0},
		{"v2.11.6", "v2.11.6-beta.1", 1},
		{"v2.11.6-beta.1", "v2.11.6-beta.2", -1},
		{"v2.11.6-beta.2", "v2.11.6-rc.1", -1},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.1", "v1.0.0-alpha", 1},
		{"v0.0.0-20240101000000-aaaaaaaaaaaa", "v0.0.0-20240201000000-bbbbbbbbbbbb", -1},
		{"v0.0.0-20240101000000-aaaaaaaaaaaa", "v0.1.0", -1},
		{"v0.2.1", "v0.2.1+incompatible", 0},
		{"v2.10.0-0.20250101120000-abcdefabcdef+dirty", "v2.10.0-0.20250101120000-abcdefabcdef", 0},
		{"garbage", "v1.0.0", -1},
		{"v1.0.0", "garbage", 1},
		{"garbage", "junk", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsPseudo(t *testing.T) {
	yes := []string{
		"v0.0.0-20240814120000-0123456789ab",
		"v1.2.3-0.20240814120000-0123456789ab",
		"v1.2.3-pre.0.20240814120000-0123456789ab",
		"v0.0.0-20240814120000-0123456789ab+incompatible",
		"v2.10.0-0.20250101120000-abcdefabcdef+dirty", // Go 1.24 stamps dirty builds this way
		"0.0.0-20240814120000-0123456789ab",           // missing v is tolerated
	}
	no := []string{
		"v1.2.3", "v1.2.3-beta.1", "",
		"v1.2.3-20240814120000-0123456789ab",     // a release base needs the "-0." form
		"v1.2.3-rc1.20240814120000-0123456789ab", // a prerelease base needs ".0."
	}
	for _, v := range yes {
		if !IsPseudo(v) {
			t.Errorf("IsPseudo(%q) should be true", v)
		}
	}
	for _, v := range no {
		if IsPseudo(v) {
			t.Errorf("IsPseudo(%q) should be false", v)
		}
	}
}

func TestMajorAndValid(t *testing.T) {
	cases := map[string]int{"v2.11.6": 2, "3.0.0": 3, "v0.0.0-20240101000000-aaaaaaaaaaaa": 0, "v10.1.0": 10, "main": -1, "": -1, "v01.2.3": -1}
	for in, want := range cases {
		if got := Major(in); got != want {
			t.Errorf("Major(%q)=%d want %d", in, got, want)
		}
	}
	if !IsValid("2.11.6") || IsValid("main") || IsValid("v1.2") == false {
		// x/mod accepts the short vMAJOR.MINOR form; callers that need the
		// full form check Canonical output themselves.
		t.Errorf("IsValid: 2.11.6=%v main=%v v1.2=%v", IsValid("2.11.6"), IsValid("main"), IsValid("v1.2"))
	}
}
