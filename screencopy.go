package main

// zwlr_screencopy_manager_v1 implementation.
//
// Protocol summary:
//   manager.capture_output(frame, overlay_cursor, output) -> zwlr_screencopy_frame_v1
//   manager.capture_output_region(frame, overlay_cursor, output, x, y, w, h) -> zwlr_screencopy_frame_v1
//
//   frame events:
//     0: buffer(format, width, height, stride)
//     1: flags(flags)      -- bit 0 = y_invert
//     2: ready(tv_sec_hi, tv_sec_lo, tv_nsec)
//     3: failed
//     4: damage(x, y, w, h)    -- v2
//     5: linux_dmabuf(...)     -- v3
//     6: buffer_done           -- v3 (signals all buffer events sent)
//
//   frame requests:
//     0: copy(buffer)
//     1: destroy
//     2: copy_with_damage(buffer)  -- v2
//
// wl_output is just a bound global; we don't need to handle its events for capture.
// wl_shm / wl_shm_pool / wl_buffer are used to provide the backing buffer.

import (
	"fmt"
	"image"
	"image/color"

	"golang.org/x/sys/unix"
)

// wl_shm formats we handle.
const (
	wlShmFormatARGB8888 = 0          // B G R A in memory (little-endian)
	wlShmFormatXRGB8888 = 1          // B G R X in memory (little-endian)
	wlShmFormatXBGR8888 = 0x34324258 // R G B X in memory
	wlShmFormatABGR8888 = 0x34324241 // R G B A in memory
)

