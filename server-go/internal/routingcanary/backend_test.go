package routingcanary

import "testing"

func TestBackendMarker(t *testing.T) {
	if got := BackendMarker(); got != "backend-routing-canary-ok" {
		t.Fatalf("BackendMarker() = %q, want %q", got, "backend-routing-canary-ok")
	}
}
