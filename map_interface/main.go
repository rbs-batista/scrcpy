package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	webview "github.com/webview/webview_go"
)

const (
	HTTPPort       = ":8080"
	UDPIp          = "127.0.0.1"
	UDPPort        = "5554"
	baseScrcpyPort = 27183 // first device gets this port, subsequent get +1, +2, …
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
	installedJars   sync.Map // serial → bool, avoids repeated adb shell ls
)

type DeviceInfo struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Model  string `json:"model"`
	IP     string `json:"ip"`
}

// Global selected device serial
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

// makeAdbCmd builds an adb command targeting the currently selected device.
func makeAdbCmd(args ...string) *exec.Cmd {
	serial := getSerial()
	if serial != "" {
		args = append([]string{"-s", serial}, args...)
	}
	return exec.Command(globalAdbPath, args...)
}

// findAdb locates the adb binary, preferring the one bundled next to the executable.
func findAdb() (string, error) {
	// Bundled adb — highest priority
	if exePath, err := os.Executable(); err == nil {
		if exePath, err = filepath.EvalSymlinks(exePath); err == nil {
			bundled := filepath.Join(filepath.Dir(exePath), "adb")
			if _, err := os.Stat(bundled); err == nil {
				return bundled, nil
			}
		}
	}

	// System-wide fallbacks
	if p, err := exec.LookPath("adb"); err == nil {
		return p, nil
	}

	homeDir, _ := os.UserHomeDir()
	candidates := []string{}
	if sdkRoot := os.Getenv("ANDROID_SDK_ROOT"); sdkRoot != "" {
		candidates = append(candidates, filepath.Join(sdkRoot, "platform-tools", "adb"))
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

// getDeviceIP tries to get the Wi-Fi IP for a given device serial.
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

	// Fallback: parse "inet <ip>/<mask>" from ip addr
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

// ensureServerJar pushes scrcpy-server.jar to the device if not already present.
func ensureServerJar(serial string) {
	if globalServerJar == "" || serial == "" {
		return
	}
	if _, ok := installedJars.Load(serial); ok {
		return
	}
	check := exec.Command(globalAdbPath, "-s", serial, "shell", "ls", "/data/local/tmp/scrcpy-server.jar")
	if check.Run() == nil {
		installedJars.Store(serial, true)
		return
	}
	push := exec.Command(globalAdbPath, "-s", serial, "push", globalServerJar, "/data/local/tmp/scrcpy-server.jar")
	if out, err := push.CombinedOutput(); err != nil {
		log.Printf("Failed to push server jar to %s: %v — %s", serial, err, out)
	} else {
		log.Printf("Installed scrcpy-server.jar on %s", serial)
		installedJars.Store(serial, true)
	}
}

// listDevices runs "adb devices -l" and returns structured device info.
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

		devices = append(devices, DeviceInfo{
			Serial: serial,
			State:  state,
			Model:  model,
		})
	}

	// Fetch IPs and pre-install server jar in parallel for connected devices
	var wg sync.WaitGroup
	for i := range devices {
		if devices[i].State != "device" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			devices[i].IP = getDeviceIP(devices[i].Serial)
			ensureServerJar(devices[i].Serial)
		}(i)
	}
	wg.Wait()

	return devices
}

// ScrcpyDirectStream connects to scrcpy-server.jar on the device and decodes
// the H.264 stream via ffmpeg, serving frames as MJPEG on /stream.
type ScrcpyDirectStream struct {
	serverJar string
	tcpPort   int          // TCP port on localhost forwarded to this device's abstract socket
	opMu      sync.Mutex   // serializes Start/Stop so only one runs at a time
	mu        sync.RWMutex // protects frame/running fields
	frame     []byte
	running   bool
	stopChan  chan struct{}
	tcpConn   net.Conn
	serverCmd *exec.Cmd
	ffmpegCmd *exec.Cmd
	wg        sync.WaitGroup
	serial    string
}

