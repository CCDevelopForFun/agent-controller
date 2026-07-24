//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// FIFO-based tests live in this Unix-only file: syscall.Mkfifo is not
// defined on Windows, so without a build tag the whole test package would
// fail to COMPILE under GOOS=windows before t.Skipf could run. Codex pass
// 3 of slice 7.4. The agentctl release builds cross-platform binaries, so
// Windows compilation has to stay clean.

func TestReadCappedFileRejectsFifoWithoutBlocking(t *testing.T) {
	// A read-side FIFO open blocks until a writer appears; readCappedFile
	// must Stat first and fail fast rather than hang. Codex pass 2 of
	// slice 7.4. (If this regresses, the test HANGS rather than fails —
	// caught by go test's overall timeout.)
	fifo := filepath.Join(t.TempDir(), "in.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	_, err := readCappedFile(fifo)
	if err == nil {
		t.Fatal("FIFO input should be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error should explain non-regular file; got %q", err.Error())
	}
	// And via the flag surfaces too.
	if _, err := parseInputFlags([]string{"text=@" + fifo}); err == nil {
		t.Error("--input KEY=@<fifo> should be rejected")
	}
	if err := mergeInputFile(map[string]string{}, fifo); err == nil {
		t.Error("--input-file <fifo> should be rejected")
	}
}

func TestOutputAlreadyExistsRejectsNonRegular(t *testing.T) {
	// A FIFO/socket/device can't be the regular file a prior run wrote, so
	// skip-if-output-exists must surface a config error rather than treat
	// it as "already done". Codex pass 1 of slice 7.4.
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	ok, err := outputAlreadyExists(fifo)
	if err == nil {
		t.Fatal("FIFO should be a configuration error, got nil")
	}
	if ok {
		t.Error("FIFO must not report as an existing regular output")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error should explain non-regular file; got %q", err.Error())
	}
}
