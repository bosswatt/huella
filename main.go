// HUELLA r1 - a macro recorder that keeps WHAT each click landed on as well as WHERE.
//
// One static program, four jobs:
//
//	camera  the primary screen, about ten times a second while recording or playing.
//	        A screen that has not changed is not a new frame, so nothing is read twice.
//	ears    the mouse buttons and three hotkeys, polled. Each click is tied to the last frame taken BEFORE it.
//	hands   SendInput, and only while a run is on.
//	page    http://127.0.0.1:9191 - CADENA LINCE r3 exactly as it stands, plus huella.js, which has LINCE read
//	        the frames and answer one question per click: was that a zone I recognise?
//
// Build:  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o huella-r1.exe .
// No dependencies beyond the Go standard library. Listens on 127.0.0.1 only.
package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rev        = "r1"
	basePort   = 9191
	pageMark   = "HUELLA" // the page puts this in its window title: a click on that window is not part of the macro
	camPeriod  = 100 * time.Millisecond
	pollPeriod = 4 * time.Millisecond
	macroName  = "huella-macro.json"

	vkLButton = 0x01
	vkRButton = 0x02
	vkStop    = 0x1B // Esc  stops a run
	vkPlay    = 0x77 // F8   play / stop
	vkRec     = 0x78 // F9   record / stop
)

//go:embed cadena-lince-r3.html
var linceHTML []byte

//go:embed huella.js
var harnessJS []byte

// Step is one click of the macro: WHERE it landed, and WHAT stood there as LINCE read it.
type Step struct {
	ID     int             `json:"id"`
	Gap    int64           `json:"gap"` // ms since the step before it (or since RECORD)
	X      int             `json:"x"`
	Y      int             `json:"y"`
	Button string          `json:"button"`
	Count  int             `json:"count"`
	Screen [2]int          `json:"screen"`
	Win    string          `json:"win,omitempty"`
	Read   bool            `json:"read"`           // LINCE has had its look at this click
	What   json.RawMessage `json:"what,omitempty"` // filled in by the page
	Alt    json.RawMessage `json:"alt,omitempty"`  // a second way to find the same thing
}

type macro struct {
	Tool  string `json:"tool"`
	Rev   string `json:"rev"`
	Note  string `json:"note"`
	Steps []Step `json:"steps"`
}

type frame struct {
	id   int64
	w, h int
	px   []byte // BGRA, top row first, as GDI hands it over
}

type click struct {
	step int
	f    *frame
}

var (
	mu        sync.Mutex
	mode      = "idle" // idle | rec | play
	seq       int64    // counts every change the page should know about
	changed   = make(chan struct{})
	steps     []Step
	nextID    = 1
	recMark   time.Time // the last recorded click, or RECORD itself
	lastDown  time.Time
	pending   []click // clicks whose frame the page has not answered yet
	latest    *frame
	frameSeq  int64
	served    []int64 // the last frames sent to the page whole: it still holds what it read off them
	active    string  // the one page that is served
	known     = map[string]bool{}
	playTok   int
	playDelay int
	lastRun   = map[string]any{}
	token     string
	port      int
	indexPage []byte
	camState  = "off" // off | ok | no picture
)

func wakeLocked()  { close(changed); changed = make(chan struct{}) }
func touchLocked() { seq++; wakeLocked() }

func setModeLocked(m, why string) {
	if mode == m {
		return
	}
	prev := mode
	mode = m
	if m != "idle" {
		latest = nil // a frame from an earlier session is not this screen
	}
	if m == "rec" {
		recMark = time.Now()
	}
	if prev == "rec" {
		saveLocked()
	}
	log.Printf("%s -> %s  %s", prev, m, why)
	touchLocked()
}

func startPlayLocked(delay int, why string) bool {
	if mode != "idle" || len(steps) == 0 {
		return false
	}
	playTok++
	playDelay = delay
	lastRun = map[string]any{"state": "running"}
	setModeLocked("play", why)
	return true
}

func stopPlayLocked(msg string) {
	if mode != "play" {
		return
	}
	lastRun = map[string]any{"state": "stopped", "ok": false, "msg": msg}
	setModeLocked("idle", msg)
}

