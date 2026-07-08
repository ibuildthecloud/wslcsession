# wslcsession

A Go library for driving [wslc](https://github.com/microsoft/WSL/tree/master/src/windows/wslcsession)
(WSL Containers) sessions programmatically from a separate Windows process,
without needing the `wslc` CLI, without exposing any TCP port (public or
loopback) unless you explicitly ask for one, and with zero external
dependencies at build or runtime.

```go
import "github.com/ibuildthecloud/wslcsession"

session, err := wslcsession.NewSession(wslcsession.Options{})
if err != nil { ... }
defer session.Close()

conn, err := session.DockerConn() // -> /var/run/docker.sock inside the VM
```

Windows only - see "Other platforms" below.

## Install

```
go get github.com/ibuildthecloud/wslcsession
```

## What this gives you

- **`Session`** - creates a dedicated wslc VM. It is *not* persistent: the
  VM dies with your process (see "Lifecycle" below).
- **`Session.DockerConn()`** - a `net.Conn` connected to dockerd's Docker
  Engine API socket inside the guest.
- **`Session.DialGuestUnix(path)`** / **`Session.DialGuestTCP(addr)`** - the
  general form: connect to *any* unix socket or TCP address reachable from
  inside the guest's own root namespace. `DockerConn` is just
  `DialGuestUnix("/var/run/docker.sock")`.
- **`Options.StoragePath`/`MaxStorageSizeMB`** and **`Options.Volumes`** -
  optionally persist the session's storage and/or create additional
  VHD-backed docker volumes as part of session creation (see "Persistent
  storage" below).

Every connection is relayed over wslc's private vsock control channel - the
same fork-and-exec primitive wslc's own internal docker.sock client uses
(`Fork(WSLC_FORK::Process)` in `WSLCVirtualMachine.cpp`). The library itself
never opens a TCP port on the Windows host to make any of this work; the
example program's `proxy` mode (below) is an opt-in convenience built on top
for when you actually want one.

## Try it

```
git clone https://github.com/ibuildthecloud/wslcsession
cd wslcsession
go run ./cmd/wslcdemo
```

This creates a session, fetches `dockerd`'s `/version` over `DockerConn`, and
runs a `DialGuestTCP` check (against a real container listener when one can
be pulled, or against a definitely-refused port otherwise, to prove the
round trip reaches the guest kernel either way).

`cmd/wslcdemo` also has a `proxy` mode, which runs a plain TCP -> dockerd
proxy so a real `docker` CLI can point at it:

```
go run ./cmd/wslcdemo proxy -listen 127.0.0.1:2375
# in another shell:
docker -H tcp://127.0.0.1:2375 version
```

Session configuration (storage, volumes, CPU/memory) is available as flags
on both modes - see `-h` on either, or `addSessionFlags` in
`cmd/wslcdemo/main.go`.

## Lifecycle

`NewSession` never sets wslc's `Persistent` session flag. Per wslc's own
session-manager source comment: a non-persistent session is torn down "when
all client refs are released" - which happens automatically when your
process exits *or crashes*, because COM releases every outstanding reference
held by a dead process. `Session.Close()` additionally calls `Terminate()`
explicitly for fast, deterministic teardown on a clean exit; it's not
required for cleanup on a crash, just faster than waiting for COM to notice.

## Persistent storage

By default sessions use ephemeral tmpfs storage - `/var/lib/docker` (images,
containers, volumes) is wiped every time the VM restarts. Set
`Options.StoragePath` to a real Windows directory to back it with a real VHD
that survives VM restarts:

```go
wslcsession.Options{
    StoragePath:      `C:\wslcsession-data`,
    MaxStorageSizeMB: 65536, // 64GB max, dynamically expanding
}
```

`Options.Volumes` additionally creates named docker volumes as part of
session creation, each backed by its own independent `.vhdx` file under
`<StoragePath>/volumes/` - not the same VHD as the main session storage
above. Requires `StoragePath` to be set (there's nowhere sensible for these
to live under ephemeral tmpfs storage). Volume creation is all-or-nothing:
if any entry fails, the whole session is torn down and `NewSession` returns
an error rather than handing back a partially-configured session.

```go
wslcsession.Options{
    StoragePath:      `C:\wslcsession-data`,
    MaxStorageSizeMB: 65536,
    Volumes: []wslcsession.VolumeOptions{
        {Name: "mydata", SizeMB: 10240},        // dynamically expanding
        {Name: "scratch", SizeMB: 2048, Fixed: true},
    },
}
```

Each ends up as a completely normal docker volume - `docker volume ls`,
`docker run -v mydata:/data ...`, etc. all work exactly as they would with
any other named volume; the only difference is the storage backing it.

The general VM root filesystem (anything outside `/var/lib/docker`) is
writable at runtime but is *not* persisted regardless of this setting - every
VM boot starts from the same shared, read-only-at-the-block-level base image
(`system.vhd`, shared with regular WSL2/WSLg), with a fresh writable layer
stacked on top each time. This was verified empirically, not just inferred:
a file written directly to the guest root before calling `Terminate()` was
gone after the next session creation.

## Important: this uses an internal, unstable API

This library talks to `IWSLCSessionManager`/`IWSLCSession`
(`src/windows/service/inc/wslc.idl` in the
[microsoft/WSL](https://github.com/microsoft/WSL) repo), **not** the
documented, versioned SDK surface (`IWSLCCompatSessionManager`/
`IWSLCCompatSession` in `WSLCCompat.idl`). `wslc.idl` says so explicitly:

> ABI breaking changes in this file are OK, since both client & server
> always ship together. The WSLC SDK must not use this file, and instead use
> WSLCCompat.idl.

The documented/compat surface doesn't expose `CreateRootNamespaceProcess` or
`MountWindowsFolder`, which this library depends on for the bridge mechanism
below - there is currently no way to build this on the stable API alone.

**Practical implication:** a future WSL update could change `wslc.idl`'s
vtable order or struct layouts with zero notice or compatibility guarantee.
If something in this library suddenly starts failing after a WSL update,
that's the first thing to check - re-derive the vtable slots and struct
layouts (see "Re-deriving the ABI" below) against the new `wslc.idl`.

## How dialing actually works

wslc's minimal guest image (Microsoft Azure Linux, confirmed via
`/etc/os-release`) ships no tool capable of bridging a process's stdio to a
unix socket or TCP address - no `socat`, `ncat`, `python3`, or `perl` among
its ~650 binaries, and BusyBox's `nc` has no `-U` (unix socket) support. So:

1. `bridgesrc/bridge.c` (~150 lines) is a tiny helper that connects to a
   target (`unix:<path>` or `tcp:<host>:<port>`) and splices stdin/stdout to
   it, handling half-close in both directions correctly (see "Half-close
   handling" below).
2. `bridgesrc/entrypoint.sh` is a build-once-exec-forever wrapper around it.
3. Both are embedded as plain *source* (`//go:embed`, no compiled binary
   checked into the repo at all) and extracted to a temp directory at
   runtime. `Session.dial` mounts that directory into the guest once,
   read-only, via `MountWindowsFolder`.
4. Each `DockerConn`/`DialGuestUnix`/`DialGuestTCP` call runs
   `entrypoint.sh` in the guest's root namespace via
   `CreateRootNamespaceProcess` (invoked as `sh entrypoint.sh <target>`,
   not relying on its own executable bit - see the comment in
   `session.go`'s `dial`). The **first** call per VM boot has no cached
   binary yet, so the script builds one on the spot using **the guest's own
   dockerd** (`docker run --rm -v ... alpine:latest sh -c 'apk add gcc
   musl-dev && gcc ...'`) - the same daemon this library already talks to
   for everything else, not anything on the Windows host. It caches the
   result under `/tmp` (tmpfs, so it naturally survives for the rest of
   that VM's lifetime and disappears on VM restart) and execs it. Every
   call after the first just execs the cached binary directly.

This means editing `bridgesrc/bridge.c` needs nothing beyond a text editor -
no `go generate`, no C toolchain, no `docker` CLI, on the machine running
this Go program at all. The tradeoff: the very first
`DockerConn`/`DialGuestUnix`/`DialGuestTCP` call on a freshly booted session
is slower (pulling `alpine` and installing `gcc`/`musl-dev` inside the
guest), and needs guest network access at that moment - a reasonable ask
given a wslc session already needs network access for image pulls in
general.

`entrypoint.sh` deliberately floats on `alpine:latest` rather than pinning a
specific version: this is a trivial, dependency-free static compile with no
exposure to Alpine-version-specific behavior, so there's nothing to gain
from pinning and a real, recurring cost to it - Alpine cuts a new release
every ~6 months and drops security support for old ones after about 2 years.

### Half-close handling

The relay loop tracks each direction's EOF independently rather than tearing
down the whole connection the instant either side finishes - this matters
for two different reasons depending on direction:

- **Target finishes first** (e.g. a server responds and half-closes while
  the client still has more to send): the loop keeps relaying
  `stdin -> target` after seeing the target's EOF, instead of discarding
  whatever the client hadn't sent yet.
- **Client finishes first**, or the target's response needs to reach the
  client immediately: `bridge` explicitly `close(1)`s (stdout) the moment
  the target's read reaches EOF, rather than just marking that direction
  done internally. This matters because the Windows-side caller is very
  likely blocked in something like `io.ReadAll` waiting for that exact EOF
  signal (a one-shot HTTP/1.0-style request never bothers to half-close its
  own write side) - if `bridge` only stopped polling the target without
  actually closing stdout, that caller would hang forever waiting for an
  EOF that would only arrive once the *whole* process exits, which by
  construction wasn't going to happen until the caller itself was done
  writing. Getting this backwards was a real regression caught by running
  the full demo after the fix, not just re-reading the diff.

`Session.DockerConn`/`DialGuestUnix`/`DialGuestTCP` return a `net.Conn` that
also implements `CloseWrite() error` (the same convention as
`*net.TCPConn`), so callers that need to half-close from the Go side - "I'm
done sending, still want to read the rest of the response" - can do so
without a full `Close()`. `cmd/wslcdemo/proxy.go` relies on this for both
directions of its relay.

## Other platforms

wslc is a Windows-only feature (WSL2 + Hyper-V), and this library talks
directly to Windows COM interfaces (`ole32.dll`, `ws2_32.dll`) that don't
exist anywhere else. `go build`/`go vet` still succeed on other platforms -
`Session`/`NewSession`/`Options` are all still there, backed by a stub
(`unsupported.go`) where every method returns a clear "not supported on
$GOOS" error - so a cross-platform CI matrix, or a larger project that
imports this package behind a runtime check rather than a build tag, won't
hit a compile failure just from depending on it.

## Code layout

- `com.go` / `types.go` - the raw ABI layer: vtable slots, C struct layouts,
  `unsafe.Pointer`, manual `runtime.KeepAlive`. Nothing outside this pair of
  files touches a raw COM pointer.
- `comapi.go` - wraps that ABI in three Go interfaces mirroring the real COM
  interfaces one-for-one (`sessionManager`/`IWSLCSessionManager`,
  `wslcSession`/`IWSLCSession`, `wslcProcess`/`IWSLCProcess`), but with
  Go-native parameter and return types (`string`, `[]string`, `bool`)
  instead of `*uint16`/`*byte`/`uintptr`. All the marshaling lives in the
  concrete `comSessionManager`/`comSession`/`comProcess` implementations.
- `session.go` / `conn.go` - the public API (`Session`, `net.Conn`). These
  only ever call the interfaces from `comapi.go` - no `unsafe`, no vtable
  slots, no raw pointers.
- `options.go` - `Options`/`VolumeOptions`, shared as-is between the real
  implementation and the non-Windows stub.
- `unsupported.go` - the non-Windows stub (see "Other platforms" below).

## Testing

- **Unit tests** (`*_test.go`, run by a plain `go test ./...`) cover GUID
  parsing, HRESULT handling, struct marshaling (`makeStringArray`),
  `Options` validation, and `Session`/`guestConn` lifecycle logic
  (`do`/`Close`/`CloseWrite` idempotency, process-release-on-close) - all
  without booting a real VM, either by testing pure logic directly or, for
  `Session`, by swapping in a fake COM-thread loop that just drains `reqCh`
  (see `newTestSession` in `session_test.go`) since `do`/`Close` never touch
  the real COM handles directly.
- **E2E tests** (`e2e_test.go`) boot a real wslc VM and exercise the full
  stack against a real dockerd: session lifecycle, `DockerConn` + `/version`,
  `DialGuestTCP` against both a refused port and (via `TestE2EPersistentVolume`)
  a real VHD-backed volume. These are skipped by default - they need an
  actual wslc-capable Windows machine - and only run with:
  ```
  $env:wslcsession_E2E="1"; go test -run E2E ./...
  ```
- `cmd/wslcdemo` remains the interactive, narrated version of the same
  checks - useful for watching what's actually happening, not just pass/fail.

## Two gotchas that cost real debugging time - documented so you don't repeat them

1. **`CO_E_NOTINITIALIZED` on the second COM call.** COM apartment state is
   per-OS-thread; Go goroutines migrate between OS threads by default. A call
   right after `NewSession` can land on a different thread than the one that
   initialized COM. Fixed by giving each `Session` a dedicated goroutine that
   calls `runtime.LockOSThread()` once and serializes every COM call for that
   session through a channel (`session.go`'s `comThread`/`do`).
2. **`ERROR_UNKNOWN_REVISION` (0x80070542) from `CreateSession`, deep inside
   what looks like a settings-validation error.** The real cause:
   `WSLService` impersonates the calling process to read its token/SID, which
   requires `RPC_C_IMP_LEVEL_IMPERSONATE`. Without an explicit
   `CoInitializeSecurity` call setting that level, the security context is
   insufficient, the service reads a garbled SID, and `CreateSession` fails
   downstream in a way that looks unrelated to security at all.

## Re-deriving the ABI (if a WSL update breaks this)

The vtable slots and struct layouts in `types.go` were verified, not guessed,
by running `midl.exe` (from the Windows SDK) directly against
`src/windows/service/inc/wslc.idl` (from the
[microsoft/WSL](https://github.com/microsoft/WSL) repo) and reading the
generated C header - not by hand-counting the IDL. If this library breaks
after a WSL update:

```
midl /out <outdir> /cpp_cmd <path-to-cl.exe> /I src/windows/service/inc /I "<SDK>/shared" /I "<SDK>/um" src/windows/service/inc/wslc.idl
```

Then diff the generated `wslc.h`'s vtable method order and struct field
layout against what's in `types.go`.

## License

Apache License 2.0 - see [LICENSE](LICENSE).
