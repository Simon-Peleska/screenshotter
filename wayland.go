package main

// Minimal Wayland wire protocol implementation.
//
// The Wayland protocol is message-passing over a Unix socket.
// Each message has an 8-byte header:
//   [0:4]  object ID (uint32 LE)
//   [4:8]  (size << 16) | opcode  (uint32 LE)
// Followed by (size - 8) bytes of arguments.
//
// File descriptors are passed out-of-band via Unix SCM_RIGHTS cmsg.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// conn wraps the Wayland Unix socket connection and tracks object IDs.
type conn struct {
	c         *net.UnixConn
	nextID    uint32
	objects   map[uint32]dispatcher
	callbacks map[uint32]func() // wl_callback done handlers keyed by object ID
}

type dispatcher interface {
	dispatch(opcode uint16, data []byte, fd int)
}

func connect() (*conn, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return nil, errors.New("XDG_RUNTIME_DIR not set")
	}
	display := os.Getenv("WAYLAND_DISPLAY")
	if display == "" {
		display = "wayland-0"
	}
	addr := runtimeDir + "/" + display

	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	wl := &conn{
		c:         c,
		nextID:    1, // ID 1 is the wl_display
		objects:   make(map[uint32]dispatcher),
		callbacks: make(map[uint32]func()),
	}
	return wl, nil
}

func (wl *conn) close() {
	wl.c.Close()
}

// alloc reserves a new object ID.
func (wl *conn) alloc() uint32 {
	wl.nextID++
	return wl.nextID
}

// register binds an object ID to a dispatcher.
func (wl *conn) register(id uint32, d dispatcher) {
	wl.objects[id] = d
}

func (wl *conn) unregister(id uint32) {
	delete(wl.objects, id)
}

// send writes a Wayland request message.
// objID: the object this request is sent on.
// opcode: request opcode.
// args: already-encoded argument bytes.
// fd: file descriptor to send alongside (or -1).
func (wl *conn) send(objID uint32, opcode uint16, args []byte, fd int) error {
	size := uint32(8 + len(args))
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], objID)
	binary.LittleEndian.PutUint32(hdr[4:8], size<<16|uint32(opcode))
	msg := append(hdr, args...)

	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}

	n, oobn, err := wl.c.WriteMsgUnix(msg, oob, nil)
	if err != nil {
		return err
	}
	if n != len(msg) || (fd >= 0 && oobn != len(oob)) {
		return fmt.Errorf("short write: n=%d oobn=%d", n, oobn)
	}
	return nil
}

var oobSpace = unix.CmsgSpace(4 * 4) // room for a few fds

// recv reads one Wayland event and dispatches it.
func (wl *conn) recv() error {
	header := make([]byte, 8)
	oob := make([]byte, oobSpace)

	n, oobn, _, _, err := wl.c.ReadMsgUnix(header, oob)
	if err != nil {
		return err
	}
	if n != 8 {
		return fmt.Errorf("short header read: %d", n)
	}

	fd := -1
	if oobn > 0 {
		fds, err := parseFds(oob[:oobn])
		if err == nil && len(fds) > 0 {
			fd = fds[0]
		}
	}

	senderID := binary.LittleEndian.Uint32(header[0:4])
	sizeOpcode := binary.LittleEndian.Uint32(header[4:8])
	opcode := uint16(sizeOpcode & 0xffff)
	size := int(sizeOpcode >> 16)

	var body []byte
	bodyLen := size - 8
	if bodyLen > 0 {
		body = make([]byte, bodyLen)
		oob2 := make([]byte, oobSpace)
		var n2, oobn2 int
		if fd == -1 {
			n2, oobn2, _, _, err = wl.c.ReadMsgUnix(body, oob2)
		} else {
			n2, err = wl.c.Read(body)
		}
		if err != nil {
			return err
		}
		if n2 != bodyLen {
			return fmt.Errorf("short body read: got %d want %d", n2, bodyLen)
		}
		if fd == -1 && oobn2 > 0 {
			fds, err := parseFds(oob2[:oobn2])
			if err == nil && len(fds) > 0 {
				fd = fds[0]
			}
		}
	}

	// wl_display (ID=1) opcode 0 = error, opcode 1 = delete_id
	if senderID == 1 {
		if opcode == 0 && len(body) >= 12 {
			objID := binary.LittleEndian.Uint32(body[0:4])
			code := binary.LittleEndian.Uint32(body[4:8])
			msgLen := int(binary.LittleEndian.Uint32(body[8:12]))
			msg := ""
			if msgLen > 0 && len(body) >= 12+msgLen {
				msg = string(body[12 : 12+msgLen-1])
			}
			return fmt.Errorf("wl_display error: obj=%d code=%d msg=%q", objID, code, msg)
		}
		if opcode == 1 && len(body) >= 4 {
			id := binary.LittleEndian.Uint32(body[0:4])
			wl.unregister(id)
		}
		return nil
	}

	// wl_callback (ID varies): opcode 0 = done
	if cb, ok := wl.callbacks[senderID]; ok {
		cb()
		delete(wl.callbacks, senderID)
		wl.unregister(senderID)
		return nil
	}

	d, ok := wl.objects[senderID]
	if !ok {
		// unknown object, ignore
		return nil
	}
	d.dispatch(opcode, body, fd)
	return nil
}