/* ---------------------------------------------------------------- camera ---------------------------------------------------------------- */

func camera() {
	runtime.LockOSThread() // the screen's device context stays with one thread
	var scratch []byte
	for {
		t0 := time.Now()
		mu.Lock()
		on := mode != "idle"
		if !on {
			camState = "off"
		}
		mu.Unlock()
		if !on {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		w, h := screenSize()
		if w <= 0 || h <= 0 {
			time.Sleep(camPeriod)
			continue
		}
		if len(scratch) != w*h*4 {
			scratch = make([]byte, w*h*4)
		}
		got := captureInto(scratch, w, h)
		mu.Lock()
		if was := camState; !got && was != "no picture" {
			log.Printf("camera: the screen could not be captured (%dx%d)", w, h)
		}
		if got {
			camState = "ok"
		} else {
			camState = "no picture"
		}
		mu.Unlock()
		if got {
			mu.Lock()
			cur := latest
			if mode != "idle" && (cur == nil || cur.w != w || cur.h != h || !bytes.Equal(cur.px, scratch)) {
				frameSeq++
				latest = &frame{id: frameSeq, w: w, h: h, px: scratch}
				scratch = nil
				wakeLocked()
			}
			mu.Unlock()
		}
		if d := camPeriod - time.Since(t0); d > 2*time.Millisecond {
			time.Sleep(d)
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

/* ---------------------------------------------------------------- ears ---------------------------------------------------------------- */

func poller() {
	keys := [...]int{vkLButton, vkRButton, vkRec, vkPlay, vkStop}
	var was [len(keys)]bool
	for _, vk := range keys { // whatever was pressed before the program started is not news
		keyDown(vk)
	}
	for {
		time.Sleep(pollPeriod)
		for i, vk := range keys {
			d := keyDown(vk)
			if d && !was[i] {
				switch vk {
				case vkLButton:
					mouseDown("left")
				case vkRButton:
					mouseDown("right")
				case vkRec:
					mu.Lock()
					if mode == "idle" {
						setModeLocked("rec", "(F9)")
					} else if mode == "rec" {
						setModeLocked("idle", "(F9)")
					}
					mu.Unlock()
				case vkPlay:
					mu.Lock()
					if mode == "idle" {
						startPlayLocked(0, "(F8)")
					} else {
						stopPlayLocked("stopped with F8")
					}
					mu.Unlock()
				case vkStop:
					mu.Lock()
					stopPlayLocked("stopped with Esc")
					mu.Unlock()
				}
			}
			was[i] = d
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func mouseDown(button string) {
	x, y := cursorPos()
	title := windowTitleAt(x, y)
	w, h := screenSize()
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if mode != "rec" || strings.Contains(title, pageMark) {
		return
	}
	if n := len(steps); n > 0 { // a second press on the same spot inside half a second is a double click
		p := &steps[n-1]
		if p.Button == button && p.Count == 1 && now.Sub(lastDown) < 500*time.Millisecond && abs(p.X-x) <= 4 && abs(p.Y-y) <= 4 {
			p.Count = 2
			lastDown = now
			touchLocked()
			return
		}
	}
	st := Step{ID: nextID, Gap: now.Sub(recMark).Milliseconds(), X: x, Y: y, Button: button, Count: 1, Screen: [2]int{w, h}, Win: title}
	nextID++
	recMark, lastDown = now, now
	// the frame that answers this click is the last one taken before it: what was on the screen when the hand decided
	if f := latest; f != nil && x >= 0 && y >= 0 && x < f.w && y < f.h && len(pending) < 8 {
		pending = append(pending, click{step: st.ID, f: f})
	} else {
		st.Read = true
	}
	steps = append(steps, st)
	log.Printf("  step %d  %s %d,%d  %s", len(steps), button, x, y, title)
	touchLocked()
}

/* ---------------------------------------------------------------- the macro on disk ---------------------------------------------------------------- */

func macroPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), macroName)
	}
	return macroName
}

func macroJSONLocked() []byte {
	m := macro{Tool: "HUELLA", Rev: rev, Steps: steps,
		Note: "each step is one click: x,y is WHERE it landed on a screen of that size; what (and alt) is WHAT stood there as LINCE read it - text to find again, and dx,dy from the middle of its box to the click"}
	if m.Steps == nil {
		m.Steps = []Step{}
	}
	b, _ := json.MarshalIndent(m, "", " ")
	return b
}

func saveLocked() {
	if err := os.WriteFile(macroPath(), macroJSONLocked(), 0644); err != nil {
		log.Printf("could not save %s: %v", macroPath(), err)
	}
}

func loadMacro() {
	b, err := os.ReadFile(macroPath())
	if err != nil {
		return
	}
	var m macro
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for i := range m.Steps {
		m.Steps[i].Read = true
		if m.Steps[i].ID >= nextID {
			nextID = m.Steps[i].ID + 1
		}
	}
	steps = m.Steps
	log.Printf("loaded %d steps from %s", len(steps), macroPath())
}

/* ---------------------------------------------------------------- the page's side ---------------------------------------------------------------- */

func localHost(h string) bool {
	return h == fmt.Sprintf("127.0.0.1:%d", port) || h == fmt.Sprintf("localhost:%d", port)
}

// Only the page this program served may talk to it: the token rides on a header no other site can send.
func guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) || r.Header.Get("X-Huella") != token {
			http.Error(w, "not from this program's page", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func claimLocked(c string) bool {
	if c == "" {
		return false
	}
	if !known[c] { // the newest page to turn up is the one that is served
		known[c] = true
		active = c
		served = nil
		log.Printf("page connected")
	}
	return c == active
}

func wasServedLocked(id int64) bool {
	for _, v := range served {
		if v == id {
			return true
		}
	}
	return false
}

func noteServedLocked(id int64) {
	served = append(served, id)
	if len(served) > 3 {
		served = served[len(served)-3:]
	}
}

func sendFrame(w http.ResponseWriter, f *frame, body bool) {
	h := w.Header()
	h.Set("X-Frame", strconv.FormatInt(f.id, 10))
	h.Set("X-W", strconv.Itoa(f.w))
	h.Set("X-H", strconv.Itoa(f.h))
	if !body {
		h.Set("X-Body", "0")
		return
	}
	h.Set("X-Body", "1")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(f.px)))
	w.Write(f.px)
}

// The page keeps one of these open at all times. It comes back with, in this order: a click that wants answering
// (with its frame, unless the page has read that frame already), the newest frame if the page has not had it,
// word that something changed, or - when the wait runs out - word that the screen is the same.
func handleNext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client := q.Get("c")
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	seen, _ := strconv.ParseInt(q.Get("seq"), 10, 64)
	wait, err := strconv.Atoi(q.Get("wait"))
	if err != nil || wait < 0 || wait > 25000 {
		wait = 25000
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	h := w.Header()
	for {
		mu.Lock()
		if !claimLocked(client) {
			mu.Unlock()
			h.Set("X-Kind", "retired")
			return
		}
		h.Set("X-Mode", mode)
		h.Set("X-Seq", strconv.FormatInt(seq, 10))
		if len(pending) > 0 {
			c := pending[0]
			pending = pending[1:]
			x, y := -1, -1
			for i := range steps {
				if steps[i].ID == c.step {
					x, y = steps[i].X, steps[i].Y
				}
			}
			body := !wasServedLocked(c.f.id)
			if body {
				noteServedLocked(c.f.id)
			}
			mu.Unlock()
			h.Set("X-Kind", "click")
			h.Set("X-Step", strconv.Itoa(c.step))
			h.Set("X-X", strconv.Itoa(x))
			h.Set("X-Y", strconv.Itoa(y))
			sendFrame(w, c.f, body)
			return
		}
		if mode != "idle" && latest != nil && latest.id != after {
			f := latest
			noteServedLocked(f.id)
			mu.Unlock()
			h.Set("X-Kind", "frame")
			sendFrame(w, f, true)
			return
		}
		if seq != seen {
			mu.Unlock()
			h.Set("X-Kind", "state")
			return
		}
		ch := changed
		mu.Unlock()
		left := time.Until(deadline)
		if left <= 0 {
			h.Set("X-Kind", "same")
			return
		}
		t := time.NewTimer(left)
		select {
		case <-ch:
			t.Stop()
		case <-t.C:
			h.Set("X-Kind", "same")
			return
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
}

func handleState(w http.ResponseWriter, r *http.Request) {
	sw, sh := screenSize()
	mu.Lock()
	st := steps
	if st == nil {
		st = []Step{}
	}
	b, _ := json.Marshal(map[string]any{
		"tool": "HUELLA", "rev": rev, "mode": mode, "seq": seq, "steps": st, "screen": [2]int{sw, sh},
		"play": map[string]int{"tok": playTok, "delay": playDelay}, "last": lastRun, "file": macroPath(), "camera": camState,
	})
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func handleWhat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   int             `json:"id"`
		What json.RawMessage `json:"what"`
		Alt  json.RawMessage `json:"alt"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if string(in.What) == "null" {
		in.What = nil
	}
	if string(in.Alt) == "null" {
		in.Alt = nil
	}
	mu.Lock()
	for i := range steps {
		if steps[i].ID == in.ID {
			steps[i].What, steps[i].Alt, steps[i].Read = in.What, in.Alt, true
		}
	}
	if mode != "rec" {
		saveLocked()
	}
	touchLocked()
	mu.Unlock()
}

func handleRec(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	if r.URL.Query().Get("on") == "1" {
		if mode == "idle" {
			setModeLocked("rec", "(page)")
		}
		return
	}
	if mode != "rec" {
		return
	}
	// stopped from the page: the click that pressed STOP is not a step. It is told by its window; where the window
	// could not be named, a step with no window that is under a second and a half old is taken to be that click.
	if n := len(steps); n > 0 && steps[n-1].Win == "" && time.Since(lastDown) < 1500*time.Millisecond {
		id := steps[n-1].ID
		steps = steps[:n-1]
		for i := range pending {
			if pending[i].step == id {
				pending = append(pending[:i], pending[i+1:]...)
				break
			}
		}
	}
	setModeLocked("idle", "(page)")
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	if r.URL.Query().Get("on") != "1" {
		stopPlayLocked("stopped from the page")
		return
	}
	delay, _ := strconv.Atoi(r.URL.Query().Get("delay"))
	if !startPlayLocked(delay, "(page)") {
		http.Error(w, "nothing to play, or busy", http.StatusConflict)
	}
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	if mode == "play" {
		stopPlayLocked("stopped from the page")
	} else {
		setModeLocked("idle", "(page)")
	}
}

func handleClear(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	if mode != "idle" {
		http.Error(w, "busy", http.StatusConflict)
		return
	}
	steps, pending = nil, nil
	lastRun = map[string]any{}
	saveLocked()
	touchLocked()
}

// The hands move only while a run is on, and only for the run that asks.
func handleClick(w http.ResponseWriter, r *http.Request) {
	var in struct {
		X, Y   int
		Button string
		Count  int
		Tok    int
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	mu.Lock()
	ok := mode == "play" && in.Tok == playTok
	mu.Unlock()
	if !ok {
		http.Error(w, "no run is on", http.StatusConflict)
		return
	}
	if in.Button != "right" {
		in.Button = "left"
	}
	injectClick(in.X, in.Y, in.Button, in.Count)
	log.Printf("  click %s %d,%d", in.Button, in.X, in.Y)
}

// A wait the browser cannot throttle: the page asks, the program sleeps. Cut short the moment the run stops.
func handleWait(w http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	tok, _ := strconv.Atoi(r.URL.Query().Get("tok"))
	if ms < 0 {
		ms = 0
	}
	if ms > 10000 {
		ms = 10000
	}
	end := time.Now().Add(time.Duration(ms) * time.Millisecond)
	for {
		mu.Lock()
		ok := mode == "play" && tok == playTok
		mu.Unlock()
		if !ok {
			http.Error(w, "no run is on", http.StatusConflict)
			return
		}
		left := time.Until(end)
		if left <= 0 || r.Context().Err() != nil {
			return
		}
		if left > 25*time.Millisecond {
			left = 25 * time.Millisecond
		}
		time.Sleep(left)
	}
}

func handleDone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tok  int    `json:"tok"`
		OK   bool   `json:"ok"`
		Msg  string `json:"msg"`
		Step int    `json:"step"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if mode != "play" || in.Tok != playTok {
		return
	}
	lastRun = map[string]any{"state": "done", "ok": in.OK, "msg": in.Msg, "step": in.Step}
	setModeLocked("idle", in.Msg)
}

func handleMacro(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	b := macroJSONLocked()
	mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || !localHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexPage)
}

func handleJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(harnessJS)
}

// A newer copy of this program asks the one already running to step aside.
func handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Huella-Quit") != "1" {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Huella", rev)
	go func() { time.Sleep(150 * time.Millisecond); os.Exit(0) }()
}

