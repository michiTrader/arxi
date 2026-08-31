package toolrun

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAWriteAndReadRoundTripInsideTheWorkspace(t *testing.T) {
	w := ws(t)
	if err := w.WriteFile("notes.txt", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := w.ReadFile("notes.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("ReadFile = %q, want %q", got, "hello")
	}
	// The inverse of every refusal below. Without it, a Resolve that refused
	// everything would pass the whole confinement suite, and a confinement that
	// blocks the legitimate case is one that gets switched off.
}

func TestAWriteOutsideTheWorkspaceIsRefusedAndLandsNowhere(t *testing.T) {
	w := ws(t)
	outside := filepath.Join(filepath.Dir(w.Root), "escaped.txt")

	if err := w.WriteFile("../escaped.txt", []byte("loot")); err == nil {
		t.Fatal("WriteFile(../escaped.txt) succeeded\n" +
			"  a write is the effect that cannot be undone by noticing later")
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("%s exists after a refused write\n"+
			"  the refusal returned an error but the file was created anyway, which "+
			"is worse than no check: the caller sees a failure and the damage is done",
			outside)
	}
}

func TestWritingThroughAFinalComponentSymlinkIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on windows")
	}
	w := ws(t)

	// The gap the package doc admits Resolve cannot close on its own: the link
	// sits INSIDE the workspace, so every parent component resolves inside it,
	// and only the last element points out.
	target := filepath.Join(filepath.Dir(w.Root), "outside.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.Root, "innocent.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// Resolve alone accepts it. Asserting this is the point: it documents in an
	// executable way WHY openNoFollow exists, so nobody deletes the second check
	// believing the first already covers it.
	if _, err := w.Resolve("innocent.txt"); err != nil {
		t.Fatalf("Resolve refused %q, so this test is no longer exercising the gap "+
			"openNoFollow exists to close: %v", "innocent.txt", err)
	}

	if err := w.WriteFile("innocent.txt", []byte("overwritten")); err == nil {
		t.Fatal("WriteFile through a final-component symlink succeeded\n" +
			"  the path resolved inside the workspace and the write landed outside " +
			"it, which is the invisible failure: the log records a completed call")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("the file outside the workspace now reads %q, want %q\n"+
			"  the refusal was reported but the bytes still landed outside", got, "original")
	}
}

func TestReadingThroughAFinalComponentSymlinkIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on windows")
	}
	w := ws(t)

	secret := filepath.Join(filepath.Dir(w.Root), "secret.txt")
	if err := os.WriteFile(secret, []byte("api-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(w.Root, "peek.txt")); err != nil {
		t.Fatal(err)
	}

	got, err := w.ReadFile("peek.txt")
	if err == nil {
		t.Fatalf("ReadFile through a final-component symlink returned %q\n"+
			"  exfiltration needs no write: the contents become tool output, tool "+
			"output becomes an event, and the event is stored in the run log", got)
	}
}

func TestTheSymlinkRefusalSaysWhichGuaranteeItIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on windows")
	}
	w := ws(t)
	target := filepath.Join(filepath.Dir(w.Root), "t.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(w.Root, "l.txt")); err != nil {
		t.Fatal(err)
	}

	err := w.WriteFile("l.txt", []byte("y"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not mention a symlink: %v\n"+
			"  a reader who cannot tell WHY the write failed will assume the tool "+
			"runner is broken and reach for a way around it", err)
	}

	// nofollowSupported is asserted rather than assumed so this suite states
	// which guarantee it verified on this platform. On unix the refusal came from
	// the kernel in the same syscall as the open; on windows it came from a
	// preceding Lstat and a narrow race remains. A test that cannot tell those
	// apart would report the stronger one.
	if runtime.GOOS != "windows" && !nofollowSupported {
		t.Error("nofollowSupported is false on a unix build, so the final-component " +
			"check silently degraded to the racy fallback")
	}
}

func TestAReadLargerThanTheLimitIsRefused(t *testing.T) {
	w := ws(t)
	if err := w.WriteFile("big.bin", bytes.Repeat([]byte("a"), maxReadBytes+10)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadFile("big.bin"); err == nil {
		t.Error("ReadFile returned a file over the limit\n" +
			"  the limit protects the run log, not memory: tool output becomes an " +
			"event, and a log nobody can open is the artefact this project produces")
	}

	// Exactly at the limit must succeed, or the boundary is off by one in the
	// direction that refuses legitimate work.
	if err := w.WriteFile("exact.bin", bytes.Repeat([]byte("b"), maxReadBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadFile("exact.bin"); err != nil {
		t.Errorf("ReadFile refused a file of exactly the limit: %v", err)
	}
}

func TestAWriteTruncatesRatherThanAppends(t *testing.T) {
	w := ws(t)
	if err := w.WriteFile("f.txt", []byte("first-and-longer")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("f.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := w.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("ReadFile = %q, want %q\n"+
			"  a retried write that appends silently doubles the content of a call "+
			"that looked idempotent", got, "second")
	}
}

func TestAWrittenFileIsNotReadableByOtherUsers(t *testing.T) {
	w := ws(t)
	if err := w.WriteFile("secret.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(w.Root, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("written file has mode %o; group and other bits must be clear\n"+
			"  confining a tool to a directory and then making its output world "+
			"readable protects the filesystem and not the contents", perm)
	}
}

func TestReadingAMissingFileFailsWithoutCreatingIt(t *testing.T) {
	w := ws(t)
	if _, err := w.ReadFile("absent.txt"); err == nil {
		t.Fatal("ReadFile of a missing file succeeded")
	}
	if _, err := os.Lstat(filepath.Join(w.Root, "absent.txt")); !os.IsNotExist(err) {
		t.Error("a failed read created the file\n" +
			"  O_CREATE leaking into the read path would let a read tool mutate the " +
			"workspace, which no reader of the log would expect")
	}
}
