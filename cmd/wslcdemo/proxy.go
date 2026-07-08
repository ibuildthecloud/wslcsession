package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"

	"github.com/ibuildthecloud/wslcsession"
)

// runProxy runs a plain local TCP -> dockerd proxy backed by a wslcsession
// session: every accepted connection gets its own DockerConn, relayed
// bidirectionally. This lets a real `docker` CLI (or anything else that
// speaks the Docker Engine API) point at 127.0.0.1:<port> and reach a wslc
// session's dockerd, entirely through the library's supported surface - the
// library itself still never opens a TCP port anywhere; this is purely a
// convenience the example program builds on top for cases where you
// actually want a TCP-facing endpoint (e.g. to hand to `docker` itself).
func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	sf := addSessionFlags(fs, "wslcsession-proxy")
	listen := fs.String("listen", "127.0.0.1:2375", "local address to listen on")
	_ = fs.Parse(args) // ExitOnError: Parse itself never returns on failure
	if err := sf.validate(); err != nil {
		return err
	}

	if host, _, err := net.SplitHostPort(*listen); err == nil && host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		fmt.Fprintf(os.Stderr, "WARNING: listening on %s exposes the Docker API with no authentication "+
			"to anything that can reach this port.\n", *listen)
	}

	fmt.Println("Creating session (first run boots a VM, can take a while)...")
	session, err := wslcsession.NewSession(sf.toOptions())
	if err != nil {
		return fmt.Errorf("NewSession: %w", err)
	}
	defer func() { _ = session.Close() }()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	fmt.Printf("Listening on %s -> dockerd inside wslc session %q\n", *listen, sf.name)
	fmt.Printf("Try: docker -H tcp://%s version\n", *listen)
	fmt.Println("Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		_ = ln.Close()
	}()

	for {
		client, err := ln.Accept()
		if err != nil {
			return nil // listener closed, e.g. via the signal handler above
		}
		go handleProxyConn(session, client)
	}
}

// closeWriter matches *net.TCPConn's convention (and wslcsession's guestConn,
// which implements the same method) for a half-close: "I'm done sending,
// but still want to read." Type-asserting for it here - rather than doing a
// full Close() on whichever side finishes first - is what lets one
// direction finish while the other is still relaying data.
type closeWriter interface {
	CloseWrite() error
}

func handleProxyConn(session *wslcsession.Session, client net.Conn) {
	defer func() { _ = client.Close() }()
	fmt.Printf("[+] %s\n", client.RemoteAddr())

	guest, err := session.DockerConn()
	if err != nil {
		fmt.Fprintln(os.Stderr, "DockerConn:", err)
		return
	}
	defer func() { _ = guest.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guest, client)
		if cw, ok := guest.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, guest)
		if cw, ok := client.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done

	fmt.Printf("[-] %s\n", client.RemoteAddr())
}
