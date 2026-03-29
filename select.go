package main

// Interactive region selection using a Wayland layer-shell overlay.
//
// Creates a fullscreen OVERLAY surface on each output, filled with a
// semi-transparent dark overlay. The user drags a rectangle; the selected
// area is rendered transparent so the content beneath shows through.
// Returns the selected rectangle in global logical compositor coordinates.
//
// Protocols used:
//   zwlr_layer_shell_v1  — fullscreen overlay surfaces
//   wl_seat / wl_pointer — mouse input
//   wl_seat / wl_keyboard — ESC to cancel

import (
	"errors"
	"fmt"
	"image"

	"golang.org/x/sys/unix"
)

// errCancelled is returned by selectRegionWayland when the user presses ESC
// or right-clicks. The caller should exit cleanly, not treat it as an error.
var errCancelled = errors.New("cancelled")

// selState holds all mutable state during the interactive selection loop.
type selState struct {
	wl    *conn
	surfs []*selSurf

	// cursor surface to show on pointer enter
	ptrID         uint32
	cursorSurfID  uint32
	cursorHot     int

	// index into surfs of the surface the pointer is currently over (-1 = none)
	activeSurf int

	// drag state in surface-local logical pixel coordinates
	pressing bool
	startX   int
	startY   int
	curX     int
	curY     int

	// result
	done      bool
	cancelled bool
	result    image.Rectangle // global logical coordinates
}

// selSurf is one fullscreen layer surface covering a single output.
type selSurf struct {
	outputIdx int
	outLogX   int // global logical offset of this output
	outLogY   int

	surfID  uint32
	lsurfID uint32
	poolID  uint32
	buf     *selBuf
	w, h    int // surface size in logical pixels
	stride  int
}

// selBuf wraps a wl_buffer and tracks whether the compositor holds it.
type selBuf struct {
	id   uint32
	data []byte // mmap'd pixel data
	busy bool
}

func (b *selBuf) dispatch(opcode uint16, _ []byte, _ int) {
	if opcode == 0 { // wl_buffer.release
		b.busy = false
	}
}

// drawOverlay fills the buffer.
// Outside the selection: dark translucent overlay.
// Inside the selection: fully transparent (content shows through).
// 2px white border around the selection rectangle.
func (ss *selSurf) drawOverlay(x0, y0, x1, y1 int, hasSelection bool) {
	data := ss.buf.data
	w, h, stride := ss.w, ss.h, ss.stride

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			di := y*stride + x*4

			if !hasSelection {
				// ARGB8888 memory layout: B G R A
				data[di+0] = 0x21 // B
				data[di+1] = 0x20 // G
				data[di+2] = 0x1d // R
				data[di+3] = 0xB0
				continue
			}

			inSel := x >= x0 && x < x1 && y >= y0 && y < y1
			onBorder := !inSel &&
				x >= x0-2 && x < x1+2 &&
				y >= y0-2 && y < y1+2

			switch {
			case inSel:
				// fully transparent — underlying content is visible
				data[di+0] = 0
				data[di+1] = 0
				data[di+2] = 0
				data[di+3] = 0
			case onBorder:
				// gruvbox bright orange #fe8019
				data[di+0] = 0x19 // B
				data[di+1] = 0x80 // G
				data[di+2] = 0xfe // R
				data[di+3] = 0xff // A
			default:
				// gruvbox dark hard background #1d2021 at ~70% opacity
				data[di+0] = 0x21 // B
				data[di+1] = 0x20 // G
				data[di+2] = 0x1d // R
				data[di+3] = 0xB0 // A
			}
		}
	}
}

// commit attaches the buffer to the surface and commits.
// Skips if the compositor still holds the buffer.
func (ss *selSurf) commit(wl *conn) {
	if ss.buf.busy {
		return
	}
	ss.buf.busy = true

	// wl_surface.attach(buffer, 0, 0) — opcode 1
	{
		args := make([]byte, 12)
		putU32(args, 0, ss.buf.id)
		wl.send(ss.surfID, 1, args, -1) //nolint:errcheck
	}
	// wl_surface.damage_buffer(0, 0, w, h) — opcode 9
	{
		args := make([]byte, 16)
		putI32(args, 8, int32(ss.w))
		putI32(args, 12, int32(ss.h))
		wl.send(ss.surfID, 9, args, -1) //nolint:errcheck
	}
	// wl_surface.commit — opcode 6
	wl.send(ss.surfID, 6, nil, -1) //nolint:errcheck
}

