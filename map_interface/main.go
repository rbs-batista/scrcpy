package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	webview "github.com/webview/webview_go"
)

const (
	HTTPPort       = ":8080"
	UDPIp          = "127.0.0.1"
	UDPPort        = "5554"
	baseScrcpyPort = 27183

	// scrcpy control message types
	ctrlMsgInjectTouch = 0x02

	// Android motion event actions
	touchActionDown = 0
	touchActionUp   = 1
	touchActionMove = 2

	// pointer IDs (int64 interpreted as uint64)
	pointerIDFinger = uint64(0xFFFFFFFFFFFFFFFE) // SC_POINTER_ID_GENERIC_FINGER
)

type LocationData struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

var (
	mainWebview     webview.WebView
	phoneWinW       int
	phoneWinH       int
	ffmpegPath      string
	globalServerJar string
	globalServerVer string
	installedJars   sync.Map
)

type DeviceInfo struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Model  string `json:"model"`
	IP     string `json:"ip"`
}

var (
	currentSerial string
	serialMu      sync.Mutex
	globalAdbPath string
)

func getSerial() string {
	serialMu.Lock()
	defer serialMu.Unlock()
	return currentSerial
}

func makeAdbCmd(args ...string) *exec.Cmd {
	serial := getSerial()
	if serial != "" {
		args = append([]string{"-s", serial}, args...)
	}
	return exec.Command(globalAdbPath, args...)
}

// findAdb locates the adb binary, preferring the one bundled next to the executable.
func findAdb() (string, error) {
	if exePath, err := os.Executable(); err == nil {
		if exePath, err = filepath.EvalSymlinks(exePath); err == nil {
			dir := filepath.Dir(exePath)
			// On Windows only accept .exe — a bare "adb" file cannot be executed.
			names := []string{"adb.exe", "adb"}
			if runtime.GOOS == "windows" {
				names = []string{"adb.exe"}
			}
			for _, name := range names {
				bundled := filepath.Join(dir, name)
				if _, err := os.Stat(bundled); err == nil {
					return bundled, nil
				}
			}
		}
	}

	if p, err := exec.LookPath("adb"); err == nil {
		return p, nil
	}

	homeDir, _ := os.UserHomeDir()
	var candidates []string
	if sdkRoot := os.Getenv("ANDROID_SDK_ROOT"); sdkRoot != "" {
		candidates = append(candidates,
			filepath.Join(sdkRoot, "platform-tools", "adb.exe"),
			filepath.Join(sdkRoot, "platform-tools", "adb"),
		)
	}
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		appData := os.Getenv("APPDATA")
		candidates = append(candidates,
			filepath.Join(localAppData, "Android", "Sdk", "platform-tools", "adb.exe"),
			filepath.Join(appData, "Android", "Sdk", "platform-tools", "adb.exe"),
			filepath.Join(homeDir, "AppData", "Local", "Android", "Sdk", "platform-tools", "adb.exe"),
		)
	}
	candidates = append(candidates,
		filepath.Join(homeDir, "Library", "Android", "sdk", "platform-tools", "adb"),
		filepath.Join(homeDir, "Android", "Sdk", "platform-tools", "adb"),
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("adb not found — place the adb binary next to the app executable")
}

