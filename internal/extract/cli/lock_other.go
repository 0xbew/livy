//go:build !unix

package cli

// lock is not available here: the pass runs unlocked, as the Python tool did without fcntl.
func lock(string, string) (func(), bool) { return func() {}, true }
