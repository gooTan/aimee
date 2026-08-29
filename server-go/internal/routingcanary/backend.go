package routingcanary

// BackendMarker returns a deterministic canary string proving backend code
// changes land and are exercised by the Go test pipeline.
func BackendMarker() string {
	return "backend-routing-canary-ok"
}
