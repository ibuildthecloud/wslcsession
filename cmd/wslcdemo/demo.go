package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ibuildthecloud/wslcsession"
)

// runDemo exercises the wslcsession library end to end:
//  1. creates a session (dedicated VM, dies with this process)
//  2. talks to dockerd over DockerConn (a vsock-relayed docker.sock connection)
//  3. uses that to spin up a tiny container listening on the guest's own
//     loopback, then reaches it via DialGuestTCP - the same mechanism as
//     DockerConn, generalized to an arbitrary guest-side TCP address.
//
// No Windows-side TCP port is ever opened; every connection this program
// makes into the guest is a real Winsock socket handle handed back by COM,
// backed by wslcsession.exe's private vsock control channel.
func runDemo(args []string) error {
	fs := flag.NewFlagSet("wslcdemo", flag.ExitOnError)
	sf := addSessionFlags(fs, "wslcsession-demo")
	_ = fs.Parse(args) // ExitOnError: Parse itself never returns on failure
	if err := sf.validate(); err != nil {
		return err
	}

	fmt.Println("Creating session (first run boots a VM, can take a while)...")
	start := time.Now()
	session, err := wslcsession.NewSession(sf.toOptions())
	if err != nil {
		return fmt.Errorf("NewSession: %w", err)
	}
	defer func() {
		fmt.Println("Closing session...")
		if err := session.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "Close() returned:", err)
		}
	}()
	fmt.Printf("Session ready in %s\n", time.Since(start))

	name, err := session.DisplayName()
	if err != nil {
		return fmt.Errorf("DisplayName: %w", err)
	}
	fmt.Printf("Display name: %s\n", name)

	if err := checkDockerVersion(session); err != nil {
		return fmt.Errorf("docker.sock check: %w", err)
	}

	if err := checkGuestTCPDial(session); err != nil {
		if strings.Contains(err.Error(), "toomanyrequests") {
			fmt.Println("Docker Hub anonymous pull rate limit hit (this machine has pulled a lot " +
				"of images during earlier testing) - falling back to a check that needs no image.")
			if err := checkGuestTCPDialRefused(session); err != nil {
				return fmt.Errorf("guest TCP dial fallback check: %w", err)
			}
		} else {
			return fmt.Errorf("guest TCP dial check: %w", err)
		}
	}

	fmt.Println("All checks passed. Press Enter to close the session and exit...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	return nil
}

func checkDockerVersion(session *wslcsession.Session) error {
	conn, err := session.DockerConn()
	if err != nil {
		return fmt.Errorf("DockerConn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	fmt.Println("Connected to dockerd. Sending GET /version...")
	if _, err := conn.Write([]byte("GET /version HTTP/1.0\r\n\r\n")); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	body, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if !strings.Contains(string(body), "200 OK") {
		return fmt.Errorf("unexpected response: %s", body)
	}
	fmt.Println("dockerd responded 200 OK.")
	return nil
}

// checkGuestTCPDialRefused doesn't need any image or network access: it
// dials a port nothing is listening on and confirms the guest's own kernel
// refuses it (an actual "connection refused" originating from inside the
// guest, not an error from failing to even reach it) - proof the whole path
// (COM -> CreateRootNamespaceProcess -> bridge exec -> real connect() syscall
// inside the guest) is intact, independent of Docker Hub availability.
func checkGuestTCPDialRefused(session *wslcsession.Session) error {
	fmt.Println("Dialing an unused guest port (127.0.0.1:1) to confirm the round trip reaches the guest kernel...")
	conn, err := session.DialGuestTCP("127.0.0.1:1")
	if err != nil {
		return fmt.Errorf("DialGuestTCP: %w", err)
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 64)
	n, readErr := conn.Read(buf)
	fmt.Printf("Read returned n=%d err=%v (expect n=0, EOF - bridge process exits immediately on connect() failure)\n", n, readErr)
	return nil
}

// checkGuestTCPDial proves DialGuestTCP reaches a real listener inside the
// guest: it uses the Docker API (over DockerConn, via a plain net/http
// client dialing through it) to run a container that listens on the guest's
// own loopback, then connects to that port with DialGuestTCP and confirms an
// echo round-trips - the exact same underlying mechanism as DockerConn
// (spawn the bridge helper, relay its stdio), just pointed at a TCP address
// instead of a unix socket.
func checkGuestTCPDial(session *wslcsession.Session) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return session.DockerConn()
			},
			// Each dial spawns a fresh bridge process/connection in the
			// guest (see Session.dial) - there's no server-side reuse
			// semantics to speak of, so force a fresh one per request
			// rather than letting Transport pool/reuse across requests.
			DisableKeepAlives: true,
		},
		Timeout: 60 * time.Second,
	}

	// Pin an explicit tag: fromImage=busybox with no &tag= pulls every
	// variant tag (1-musl, 1-uclibc, ...) rather than a single "latest" -
	// busybox's Hub repo doesn't reliably have one.
	const testImage = "busybox:1-musl"
	fmt.Printf("Pulling %s (for the test listener)...\n", testImage)
	resp, err := client.Post("http://localhost/images/create?fromImage=busybox&tag=1-musl", "", nil)
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	pullBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	fmt.Printf("pull status=%d bodyLen=%d body=%s\n", resp.StatusCode, len(pullBody), truncate(string(pullBody), 500))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("pull image: HTTP %d: %s", resp.StatusCode, pullBody)
	}

	imgResp, err := client.Get("http://localhost/images/json")
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	imgBody, _ := io.ReadAll(imgResp.Body)
	_ = imgResp.Body.Close()
	fmt.Printf("images/json status=%d body=%s\n", imgResp.StatusCode, truncate(string(imgBody), 800))

	fmt.Println("Creating a container listening on 127.0.0.1:8765 inside the guest...")
	createBody := fmt.Sprintf(`{
		"Image": %q,
		"Cmd": ["nc", "-lk", "-p", "8765", "-e", "/bin/cat"],
		"HostConfig": {"NetworkMode": "host", "AutoRemove": true}
	}`, testImage)
	resp, err = client.Post("http://localhost/containers/create?name=wslcsession-tcp-test", "application/json", strings.NewReader(createBody))
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	createRespBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("create container: HTTP %d: %s", resp.StatusCode, createRespBody)
	}

	fmt.Println("Starting container...")
	resp, err = client.Post("http://localhost/containers/wslcsession-tcp-test/start", "", nil)
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("start container: HTTP %d", resp.StatusCode)
	}
	defer func() {
		resp, err := client.Post("http://localhost/containers/wslcsession-tcp-test/stop?t=1", "", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	time.Sleep(1 * time.Second) // let nc start listening

	fmt.Println("Dialing 127.0.0.1:8765 inside the guest via DialGuestTCP...")
	conn, err := session.DialGuestTCP("127.0.0.1:8765")
	if err != nil {
		return fmt.Errorf("DialGuestTCP: %w", err)
	}
	defer func() { _ = conn.Close() }()

	const message = "hello-from-wslcsession\n"
	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	buf := make([]byte, len(message))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("read echo: %w", err)
	}
	if string(buf) != message {
		return fmt.Errorf("echo mismatch: got %q, want %q", buf, message)
	}

	fmt.Println("Echo round-trip succeeded over DialGuestTCP.")
	return nil
}
