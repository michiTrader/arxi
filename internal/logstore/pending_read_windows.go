//go:build windows

package logstore

import (
	"errors"
	"syscall"
	"time"
)

// readPendingFile is the reader-side mirror of removePendingFile. On POSIX a
// concurrent unlink of an open file is invisible to a reader, so the marker is
// either fully present or absent and a single os.ReadFile suffices. Windows has
// no such guarantee: while a lock-free reader and a committing writer touch
// pending.commit at the same instant, an open can transiently fail two ways --
// ERROR_SHARING_VIOLATION while the handles overlap, and ERROR_ACCESS_DENIED
// while the file sits in the delete-pending state a remove leaves behind until
// the last handle closes. removePendingFile already retries the remover side;
// without the symmetric retry here a follower's ReadConfirmed surfaces the
// error as a read failure, and because a batch completing is the NORMAL end of
// a commit, every follower would report a corrupt log exactly when a write
// lands.
//
// The retry deliberately covers ACCESS_DENIED as well, which is where the
// reader legitimately diverges from the remover. The remover treats
// ACCESS_DENIED as a permanent durability failure and surfaces it at once,
// because a marker it cannot delete reappears and rolls back an acknowledged
// write -- silence there loses data. The reader has no such asymmetry: a
// permanent ACCESS_DENIED is still returned once the attempts are exhausted, so
// it fails closed just the same, only later; retrying merely lets the common
// transient delete-pending case resolve into the ErrNotExist the caller reads
// as an absent marker. ErrNotExist and every other error return immediately.
func readPendingFile(path string) ([]byte, error) {
	var (
		body []byte
		err  error
	)
	for attempt := 0; attempt < windowsPendingRetryAttempts; attempt++ {
		body, err = readFile(path)
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, windowsSharingViolation) && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return body, err
		}
		time.Sleep(time.Millisecond)
	}
	return body, err
}
