//go:build !unix

package main

import "os"

// lockFile is a no-op where flock does not exist. herdr does not run on these
// platforms, so the herdr registry is never opened here; the in-process mutex
// in herdrRegistry still orders this process's own writes.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
