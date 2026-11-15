//go:build windows

// The Windows side: user32.dll and gdi32.dll called directly. No CGo, nothing to install.
// Built for amd64 (a POINT passed by value is packed into one 64-bit argument below).
package main

import (
	"encoding/binary"
	"net/http"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	gdi32  = syscall.NewLazyDLL("gdi32.dll")

	pSendInput        = user32.NewProc("SendInput")
	pGetCursorPos     = user32.NewProc("GetCursorPos")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	pGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	pGetDC            = user32.NewProc("GetDC")
	pReleaseDC        = user32.NewProc("ReleaseDC")
	pWindowFromPoint  = user32.NewProc("WindowFromPoint")
	pGetAncestor      = user32.NewProc("GetAncestor")
	pGetWindowTextW   = user32.NewProc("GetWindowTextW")
	pSetDpiContext    = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetDpiAware      = user32.NewProc("SetProcessDPIAware")

	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pGetDIBits              = gdi32.NewProc("GetDIBits")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
)

// Before anything else: say this process counts in real pixels. Without it, on a display scaled to 150% or 200%,
// the screen size, the cursor position and the capture would all be in scaled-down pixels that match nothing.
func platInit() {
	if pSetDpiContext.Find() == nil {
		if r, _, _ := pSetDpiContext.Call(^uintptr(3)); r != 0 { // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
			return
		}
	}
	if pSetDpiAware.Find() == nil {
		pSetDpiAware.Call()
	}
}

func metric(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

// the primary screen, in real pixels
func screenSize() (int, int) { return metric(0), metric(1) }

func cursorPos() (int, int) {
	var p [2]int32
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return int(p[0]), int(p[1])
}

// down now, or pressed and let go since the last look (so a very short tap is not missed)
func keyDown(vk int) bool {
	r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
	return uint16(r)&0x8001 != 0
}

// the title of the top-level window under a point
func windowTitleAt(x, y int) string {
	pt := uintptr(uint32(int32(x))) | uintptr(uint32(int32(y)))<<32
	h, _, _ := pWindowFromPoint.Call(pt)
	if h == 0 {
		return ""
	}
	if root, _, _ := pGetAncestor.Call(h, 2); root != 0 { // GA_ROOT
		h = root
	}
	var buf [256]uint16
	n, _, _ := pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n > uintptr(len(buf)) {
		n = uintptr(len(buf))
	}
	return syscall.UTF16ToString(buf[:n])
}

/* ---- camera: BitBlt the screen into a bitmap, GetDIBits it out as 32-bit rows, top row first ---- */

var cam struct {
	screen, mem, bmp uintptr
	w, h             int
}

func camFree() {
	if cam.bmp != 0 {
		pDeleteObject.Call(cam.bmp)
	}
	if cam.mem != 0 {
		pDeleteDC.Call(cam.mem)
	}
	if cam.screen != 0 {
		pReleaseDC.Call(0, cam.screen)
	}
	cam.screen, cam.mem, cam.bmp, cam.w, cam.h = 0, 0, 0, 0, 0
}

// Called from the camera's own thread only. buf is w*h*4 bytes and comes back as BGRA.
func captureInto(buf []byte, w, h int) bool {
	if cam.mem == 0 || cam.w != w || cam.h != h {
		camFree()
		cam.screen, _, _ = pGetDC.Call(0)
		if cam.screen == 0 {
			return false
		}
		cam.mem, _, _ = pCreateCompatibleDC.Call(cam.screen)
		cam.bmp, _, _ = pCreateCompatibleBitmap.Call(cam.screen, uintptr(w), uintptr(h))
		if cam.mem == 0 || cam.bmp == 0 {
			camFree()
			return false
		}
		cam.w, cam.h = w, h
	}
	old, _, _ := pSelectObject.Call(cam.mem, cam.bmp)
	ok, _, _ := pBitBlt.Call(cam.mem, 0, 0, uintptr(w), uintptr(h), cam.screen, 0, 0, 0x00CC0020|0x40000000) // SRCCOPY | CAPTUREBLT
	pSelectObject.Call(cam.mem, old)                                                                         // GetDIBits wants the bitmap out of every DC
	if ok == 0 {
		camFree()
		return false
	}
	var bi [40]byte // BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(bi[0:], 40)
	binary.LittleEndian.PutUint32(bi[4:], uint32(int32(w)))
	binary.LittleEndian.PutUint32(bi[8:], uint32(int32(-h))) // a negative height asks for the top row first
	binary.LittleEndian.PutUint16(bi[12:], 1)                // planes
	binary.LittleEndian.PutUint16(bi[14:], 32)               // bits per pixel; compression stays 0 = BI_RGB
	n, _, _ := pGetDIBits.Call(cam.screen, cam.bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi[0])), 0)
	return n != 0
}

/* ---- hands: SendInput. The INPUT struct is 40 bytes on x64 (4 type + 4 pad + 32 union), written byte by byte so
   no struct layout can go wrong: dx at 8, dy at 12, dwFlags at 20. ---- */

const (
	mfMove        = 0x0001
	mfLeftDown    = 0x0002
	mfLeftUp      = 0x0004
	mfRightDown   = 0x0008
	mfRightUp     = 0x0010
	mfVirtualDesk = 0x4000
	mfAbsolute    = 0x8000
)

func sendMouse(dx, dy int32, flags uint32) {
	var b [40]byte
	binary.LittleEndian.PutUint32(b[8:], uint32(dx))
	binary.LittleEndian.PutUint32(b[12:], uint32(dy))
	binary.LittleEndian.PutUint32(b[20:], flags)
	pSendInput.Call(1, uintptr(unsafe.Pointer(&b[0])), 40)
}

// move to a real pixel, then press and let go. Absolute moves count 0..65535 across the whole desktop.
func injectClick(x, y int, button string, count int) {
	vx, vy, vw, vh := metric(76), metric(77), metric(78), metric(79) // the virtual desktop: origin and size
	if vw <= 0 || vh <= 0 {
		vx, vy = 0, 0
		vw, vh = screenSize()
	}
	nx := int32((x-vx)*65536/vw + 1)
	ny := int32((y-vy)*65536/vh + 1)
	down, up := uint32(mfLeftDown), uint32(mfLeftUp)
	if button == "right" {
		down, up = mfRightDown, mfRightUp
	}
	sendMouse(nx, ny, mfMove|mfAbsolute|mfVirtualDesk)
	time.Sleep(60 * time.Millisecond)
	if count < 1 {
		count = 1
	}
	if count > 2 {
		count = 2
	}
	for i := 0; i < count; i++ {
		if i > 0 {
			time.Sleep(90 * time.Millisecond)
		}
		sendMouse(0, 0, down)
		time.Sleep(50 * time.Millisecond)
		sendMouse(0, 0, up)
	}
}

func openBrowser(url string) { exec.Command("cmd", "/c", "start", "", url).Start() }

func platRoutes(mux *http.ServeMux) {}
