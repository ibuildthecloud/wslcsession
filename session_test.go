//go:build windows

package wslcsession

import "testing"

// newTestSession returns a Session whose "COM thread" is faked out to just
// drain reqCh, so do()/Close()/releaseProcess can be exercised without any
// real COM activation. This works because do()/Close() never touch s.mgr/
// s.sess directly - only comThread's own loop-exit code does, and that's not
// under test here.
func newTestSession() *Session {
	s := &Session{reqCh: make(chan func()), closed: make(chan struct{})}
	go func() {
		for req := range s.reqCh {
			req()
		}
		close(s.closed)
	}()
	return s
}

func TestNewSessionRequiresStoragePathForVolumes(t *testing.T) {
	_, err := NewSession(Options{Volumes: []VolumeOptions{{Name: "x", SizeMB: 1024}}})
	if err == nil {
		t.Fatal("expected an error when Options.Volumes is set without Options.StoragePath")
	}
}

func TestSessionDoNoopsAfterClose(t *testing.T) {
	s := newTestSession()

	ran := false
	s.do(func() { ran = true })
	if !ran {
		t.Fatal("do() should run f before Close()")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ran = false
	s.do(func() { ran = true })
	if ran {
		t.Fatal("do() should no-op after Close()")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	s := newTestSession()
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
