package semver

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v2.6.2", "v2.11.6", -1},
		{"2.6.2", "v2.6.2", 0},
		{"v2.11.6", "v2.11.6-beta.1", 1},
		{"v2.11.6-beta.1", "v2.11.6-beta.2", -1},
		{"v2.11.6-beta.2", "v2.11.6-rc.1", -1},
		{"v0.0.0-20240101000000-aaaaaaaaaaaa", "v0.0.0-20240201000000-bbbbbbbbbbbb", -1},
		{"v0.0.0-20240101000000-aaaaaaaaaaaa", "v0.1.0", -1},
		{"v0.2.1", "v0.2.1+incompatible", 0},
		{"garbage", "v1.0.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsPseudo(t *testing.T) {
	if !IsPseudo("v0.0.0-20240814120000-0123456789ab") {
		t.Error("expected pseudo")
	}
	if !IsPseudo("v1.2.3-0.20240814120000-0123456789ab") {
		t.Error("expected pseudo (post-release form)")
	}
	if IsPseudo("v1.2.3") || IsPseudo("v1.2.3-beta.1") {
		t.Error("unexpected pseudo")
	}
}

func TestMajor(t *testing.T) {
	cases := map[string]int{"v2.11.6": 2, "3.0.0": 3, "v0.0.0-20240101000000-aaaaaaaaaaaa": 0, "main": -1, "": -1}
	for in, want := range cases {
		if got := Major(in); got != want {
			t.Errorf("Major(%q)=%d want %d", in, got, want)
		}
	}
}