/* ---------------------------------------------------------------- start ---------------------------------------------------------------- */

func listen() net.Listener {
	for p := basePort; p < basePort+10; p++ {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		ln, err := net.Listen("tcp", addr)
		if err != nil && p == basePort {
			req, _ := http.NewRequest("POST", "http://"+addr+"/api/quit", nil)
			req.Header.Set("X-Huella-Quit", "1")
			c := http.Client{Timeout: 700 * time.Millisecond}
			if resp, e := c.Do(req); e == nil {
				resp.Body.Close()
				if resp.Header.Get("X-Huella") != "" {
					log.Printf("an earlier HUELLA was running: asked it to step aside")
					for i := 0; i < 25 && err != nil; i++ {
						time.Sleep(100 * time.Millisecond)
						ln, err = net.Listen("tcp", addr)
					}
				}
			}
		}
		if err == nil {
			port = p
			return ln
		}
	}
	log.Fatalf("no free port between %d and %d", basePort, basePort+9)
	return nil
}

func main() {
	log.SetFlags(log.Ltime)
	platInit()
	if p, err := strconv.Atoi(os.Getenv("HUELLA_PORT")); err == nil && p > 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			log.Fatal(err)
		}
		port = p
		serve(ln)
		return
	}
	serve(listen())
}

