package main

import (
	"bufio"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// scrcpy's on-device server (Apache 2.0, github.com/Genymobile/scrcpy). It must match the
// version string passed to it exactly.
//
//go:embed third_party/scrcpy/scrcpy-server-v3.3.4
var scrcpyServer []byte

const scrcpyVersion = "3.3.4"

// Messages to the browser: [u8 type][u32 length][payload].
const (
	msgMeta byte = iota
	msgConfig
	msgKey
	msgDelta
	msgEnd
)

const (
	flagConfig   = uint64(1) << 63
	flagKeyFrame = uint64(1) << 62
	idleStop     = 15 * time.Second
	clientQueue  = 256
)

func frameMessage(kind byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = kind
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

type packet struct {
	key  bool
	data []byte
}

type streamClient struct {
	out     chan []byte
	needKey bool
}

// phoneStream is one scrcpy session (the phone's screen, or its camera) shared by every viewer.
//
// Protocol (scrcpy v3, forward tunnel): the first socket gets a dummy 0 byte, then codec meta
// (u32 codec id, u32 width, u32 height), then packets of u64 pts+flags, u32 length, data. The
// second socket carries control messages.
type phoneStream struct {
	adb    *adbTool
	serial string
	source string
	port   int

	mu      sync.Mutex
	clients map[*streamClient]struct{}
	meta    []byte
	config  []byte
	gop     []packet
	stopped bool
	idle    *time.Timer
	ready   chan struct{}
	err     error

	proc    *exec.Cmd
	output  *tailBuffer
	video   net.Conn
	control net.Conn
	ctlMu   sync.Mutex
	onEnd   func(reason string)
}

func newPhoneStream(adb *adbTool, serial, source string, port int) *phoneStream {
	return &phoneStream{adb: adb, serial: serial, source: source, port: port, clients: map[*streamClient]struct{}{}, ready: make(chan struct{}), output: &tailBuffer{}}
}

func (s *phoneStream) start() {
	err := s.connectPhone()
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.ready)
	if err != nil {
		s.stop(err.Error())
		return
	}
	go s.readVideo()
}

// wait blocks until the stream is running or has failed.
func (s *phoneStream) wait() error {
	<-s.ready
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *phoneStream) connectPhone() error {
	jar, err := os.CreateTemp("", "scrcpy-server-*.jar")
	if err != nil {
		return err
	}
	defer os.Remove(jar.Name())
	if _, err := jar.Write(scrcpyServer); err != nil {
		return err
	}
	jar.Close()

	adbPath := s.adb.find()
	if adbPath == "" {
		return errors.New("Android tools aren't installed yet")
	}
	if out, err := s.adb.run(30*time.Second, "-s", s.serial, "push", jar.Name(), "/data/local/tmp/scrcpy-server.jar"); err != nil {
		return fmt.Errorf("could not copy to the phone: %s", out)
	}
	scid := fmt.Sprintf("%08x", rand.Uint32()&0x7fffffff)
	if out, err := s.adb.run(10*time.Second, "-s", s.serial, "forward", fmt.Sprintf("tcp:%d", s.port), "localabstract:scrcpy_"+scid); err != nil {
		return fmt.Errorf("could not reach the phone: %s", out)
	}

	params := []string{"scid=" + scid, "log_level=info", "tunnel_forward=true", "audio=false", "video_codec=h264", "send_device_meta=false", "cleanup=true", "max_size=1280"}
	if s.source == "camera" {
		params = append(params, "video_source=camera", "camera_facing=back", "control=false")
	} else {
		params = append(params, "stay_awake=true")
	}
	args := append([]string{"-s", s.serial, "shell", "CLASSPATH=/data/local/tmp/scrcpy-server.jar", "app_process", "/", "com.genymobile.scrcpy.Server", scrcpyVersion}, params...)
	s.proc = exec.Command(adbPath, args...)
	s.proc.Stdout = s.output
	s.proc.Stderr = s.output
	if err := s.proc.Start(); err != nil {
		return err
	}
	go func() {
		_ = s.proc.Wait()
		s.stop(s.output.lastError("the phone stream ended"))
	}()

	if s.video, err = s.dial(true); err != nil {
		return err
	}
	if s.source == "display" {
		if s.control, err = s.dial(false); err != nil {
			return err
		}
		go io.Copy(io.Discard, s.control) // clipboard and acks from the phone: not used
	}
	return nil
}

// dial retries because adb accepts the connection before the phone's server is listening; the
// first socket is only real once its dummy byte arrives.
func (s *phoneStream) dial(expectDummy bool) (net.Conn, error) {
	for i := 0; i < 100; i++ {
		if s.isStopped() {
			return nil, errors.New("stopped")
		}
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(s.port))
		if err == nil {
			if !expectDummy {
				return conn, nil
			}
			one := make([]byte, 1)
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(conn, one); err == nil {
				conn.SetReadDeadline(time.Time{})
				return conn, nil
			}
			conn.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New(s.output.lastError("could not connect to the phone"))
}

func (s *phoneStream) readVideo() {
	r := bufio.NewReaderSize(s.video, 1<<16)
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		s.stop("the phone stream closed")
		return
	}
	meta, _ := json.Marshal(map[string]uint32{
		"codec": binary.BigEndian.Uint32(head[0:4]), "width": binary.BigEndian.Uint32(head[4:8]), "height": binary.BigEndian.Uint32(head[8:12]),
	})
	s.mu.Lock()
	s.meta = meta
	s.mu.Unlock()
	s.broadcast(frameMessage(msgMeta, meta))
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			s.stop("the phone stream closed")
			return
		}
		flags := binary.BigEndian.Uint64(head[0:8])
		size := binary.BigEndian.Uint32(head[8:12])
		if size > 32<<20 {
			s.stop("the phone sent a packet too large to be video")
			return
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			s.stop("the phone stream closed")
			return
		}
		s.packet(flags, data)
	}
}