// lsConfigureDispatcher receives zwlr_layer_surface_v1.configure events.
type lsConfigureDispatcher struct {
	serial     uint32
	width      uint32
	height     uint32
	configured bool
}

func (l *lsConfigureDispatcher) dispatch(opcode uint16, data []byte, _ int) {
	if opcode == 0 && len(data) >= 12 { // configure(serial, width, height)
		l.serial = readUint32(data, 0)
		l.width = readUint32(data, 4)
		l.height = readUint32(data, 8)
		l.configured = true
	}
}

// pointerDispatcher handles wl_pointer events.
type pointerDispatcher struct {
	sel *selState
}

func (p *pointerDispatcher) dispatch(opcode uint16, data []byte, _ int) {
	sel := p.sel
	switch opcode {
	case 0: // enter(serial, surface, sx:fixed, sy:fixed)
		if len(data) < 16 {
			return
		}
		serial := readUint32(data, 0)
		surfID := readUint32(data, 4)
		// wl_fixed is a 24.8 signed fixed-point integer
		sx := int(readInt32(data, 8)) >> 8
		sy := int(readInt32(data, 12)) >> 8
		sel.activeSurf = -1
		for i, ss := range sel.surfs {
			if ss.surfID == surfID {
				sel.activeSurf = i
				sel.curX = sx
				sel.curY = sy
				break
			}
		}
		// wl_pointer.set_cursor(serial, surface, hotspot_x, hotspot_y) — opcode 0
		{
			args := make([]byte, 16)
			putU32(args, 0, serial)
			putU32(args, 4, sel.cursorSurfID)
			putI32(args, 8, int32(sel.cursorHot))
			putI32(args, 12, int32(sel.cursorHot))
			sel.wl.send(sel.ptrID, 0, args, -1) //nolint:errcheck
		}

	case 1: // leave(serial, surface)
		sel.activeSurf = -1

	case 2: // motion(time, sx:fixed, sy:fixed)
		if len(data) < 12 {
			return
		}
		sx := int(readInt32(data, 4)) >> 8
		sy := int(readInt32(data, 8)) >> 8
		sel.curX = sx
		sel.curY = sy
		if sel.pressing && sel.activeSurf >= 0 {
			ss := sel.surfs[sel.activeSurf]
			if !ss.buf.busy {
				x0, y0 := minI(sel.startX, sx), minI(sel.startY, sy)
				x1, y1 := maxI(sel.startX, sx), maxI(sel.startY, sy)
				ss.drawOverlay(x0, y0, x1, y1, true)
				ss.commit(sel.wl)
			}
		}

	case 3: // button(serial, time, button, state)
		if len(data) < 16 {
			return
		}
		button := readUint32(data, 8)
		state := readUint32(data, 12)

		const (
			btnLeft  = 0x110
			btnRight = 0x111
		)
		switch {
		case button == btnLeft && state == 1: // left pressed
			sel.pressing = true
			sel.startX = sel.curX
			sel.startY = sel.curY
		case button == btnLeft && state == 0: // left released
			if sel.pressing && sel.activeSurf >= 0 {
				sel.pressing = false
				ss := sel.surfs[sel.activeSurf]
				x0 := minI(sel.startX, sel.curX)
				y0 := minI(sel.startY, sel.curY)
				x1 := maxI(sel.startX, sel.curX)
				y1 := maxI(sel.startY, sel.curY)
				if x1 > x0 && y1 > y0 {
					sel.result = image.Rect(
						ss.outLogX+x0, ss.outLogY+y0,
						ss.outLogX+x1, ss.outLogY+y1,
					)
					sel.done = true
				}
			}
		case button == btnRight && state == 1:
			sel.cancelled = true
		}
	}
}

// keyboardDispatcher handles wl_keyboard events.
type keyboardDispatcher struct {
	sel *selState
}