func serve(ln net.Listener) {
	b := make([]byte, 16)
	rand.Read(b)
	token = hex.EncodeToString(b)
	loadMacro()

	inject := fmt.Sprintf(`<script>window.HUELLA={token:%q,rev:%q,mark:%q};</script><script src="/huella.js"></script>`, token, rev, pageMark)
	if i := bytes.LastIndex(linceHTML, []byte("</body>")); i >= 0 {
		indexPage = append(append(append([]byte{}, linceHTML[:i]...), inject...), linceHTML[i:]...)
	} else {
		indexPage = append(append([]byte{}, linceHTML...), inject...)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/huella.js", handleJS)
	mux.HandleFunc("/api/next", guard(handleNext))
	mux.HandleFunc("/api/state", guard(handleState))
	mux.HandleFunc("/api/what", guard(handleWhat))
	mux.HandleFunc("/api/rec", guard(handleRec))
	mux.HandleFunc("/api/play", guard(handlePlay))
	mux.HandleFunc("/api/stop", guard(handleStop))
	mux.HandleFunc("/api/clear", guard(handleClear))
	mux.HandleFunc("/api/click", guard(handleClick))
	mux.HandleFunc("/api/wait", guard(handleWait))
	mux.HandleFunc("/api/done", guard(handleDone))
	mux.HandleFunc("/api/macro", guard(handleMacro))
	mux.HandleFunc("/api/quit", handleQuit)
	platRoutes(mux)

	go camera()
	go poller()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	w, h := screenSize()
	log.Printf("HUELLA %s   %s   screen %dx%d", rev, url, w, h)
	log.Printf("F9 record / stop     F8 play / stop     Esc stops a run     close this window to quit")
	log.Printf("the macro is kept in %s", macroPath())
	if os.Getenv("HUELLA_NO_BROWSER") == "" {
		go func() { time.Sleep(400 * time.Millisecond); openBrowser(url) }()
	}
	log.Fatal(http.Serve(ln, mux))
}
