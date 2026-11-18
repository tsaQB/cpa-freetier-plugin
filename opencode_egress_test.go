package main

import "testing"

// The sidecar used to win whenever :8789 happened to be reachable, which sent
// traffic down the slower external path even though the in-process transport
// passes the gate. These cases pin the precedence down.

func TestResolveOpenCodeBaseURLPrecedence(t *testing.T) {
	up := func() bool { return true }
	down := func() bool { return false }

	cases := []struct {
		name    string
		sidecar string
		env     string
		native  bool
		probe   func() bool
		want    string
	}{
		{"settings host wins over everything", "10.0.0.5:9000", "http://env:1/", true, up, "http://10.0.0.5:9000"},
		{"settings url kept as-is", "http://zen.local:9900/", "", true, up, "http://zen.local:9900"},
		{"settings dash forces direct even with sidecar up", "-", "", true, up, ""},
		{"settings auto probes the sidecar", "auto", "", true, up, "http://127.0.0.1:8789"},
		{"settings auto with dead port stays direct", "auto", "", true, down, ""},
		{"env base url beats native default", "", "http://env:8080/", true, up, "http://env:8080"},
		{"native default never probes", "", "", true, up, ""},
		{"native disabled falls back to sidecar probe", "", "", false, up, "http://127.0.0.1:8789"},
		{"native disabled and dead port is direct", "", "", false, down, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveOpenCodeBaseURL(tc.sidecar, tc.env, tc.native, tc.probe)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveOpenCodeBaseURLIgnoresBlankEnv(t *testing.T) {
	if got := resolveOpenCodeBaseURL("", "   ", true, func() bool { return true }); got != "" {
		t.Fatalf("blank env must not become a base URL, got %q", got)
	}
}