// findFfmpeg locates ffmpeg. Optional — only needed for MJPEG /stream fallback.
func findFfmpeg() string {
	if exePath, err := os.Executable(); err == nil {
		if exePath, err = filepath.EvalSymlinks(exePath); err == nil {
			dir := filepath.Dir(exePath)
			names := []string{"ffmpeg.exe", "ffmpeg"}
			if runtime.GOOS == "windows" {
				names = []string{"ffmpeg.exe"}
			}
			for _, name := range names {
				bundled := filepath.Join(dir, name)
				if _, err := os.Stat(bundled); err == nil {
					return bundled
				}
			}
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\ffmpeg\bin\ffmpeg.exe`,
			`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func getDeviceIP(serial string) string {
	cmd := exec.Command(globalAdbPath, "-s", serial, "shell", "ip", "route", "show", "dev", "wlan0")
	out, err := cmd.Output()
	if err == nil {
		for part := range strings.FieldsSeq(string(out)) {
			if net.ParseIP(part) != nil {
				return part
			}
		}
	}
	cmd2 := exec.Command(globalAdbPath, "-s", serial, "shell", "ip", "addr", "show", "wlan0")
	out2, err := cmd2.Output()
	if err == nil {
		for line := range strings.SplitSeq(string(out2), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "inet ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					ip := strings.Split(parts[1], "/")[0]
					if net.ParseIP(ip) != nil {
						return ip
					}
				}
			}
		}
	}
	return ""
}

func ensureServerJar(serial string) {
	if serial == "" || globalServerJar == "" {
		return
	}

	// Mata qualquer scrcpy antigo + remove jar
	exec.Command(globalAdbPath, "-s", serial, "shell",
		"sh", "-c",
		"pkill -9 -f scrcpy; rm -f /data/local/tmp/scrcpy-server",
	).Run()

	// Push do jar correto
	push := exec.Command(globalAdbPath, "-s", serial, "push",
		globalServerJar,
		"/data/local/tmp/scrcpy-server",
	)

	if out, err := push.CombinedOutput(); err != nil {
		log.Printf("Failed to push server jar to %s: %v — %s", serial, err, out)
		return
	}

	log.Printf("scrcpy-server atualizado em %s", serial)
}

func listDevices() []DeviceInfo {
	cmd := exec.Command(globalAdbPath, "devices", "-l")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("adb devices error: %v", err)
		return nil
	}
	var devices []DeviceInfo
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		serial := fields[0]
		state := fields[1]
		model := ""
		for _, f := range fields[2:] {
			if m, ok := strings.CutPrefix(f, "model:"); ok {
				model = strings.ReplaceAll(m, "_", " ")
				break
			}
		}
		devices = append(devices, DeviceInfo{Serial: serial, State: state, Model: model})
	}
	var wg sync.WaitGroup
	for i := range devices {
		if devices[i].State != "device" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			devices[i].IP = getDeviceIP(devices[i].Serial)
			// ensureServerJar(devices[i].Serial)
		}(i)
	}
	wg.Wait()
	return devices
}

// ─── H.264 NAL Unit Parser ─────────────────────────────────────────────────

// findStartCode returns the position and header length (3 or 4) of the next
// Annex B start code in data starting at offset. Returns (-1, 0) if not found.
func findStartCode(data []byte, offset int) (pos, headerLen int) {
	for i := offset; i < len(data)-2; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				return i, 4
			}
			if data[i+2] == 1 {
				return i, 3
			}
		}
	}
	return -1, 0
}

// extractNextNAL returns the first complete NAL unit (including its start code),
// its NAL type, and the number of bytes consumed from data.
// Returns (nil, 0, 0) when the next NAL unit is not yet complete.
func extractNextNAL(data []byte) (nal []byte, nalType byte, consumed int) {
	first, firstLen := findStartCode(data, 0)
	if first < 0 {
		return nil, 0, 0
	}
	next, _ := findStartCode(data, first+firstLen+1)
	if next < 0 {
		return nil, 0, 0
	}
	nal = data[first:next]
	if len(nal) > firstLen {
		nalType = nal[firstLen] & 0x1F
	}
	return nal, nalType, next
}

// ─── WebSocket client ──────────────────────────────────────────────────────

type wsClient struct {
	ch chan []byte
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 256 * 1024,
}

// ─── ScrcpyDirectStream ────────────────────────────────────────────────────

