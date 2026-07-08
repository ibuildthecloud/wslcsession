// wslcdemo has two modes:
//
//	wslcdemo            exercises the wslcgo library end to end (session,
//	                     docker.sock check, DialGuestTCP check)
//	wslcdemo proxy       runs a local TCP -> dockerd proxy backed by a
//	                     wslcgo session, so a real `docker` CLI can point
//	                     at it (docker -H tcp://127.0.0.1:2375)
//
// See demo.go and proxy.go respectively. Session configuration (storage
// path/size, additional VHD-backed volumes, CPU/memory) is shared between
// both modes via the flags in this file.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ibuildthecloud/wslcgo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "proxy" {
		if err := runProxy(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := runDemo(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// volumeFlag implements flag.Value for a repeatable -volume flag, each
// instance describing one additional VHD-backed docker volume to create
// alongside the session (see wslcgo.Options.Volumes).
type volumeFlag []wslcgo.VolumeOptions

func (v *volumeFlag) String() string {
	return fmt.Sprintf("%v", []wslcgo.VolumeOptions(*v))
}

func (v *volumeFlag) Set(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return fmt.Errorf("invalid -volume %q, want name:sizeMB[:fixed]", s)
	}
	sizeMB, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid -volume %q: size must be a number: %w", s, err)
	}
	var fixed bool
	if len(parts) > 2 {
		fixed, err = strconv.ParseBool(parts[2])
		if err != nil {
			return fmt.Errorf("invalid -volume %q: fixed must be true/false: %w", s, err)
		}
	}
	*v = append(*v, wslcgo.VolumeOptions{Name: parts[0], SizeMB: sizeMB, Fixed: fixed})
	return nil
}

// sessionFlags are shared between demo mode and proxy mode: both create a
// wslcgo.Session and both should let you configure it the same way.
type sessionFlags struct {
	name          string
	cpuCount      uint
	memoryMB      uint
	storagePath   string
	storageSizeMB uint64
	volumes       volumeFlag
}

func addSessionFlags(fs *flag.FlagSet, defaultName string) *sessionFlags {
	sf := &sessionFlags{}
	fs.StringVar(&sf.name, "name", defaultName, "wslc session display name")
	fs.UintVar(&sf.cpuCount, "cpu-count", 2, "vCPU count")
	fs.UintVar(&sf.memoryMB, "memory-mb", 2048, "memory (MB)")
	fs.StringVar(&sf.storagePath, "storage-path", "",
		"Windows directory to persist /var/lib/docker in a real VHD (empty = ephemeral tmpfs, wiped on VM restart)")
	fs.Uint64Var(&sf.storageSizeMB, "storage-size-mb", 65536,
		"max size (MB) for the persistent storage VHD, dynamically expanding (only used if -storage-path is set)")
	fs.Var(&sf.volumes, "volume",
		"additional VHD-backed docker volume: name:sizeMB[:fixed] (repeatable; requires -storage-path)")
	return sf
}

func (sf *sessionFlags) validate() error {
	if len(sf.volumes) > 0 && sf.storagePath == "" {
		return fmt.Errorf("-volume requires -storage-path to be set (VHD-backed volumes live under <storage-path>/volumes/)")
	}
	return nil
}

func (sf *sessionFlags) toOptions() wslcgo.Options {
	return wslcgo.Options{
		DisplayName:      sf.name,
		CPUCount:         uint32(sf.cpuCount),
		MemoryMB:         uint32(sf.memoryMB),
		StoragePath:      sf.storagePath,
		MaxStorageSizeMB: sf.storageSizeMB,
		Volumes:          sf.volumes,
	}
}