// roundtrip sends a wl_display.sync and processes events until the
// callback fires, ensuring all pending events have been processed.
func (wl *conn) roundtrip() error {
	cbID := wl.alloc()

	done := false
	wl.callbacks[cbID] = func() { done = true }

	// wl_display.sync(new_id<wl_callback>) — opcode 0 on object 1
	args := make([]byte, 4)
	binary.LittleEndian.PutUint32(args[0:4], cbID)
	if err := wl.send(1, 0, args, -1); err != nil {
		return err
	}

	for !done {
		if err := wl.recv(); err != nil {
			return err
		}
	}
	return nil
}

// --- Registry ---

const wlRegistryOpBind = 0

type registryGlobal struct {
	name    uint32
	iface   string
	version uint32
}

type registry struct {
	wl      *conn
	id      uint32
	globals []registryGlobal
}

func getRegistry(wl *conn) (*registry, error) {
	regID := wl.alloc()
	r := &registry{wl: wl, id: regID}
	wl.register(regID, r)

	// wl_display.get_registry(new_id<wl_registry>) — opcode 1 on object 1
	args := make([]byte, 4)
	binary.LittleEndian.PutUint32(args[0:4], regID)
	if err := wl.send(1, 1, args, -1); err != nil {
		return nil, err
	}

	// Roundtrip to collect all globals.
	if err := wl.roundtrip(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *registry) dispatch(opcode uint16, data []byte, _ int) {
	switch opcode {
	case 0: // global
		if len(data) < 12 {
			return
		}
		name := binary.LittleEndian.Uint32(data[0:4])
		ifaceLen := int(binary.LittleEndian.Uint32(data[4:8]))
		if len(data) < 8+ifaceLen {
			return
		}
		iface := nullStr(data[8 : 8+ifaceLen])
		off := 8 + padded(ifaceLen)
		if len(data) < off+4 {
			return
		}
		version := binary.LittleEndian.Uint32(data[off : off+4])
		r.globals = append(r.globals, registryGlobal{name, iface, version})
	case 1: // global_remove — ignore for our purposes
	}
}

// bind creates a new object ID and sends wl_registry.bind for the named global.
func (r *registry) bind(name uint32, iface string, version uint32) (uint32, error) {
	newID := r.wl.alloc()

	// wl_registry.bind args: name(uint32), iface(string), version(uint32), new_id(uint32)
	ifaceBytes := encodeString(iface)
	args := make([]byte, 4+len(ifaceBytes)+4+4)
	off := 0
	binary.LittleEndian.PutUint32(args[off:], name)
	off += 4
	copy(args[off:], ifaceBytes)
	off += len(ifaceBytes)
	binary.LittleEndian.PutUint32(args[off:], version)
	off += 4
	binary.LittleEndian.PutUint32(args[off:], newID)

	return newID, r.wl.send(r.id, wlRegistryOpBind, args, -1)
}

// findGlobal returns the first global matching the given interface.
func (r *registry) findGlobal(iface string) (registryGlobal, bool) {
	for _, g := range r.globals {
		if g.iface == iface {
			return g, true
		}
	}
	return registryGlobal{}, false
}

// --- Encoding helpers ---

// encodeString encodes a Wayland string (uint32 length + bytes + null + padding).
func encodeString(s string) []byte {
	l := len(s) + 1 // include null terminator
	p := padded(l)
	b := make([]byte, 4+p)
	binary.LittleEndian.PutUint32(b[0:4], uint32(l))
	copy(b[4:], s)
	// rest is zero (null + padding)
	return b
}

// padded returns l rounded up to a 4-byte boundary.
func padded(l int) int {
	return (l + 3) &^ 3
}

// nullStr extracts a null-terminated string from a length-prefixed Wayland string buffer.
func nullStr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// encodeUint32 encodes a uint32 as 4 LE bytes.
func encodeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// encodeInt32 encodes an int32 as 4 LE bytes.
func encodeInt32(v int32) []byte {
	return encodeUint32(uint32(v))
}

// appendUint32 appends a uint32 in LE to b.
func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// readUint32 reads a LE uint32 from b at offset off.
func readUint32(b []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(b[off : off+4])
}

// readInt32 reads a LE int32 from b at offset off.
func readInt32(b []byte, off int) int32 {
	return int32(readUint32(b, off))
}

// parseFds extracts file descriptors from a cmsg OOB buffer.
func parseFds(oob []byte) ([]int, error) {
	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, scm := range scms {
		got, err := unix.ParseUnixRights(&scm)
		if err != nil {
			continue
		}
		fds = append(fds, got...)
	}
	return fds, nil
}

// shmCreate creates an anonymous shared memory file of the given size
// and returns its fd. Uses memfd_create on Linux.
func shmCreate(size int) (int, error) {
	fd, err := unix.MemfdCreate("screenshotter-shm", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		// Fallback: shm_open with a temp name
		return shmCreateFallback(size)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func shmCreateFallback(size int) (int, error) {
	name := fmt.Sprintf("/screenshotter-%d", os.Getpid())
	fd, err := unix.Open("/dev/shm"+name, unix.O_RDWR|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC, 0600)
	if err != nil {
		return -1, fmt.Errorf("shm fallback: %w", err)
	}
	unix.Unlink("/dev/shm" + name)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// mmap maps the given fd into memory and returns the slice.
func mmap(fd, size int) ([]byte, error) {
	return unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
}

func munmap(b []byte) {
	unix.Munmap(b)
}

// ptrToUintptr is used to avoid unsafe.Pointer rules when passing memory addresses.
func ptrToUintptr(p unsafe.Pointer) uintptr {
	return uintptr(p)
}