type ScrcpyDirectStream struct {
	serverJar string
	tcpPort   int
	opMu      sync.Mutex
	mu        sync.RWMutex
	ctrlMu    sync.Mutex // serializes writes to controlConn

	// state
	running     bool
	stopChan    chan struct{}
	tcpConn     net.Conn // video socket
	controlConn net.Conn // control channel socket
	serverCmd   *exec.Cmd
	ffmpegCmd   *exec.Cmd
	wg          sync.WaitGroup
	serial      string

	// MJPEG fallback (only when ffmpeg available)
	frame []byte

	// WebSocket H.264 broadcast
	wsClients sync.Map // *wsClient → struct{}

	// device resolution (fetched after connect)
	deviceW int
	deviceH int
}

func (s *ScrcpyDirectStream) Start() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	if s.running {
		return fmt.Errorf("stream already running")
	}

	serial := s.serial
	ensureServerJar(serial)

	// kill anterior
	exec.Command(globalAdbPath, "-s", serial, "shell",
		"sh", "-c",
		"pkill -9 -f scrcpy || true",
	).Run()

	time.Sleep(500 * time.Millisecond)

	// forward
	if out, err := exec.Command(globalAdbPath, "-s", serial, "forward",
		fmt.Sprintf("tcp:%d", s.tcpPort), "localabstract:scrcpy",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("adb forward failed: %v — %s", err, out)
	}

	// start server
	serverCmd := exec.Command(globalAdbPath, "-s", serial, "shell",
		"CLASSPATH=/data/local/tmp/scrcpy-server",
		"app_process", "/",
		"com.genymobile.scrcpy.Server", globalServerVer,
		"log_level=info",
		"tunnel_forward=true",
		"video=true",
		"audio=false",
		"control=true",
		"max_fps=30",
		// 👇 REMOVA raw_stream
		// "raw_stream=true",
	)
	serverCmd.Stderr = os.Stderr
	serverCmd.Stdout = os.Stdout

	if err := serverCmd.Start(); err != nil {
		return err
	}

	time.Sleep(800 * time.Millisecond)

	addr := fmt.Sprintf("127.0.0.1:%d", s.tcpPort)

	// conectar sockets
	var videoConn, ctrlConn net.Conn
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		vc, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		cc, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			vc.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		videoConn = vc
		ctrlConn = cc
		break
	}

	if videoConn == nil {
		return fmt.Errorf("failed to connect sockets")
	}

	// 🔥 =========================
	// 🔥 HANDSHAKE CORRETO AQUI
	// 🔥 =========================

	videoConn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 1. dummy byte
	dummy := make([]byte, 1)
	if _, err := io.ReadFull(videoConn, dummy); err != nil {
		return fmt.Errorf("handshake dummy failed: %v", err)
	}

	// 2. device name (64 bytes)
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(videoConn, nameBuf); err != nil {
		return fmt.Errorf("handshake device name failed: %v", err)
	}

	deviceName := strings.TrimRight(string(nameBuf), "\x00")
	log.Printf("[%s] device name: %s", serial, deviceName)

	// 3. resolution (4 bytes)
	sizeBuf := make([]byte, 4)
	if _, err := io.ReadFull(videoConn, sizeBuf); err != nil {
		return fmt.Errorf("handshake size failed: %v", err)
	}

	w := binary.BigEndian.Uint16(sizeBuf[0:2])
	h := binary.BigEndian.Uint16(sizeBuf[2:4])

	log.Printf("[%s] device resolution: %dx%d", serial, w, h)

	videoConn.SetReadDeadline(time.Time{})

	// salvar estado
	stopChan := make(chan struct{})

	s.mu.Lock()
	s.running = true
	s.stopChan = stopChan
	s.tcpConn = videoConn
	s.controlConn = ctrlConn
	s.serverCmd = serverCmd
	s.deviceW = int(w)
	s.deviceH = int(h)
	s.mu.Unlock()

	log.Printf("[%s] stream started OK", serial)

	// parser H264
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.extractH264Frames(videoConn, stopChan)
	}()

	return nil
}

