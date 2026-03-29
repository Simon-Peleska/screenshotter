package main

// zwlr_data_control_manager_v1 clipboard implementation.
//
// This protocol lets privileged clients (like screenshot tools) set the
// clipboard without needing keyboard focus or a serial.
//
// Protocol flow for setting clipboard:
//   1. Bind zwlr_data_control_manager_v1 from registry.
//   2. Bind wl_seat from registry.
//   3. manager.get_data_device(seat) -> zwlr_data_control_device_v1
//   4. manager.create_data_source() -> zwlr_data_control_source_v1
//   5. source.offer("text/plain;charset=utf-8")
//   6. device.set_selection(source)
//   7. Roundtrip.
//   8. Wait for source.send events, writing data to the provided fd.
//   9. Keep the process alive / event loop running while the source is owned.
//      When cancelled, we're done.
//
// For a screenshot tool, we serve the clipboard synchronously: we enter
// the event loop and respond to send events until the source is cancelled
// (i.e. someone else owns the clipboard).

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"

	"golang.org/x/sys/unix"
)

const (
	mimeTypePlain = "text/plain;charset=utf-8"
	mimeTypePNG   = "image/png"
)

// setClipboard puts text into the Wayland clipboard.
func setClipboard(text string) error {
	return setClipboardData(mimeTypePlain, []byte(text))
}

// setClipboardImage puts a PNG-encoded image into the Wayland clipboard.
func setClipboardImage(img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return fmt.Errorf("encode PNG for clipboard: %w", err)
	}
	return setClipboardData(mimeTypePNG, buf.Bytes())
}

// setClipboardData puts arbitrary data into the Wayland clipboard using
// zwlr_data_control_manager_v1. It blocks until another client takes ownership.
func setClipboardData(mimeType string, data []byte) error {
	wl, err := connect()
	if err != nil {
		return err
	}
	defer wl.close()

	reg, err := getRegistry(wl)
	if err != nil {
		return err
	}

	// --- Bind zwlr_data_control_manager_v1 ---
	mgrGlobal, ok := reg.findGlobal("zwlr_data_control_manager_v1")
	if !ok {
		return fmt.Errorf("compositor does not support zwlr_data_control_manager_v1 (try wl-copy as fallback)")
	}
	mgrID, err := reg.bind(mgrGlobal.name, "zwlr_data_control_manager_v1", 1)
	if err != nil {
		return fmt.Errorf("bind zwlr_data_control_manager_v1: %w", err)
	}
	wl.register(mgrID, &nullDispatcher{})

	// --- Bind wl_seat ---
	seatGlobal, ok := reg.findGlobal("wl_seat")
	if !ok {
		return fmt.Errorf("no wl_seat advertised")
	}
	seatID, err := reg.bind(seatGlobal.name, "wl_seat", 1)
	if err != nil {
		return fmt.Errorf("bind wl_seat: %w", err)
	}
	wl.register(seatID, &nullDispatcher{})

	// --- get_data_device(seat) — opcode 1 on manager ---
	deviceID := wl.alloc()
	{
		args := make([]byte, 8)
		putU32(args, 0, deviceID)
		putU32(args, 4, seatID)
		if err := wl.send(mgrID, 1, args, -1); err != nil {
			return fmt.Errorf("get_data_device: %w", err)
		}
	}
	device := &dataControlDevice{}
	wl.register(deviceID, device)

	// --- create_data_source() — opcode 0 on manager ---
	sourceID := wl.alloc()
	{
		args := encodeUint32(sourceID)
		if err := wl.send(mgrID, 0, args, -1); err != nil {
			return fmt.Errorf("create_data_source: %w", err)
		}
	}
	source := &dataControlSource{data: data}
	wl.register(sourceID, source)

	// --- source.offer(mime) — opcode 0 on source ---
	{
		args := encodeString(mimeType)
		if err := wl.send(sourceID, 0, args, -1); err != nil {
			return fmt.Errorf("source.offer: %w", err)
		}
	}

	// --- device.set_selection(source) — opcode 0 on device ---
	{
		args := encodeUint32(sourceID)
		if err := wl.send(deviceID, 0, args, -1); err != nil {
			return fmt.Errorf("set_selection: %w", err)
		}
	}

	// Roundtrip to flush.
	if err := wl.roundtrip(); err != nil {
		return fmt.Errorf("roundtrip after set_selection: %w", err)
	}

	// --- Serve send events until pasted or cancelled ---
	for !source.cancelled {
		if err := wl.recv(); err != nil {
			// Connection closed — clipboard is gone.
			break
		}
	}

	return nil
}

// dataControlDevice handles events from zwlr_data_control_device_v1.
// We don't need to act on them for our use case, but we must not ignore fds.
type dataControlDevice struct{}

func (d *dataControlDevice) dispatch(_ uint16, data []byte, fd int) {
	// Offer events send a new_id for a data offer object — we just ignore it.
	// Close any fd we receive.
	if fd >= 0 {
		unix.Close(fd)
	}
	_ = data
}

// dataControlSource handles events from zwlr_data_control_source_v1.
//
// Events:
//
//	0: send(mime_type string, fd fd)  — write data to fd
//	1: cancelled                       — source is no longer the selection
type dataControlSource struct {
	data      []byte
	cancelled bool
}

func (s *dataControlSource) dispatch(opcode uint16, data []byte, fd int) {
	switch opcode {
	case 0: // send(mime_type, fd)
		if fd < 0 {
			return
		}
		f := os.NewFile(uintptr(fd), "clipboard-pipe")
		f.Write(s.data)
		f.Close()
	case 1: // cancelled
		s.cancelled = true
	default:
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}
