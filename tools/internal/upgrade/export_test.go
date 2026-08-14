package upgrade

import "os"

// WriteAtomicForTest exposes writeAtomic for testing the exclusive-create
// behavior. It is not part of the public API.
func WriteAtomicForTest(root *os.Root, name string, contents []byte) error {
	return writeAtomic(root, name, contents)
}
