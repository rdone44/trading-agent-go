package llm

import (
	"os"
	"testing"
)

// Go 1.23's http.ProxyFromEnvironment snapshots the proxy environment once,
// on the first lookup the process performs, and keeps that snapshot forever.
// The proxy tests in transport_test.go call t.Setenv AFTER the earlier tests
// in this package already ran a request through the transport, so on a host
// without a system proxy the first lookup happens with an empty environment
// and the snapshot is permanently "no proxy" — making the later proxy tests
// fail depending on test order. Seeding the variable before the first test
// runs makes the snapshot deterministic on every host.
func TestMain(m *testing.M) {
	os.Setenv("HTTPS_PROXY", "http://127.0.0.1:10808")
	os.Setenv("HTTP_PROXY", "http://127.0.0.1:10808")
	os.Setenv("NO_PROXY", "127.0.0.1,localhost")
	os.Exit(m.Run())
}