// fetchDeviceSize queries the device resolution via ADB and caches it.
func (s *ScrcpyDirectStream) fetchDeviceSize() {
	cmd := exec.Command(globalAdbPath, "-s", s.serial, "shell", "wm", "size")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "size:") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}
		dims := strings.TrimSpace(parts[1])
		wh := strings.Split(dims, "x")
		if len(wh) != 2 {
			continue
		}
		w, errW := strconv.Atoi(strings.TrimSpace(wh[0]))
		h, errH := strconv.Atoi(strings.TrimSpace(wh[1]))
		if errW == nil && errH == nil && w > 0 && h > 0 {
			s.mu.Lock()
			s.deviceW, s.deviceH = w, h
			s.mu.Unlock()
			log.Printf("[%s] device size: %dx%d", s.serial, w, h)
			return
		}
	}
}

// extractH264Frames reads raw H.264 Annex B, groups NAL units into access units,
// and broadcasts each frame to WebSocket clients.
func (s *ScrcpyDirectStream) extractH264Frames(r io.Reader, stopChan <-chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		log.Printf("[%s] H.264 stream ended", s.serial)
	}()

	var buf []byte
	readBuf := make([]byte, 64*1024)
	var sps, pps []byte // latest config NALs
	var frameBuf []byte
	frameIsKey := false
	frameCount := 0

	for {
		select {
		case <-stopChan:
			return
		default:
		}

		n, err := r.Read(readBuf)
		if n > 0 {
			buf = append(buf, readBuf[:n]...)
		}

		// Drain all complete NAL units from the buffer.
		for {
			nal, nalType, consumed := extractNextNAL(buf)
			if nal == nil {
				break
			}
			buf = buf[consumed:]

			switch nalType {
			case 7: // SPS — flush any pending frame, then cache
				if len(frameBuf) > 0 {
					s.dispatchFrame(frameBuf, frameIsKey)
					frameBuf = nil
					frameCount++
					if frameCount == 1 {
						log.Printf("[%s] first frame dispatched", s.serial)
					}
				}
				sps = append([]byte(nil), nal...)

			case 8: // PPS — cache (belongs to next IDR)
				pps = append([]byte(nil), nal...)

			case 5: // IDR slice — start a new keyframe
				if len(frameBuf) > 0 {
					s.dispatchFrame(frameBuf, frameIsKey)
					frameBuf = nil
					frameCount++
				}
				frameIsKey = true
				if len(sps) > 0 {
					frameBuf = append(frameBuf, sps...)
				}
				if len(pps) > 0 {
					frameBuf = append(frameBuf, pps...)
				}
				frameBuf = append(frameBuf, nal...)

			case 1: // Non-IDR slice — flush previous frame, start new P-frame
				if len(frameBuf) > 0 {
					s.dispatchFrame(frameBuf, frameIsKey)
					frameBuf = nil
					frameCount++
				}
				frameIsKey = false
				frameBuf = append(frameBuf, nal...)

			case 9: // AUD — ignore
			default: // SEI, etc. — append to current frame
				frameBuf = append(frameBuf, nal...)
			}
		}

		if err != nil {
			if len(frameBuf) > 0 {
				s.dispatchFrame(frameBuf, frameIsKey)
			}
			log.Printf("[%s] stream read error: %v (frames=%d)", s.serial, err, frameCount)
			return
		}

		if len(buf) > 4*1024*1024 {
			buf = buf[len(buf)-256*1024:]
		}
	}
}

// dispatchFrame broadcasts a complete H.264 access unit to all WebSocket subscribers.
// msg format: [1 byte flags (0x01=keyframe, 0x00=delta)] + [H.264 Annex B data]
func (s *ScrcpyDirectStream) dispatchFrame(frameData []byte, isKey bool) {
	if len(frameData) == 0 {
		return
	}
	msg := make([]byte, 1+len(frameData))
	if isKey {
		msg[0] = 0x01
	}
	copy(msg[1:], frameData)

	s.wsClients.Range(func(k, _ interface{}) bool {
		client := k.(*wsClient)
		select {
		case client.ch <- msg:
		default:
			// Drop frame for slow client — matches scrcpy's single-frame buffer strategy.
		}
		return true
	})
}

