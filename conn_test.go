//go:build windows

package wslcsession

import "testing"

type fakeProcess struct {
	released bool
}

func (p *fakeProcess) Release()                             { p.released = true }
func (p *fakeProcess) GetStdHandle(int32) (socketHandle, error) { return 0, nil }

func TestGuestConnCloseReleasesProcessAndIsIdempotent(t *testing.T) {
	s := newTestSession()
	defer func() { _ = s.Close() }()

	fp := &fakeProcess{}
	conn := newGuestConn(s, fp, 0, 0)

	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !fp.released {
		t.Error("expected the underlying process to be released")
	}
}

func TestGuestConnCloseWriteIsIdempotent(t *testing.T) {
	s := newTestSession()
	defer func() { _ = s.Close() }()

	conn := newGuestConn(s, &fakeProcess{}, 0, 0)
	defer func() { _ = conn.Close() }()

	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("first CloseWrite: %v", err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("second CloseWrite: %v", err)
	}
}

// Handle 0 is never a valid Winsock SOCKET, so Read/Write should surface an
// error from the underlying recv()/send() call rather than panic or silently
// succeed.
func TestGuestConnReadWriteOnInvalidHandle(t *testing.T) {
	s := newTestSession()
	defer func() { _ = s.Close() }()

	conn := newGuestConn(s, &fakeProcess{}, 0, 0)
	defer func() { _ = conn.Close() }()

	if _, err := conn.Read(make([]byte, 16)); err == nil {
		t.Error("expected an error reading from an invalid socket handle")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("expected an error writing to an invalid socket handle")
	}
}

func TestGuestAddr(t *testing.T) {
	var a guestAddr
	if a.Network() != "vsock" {
		t.Errorf("Network() = %q, want %q", a.Network(), "vsock")
	}
	if a.String() == "" {
		t.Error("String() should not be empty")
	}
}
