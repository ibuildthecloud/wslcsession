//go:build windows

package wslcgo

import (
	"fmt"
	"testing"
)

func TestParseGUID(t *testing.T) {
	want := GUID{
		Data1: 0xa9b7a1b9,
		Data2: 0x0671,
		Data3: 0x405c,
		Data4: [8]byte{0x95, 0xf1, 0xe0, 0x61, 0x2c, 0xb4, 0xce, 0x8f},
	}

	tests := []struct {
		name    string
		in      string
		want    GUID
		wantErr bool
	}{
		{name: "plain", in: "a9b7a1b9-0671-405c-95f1-e0612cb4ce8f", want: want},
		{name: "braced", in: "{a9b7a1b9-0671-405c-95f1-e0612cb4ce8f}", want: want},
		{name: "wrong group count", in: "a9b7a1b9-0671-405c", wantErr: true},
		{name: "non-hex", in: "zzzzzzzz-0671-405c-95f1-e0612cb4ce8f", wantErr: true},
		{name: "wrong group length", in: "a9b7a1-0671-405c-95f1-e0612cb4ce8f", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGUID(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGUID(%q): expected error, got %+v", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGUID(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseGUID(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestMustGUIDPanicsOnInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("mustGUID: expected panic for an invalid GUID string")
		}
	}()
	mustGUID("not-a-guid")
}

func TestHrErr(t *testing.T) {
	if err := hrErr(0); err != nil {
		t.Fatalf("hrErr(S_OK) = %v, want nil", err)
	}
	if err := hrErr(1); err != nil {
		t.Fatalf("hrErr(S_FALSE) = %v, want nil", err)
	}

	err := hrErr(uintptr(hrErrorAlreadyExists))
	if err == nil {
		t.Fatal("hrErr(ERROR_ALREADY_EXISTS): expected a non-nil error")
	}
	if !isHRESULT(err, hrErrorAlreadyExists) {
		t.Fatalf("isHRESULT: expected %v to match 0x%08X", err, hrErrorAlreadyExists)
	}
}

func TestIsHRESULTFalseForUnrelatedError(t *testing.T) {
	if isHRESULT(fmt.Errorf("boom"), hrErrorAlreadyExists) {
		t.Fatal("isHRESULT: a plain error should never match")
	}
	if isHRESULT(hrErr(uintptr(hrRPCETooLate)), hrErrorAlreadyExists) {
		t.Fatal("isHRESULT: a different HRESULT should not match")
	}
}
