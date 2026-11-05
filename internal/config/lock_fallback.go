//go:build !unix && !windows

package config

// lockSidecar is a no-op on platforms without a supported locking primitive:
// writes remain atomic (temp file + rename), only write-write serialization
// is best-effort there. Plan 9 and wasm run milk's state root nowhere of
// consequence; the build just has to compile.
func lockSidecar(path string) (func(), error) {
	return func() {}, nil
}
