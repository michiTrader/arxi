//go:build linux

package toolrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandleRelativeWriteRefusesParentSwap(t *testing.T) {
	w := ws(t)
	parent := filepath.Join(w.Root, "parent")
	outside := filepath.Join(filepath.Dir(w.Root), "outside")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(w.Root, "old-parent")
	if err := os.Rename(parent, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("parent/escaped.txt", []byte("escape")); err == nil {
		t.Fatal("write followed a swapped parent symlink: parent traversal must be relative to directory handles with O_NOFOLLOW")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file after refused parent swap = %v: a path refusal that still writes is not confinement", err)
	}
}

func TestHandleRelativeToolsRejectAbsolutePaths(t *testing.T) {
	w := ws(t)
	outside := filepath.Join(filepath.Dir(w.Root), "absolute.txt")
	if err := w.WriteFile(outside, []byte("escape")); err == nil {
		t.Fatal("absolute direct-file path was accepted: handle-relative profiles accept only workspace-relative paths")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("absolute target after refusal = %v: no write may happen before path validation", err)
	}
}