func (s *ScrcpyDirectStream) Start() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if running {
		return fmt.Errorf("direct stream already running")
	}

	if s.serverJar == "" {
		return fmt.Errorf("scrcpy-server.jar not found")
	}
	if ffmpegPath == "" {
		return fmt.Errorf("ffmpeg not found")
	}

	serial := s.serial
	ensureServerJar(serial)

	// Kill any stale scrcpy-server still holding the abstract socket.
	// Use multiple kill methods for compatibility across Android versions:
	// - pkill by cmdline pattern (works when pkill supports -f)
	// - ps -ef column 2 (POSIX ps: UID PID PPID ...)
	// - ps -A column 2 (toybox ps on newer Android)
	exec.Command(globalAdbPath, "-s", serial, "shell",
		"sh", "-c",
		"pkill -9 -f scrcpy 2>/dev/null; "+
			"ps -ef 2>/dev/null | awk '/scrcpy/&&!/grep/{print $2}' | xargs kill -9 2>/dev/null; "+
			"ps -A  2>/dev/null | awk '/scrcpy/&&!/grep/{print $2}' | xargs kill -9 2>/dev/null; true",
	).Run()
	time.Sleep(800 * time.Millisecond)

	// Forward TCP port to the device abstract socket
	if out, err := exec.Command(globalAdbPath, "-s", serial, "forward",
		fmt.Sprintf("tcp:%d", s.tcpPort), "localabstract:scrcpy",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("adb forward failed: %v — %s", err, out)
	}

	// Start scrcpy-server on device (background, do not Wait)
	serverCmd := exec.Command(globalAdbPath, "-s", serial, "shell",
		"CLASSPATH=/data/local/tmp/scrcpy-server.jar",
		"app_process", "/",
		"com.genymobile.scrcpy.Server", "3.2",
		"log_level=info", "tunnel_forward=true",
		"video=true", "audio=false", "control=false",
		"raw_stream=true", "max_fps=30",
	)
	serverCmd.Stderr = os.Stderr // server logs visible in terminal
	if err := serverCmd.Start(); err != nil {
		return fmt.Errorf("failed to start scrcpy-server: %v", err)
	}

	// Connect TCP and wait for first H.264 byte, retrying both as a unit.
	// A successful dial may still yield immediate EOF if the adb abstract-socket
	// forward races ahead of the server's listener — reconnecting resolves it.
	var tcpConn net.Conn
	var ffmpegInput io.Reader
	{
		connDeadline := time.Now().Add(10 * time.Second)
		var lastErr error
		for time.Now().Before(connDeadline) {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.tcpPort), 300*time.Millisecond)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				lastErr = err
				continue
			}
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var b [1]byte
			_, err = io.ReadFull(c, b[:])
			c.SetReadDeadline(time.Time{})
			if err == nil {
				log.Printf("[%s] TCP first byte: 0x%02x", serial, b[0])
				tcpConn = c
				ffmpegInput = io.MultiReader(bytes.NewReader(b[:]), c)
				break
			}
			c.Close()
			lastErr = err
			time.Sleep(150 * time.Millisecond)
		}
		if tcpConn == nil {
			serverCmd.Process.Kill()
			exec.Command(globalAdbPath, "-s", serial, "forward", "--remove", fmt.Sprintf("tcp:%d", s.tcpPort)).Run()
			return fmt.Errorf("no H.264 data within 10s: %v", lastErr)
		}
	}

	// Pipe TCP → ffmpeg → JPEG frames
	ffmpegCmd := exec.Command(ffmpegPath,
		"-loglevel", "warning",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-f", "h264",
		"-i", "pipe:0",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-pix_fmt", "yuvj420p",
		"-q:v", "3",
		"pipe:1",
	)
	ffmpegCmd.Stdin = ffmpegInput
	ffmpegCmd.Stderr = os.Stderr
	ffmpegOut, err := ffmpegCmd.StdoutPipe()
	if err != nil {
		tcpConn.Close()
		serverCmd.Process.Kill()
		return fmt.Errorf("ffmpeg pipe: %v", err)
	}
	if err := ffmpegCmd.Start(); err != nil {
		tcpConn.Close()
		serverCmd.Process.Kill()
		return fmt.Errorf("ffmpeg start: %v", err)
	}

	stopChan := make(chan struct{})
	s.mu.Lock()
	s.stopChan = stopChan
	s.tcpConn = tcpConn
	s.serverCmd = serverCmd
	s.ffmpegCmd = ffmpegCmd
	s.frame = nil
	s.running = true
	s.mu.Unlock()

	log.Printf("[%s] ScrcpyDirectStream started on port %d", serial, s.tcpPort)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.extractJPEGFrames(ffmpegOut, stopChan)
	}()
	return nil
}

