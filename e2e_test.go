//go:build windows

package wslcgo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// These tests boot a real wslc VM and talk to a real dockerd inside it, so
// they're skipped unless explicitly opted into via WSLCGO_E2E=1 - they need
// an actual wslc-capable Windows machine (WSL2 + Hyper-V), which an ordinary
// CI runner won't have. cmd/wslcdemo covers the same ground interactively;
// these are the same checks as automated, assertion-based `go test` cases.
func skipUnlessE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("WSLCGO_E2E") == "" {
		t.Skip("set WSLCGO_E2E=1 to run wslc e2e tests (requires a real wslc-capable Windows machine)")
	}
}

func TestE2ESessionLifecycle(t *testing.T) {
	skipUnlessE2E(t)

	session, err := NewSession(Options{DisplayName: "wslcgo-e2e-lifecycle"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	name, err := session.DisplayName()
	if err != nil {
		t.Fatalf("DisplayName: %v", err)
	}
	if name != "wslcgo-e2e-lifecycle" {
		t.Errorf("DisplayName() = %q, want %q", name, "wslcgo-e2e-lifecycle")
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil { // idempotent
		t.Fatalf("second Close: %v", err)
	}
}

func TestE2EDockerConnVersion(t *testing.T) {
	skipUnlessE2E(t)

	session, err := NewSession(Options{DisplayName: "wslcgo-e2e-docker"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	conn, err := session.DockerConn()
	if err != nil {
		t.Fatalf("DockerConn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("GET /version HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var version struct {
		Version string `json:"Version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if version.Version == "" {
		t.Error("dockerd reported an empty Version")
	}
}

// TestE2EDialGuestTCPRefused proves the full round trip (COM ->
// CreateRootNamespaceProcess -> bridge exec -> real connect() syscall inside
// the guest) is intact, independent of Docker Hub availability: dialing a
// port nothing listens on must surface the guest kernel's own connection
// refusal, not an error from failing to even reach the guest. Per
// bridgesrc/bridge.c, the bridge process exits immediately without writing
// anything when connect() fails, so the Windows side sees a clean EOF.
func TestE2EDialGuestTCPRefused(t *testing.T) {
	skipUnlessE2E(t)

	session, err := NewSession(Options{DisplayName: "wslcgo-e2e-refused"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	conn, err := session.DialGuestTCP("127.0.0.1:1")
	if err != nil {
		t.Fatalf("DialGuestTCP: %v", err)
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("Read() = (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestE2EPersistentVolume creates a session with a VHD-backed named volume
// and confirms dockerd inside the guest actually sees it as a normal volume.
func TestE2EPersistentVolume(t *testing.T) {
	skipUnlessE2E(t)

	dir := t.TempDir()
	const volName = "wslcgo-e2e-vol"

	session, err := NewSession(Options{
		DisplayName:      "wslcgo-e2e-volume",
		StoragePath:      dir,
		MaxStorageSizeMB: 8192,
		Volumes:          []VolumeOptions{{Name: volName, SizeMB: 1024}},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return session.DockerConn()
			},
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(fmt.Sprintf("http://localhost/volumes/%s", volName))
	if err != nil {
		t.Fatalf("GET /volumes/%s: %v", volName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /volumes/%s: status %d: %s", volName, resp.StatusCode, body)
	}

	var info struct {
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if info.Name != volName {
		t.Errorf("volume Name = %q, want %q", info.Name, volName)
	}
}
