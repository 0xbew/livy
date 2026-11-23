//go:build !unix

package ledger

import "os"

// Windows file locks (LockFileEx) land with the Windows build; until then every line spills.
func tryLock(*os.File) bool { return false }

func unlock(*os.File) {}
