package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agtd.sh/internal/transport"
)

// `agt host add` takes a bare host as the daemon's default port, and
// compares it with a saved host on the full host:port, so an added host
// can be paired again by either spelling.
func TestHostAddBareHostMeansDefaultPort(t *testing.T) {
	a := &app{paths: testClient(t).paths}
	if err := os.MkdirAll(filepath.Dir(a.paths.HostsPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	const fp = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
	if err := transport.AddHost(a.paths.HostsPath(), transport.Host{Name: "dev", Address: "10.0.0.1:7433", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}

	// The same address without its port is the saved host, so this is a
	// re-pairing, which stops only at the missing terminal for the code.
	err := a.hostAdd("dev", "10.0.0.1", "", "")
	if err == nil || !strings.Contains(err.Error(), "no terminal to enter a pairing code") {
		t.Fatalf("bare host: %v", err)
	}
	// Another port is another address.
	if err := a.hostAdd("dev", "10.0.0.1:9000", "", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("other port: %v", err)
	}
	// What is not an address at all is refused before anything is dialed.
	if err := a.hostAdd("other", "a b", "", ""); err == nil || !strings.Contains(err.Error(), "is not an address") {
		t.Fatalf("bad address: %v", err)
	}
}