func (s *ScrcpyDirectStream) Stop() {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	stopChan := s.stopChan
	tcpConn := s.tcpConn
	ctrlConn := s.controlConn
	serverCmd := s.serverCmd
	ffmpegCmd := s.ffmpegCmd
	serial := s.serial
	s.mu.Unlock()

	exec.Command(globalAdbPath, "-s", serial, "shell",
		"sh", "-c",
		"pkill -9 -f scrcpy.Server 2>/dev/null; "+
			"ps -A 2>/dev/null | grep scrcpy | grep -v grep | awk '{print $2}' | xargs kill -9 2>/dev/null; true",
	).Run()

	close(stopChan)
	if tcpConn != nil {
		tcpConn.Close()
	}
	if ctrlConn != nil {
		ctrlConn.Close()
	}
	if ffmpegCmd != nil && ffmpegCmd.Process != nil {
		ffmpegCmd.Process.Kill()
	}
	if serverCmd != nil && serverCmd.Process != nil {
		serverCmd.Process.Kill()
	}
	s.wg.Wait()

	s.mu.Lock()
	s.frame = nil
	s.controlConn = nil
	s.mu.Unlock()

	exec.Command(globalAdbPath, "-s", serial, "forward", "--remove",
		fmt.Sprintf("tcp:%d", s.tcpPort)).Run()
}

func (s *ScrcpyDirectStream) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

func (s *ScrcpyDirectStream) HasControl() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controlConn != nil && s.deviceW > 0 && s.deviceH > 0
}

func (s *ScrcpyDirectStream) GetFrame() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frame
}

// SendTouch sends a binary touch event over the scrcpy control channel.
// action: touchActionDown / touchActionUp / touchActionMove
func (s *ScrcpyDirectStream) SendTouch(action, x, y int) error {
	s.mu.RLock()
	ctrl := s.controlConn
	dw := s.deviceW
	dh := s.deviceH
	s.mu.RUnlock()

	if ctrl == nil {
		return fmt.Errorf("no control connection")
	}
	if dw == 0 || dh == 0 {
		return fmt.Errorf("device size unknown")
	}

	var pressure uint16
	if action == touchActionDown || action == touchActionMove {
		pressure = 0xFFFF
	}

	// 32-byte message per control_msg.c:sc_control_msg_serialize (INJECT_TOUCH_EVENT)
	var buf [32]byte
	buf[0] = ctrlMsgInjectTouch
	buf[1] = byte(action)
	binary.BigEndian.PutUint64(buf[2:], pointerIDFinger)
	binary.BigEndian.PutUint32(buf[10:], uint32(int32(x)))
	binary.BigEndian.PutUint32(buf[14:], uint32(int32(y)))
	binary.BigEndian.PutUint16(buf[18:], uint16(dw))
	binary.BigEndian.PutUint16(buf[20:], uint16(dh))
	binary.BigEndian.PutUint16(buf[22:], pressure)
	// buf[24:32] = action_button(4) + buttons(4) — already zero

	s.ctrlMu.Lock()
	_, err := ctrl.Write(buf[:])
	s.ctrlMu.Unlock()
	return err
}

// ServeHTTP serves MJPEG when a frame is available (fallback for browsers without WebCodecs).
func (s *ScrcpyDirectStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		default:
			frame := s.GetFrame()
			if frame == nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame))
			w.Write(frame)
			fmt.Fprintf(w, "\r\n")
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// ─── StreamPool ────────────────────────────────────────────────────────────

type StreamPool struct {
	mu       sync.Mutex
	streams  map[string]*ScrcpyDirectStream
	nextPort int
}

func newStreamPool() *StreamPool {
	return &StreamPool{streams: make(map[string]*ScrcpyDirectStream), nextPort: baseScrcpyPort}
}

func (p *StreamPool) get(serial string) *ScrcpyDirectStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams[serial]
}

