//go:build windows

package wslcsession

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

// bytePtrToString decodes a NUL-terminated C string back to a Go string, for
// asserting on what makeStringArray actually wrote - mirroring
// syscall.BytePtrFromString's encoding without pulling in a decode-only
// dependency for it.
func bytePtrToString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func TestMakeStringArray(t *testing.T) {
	items := []string{"a", "bb", "ccc"}
	arr, keepAlive, err := makeStringArray(items)
	if err != nil {
		t.Fatalf("makeStringArray: %v", err)
	}
	if arr.Count != uint32(len(items)) {
		t.Fatalf("Count = %d, want %d", arr.Count, len(items))
	}

	ptrs := unsafe.Slice((**byte)(unsafe.Pointer(arr.Values)), arr.Count)
	for i, want := range items {
		if got := bytePtrToString(ptrs[i]); got != want {
			t.Errorf("item %d = %q, want %q", i, got, want)
		}
	}
	runtime.KeepAlive(keepAlive)
}

func TestMakeStringArrayEmpty(t *testing.T) {
	arr, keepAlive, err := makeStringArray(nil)
	if err != nil {
		t.Fatalf("makeStringArray(nil): %v", err)
	}
	if arr.Count != 0 || arr.Values != nil {
		t.Fatalf("makeStringArray(nil) = %+v, want zero value", arr)
	}
	if keepAlive != nil {
		t.Fatalf("makeStringArray(nil) keepAlive = %v, want nil", keepAlive)
	}
}

// CreateVolume validates Name/SizeMB before ever touching s.ptr, so this is
// safe to exercise against a comSession with no real COM object behind it.
func TestComSessionCreateVolumeValidation(t *testing.T) {
	s := &comSession{}

	if err := s.CreateVolume(VolumeOptions{SizeMB: 1024}); err == nil {
		t.Error("expected an error for a missing volume name")
	}
	if err := s.CreateVolume(VolumeOptions{Name: "x"}); err == nil {
		t.Error("expected an error for a zero SizeMB")
	}
}

func TestGuestExecErrorUnwrapAndMessage(t *testing.T) {
	base := fmt.Errorf("boom")
	e := &GuestExecError{Err: base, Errno: 42}

	if !errors.Is(e, base) {
		t.Error("GuestExecError should unwrap to its underlying error")
	}
	if !strings.Contains(e.Error(), "42") {
		t.Errorf("Error() = %q, want it to mention the errno", e.Error())
	}
}
