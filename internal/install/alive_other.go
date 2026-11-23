//go:build !unix

package install

// alive cannot ask on this platform, so it trusts the answer on the port.
func alive(int) bool { return true }
