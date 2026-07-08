//go:build windows

package wslcgo

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

// Session owns a dedicated wslc VM. It is NOT persistent: the underlying VM
// is torn down when the last reference to it is released, which happens
// automatically when this process exits or crashes (COM releases all of a
// dead process's outstanding references), or explicitly via Close.
//
// All COM calls for a Session run on one dedicated, runtime.LockOSThread'd
// goroutine (started in NewSession, serialized via reqCh). This isn't
// optional: COM apartment state is per-OS-thread, but Go goroutines are free
// to migrate between OS threads whenever they block or get preempted. Without
// pinning every call for a given COM object to the same OS thread, a call
// made shortly after NewSession returns can land on a different thread than
// the one that initialized COM, and fail with CO_E_NOTINITIALIZED
// (0x800401F0) - which is exactly what happened during development here
// before this was added.
type Session struct {
	reqCh  chan func()
	closed chan struct{}

	// closeMu/stopped guard against a real race: something outside this
	// package's control (e.g. net/http's Transport closing an idle
	// connection from its own background goroutine) can call into a method
	// that calls do() at the same moment Close() runs. Without this guard,
	// do() sending on reqCh right as Close() closes it panics with "send on
	// closed channel" - reproduced during development via exactly that
	// net/http idle-connection-cleanup path. Close() takes the write lock
	// before closing reqCh, so it cannot proceed while any do() call is
	// mid-send, and any do() call arriving after stopped=true is set simply
	// no-ops instead of racing the close.
	closeMu   sync.RWMutex
	stopped   bool
	closeOnce sync.Once

	ptr uintptr
	mgr uintptr

	bridgeDir  string
	bridgeDone bool
}

// NewSession creates a brand-new dedicated wslc VM. This can take a while
// the first time (VM boot).
func NewSession(opts Options) (*Session, error) {
	if len(opts.Volumes) > 0 && opts.StoragePath == "" {
		return nil, fmt.Errorf("wslcgo: Options.Volumes requires Options.StoragePath to be set " +
			"(VHD-backed volumes live under <StoragePath>/volumes/)")
	}

	s := &Session{
		reqCh:  make(chan func()),
		closed: make(chan struct{}),
	}

	initErr := make(chan error, 1)
	go s.comThread(opts, initErr)

	if err := <-initErr; err != nil {
		return nil, err
	}

	runtime.SetFinalizer(s, (*Session).Close)
	return s, nil
}

// comThread owns this session's COM apartment for its entire lifetime: one
// OS thread, CoInitialize'd once, processing every COM call for this session
// off reqCh until Close() closes it, at which point it terminates the VM and
// releases both COM references before exiting.
func (s *Session) comThread(opts Options, initErr chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fail := func(err error) {
		initErr <- err
		close(s.closed)
	}

	if err := initCOM(); err != nil {
		fail(fmt.Errorf("wslcgo: COM init: %w", err))
		return
	}

	mgrPtr, err := coCreateInstance(&clsidWSLCSessionManager, &iidIWSLCSessionManager, clsctxLocalServer)
	if err != nil {
		fail(fmt.Errorf("wslcgo: activate WSLCSessionManager: %w", err))
		return
	}

	sessionPtr, err := createSession(mgrPtr, opts)
	if err != nil {
		comRelease(mgrPtr)
		fail(err)
		return
	}

	for _, v := range opts.Volumes {
		if err := createVolume(sessionPtr, v); err != nil {
			// All-or-nothing: don't hand back a session with only some of
			// the requested volumes created.
			vtblCall(sessionPtr, slotSessionTerminate)
			comRelease(sessionPtr)
			comRelease(mgrPtr)
			fail(fmt.Errorf("wslcgo: create volume %q: %w", v.Name, err))
			return
		}
	}

	s.ptr = sessionPtr
	s.mgr = mgrPtr
	initErr <- nil

	for req := range s.reqCh {
		req()
	}

	if s.ptr != 0 {
		vtblCall(s.ptr, slotSessionTerminate) // best-effort
		comRelease(s.ptr)
	}
	comRelease(s.mgr)
	close(s.closed)
}

