package transport

import (
	"strings"
	"testing"
)

func TestWithDefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"100.64.0.5":      "100.64.0.5:7433",
		"100.64.0.5:9000": "100.64.0.5:9000",
		"devbox.ts.net":   "devbox.ts.net:7433",
		"fd7a::1":         "[fd7a::1]:7433",
		"[fd7a::1]":       "[fd7a::1]:7433",
		"[fd7a::1]:9000":  "[fd7a::1]:9000",
	} {
		if got, err := WithDefaultPort(in); err != nil || got != want {
			t.Errorf("WithDefaultPort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ":7433", "a b", "host:", "devbox/foo"} {
		if got, err := WithDefaultPort(bad); err == nil {
			t.Errorf("WithDefaultPort(%q) = %q", bad, got)
		}
	}
}

// A bare host is dialed on the default port: the port is added before the
// address is resolved, so a hosts.toml edited by hand works too.
func TestDialBareHostUsesDefaultPort(t *testing.T) {
	// Nothing is expected to listen on 127.0.0.1:7433 during the test, but
	// the address must at least parse: a bare host used to fail at once
	// with "missing port in address".
	_, err := ProbeFingerprint(t.Context(), "127.0.0.1", mustIdentity(t))
	if err == nil {
		t.Skip("something is listening on the default port")
	}
	if strings.Contains(err.Error(), "missing port") {
		t.Fatalf("bare host not given the default port: %v", err)
	}
}
