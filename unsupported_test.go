//go:build !windows

package wslcgo

import "testing"

func TestUnsupportedPlatformReturnsClearError(t *testing.T) {
	session, err := NewSession(Options{})
	if err == nil {
		t.Fatal("NewSession: expected an error on a non-Windows platform")
	}
	if session != nil {
		t.Fatal("NewSession: expected a nil Session alongside the error")
	}

	s := &Session{}
	if err := s.Close(); err == nil {
		t.Error("Close: expected an error")
	}
	if _, err := s.DisplayName(); err == nil {
		t.Error("DisplayName: expected an error")
	}
	if _, err := s.DockerConn(); err == nil {
		t.Error("DockerConn: expected an error")
	}
	if _, err := s.DialGuestUnix("/var/run/docker.sock"); err == nil {
		t.Error("DialGuestUnix: expected an error")
	}
	if _, err := s.DialGuestTCP("127.0.0.1:80"); err == nil {
		t.Error("DialGuestTCP: expected an error")
	}
}