func (s *phoneStream) packet(flags uint64, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flags&flagConfig != 0 {
		s.config = data
		s.gop = nil
		for c := range s.clients {
			s.send(c, frameMessage(msgConfig, data))
		}
		return
	}
	key := flags&flagKeyFrame != 0
	if key {
		s.gop = nil
	}
	s.gop = append(s.gop, packet{key: key, data: data})
	kind := msgDelta
	if key {
		kind = msgKey
	}
	message := frameMessage(kind, data)
	for c := range s.clients {
		// A viewer that fell behind skips ahead to the next keyframe instead.
		if c.needKey && !key {
			continue
		}
		if c.needKey && s.config != nil {
			s.send(c, frameMessage(msgConfig, s.config))
		}
		c.needKey = false
		s.send(c, message)
	}
}

// send queues without blocking; a full queue marks the viewer as needing a keyframe.
func (s *phoneStream) send(c *streamClient, message []byte) {
	select {
	case c.out <- message:
	default:
		c.needKey = true
	}
}

func (s *phoneStream) broadcast(message []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		s.send(c, message)
	}
}

// addClient replays what a late viewer needs to start decoding, then keeps it fed.
func (s *phoneStream) addClient() (*streamClient, func()) {
	s.mu.Lock()
	c := &streamClient{out: make(chan []byte, clientQueue+len(s.gop)+2)}
	if s.idle != nil {
		s.idle.Stop()
	}
	if s.meta != nil {
		c.out <- frameMessage(msgMeta, s.meta)
	}
	if s.config != nil {
		c.out <- frameMessage(msgConfig, s.config)
	}
	for _, p := range s.gop {
		kind := msgDelta
		if p.key {
			kind = msgKey
		}
		select {
		case c.out <- frameMessage(kind, p.data):
		default:
		}
	}
	stopped := s.stopped
	if !stopped {
		s.clients[c] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		close(c.out)
	}
	return c, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.clients[c]; !ok {
			return
		}
		delete(s.clients, c)
		if len(s.clients) == 0 && !s.stopped {
			s.idle = time.AfterFunc(idleStop, func() { s.stop("no viewers") })
		}
	}
}

func (s *phoneStream) sendControl(message []byte) {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	if s.control != nil {
		s.control.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = s.control.Write(message)
	}
}

func (s *phoneStream) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *phoneStream) stop(reason string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	if s.idle != nil {
		s.idle.Stop()
	}
	end := frameMessage(msgEnd, []byte(reason))
	for c := range s.clients {
		select {
		case c.out <- end:
		default:
		}
		close(c.out)
	}
	s.clients = map[*streamClient]struct{}{}
	s.mu.Unlock()

	if s.video != nil {
		s.video.Close()
	}
	if s.control != nil {
		s.control.Close()
	}
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Kill()
	}
	go s.adb.run(5*time.Second, "-s", s.serial, "forward", "--remove", fmt.Sprintf("tcp:%d", s.port))
	log.Printf("%s stream ended: %s", s.source, reason)
	if s.onEnd != nil {
		s.onEnd(reason)
	}
}

// tailBuffer keeps the last few KB of the phone server's output, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 8192 {
		t.buf = t.buf[len(t.buf)-8192:]
	}
	return len(p), nil
}

var errorLine = regexp.MustCompile(`(?m)ERROR:.*$`)

func (t *tailBuffer) lastError(fallback string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := errorLine.FindAll(t.buf, -1); len(m) > 0 {
		return string(m[len(m)-1])
	}
	return fallback
}