// do runs f on the session's COM thread and waits for it to finish. Safe to
// call after Close (f simply never runs).
func (s *Session) do(f func()) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.stopped {
		return
	}
	done := make(chan struct{})
	s.reqCh <- func() { defer close(done); f() }
	<-done
}

func createSession(mgrPtr uintptr, opts Options) (uintptr, error) {
	displayName := opts.DisplayName
	if displayName == "" {
		displayName = fmt.Sprintf("wslcgo-%d", os.Getpid())
	}

	dnPtr, err := syscall.UTF16PtrFromString(displayName)
	if err != nil {
		return 0, err
	}

	var spPtr *uint16
	if opts.StoragePath != "" {
		spPtr, err = syscall.UTF16PtrFromString(opts.StoragePath)
		if err != nil {
			return 0, err
		}
	}

	cpuCount := opts.CPUCount
	if cpuCount == 0 {
		cpuCount = 2
	}
	memoryMB := opts.MemoryMB
	if memoryMB == 0 {
		memoryMB = 2048
	}
	bootTimeoutMs := uint32(60000)
	if opts.BootTimeout > 0 {
		bootTimeoutMs = uint32(opts.BootTimeout.Milliseconds())
	}

	settings := wslcSessionSettings{
		DisplayName:          dnPtr,
		StoragePath:          spPtr,
		MaximumStorageSizeMb: opts.MaxStorageSizeMB,
		CpuCount:             cpuCount,
		MemoryMb:             memoryMB,
		BootTimeoutMs:        bootTimeoutMs,
		NetworkingMode:       networkingModeNAT,
	}

	var sessionPtr uintptr
	hr := vtblCall(mgrPtr, slotSessionManagerCreateSession,
		uintptr(unsafe.Pointer(&settings)),
		uintptr(sessionFlagNone), // no Persistent: see Session doc comment.
		0,                        // warning callback
		uintptr(unsafe.Pointer(&sessionPtr)))
	runtime.KeepAlive(dnPtr)
	runtime.KeepAlive(spPtr)
	runtime.KeepAlive(settings)
	if err := hrErr(hr); err != nil {
		return 0, fmt.Errorf("wslcgo: CreateSession: %w", err)
	}
	return sessionPtr, nil
}

// createVolume calls IWSLCSession::CreateVolume with the "vhd" driver (see
// WSLCVhdVolume.cpp / WSLCVolumeMetadata.h) to create a named docker volume
// backed by its own .vhdx, independent of the session's main storage.
// Called from comThread during session creation itself (see NewSession's
// Options.Volumes handling), so - like createSession - it runs on the COM
// thread by construction and doesn't need to go through Session.do.
func createVolume(sessionPtr uintptr, v VolumeOptions) error {
	if v.Name == "" {
		return fmt.Errorf("wslcgo: volume name is required")
	}
	if v.SizeMB == 0 {
		return fmt.Errorf("wslcgo: volume %q: SizeMB must be > 0", v.Name)
	}

	namePtr, err := syscall.BytePtrFromString(v.Name)
	if err != nil {
		return err
	}
	driverPtr, err := syscall.BytePtrFromString(volumeDriverVhd)
	if err != nil {
		return err
	}

	sizeBytesKeyPtr, _ := syscall.BytePtrFromString(driverOptSizeBytes)
	sizeBytesValPtr, _ := syscall.BytePtrFromString(strconv.FormatUint(v.SizeMB*1024*1024, 10))
	fixedKeyPtr, _ := syscall.BytePtrFromString(driverOptFixed)
	fixedValPtr, _ := syscall.BytePtrFromString(strconv.FormatBool(v.Fixed))

	driverOpts := []wslcKeyValuePair{
		{Key: sizeBytesKeyPtr, Value: sizeBytesValPtr},
		{Key: fixedKeyPtr, Value: fixedValPtr},
	}

	options := wslcVolumeOptions{
		Name:            namePtr,
		Driver:          driverPtr,
		DriverOpts:      &driverOpts[0],
		DriverOptsCount: uint32(len(driverOpts)),
	}

	var info wslcVolumeInformation
	hr := vtblCall(sessionPtr, slotSessionCreateVolume,
		uintptr(unsafe.Pointer(&options)),
		uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(namePtr)
	runtime.KeepAlive(driverPtr)
	runtime.KeepAlive(driverOpts)
	runtime.KeepAlive(sizeBytesKeyPtr)
	runtime.KeepAlive(sizeBytesValPtr)
	runtime.KeepAlive(fixedKeyPtr)
	runtime.KeepAlive(fixedValPtr)
	return hrErr(hr)
}

// Close terminates the VM and releases COM references. Safe to call
// multiple times; blocks until teardown completes.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.stopped = true
		close(s.reqCh)
		s.closeMu.Unlock()

		<-s.closed
		runtime.SetFinalizer(s, nil)
	})
	return nil
}

