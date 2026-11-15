//go:build !windows

// A stand-in for the screen, the mouse and the hands, so that everything else can be run and tested on a machine
// with no Windows on it. The screen is a PNG; a test presses the mouse and reads back what the hands were told to do.
//
//	/sim/screen?png=path     put this picture on the stand-in screen
//	/sim/down?x=&y=&vk=1&ms= press a key or mouse button (vk 1 = left) at x,y for ms
//	/sim/title?t=            the window title reported under the cursor
//	/sim/log                 every click the hands were asked for; /sim/log?clear=1 empties it
package main

import (
	"encoding/json"
	"image"
	"image/draw"
	_ "image/png"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var sim struct {
	sync.Mutex
	w, h  int
	px    []byte
	x, y  int
	until map[int]time.Time
	title string
	log   []map[string]any
}

func simLoad(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	n := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(n, n.Bounds(), img, b.Min, draw.Src)
	px := make([]byte, len(n.Pix))
	for i := 0; i < len(px); i += 4 { // RGBA -> BGRA, as GDI would hand it over
		px[i], px[i+1], px[i+2], px[i+3] = n.Pix[i+2], n.Pix[i+1], n.Pix[i], 0
	}
	sim.Lock()
	sim.w, sim.h, sim.px = b.Dx(), b.Dy(), px
	sim.Unlock()
	return nil
}

func platInit() {
	sim.until = map[int]time.Time{}
	sim.title = "Sim Window"
	sim.w, sim.h, sim.px = 640, 360, make([]byte, 640*360*4)
	if p := os.Getenv("HUELLA_SIM"); p != "" {
		if err := simLoad(p); err != nil {
			log.Fatalf("sim screen %s: %v", p, err)
		}
	}
}

func screenSize() (int, int) { sim.Lock(); defer sim.Unlock(); return sim.w, sim.h }
func cursorPos() (int, int)  { sim.Lock(); defer sim.Unlock(); return sim.x, sim.y }

func keyDown(vk int) bool {
	sim.Lock()
	defer sim.Unlock()
	return time.Now().Before(sim.until[vk])
}

func windowTitleAt(x, y int) string { sim.Lock(); defer sim.Unlock(); return sim.title }

func captureInto(buf []byte, w, h int) bool {
	sim.Lock()
	defer sim.Unlock()
	if w != sim.w || h != sim.h || len(buf) != len(sim.px) {
		return false
	}
	copy(buf, sim.px)
	return true
}

func injectClick(x, y int, button string, count int) {
	sim.Lock()
	sim.x, sim.y = x, y
	sim.log = append(sim.log, map[string]any{"x": x, "y": y, "button": button, "count": count})
	sim.Unlock()
}

func openBrowser(url string) {}

func platRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/sim/screen", func(w http.ResponseWriter, r *http.Request) {
		if err := simLoad(r.URL.Query().Get("png")); err != nil {
			http.Error(w, err.Error(), 400)
		}
	})
	mux.HandleFunc("/sim/down", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		vk, _ := strconv.Atoi(q.Get("vk"))
		ms, _ := strconv.Atoi(q.Get("ms"))
		if ms <= 0 {
			ms = 80
		}
		sim.Lock()
		if q.Get("x") != "" {
			sim.x, _ = strconv.Atoi(q.Get("x"))
			sim.y, _ = strconv.Atoi(q.Get("y"))
		}
		sim.until[vk] = time.Now().Add(time.Duration(ms) * time.Millisecond)
		sim.Unlock()
	})
	mux.HandleFunc("/sim/title", func(w http.ResponseWriter, r *http.Request) {
		sim.Lock()
		sim.title = r.URL.Query().Get("t")
		sim.Unlock()
	})
	mux.HandleFunc("/sim/log", func(w http.ResponseWriter, r *http.Request) {
		sim.Lock()
		b, _ := json.Marshal(sim.log)
		if r.URL.Query().Get("clear") != "" {
			sim.log = nil
		}
		sim.Unlock()
		if b == nil || string(b) == "null" {
			b = []byte("[]")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
}