func (p *StreamPool) ensure(serial string) *ScrcpyDirectStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ds, ok := p.streams[serial]; ok {
		return ds
	}
	port := p.nextPort
	p.nextPort++
	ds := &ScrcpyDirectStream{serverJar: globalServerJar, tcpPort: port, serial: serial}
	p.streams[serial] = ds
	return ds
}

func (p *StreamPool) sync() {
	devices := listDevices()
	active := make(map[string]bool)
	for _, d := range devices {
		if d.State != "device" {
			continue
		}
		active[d.Serial] = true
		ds := p.ensure(d.Serial)
		if !ds.IsRunning() {
			go func(ds *ScrcpyDirectStream) {
				if err := ds.Start(); err != nil {
					log.Printf("[%s] stream start failed: %v", ds.serial, err)
				}
			}(ds)
		}
	}
	p.mu.Lock()
	var gone []string
	for serial := range p.streams {
		if !active[serial] {
			gone = append(gone, serial)
		}
	}
	for _, serial := range gone {
		ds := p.streams[serial]
		delete(p.streams, serial)
		go ds.Stop()
	}
	p.mu.Unlock()
}

func (p *StreamPool) watch() {
	for {
		p.sync()
		time.Sleep(3 * time.Second)
	}
}

// ─── misc helpers ──────────────────────────────────────────────────────────