// DisplayName returns the session's display name.
func (s *Session) DisplayName() (string, error) {
	var name string
	var callErr error
	s.do(func() {
		var p *uint16
		hr := vtblCall(s.ptr, slotSessionGetDisplayName, uintptr(unsafe.Pointer(&p)))
		if err := hrErr(hr); err != nil {
			callErr = err
			return
		}
		name = syscall.UTF16ToString(unsafe.Slice(p, 256))
	})
	return name, callErr
}

// DockerConn returns a connection to dockerd's Docker Engine API socket
// inside the guest, relayed entirely over the guest's private vsock control
// channel - no TCP port, public or otherwise, is ever created.
func (s *Session) DockerConn() (net.Conn, error) {
	return s.DialGuestUnix("/var/run/docker.sock")
}

// DialGuestUnix connects to a unix domain socket at the given path inside
// the guest's root namespace.
func (s *Session) DialGuestUnix(path string) (net.Conn, error) {
	return s.dial("unix:" + path)
}

// DialGuestTCP connects to a TCP address as seen from inside the guest's own
// network namespace (e.g. a container-published port bound to
// 127.0.0.1:<port> inside the VM). This is the general-purpose counterpart
// to DockerConn - same mechanism, arbitrary target.
func (s *Session) DialGuestTCP(addr string) (net.Conn, error) {
	return s.dial("tcp:" + addr)
}

func (s *Session) dial(target string) (net.Conn, error) {
	bridgeDir, err := s.ensureBridgeMounted()
	if err != nil {
		return nil, err
	}

	// Exec via /bin/sh explicitly rather than relying on entrypoint.sh's own
	// executable bit: it arrives in the guest through a 9p-mounted Windows
	// folder (MountWindowsFolder), and Windows has no POSIX-mode concept to
	// preserve in the first place - not worth depending on whatever
	// permission bits the 9p layer happens to expose.
	script := bridgeDir + "/entrypoint.sh"
	processPtr, errno, err := s.execRootNamespace("/bin/sh", []string{"/bin/sh", script, target})
	if err != nil {
		return nil, fmt.Errorf("wslcgo: spawn bridge for %q (guest errno=%d): %w", target, errno, err)
	}

	var stdin, stdout wslcHandle
	var hErr error
	s.do(func() {
		stdin, hErr = getStdHandle(processPtr, fdStdin)
		if hErr != nil {
			return
		}
		stdout, hErr = getStdHandle(processPtr, fdStdout)
	})
	if hErr != nil {
		s.do(func() { comRelease(processPtr) })
		return nil, hErr
	}

	return newGuestConn(s, processPtr, stdin.Handle, stdout.Handle), nil
}

// releaseProcess is used by guestConn.Close to release the IWSLCProcess
// reference on the session's COM thread.
func (s *Session) releaseProcess(processPtr uintptr) {
	s.do(func() { comRelease(processPtr) })
}