func (s *ScrcpyDirectStream) extractJPEGFrames(r io.Reader, stopChan <-chan struct{}) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		log.Printf("[%s] ScrcpyDirectStream stopped", s.serial)
	}()

	var buf []byte
	chunk := make([]byte, 64*1024)
	frameCount := 0
	totalBytes := 0

	jpegStart := []byte{0xFF, 0xD8, 0xFF}
	jpegEnd := []byte{0xFF, 0xD9}

	for {
		select {
		case <-stopChan:
			return
		default:
		}

		n, err := r.Read(chunk)
		if n > 0 {
			totalBytes += n
			if frameCount == 0 && totalBytes > 0 {
				log.Printf("[%s] ffmpeg producing output (%d bytes so far)", s.serial, totalBytes)
			}
			buf = append(buf, chunk[:n]...)
			for {
				start := bytes.Index(buf, jpegStart)
				if start < 0 {
					break
				}
				end := bytes.Index(buf[start+3:], jpegEnd)
				if end < 0 {
					break
				}
				end = start + 3 + end + 2
				frame := make([]byte, end-start)
				copy(frame, buf[start:end])
				s.mu.Lock()
				s.frame = frame
				s.mu.Unlock()
				buf = buf[end:]
				frameCount++
				if frameCount == 1 || frameCount%150 == 0 {
					log.Printf("[%s] frame #%d extracted (%d bytes)", s.serial, frameCount, len(frame))
				}
			}
			if len(buf) > 8*1024*1024 {
				buf = buf[len(buf)-1024*1024:]
			}
		}
		if err != nil {
			log.Printf("[%s] ffmpeg output ended: %v (frames=%d, bytes=%d)", s.serial, err, frameCount, totalBytes)
			return
		}
	}
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
	serverCmd := s.serverCmd
	ffmpegCmd := s.ffmpegCmd
	serial := s.serial
	s.mu.Unlock()

	// Kill device-side server before closing the connection.
	exec.Command(globalAdbPath, "-s", serial, "shell",
		"sh", "-c",
		"pkill -9 -f scrcpy.Server 2>/dev/null; "+
			"ps -A 2>/dev/null | grep scrcpy | grep -v grep | awk '{print $2}' | xargs kill -9 2>/dev/null; true",
	).Run()

	close(stopChan)
	if tcpConn != nil {
		tcpConn.Close()
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
	s.mu.Unlock()

	exec.Command(globalAdbPath, "-s", serial, "forward", "--remove", fmt.Sprintf("tcp:%d", s.tcpPort)).Run()
}

func (s *ScrcpyDirectStream) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

func (s *ScrcpyDirectStream) GetFrame() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.frame
}

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

// StreamPool manages one ScrcpyDirectStream per connected device,
// pre-connecting in background so switching devices is instant.
type StreamPool struct {
	mu       sync.Mutex
	streams  map[string]*ScrcpyDirectStream
	nextPort int
}

