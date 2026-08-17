// Package buildinfo holds the identity of the engine build.
//
// The version and the pinned toolchain live here so the command line, the
// environment doctor, tools/go.mod, and soapbox.yaml cannot drift apart. A test
// in the root package asserts that they agree.
package buildinfo

const (
	// Version is the engine version. Released engines are tagged tools/vX.Y.Z.
	Version = "0.2.2"

	// Toolchain is the exact Go toolchain the engine pins. Generated formatting
	// and module metadata must be byte identical across machines, so the patch
	// release is part of the contract.
	Toolchain = "go1.26.5"

	// GoDirective is the language version the engine module declares. It matches
	// the go directive of the Kubernetes release the profile extracts from.
	GoDirective = "1.26.0"
)