// execRootNamespace runs argv[0] inside the guest's root namespace (i.e. not
// inside any container) with a writable stdin pipe, via
// IWSLCSession::CreateRootNamespaceProcess. Internally, wslcsession.exe
// backs this with the exact same Fork(WSLC_FORK::Process) primitive its own
// DockerHTTPClient uses for its private docker.sock relay
// (WSLCVirtualMachine.cpp) - this is the supported front door onto that
// same fork machinery.
func (s *Session) execRootNamespace(executable string, argv []string) (uintptr, int32, error) {
	exePtr, err := syscall.BytePtrFromString(executable)
	if err != nil {
		return 0, 0, err
	}

	cmdLine, keepAlive, err := makeStringArray(argv)
	if err != nil {
		return 0, 0, err
	}
	env, envKeepAlive, err := makeStringArray(nil)
	if err != nil {
		return 0, 0, err
	}

	options := wslcProcessOptions{
		CommandLine: cmdLine,
		Environment: env,
		Flags:       processFlagStdin,
	}

	var processPtr uintptr
	var errNo int32
	var callErr error
	s.do(func() {
		hr := vtblCall(s.ptr, slotSessionCreateRootNamespaceProcess,
			uintptr(unsafe.Pointer(exePtr)),
			uintptr(unsafe.Pointer(&options)),
			0, 0, // ttyRows, ttyColumns - unused, no Tty flag set
			uintptr(unsafe.Pointer(&processPtr)),
			uintptr(unsafe.Pointer(&errNo)))
		callErr = hrErr(hr)
	})
	runtime.KeepAlive(exePtr)
	runtime.KeepAlive(keepAlive)
	runtime.KeepAlive(envKeepAlive)
	runtime.KeepAlive(options)
	if callErr != nil {
		return 0, errNo, callErr
	}
	return processPtr, errNo, nil
}

func getStdHandle(processPtr uintptr, fd int32) (wslcHandle, error) {
	var h wslcHandle
	hr := vtblCall(processPtr, slotProcessGetStdHandle, uintptr(fd), uintptr(unsafe.Pointer(&h)))
	if err := hrErr(hr); err != nil {
		return h, err
	}
	// Observed to always be Socket in practice, but WSLCHandle is a genuine
	// discriminated union - fail clearly here rather than silently handing
	// a File or Pipe handle to raw Winsock recv()/send() later, which would
	// misbehave instead of erroring.
	if h.Type != handleTypeSocket {
		return h, fmt.Errorf("wslcgo: expected a Socket handle, got %s", h.Type)
	}
	return h, nil
}

// makeStringArray builds a wslcStringArray (an array of char*) from a Go
// string slice. The returned keepAlive value must stay reachable for as
// long as the resulting wslcStringArray is passed to a syscall - it holds
// the actual *byte pointers (and the slice backing them) so Go's GC doesn't
// collect them out from under the in-flight call.
func makeStringArray(items []string) (wslcStringArray, any, error) {
	if len(items) == 0 {
		return wslcStringArray{}, nil, nil
	}

	ptrs := make([]*byte, len(items))
	for i, s := range items {
		p, err := syscall.BytePtrFromString(s)
		if err != nil {
			return wslcStringArray{}, nil, err
		}
		ptrs[i] = p
	}

	return wslcStringArray{
		Values: (*uintptr)(unsafe.Pointer(&ptrs[0])),
		Count:  uint32(len(ptrs)),
	}, ptrs, nil
}

func (s *Session) ensureBridgeMounted() (string, error) {
	var dir string
	var callErr error
	s.do(func() {
		if s.bridgeDone {
			dir = s.bridgeDir
			return
		}

		extractedDir, err := extractBridgeBinary()
		if err != nil {
			callErr = err
			return
		}

		wPtr, err := syscall.UTF16PtrFromString(extractedDir)
		if err != nil {
			callErr = err
			return
		}
		lPtr, err := syscall.BytePtrFromString(guestBridgeMountPath)
		if err != nil {
			callErr = err
			return
		}

		hr := vtblCall(s.ptr, slotSessionMountWindowsFolder,
			uintptr(unsafe.Pointer(wPtr)), uintptr(unsafe.Pointer(lPtr)), 1 /* read-only */)
		runtime.KeepAlive(wPtr)
		runtime.KeepAlive(lPtr)
		if err := hrErr(hr); err != nil && !isHRESULT(err, hrErrorAlreadyExists) {
			callErr = fmt.Errorf("wslcgo: MountWindowsFolder: %w", err)
			return
		}

		s.bridgeDir = guestBridgeMountPath
		s.bridgeDone = true
		dir = s.bridgeDir
	})
	return dir, callErr
}
