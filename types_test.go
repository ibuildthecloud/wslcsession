//go:build windows

package wslcsession

import "testing"

func TestWSLCHandleTypeString(t *testing.T) {
	tests := []struct {
		t    wslcHandleType
		want string
	}{
		{handleTypeUnknown, "Unknown"},
		{handleTypeFile, "File"},
		{handleTypePipe, "Pipe"},
		{handleTypeSocket, "Socket"},
		{wslcHandleType(99), "wslcHandleType(99)"},
	}
	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("wslcHandleType(%d).String() = %q, want %q", int32(tt.t), got, tt.want)
		}
	}
}