func (k *keyboardDispatcher) dispatch(opcode uint16, data []byte, fd int) {
	switch opcode {
	case 0: // keymap(format, fd, size) — close the fd, we don't need the keymap
		if fd >= 0 {
			unix.Close(fd)
		}
	case 3: // key(serial, time, key, state)
		if len(data) < 16 {
			return
		}
		key := readUint32(data, 8)
		state := readUint32(data, 12)
		if key == 1 && state == 1 { // KEY_ESC pressed
			k.sel.cancelled = true
		}
	default:
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}

// createSelSurf creates a fullscreen OVERLAY layer surface for one output.
// Blocks until the compositor sends the configure event.
func createSelSurf(wl *conn, compID, lsID, shmID, outID uint32, out *outputGeom, idx int) (*selSurf, error) {
	// wl_compositor.create_surface — opcode 0
	surfID := wl.alloc()
	{
		args := encodeUint32(surfID)
		if err := wl.send(compID, 0, args, -1); err != nil {
			return nil, err
		}
		wl.register(surfID, &nullDispatcher{})
	}

	// zwlr_layer_shell_v1.get_layer_surface — opcode 0
	// args: new_id, wl_surface, wl_output, layer(uint), namespace(string)
	lsurfID := wl.alloc()
	ls := &lsConfigureDispatcher{}
	wl.register(lsurfID, ls)
	{
		ns := encodeString("screenshotter-select")
		args := make([]byte, 16+len(ns))
		off := 0
		off = putU32(args, off, lsurfID)
		off = putU32(args, off, surfID)
		off = putU32(args, off, outID)
		off = putU32(args, off, 3) // OVERLAY layer
		copy(args[off:], ns)
		if err := wl.send(lsID, 0, args, -1); err != nil {
			return nil, err
		}
	}

	// set_size(0, 0) — fill to output size
	{
		args := make([]byte, 8)
		if err := wl.send(lsurfID, 0, args, -1); err != nil {
			return nil, err
		}
	}
	// set_anchor(top|bottom|left|right = 15)
	{
		args := encodeUint32(15)
		if err := wl.send(lsurfID, 1, args, -1); err != nil {
			return nil, err
		}
	}
	// set_exclusive_zone(-1) — don't displace other surfaces
	{
		args := encodeInt32(-1)
		if err := wl.send(lsurfID, 2, args, -1); err != nil {
			return nil, err
		}
	}
	// set_keyboard_interactivity(1 = exclusive) — receive ESC
	{
		args := encodeUint32(1)
		if err := wl.send(lsurfID, 4, args, -1); err != nil {
			return nil, err
		}
	}
	// initial commit — triggers configure
	if err := wl.send(surfID, 6, nil, -1); err != nil {
		return nil, err
	}

	// Wait for configure event.
	for !ls.configured {
		if err := wl.recv(); err != nil {
			return nil, fmt.Errorf("waiting for layer surface configure: %w", err)
		}
	}

	// ack_configure — opcode 6
	{
		args := encodeUint32(ls.serial)
		if err := wl.send(lsurfID, 6, args, -1); err != nil {
			return nil, err
		}
	}

	// Use configured size; fall back to output logical size if compositor sent 0.
	w, h := int(ls.width), int(ls.height)
	if w == 0 || h == 0 {
		w, h = out.w, out.h
	}
	stride := w * 4
	shmSize := stride * h

	fd, err := shmCreate(shmSize)
	if err != nil {
		return nil, fmt.Errorf("shm create: %w", err)
	}
	pixData, err := mmap(fd, shmSize)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mmap: %w", err)
	}

	// wl_shm.create_pool(new_id, fd, size) — opcode 0
	poolID := wl.alloc()
	{
		args := make([]byte, 8)
		putU32(args, 0, poolID)
		putI32(args, 4, int32(shmSize))
		if err := wl.send(shmID, 0, args, fd); err != nil {
			unix.Close(fd)
			munmap(pixData)
			return nil, fmt.Errorf("create_pool: %w", err)
		}
		wl.register(poolID, &nullDispatcher{})
	}
	unix.Close(fd)

	// wl_shm_pool.create_buffer(new_id, 0, w, h, stride, ARGB8888) — opcode 0
	bufID := wl.alloc()
	buf := &selBuf{id: bufID, data: pixData}
	{
		args := make([]byte, 24)
		off := 0
		off = putU32(args, off, bufID)
		off = putI32(args, off, 0) // offset
		off = putI32(args, off, int32(w))
		off = putI32(args, off, int32(h))
		off = putI32(args, off, int32(stride))
		putU32(args, off, 0) // ARGB8888
		if err := wl.send(poolID, 0, args, -1); err != nil {
			munmap(pixData)
			return nil, fmt.Errorf("create_buffer: %w", err)
		}
		wl.register(bufID, buf)
	}

	ss := &selSurf{
		outputIdx: idx,
		outLogX:   out.x,
		outLogY:   out.y,
		surfID:    surfID,
		lsurfID:   lsurfID,
		poolID:    poolID,
		buf:       buf,
		w:         w,
		h:         h,
		stride:    stride,
	}

	// Commit the surface immediately with the initial overlay so the compositor
	// presents a frame without waiting for the first user interaction.
	ss.drawOverlay(0, 0, 0, 0, false)
	ss.commit(wl)

	return ss, nil
}