// screenshot captures the given wl_output (by its registry name) and returns an image.
// If region is non-nil, only that sub-rectangle is captured.
func screenshot(wl *conn, reg *registry, outputName uint32, region *image.Rectangle) (*image.RGBA, error) {
	// --- Bind wl_output ---
	outputID, err := reg.bind(outputName, "wl_output", 4)
	if err != nil {
		return nil, fmt.Errorf("bind wl_output: %w", err)
	}
	// wl_output sends geometry/mode/done events; we just ignore them for capture.
	wl.register(outputID, &nullDispatcher{})

	// --- Bind zwlr_screencopy_manager_v1 ---
	mgr, ok := reg.findGlobal("zwlr_screencopy_manager_v1")
	if !ok {
		return nil, fmt.Errorf("compositor does not support zwlr_screencopy_manager_v1")
	}
	mgrID, err := reg.bind(mgr.name, "zwlr_screencopy_manager_v1", 3)
	if err != nil {
		return nil, fmt.Errorf("bind zwlr_screencopy_manager_v1: %w", err)
	}

	// --- Bind wl_shm ---
	shmGlobal, ok := reg.findGlobal("wl_shm")
	if !ok {
		return nil, fmt.Errorf("compositor does not advertise wl_shm")
	}
	shmID, err := reg.bind(shmGlobal.name, "wl_shm", 1)
	if err != nil {
		return nil, fmt.Errorf("bind wl_shm: %w", err)
	}
	wl.register(shmID, &nullDispatcher{}) // format events — we don't need them

	// --- Create screencopy frame ---
	frameID := wl.alloc()
	frame := &screencopyFrame{}
	wl.register(frameID, frame)

	if region != nil {
		// capture_output_region: opcode 1 on manager
		// args: frame(new_id), overlay_cursor(int), output(object), x,y,w,h (int)
		// region must be in output-local coordinates.
		args := make([]byte, 7*4)
		off := 0
		off = putU32(args, off, frameID)
		off = putU32(args, off, 0) // no cursor overlay
		off = putU32(args, off, outputID)
		off = putI32(args, off, int32(region.Min.X))
		off = putI32(args, off, int32(region.Min.Y))
		off = putI32(args, off, int32(region.Dx()))
		_ = putI32(args, off, int32(region.Dy()))
		if err := wl.send(mgrID, 1, args, -1); err != nil {
			return nil, fmt.Errorf("capture_output_region: %w", err)
		}
	} else {
		// capture_output: opcode 0 on manager
		// args: frame(new_id), overlay_cursor(int), output(object)
		args := make([]byte, 3*4)
		off := 0
		off = putU32(args, off, frameID)
		off = putU32(args, off, 0) // no cursor overlay
		_ = putU32(args, off, outputID)
		if err := wl.send(mgrID, 0, args, -1); err != nil {
			return nil, fmt.Errorf("capture_output: %w", err)
		}
	}

	// --- Wait for buffer event (tells us the required shm format/size) ---
	// With protocol v3 we wait for buffer_done; with v1/v2 just buffer is enough.
	// We loop until we have the buffer info.
	useV3 := mgr.version >= 3
	if useV3 {
		for !frame.bufferDone {
			if err := wl.recv(); err != nil {
				return nil, fmt.Errorf("waiting for buffer_done: %w", err)
			}
			if frame.failed {
				return nil, fmt.Errorf("screencopy frame failed before buffer_done")
			}
		}
	} else {
		for !frame.hasBuffer {
			if err := wl.recv(); err != nil {
				return nil, fmt.Errorf("waiting for buffer event: %w", err)
			}
			if frame.failed {
				return nil, fmt.Errorf("screencopy frame failed")
			}
		}
	}

	switch frame.format {
	case wlShmFormatARGB8888, wlShmFormatXRGB8888, wlShmFormatXBGR8888, wlShmFormatABGR8888:
		// handled below
	default:
		return nil, fmt.Errorf("unsupported shm format: %d (0x%x)", frame.format, frame.format)
	}

	w, h, stride := int(frame.width), int(frame.height), int(frame.stride)
	shmSize := stride * h

	// --- Create shm pool and buffer ---
	fd, err := shmCreate(shmSize)
	if err != nil {
		return nil, fmt.Errorf("shm create: %w", err)
	}
	defer unix.Close(fd)

	data, err := mmap(fd, shmSize)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	defer munmap(data)

	// wl_shm.create_pool(new_id, fd, size) — opcode 0
	poolID := wl.alloc()
	{
		args := make([]byte, 8) // new_id + size; fd goes OOB
		putU32(args, 0, poolID)
		putI32(args, 4, int32(shmSize))
		if err := wl.send(shmID, 0, args, fd); err != nil {
			return nil, fmt.Errorf("wl_shm.create_pool: %w", err)
		}
		wl.register(poolID, &nullDispatcher{})
	}

	// wl_shm_pool.create_buffer(new_id, offset, width, height, stride, format) — opcode 0
	bufID := wl.alloc()
	{
		args := make([]byte, 6*4)
		off := 0
		off = putU32(args, off, bufID)
		off = putI32(args, off, 0) // offset
		off = putI32(args, off, int32(w))
		off = putI32(args, off, int32(h))
		off = putI32(args, off, int32(stride))
		_ = putU32(args, off, frame.format)
		if err := wl.send(poolID, 0, args, -1); err != nil {
			return nil, fmt.Errorf("wl_shm_pool.create_buffer: %w", err)
		}
		wl.register(bufID, &nullDispatcher{})
	}

	// wl_shm_pool.destroy — opcode 1 (pool no longer needed once buffer is created)
	if err := wl.send(poolID, 1, nil, -1); err != nil {
		return nil, fmt.Errorf("wl_shm_pool.destroy: %w", err)
	}

	// --- Copy frame into buffer ---
	// zwlr_screencopy_frame_v1.copy(buffer) — opcode 0
	{
		args := encodeUint32(bufID)
		if err := wl.send(frameID, 0, args, -1); err != nil {
			return nil, fmt.Errorf("screencopy_frame.copy: %w", err)
		}
	}

	// --- Wait for ready or failed ---
	for !frame.ready && !frame.failed {
		if err := wl.recv(); err != nil {
			return nil, fmt.Errorf("waiting for frame ready: %w", err)
		}
	}
	if frame.failed {
		return nil, fmt.Errorf("screencopy frame capture failed")
	}

	// --- Convert shm buffer to image.RGBA ---
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	yInvert := frame.flags&1 != 0

	for y := 0; y < h; y++ {
		srcRow := y
		if yInvert {
			srcRow = h - 1 - y
		}
		for x := 0; x < w; x++ {
			off := srcRow*stride + x*4
			var r, g, b byte
			switch frame.format {
			case wlShmFormatARGB8888, wlShmFormatXRGB8888:
				// Memory layout: B G R X/A
				b = data[off+0]
				g = data[off+1]
				r = data[off+2]
			case wlShmFormatABGR8888, wlShmFormatXBGR8888:
				// Memory layout: R G B X/A
				r = data[off+0]
				g = data[off+1]
				b = data[off+2]
			}
			// Compositors routinely return alpha=0 in screencopy buffers even
			// for ARGB8888 — always treat as fully opaque for screenshots.
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	// --- Destroy frame and buffer ---
	wl.send(frameID, 1, nil, -1) // zwlr_screencopy_frame_v1.destroy
	wl.send(bufID, 0, nil, -1)   // wl_buffer.destroy opcode 0

	return img, nil
}

// screencopyFrame holds state accumulated from frame events.
type screencopyFrame struct {
	// from buffer event
	hasBuffer bool
	format    uint32
	width     uint32
	height    uint32
	stride    uint32
	// from buffer_done event (v3)
	bufferDone bool
	// from flags event
	flags uint32
	// terminal events
	ready  bool
	failed bool
}

func (f *screencopyFrame) dispatch(opcode uint16, data []byte, _ int) {
	switch opcode {
	case 0: // buffer(format, width, height, stride)
		if len(data) < 16 {
			return
		}
		f.format = readUint32(data, 0)
		f.width = readUint32(data, 4)
		f.height = readUint32(data, 8)
		f.stride = readUint32(data, 12)
		f.hasBuffer = true
	case 1: // flags
		if len(data) >= 4 {
			f.flags = readUint32(data, 0)
		}
	case 2: // ready
		f.ready = true
	case 3: // failed
		f.failed = true
	case 4: // damage (v2) — ignore
	case 5: // linux_dmabuf (v3) — ignore; we only use shm
	case 6: // buffer_done (v3)
		f.bufferDone = true
	}
}

// nullDispatcher silently drops all events.
type nullDispatcher struct{}

func (n *nullDispatcher) dispatch(_ uint16, _ []byte, fd int) {
	if fd >= 0 {
		unix.Close(fd)
	}
}

// --- Encoding helpers local to this file ---

func putU32(b []byte, off int, v uint32) int {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
	return off + 4
}

func putI32(b []byte, off int, v int32) int {
	return putU32(b, off, uint32(v))
}

// outputGeom stores the global compositor position and pixel size of a wl_output.
type outputGeom struct {
	regName uint32
	x, y    int
	w, h    int
}

func (g *outputGeom) dispatch(opcode uint16, data []byte, _ int) {
	switch opcode {
	case 0: // geometry: x, y, phys_w, phys_h, subpixel, make(str), model(str), transform
		if len(data) >= 8 {
			g.x = int(readInt32(data, 0))
			g.y = int(readInt32(data, 4))
		}
	case 1: // mode: flags, w, h, refresh
		if len(data) >= 12 && readUint32(data, 0)&1 != 0 { // bit 0 = current mode
			g.w = int(readInt32(data, 4))
			g.h = int(readInt32(data, 8))
		}
	}
}

// xdgOutputDispatcher collects logical_position and logical_size from zxdg_output_v1.
// slurp reports regions in xdg-output logical coordinates, so this is what we need.
type xdgOutputDispatcher struct {
	geom *outputGeom
}

func (d *xdgOutputDispatcher) dispatch(opcode uint16, data []byte, _ int) {
	switch opcode {
	case 0: // logical_position(x, y)
		if len(data) >= 8 {
			d.geom.x = int(readInt32(data, 0))
			d.geom.y = int(readInt32(data, 4))
		}
	case 1: // logical_size(w, h)
		if len(data) >= 8 {
			d.geom.w = int(readInt32(data, 0))
			d.geom.h = int(readInt32(data, 4))
		}
	}
}

// gatherOutputGeoms collects the logical position and size of every wl_output.
// It uses zxdg_output_manager_v1 when available (same coordinate space as slurp),
// falling back to wl_output.geometry.
func gatherOutputGeoms(wl *conn, reg *registry) ([]*outputGeom, error) {
	var wlOutputs []registryGlobal
	for _, g := range reg.globals {
		if g.iface == "wl_output" {
			wlOutputs = append(wlOutputs, g)
		}
	}
	if len(wlOutputs) == 0 {
		return nil, fmt.Errorf("no wl_output globals advertised")
	}

	var geoms []*outputGeom

	xdgMgr, hasXdg := reg.findGlobal("zxdg_output_manager_v1")
	if hasXdg {
		xdgMgrID, err := reg.bind(xdgMgr.name, "zxdg_output_manager_v1", 2)
		if err != nil {
			return nil, fmt.Errorf("bind zxdg_output_manager_v1: %w", err)
		}
		wl.register(xdgMgrID, &nullDispatcher{})

		for _, g := range wlOutputs {
			outID, err := reg.bind(g.name, "wl_output", 2)
			if err != nil {
				return nil, fmt.Errorf("bind wl_output: %w", err)
			}
			wl.register(outID, &nullDispatcher{})

			// zxdg_output_manager_v1.get_xdg_output: opcode 1, args: new_id + wl_output
			// (opcode 0 is destroy)
			xdgOutID := wl.alloc()
			args := make([]byte, 8)
			putU32(args, 0, xdgOutID)
			putU32(args, 4, outID)
			if err := wl.send(xdgMgrID, 1, args, -1); err != nil {
				return nil, fmt.Errorf("get_xdg_output: %w", err)
			}

			geom := &outputGeom{regName: g.name}
			geoms = append(geoms, geom)
			wl.register(xdgOutID, &xdgOutputDispatcher{geom: geom})
		}
	} else {
		// Fall back to wl_output.geometry for x, y and wl_output.mode for w, h.
		for _, g := range wlOutputs {
			id, err := reg.bind(g.name, "wl_output", 2)
			if err != nil {
				return nil, fmt.Errorf("bind wl_output: %w", err)
			}
			geom := &outputGeom{regName: g.name}
			geoms = append(geoms, geom)
			wl.register(id, geom)
		}
	}

	if err := wl.roundtrip(); err != nil {
		return nil, fmt.Errorf("gather output geoms: %w", err)
	}
	return geoms, nil
}