func findServerJar(exeDir string) string {
	dir := exeDir
	for range 8 {
		// Official scrcpy release: scrcpy-server alongside the exe
		for _, name := range []string{"scrcpy-server", "scrcpy-server"} {
			if p := filepath.Join(dir, name); fileExists(p) {
				return p
			}
			// one level deep
			if p := filepath.Join(dir, "server", name); fileExists(p) {
				return p
			}
			// original project layout
			if p := filepath.Join(dir, "x", "server", name); fileExists(p) {
				return p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// detectServerVersion inspects the directory containing the server JAR for a
// file like "scrcpy-win64-v3.3.1.txt" and returns the embedded version string.
// Falls back to "3.2" if nothing is found.
func detectServerVersion(serverPath string) string {
	entries, err := os.ReadDir(filepath.Dir(serverPath))
	if err != nil {
		return "3.2"
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "scrcpy-") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		// e.g. "scrcpy-win64-v3.3.1.txt" → split on "-v" → last part → "3.3.1"
		parts := strings.SplitN(name, "-v", 2)
		if len(parts) == 2 {
			ver := strings.TrimSuffix(parts[1], ".txt")
			if ver != "" {
				return ver
			}
		}
	}
	return "3.2"
}

func phoneWindowSize() (int, int) {
	return 420, 920
}

// ─── main ──────────────────────────────────────────────────────────────────

func main() {
	adbPath, err := findAdb()
	if err != nil {
		log.Fatalf("Cannot start: %v", err)
	}
	log.Printf("Using adb: %s", adbPath)
	globalAdbPath = adbPath

	udpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%s", UDPIp, UDPPort))
	if err != nil {
		log.Fatalf("Failed to resolve UDP address: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		log.Fatalf("Failed to connect to UDP socket: %v", err)
	}
	defer conn.Close()

	ffmpegPath = findFfmpeg()
	if ffmpegPath != "" {
		log.Printf("Using ffmpeg (MJPEG fallback): %s", ffmpegPath)
	} else {
		log.Println("ffmpeg not found — MJPEG /stream fallback unavailable (WebSocket H.264 will be used)")
	}

	exePath, _ := os.Executable()
	exePath, _ = filepath.EvalSymlinks(exePath)
	globalServerJar = filepath.Join("scrcpy", "scrcpy-server")
	if globalServerJar == "" {
		if cwd, err := os.Getwd(); err == nil {
			globalServerJar = findServerJar(cwd)
		}
	}
	// Also search alongside adb — users often keep all scrcpy tools together.
	if globalServerJar == "" && globalAdbPath != "" {
		globalServerJar = findServerJar(filepath.Dir(globalAdbPath))
	}
	if globalServerJar == "" {
		log.Println("WARNING: scrcpy-server not found — JAR must already be on device")
		globalServerVer = "3.2"
	} else {
		globalServerVer = detectServerVersion(globalServerJar)
		log.Printf("Using server JAR: %s (version %s)", globalServerJar, globalServerVer)
	}

	pool := newStreamPool()
	go pool.watch()

	// ── GET /devices
	http.HandleFunc("/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		devices := listDevices()
		if devices == nil {
			devices = []DeviceInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(devices)
	})

	// ── POST /devices/select
	http.HandleFunc("/devices/select", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Serial string `json:"serial"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		serialMu.Lock()
		currentSerial = body.Serial
		serialMu.Unlock()
		log.Printf("Selected device: %s", body.Serial)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "serial": body.Serial})
	})

	// ── POST /location
	http.HandleFunc("/location", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var data LocationData
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		message := fmt.Sprintf("%f,%f", data.Lat, data.Lng)
		conn.Write([]byte(message))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// ── GET /ws/video — WebSocket H.264 stream (primary, low-latency)
	http.HandleFunc("/ws/video", func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("ws upgrade: %v", err)
			return
		}
		defer wsConn.Close()

		serial := getSerial()
		ds := pool.get(serial)
		if ds == nil || !ds.IsRunning() {
			return
		}

		client := &wsClient{ch: make(chan []byte, 2)}
		ds.wsClients.Store(client, struct{}{})
		defer ds.wsClients.Delete(client)

		// Drain reads (browser may send pings).
		go func() {
			for {
				if _, _, err := wsConn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		for {
			select {
			case msg := <-client.ch:
				if err := wsConn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	})

	// ── GET /stream — MJPEG fallback (requires ffmpeg)
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		serial := getSerial()
		if ds := pool.get(serial); ds != nil && ds.IsRunning() && ds.GetFrame() != nil {
			ds.ServeHTTP(w, r)
			return
		}
		http.Error(w, "stream not available (use /ws/video for WebSocket H.264)", http.StatusServiceUnavailable)
	})

	// ── GET /frame — latest JPEG (MJPEG fallback polling)
	frameNoFrameLogCount := 0
	http.HandleFunc("/frame", func(w http.ResponseWriter, r *http.Request) {
		serial := getSerial()
		var frame []byte
		if ds := pool.get(serial); ds != nil {
			frame = ds.GetFrame()
		}
		if frame == nil {
			frameNoFrameLogCount++
			if frameNoFrameLogCount <= 5 || frameNoFrameLogCount%30 == 0 {
				log.Printf("/frame: no frame (serial=%q, count=%d)", serial, frameNoFrameLogCount)
			}
			http.Error(w, "no frame", http.StatusServiceUnavailable)
			return
		}
		frameNoFrameLogCount = 0
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(frame)
	})

	// ── POST /scrcpy/start
	http.HandleFunc("/scrcpy/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serial := getSerial()
		if serial != "" {
			ds := pool.ensure(serial)
			if !ds.IsRunning() {
				go ds.Start()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// ── POST /scrcpy/stop
	http.HandleFunc("/scrcpy/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serial := getSerial()
		if ds := pool.get(serial); ds != nil {
			go ds.Stop()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	})

	// ── GET /scrcpy/status
	http.HandleFunc("/scrcpy/status", func(w http.ResponseWriter, r *http.Request) {
		serial := getSerial()
		streaming := false
		if ds := pool.get(serial); ds != nil {
			streaming = ds.IsRunning()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"running":   streaming,
			"streaming": streaming,
		})
	})

	// ── GET /device/size
	http.HandleFunc("/device/size", func(w http.ResponseWriter, r *http.Request) {
		cmd := makeAdbCmd("shell", "wm", "size")
		output, err := cmd.Output()
		if err != nil {
			http.Error(w, "Failed to get device size", http.StatusInternalServerError)
			return
		}
		lines := strings.Split(string(output), "\n")
		width, height := 1080, 2280
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Override size:") || strings.HasPrefix(line, "Physical size:") {
				parts := strings.Split(line, ":")
				if len(parts) == 2 {
					dims := strings.TrimSpace(parts[1])
					wh := strings.Split(dims, "x")
					if len(wh) == 2 {
						if w2, err := strconv.Atoi(strings.TrimSpace(wh[0])); err == nil {
							width = w2
						}
						if h2, err := strconv.Atoi(strings.TrimSpace(wh[1])); err == nil {
							height = h2
						}
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"width": width, "height": height})
	})

	// ── POST /touch
	http.HandleFunc("/touch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var data struct {
			Action   string `json:"action"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
			X2       int    `json:"x2"`
			Y2       int    `json:"y2"`
			Duration int    `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		serial := getSerial()
		ds := pool.get(serial)

		switch data.Action {
		case "tap":
			if ds != nil && ds.HasControl() {
				go func() {
					ds.SendTouch(touchActionDown, data.X, data.Y)
					time.Sleep(50 * time.Millisecond)
					ds.SendTouch(touchActionUp, data.X, data.Y)
				}()
			} else {
				go makeAdbCmd("shell", "input", "tap",
					strconv.Itoa(data.X), strconv.Itoa(data.Y)).Run()
			}

		case "swipe":
			dur := data.Duration
			if dur == 0 {
				dur = 300
			}
			if ds != nil && ds.HasControl() {
				go func() {
					steps := 10
					if dur > 500 {
						steps = 20
					}
					stepDur := time.Duration(dur/steps) * time.Millisecond
					ds.SendTouch(touchActionDown, data.X, data.Y)
					for i := 1; i <= steps; i++ {
						x := data.X + (data.X2-data.X)*i/steps
						y := data.Y + (data.Y2-data.Y)*i/steps
						ds.SendTouch(touchActionMove, x, y)
						time.Sleep(stepDur)
					}
					ds.SendTouch(touchActionUp, data.X2, data.Y2)
				}()
			} else {
				go makeAdbCmd("shell", "input", "swipe",
					strconv.Itoa(data.X), strconv.Itoa(data.Y),
					strconv.Itoa(data.X2), strconv.Itoa(data.Y2),
					strconv.Itoa(dur)).Run()
			}

		case "keyevent":
			// Key events go via ADB (binary keycode injection is more complex).
			go makeAdbCmd("shell", "input", "keyevent", strconv.Itoa(data.X)).Run()

		default:
			http.Error(w, "Unknown action", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// ── GET /window/phone-size
	http.HandleFunc("/window/phone-size", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"width": phoneWinW, "height": phoneWinH})
	})

	// ── POST /window/resize
	http.HandleFunc("/window/resize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Width == 0 || body.Height == 0 {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if mainWebview != nil {
			mainWebview.Dispatch(func() {
				mainWebview.SetSize(body.Width, body.Height, webview.HintNone)
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	exeDir := filepath.Dir(exePath)
	serveDir := exeDir
	if _, err := os.Stat(filepath.Join(serveDir, "index.html")); err != nil {
		if cwd, err := os.Getwd(); err == nil {
			if _, err := os.Stat(filepath.Join(cwd, "index.html")); err == nil {
				serveDir = cwd
				log.Printf("Serving files from working directory: %s", serveDir)
			}
		}
	}
	http.Handle("/", http.FileServer(http.Dir(serveDir)))

	log.Printf("Map server running at http://localhost%s", HTTPPort)
	go func() {
		if err := http.ListenAndServe(HTTPPort, nil); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	time.Sleep(300 * time.Millisecond)

	phoneWinW, phoneWinH = phoneWindowSize()
	mainWebview = webview.New(false)
	defer mainWebview.Destroy()
	mainWebview.SetTitle("DM Tools")
	mainWebview.SetSize(phoneWinW, phoneWinH, webview.HintNone)
	mainWebview.Navigate("http://localhost" + HTTPPort)
	mainWebview.Run()
}