// selectRegionWayland opens an interactive region selector using a Wayland
// layer-shell overlay. The user drags a rectangle with the left mouse button.
// ESC or right-click cancels. Returns the selection in global logical coordinates.
func selectRegionWayland(outputs []*outputGeom) (image.Rectangle, error) {
	wl, err := connect()
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("wayland connect: %w", err)
	}
	defer wl.close()

	reg, err := getRegistry(wl)
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("get registry: %w", err)
	}

	// Bind wl_compositor
	compG, ok := reg.findGlobal("wl_compositor")
	if !ok {
		return image.Rectangle{}, fmt.Errorf("wl_compositor not found")
	}
	compID, err := reg.bind(compG.name, "wl_compositor", 4)
	if err != nil {
		return image.Rectangle{}, err
	}
	wl.register(compID, &nullDispatcher{})

	// Bind wl_shm
	shmG, ok := reg.findGlobal("wl_shm")
	if !ok {
		return image.Rectangle{}, fmt.Errorf("wl_shm not found")
	}
	shmID, err := reg.bind(shmG.name, "wl_shm", 1)
	if err != nil {
		return image.Rectangle{}, err
	}
	wl.register(shmID, &nullDispatcher{})

	// Bind zwlr_layer_shell_v1
	lsG, ok := reg.findGlobal("zwlr_layer_shell_v1")
	if !ok {
		return image.Rectangle{}, fmt.Errorf("zwlr_layer_shell_v1 not found — compositor must support wlr-layer-shell")
	}
	lsID, err := reg.bind(lsG.name, "zwlr_layer_shell_v1", 4)
	if err != nil {
		return image.Rectangle{}, err
	}
	wl.register(lsID, &nullDispatcher{})

	// Bind wl_seat
	seatG, ok := reg.findGlobal("wl_seat")
	if !ok {
		return image.Rectangle{}, fmt.Errorf("wl_seat not found")
	}
	seatID, err := reg.bind(seatG.name, "wl_seat", 5)
	if err != nil {
		return image.Rectangle{}, err
	}
	wl.register(seatID, &nullDispatcher{})

	sel := &selState{
		wl:         wl,
		activeSurf: -1,
		cursorHot:  cursorHot,
	}

	// Create a layer surface for each output.
	for i, out := range outputs {
		outID, err := reg.bind(out.regName, "wl_output", 4)
		if err != nil {
			continue
		}
		wl.register(outID, &nullDispatcher{})

		ss, err := createSelSurf(wl, compID, lsID, shmID, outID, out, i)
		if err != nil {
			continue
		}
		sel.surfs = append(sel.surfs, ss)
	}

	if len(sel.surfs) == 0 {
		return image.Rectangle{}, fmt.Errorf("no selection surfaces created")
	}

	// Create the crosshair cursor surface.
	cursorSurfID, cursorPoolID, cursorBufID, cursorData, err := createCursorSurf(wl, compID, shmID)
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("create cursor: %w", err)
	}
	sel.cursorSurfID = cursorSurfID

	// Set up pointer input.
	ptrID := wl.alloc()
	sel.ptrID = ptrID
	{
		args := encodeUint32(ptrID)
		wl.send(seatID, 0, args, -1) //nolint:errcheck — wl_seat.get_pointer
	}
	wl.register(ptrID, &pointerDispatcher{sel: sel})

	// Set up keyboard input (for ESC).
	kbdID := wl.alloc()
	{
		args := encodeUint32(kbdID)
		wl.send(seatID, 1, args, -1) //nolint:errcheck — wl_seat.get_keyboard
	}
	wl.register(kbdID, &keyboardDispatcher{sel: sel})

	// Flush all requests.
	if err := wl.roundtrip(); err != nil {
		return image.Rectangle{}, fmt.Errorf("initial roundtrip: %w", err)
	}

	// Event loop: process input until done or cancelled.
	for !sel.done && !sel.cancelled {
		if err := wl.recv(); err != nil {
			return image.Rectangle{}, fmt.Errorf("wayland recv: %w", err)
		}
	}

	// Destroy all surfaces before returning.
	for _, ss := range sel.surfs {
		wl.send(ss.lsurfID, 7, nil, -1) //nolint:errcheck — zwlr_layer_surface_v1.destroy
		wl.send(ss.surfID, 0, nil, -1)  //nolint:errcheck — wl_surface.destroy
		wl.send(ss.buf.id, 0, nil, -1)  //nolint:errcheck — wl_buffer.destroy
		wl.send(ss.poolID, 1, nil, -1)  //nolint:errcheck — wl_shm_pool.destroy
		munmap(ss.buf.data)
	}
	wl.send(cursorSurfID, 0, nil, -1) //nolint:errcheck — wl_surface.destroy
	wl.send(cursorBufID, 0, nil, -1)  //nolint:errcheck — wl_buffer.destroy
	wl.send(cursorPoolID, 1, nil, -1) //nolint:errcheck — wl_shm_pool.destroy
	munmap(cursorData)
	wl.roundtrip() //nolint:errcheck

	if sel.cancelled {
		return image.Rectangle{}, errCancelled
	}
	return sel.result, nil
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const (
	cursorSize = 24
	cursorHot  = 11 // hotspot at center pixel
)

