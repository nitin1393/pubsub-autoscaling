package main

import "testing"

func TestLeaderNamespace(t *testing.T) {
	cases := []struct{ explicit, watched, want string }{
		{"vpa-gate", "my-app", "vpa-gate"}, // chart: lease in release namespace
		{"", "my-app", "my-app"},           // plain manifest: same namespace
		{"", "", ""},                       // all namespaces: pod's own namespace
	}
	for _, c := range cases {
		if got := leaderNamespace(c.explicit, c.watched); got != c.want {
			t.Errorf("leaderNamespace(%q, %q) = %q, want %q", c.explicit, c.watched, got, c.want)
		}
	}
}
