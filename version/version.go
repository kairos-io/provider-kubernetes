// Package version exposes the build version of the provider, injected at
// build time via -ldflags (see the Makefile).
package version

// Version is the provider build version. It is overridden at link time with
//
//	-X github.com/kairos-io/provider-kubernetes/version.Version=<value>
//
// and defaults to "dev" for local builds.
var Version = "dev"
