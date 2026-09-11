//go:build !unix && !windows

package advisorylock

import (
	"fmt"
	"os"
	"runtime"
)

func lockFile(*os.File, bool) error {
	return fmt.Errorf("advisory file locking is unavailable on %s", runtime.GOOS)
}

func unlockFile(*os.File) error { return nil }