func newStreamPool() *StreamPool {
	return &StreamPool{
		streams:  make(map[string]*ScrcpyDirectStream),
		nextPort: baseScrcpyPort,
	}
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

// sync detects connected/disconnected devices and manages their streams.
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

// watch syncs immediately on startup then every 3 seconds.
func (p *StreamPool) watch() {
	for {
		p.sync()
		time.Sleep(3 * time.Second)
	}
}

func findFfmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func findServerJar(exeDir string) string {
	dir := exeDir
	for range 8 {
		candidate := filepath.Join(dir, "x", "server", "scrcpy-server")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func phoneWindowSize() (int, int) {
	return 420, 920
}

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
	if ffmpegPath == "" {
		log.Println("WARNING: ffmpeg not found — streaming unavailable")
	} else {
		log.Printf("Using ffmpeg: %s", ffmpegPath)
	}
	exePath, _ := os.Executable()
	exePath, _ = filepath.EvalSymlinks(exePath)
	globalServerJar = findServerJar(filepath.Dir(exePath))
	if globalServerJar == "" {
		// Fallback for "go run": search from working directory
		if cwd, err := os.Getwd(); err == nil {
			globalServerJar = findServerJar(cwd)
		}
	}
	if globalServerJar == "" {
		log.Println("WARNING: scrcpy-server.jar not found — JAR must already be on device")
	} else {
		log.Printf("Using server JAR: %s", globalServerJar)
	}

	pool := newStreamPool()
	go pool.watch() // syncs immediately; devices connect before the webview opens

	// GET /devices — list connected ADB devices with serial, model and IP
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

	// POST /devices/select — set the active device serial
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

	// POST /location — send coordinates via UDP
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

	// GET /stream — MJPEG stream for the selected device (kept for compatibility)
	http.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		serial := getSerial()
		if ds := pool.get(serial); ds != nil && ds.IsRunning() {
			ds.ServeHTTP(w, r)
			return
		}
		http.Error(w, "stream not available", http.StatusServiceUnavailable)
	})

	// GET /frame — latest JPEG frame for the selected device (polling-based)
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
				log.Printf("/frame: no frame available (serial=%q, count=%d)", serial, frameNoFrameLogCount)
			}
			http.Error(w, "no frame", http.StatusServiceUnavailable)
			return
		}
		frameNoFrameLogCount = 0
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(frame)
	})

	// POST /scrcpy/start — ensure the selected device stream is running (pool handles this automatically)
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

	// POST /scrcpy/stop — stop the selected device stream
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

	// GET /scrcpy/status — streaming status for the selected device
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

	// GET /device/size
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

	// POST /touch
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

		var cmd *exec.Cmd
		switch data.Action {
		case "tap":
			cmd = makeAdbCmd("shell", "input", "tap",
				strconv.Itoa(data.X), strconv.Itoa(data.Y))
		case "swipe":
			dur := data.Duration
			if dur == 0 {
				dur = 300
			}
			cmd = makeAdbCmd("shell", "input", "swipe",
				strconv.Itoa(data.X), strconv.Itoa(data.Y),
				strconv.Itoa(data.X2), strconv.Itoa(data.Y2),
				strconv.Itoa(dur))
		case "keyevent":
			cmd = makeAdbCmd("shell", "input", "keyevent", strconv.Itoa(data.X))
		default:
			http.Error(w, "Unknown action", http.StatusBadRequest)
			return
		}

		go cmd.Run()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// GET /window/phone-size — return the initial phone-sized window dimensions
	http.HandleFunc("/window/phone-size", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"width": phoneWinW, "height": phoneWinH})
	})

	// POST /window/resize — resize the native window from the frontend
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
	// When running via "go run", the exe is in a temp dir without index.html.
	// Fall back to the working directory so "go run main.go" works for development.
	serveDir := exeDir
	if _, err := os.Stat(filepath.Join(serveDir, "index.html")); err != nil {
		if cwd, err := os.Getwd(); err == nil {
			if _, err := os.Stat(filepath.Join(cwd, "index.html")); err == nil {
				serveDir = cwd
				log.Printf("Serving files from working directory: %s", serveDir)
			}
		}
	}
	fs := http.FileServer(http.Dir(serveDir))
	http.Handle("/", fs)

	log.Printf("Map server running at http://localhost%s\n", HTTPPort)
	go func() {
		if err := http.ListenAndServe(HTTPPort, nil); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	time.Sleep(300 * time.Millisecond)

	phoneWinW, phoneWinH = phoneWindowSize()
	mainWebview = webview.New(false)
	defer mainWebview.Destroy()
	mainWebview.SetTitle("Scrcpy Fake GPS")
	mainWebview.SetSize(phoneWinW, phoneWinH, webview.HintNone)
	mainWebview.Navigate("http://localhost" + HTTPPort)
	mainWebview.Run()
}