// createCursorSurf creates a wl_surface with a crosshair cursor image and
// returns (surfID, poolID, bufID, munmapFn). The caller is responsible for
// destroying the surface, pool, and buffer, and calling the munmap function.
func createCursorSurf(wl *conn, compID, shmID uint32) (surfID, poolID, bufID uint32, pixData []byte, err error) {
	stride := cursorSize * 4
	shmSize := stride * cursorSize

	// Draw a white crosshair with a 1px black outline.
	pixels := make([]byte, shmSize)
	for y := 0; y < cursorSize; y++ {
		for x := 0; x < cursorSize; x++ {
			di := y*stride + x*4
			onCross := x == cursorHot || y == cursorHot
			if onCross {
				// gruvbox bright orange #fe8019
				pixels[di+0] = 0x19 // B
				pixels[di+1] = 0x80 // G
				pixels[di+2] = 0xfe // R
				pixels[di+3] = 0xff // A
			}
			// outline pixels stay transparent so the selection border shows through
		}
	}

	fd, err := shmCreate(shmSize)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	data, err := mmap(fd, shmSize)
	if err != nil {
		unix.Close(fd)
		return 0, 0, 0, nil, err
	}
	copy(data, pixels)

	poolID = wl.alloc()
	{
		args := make([]byte, 8)
		putU32(args, 0, poolID)
		putI32(args, 4, int32(shmSize))
		if err := wl.send(shmID, 0, args, fd); err != nil {
			unix.Close(fd)
			munmap(data)
			return 0, 0, 0, nil, err
		}
		wl.register(poolID, &nullDispatcher{})
	}
	unix.Close(fd)

	bufID = wl.alloc()
	{
		args := make([]byte, 24)
		off := 0
		off = putU32(args, off, bufID)
		off = putI32(args, off, 0)
		off = putI32(args, off, cursorSize)
		off = putI32(args, off, cursorSize)
		off = putI32(args, off, int32(stride))
		putU32(args, off, 0) // ARGB8888
		if err := wl.send(poolID, 0, args, -1); err != nil {
			munmap(data)
			return 0, 0, 0, nil, err
		}
		wl.register(bufID, &nullDispatcher{})
	}

	surfID = wl.alloc()
	{
		args := encodeUint32(surfID)
		if err := wl.send(compID, 0, args, -1); err != nil {
			munmap(data)
			return 0, 0, 0, nil, err
		}
		wl.register(surfID, &nullDispatcher{})
	}

	// attach buffer and commit the cursor surface
	{
		args := make([]byte, 12)
		putU32(args, 0, bufID)
		wl.send(surfID, 1, args, -1) //nolint:errcheck — wl_surface.attach
	}
	{
		args := make([]byte, 16)
		putI32(args, 8, cursorSize)
		putI32(args, 12, cursorSize)
		wl.send(surfID, 9, args, -1) //nolint:errcheck — wl_surface.damage_buffer
	}
	wl.send(surfID, 6, nil, -1) //nolint:errcheck — wl_surface.commit

	return surfID, poolID, bufID, data, nil
}
