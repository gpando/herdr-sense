// Command herdr-sense shows the status of a herdr agent on a Raspberry Pi
// Sense HAT LED matrix. It subscribes to an MQTT topic carrying plain-text
// statuses (working|done|idle|blocked) and paints the 8x8 matrix with a
// solid color per status.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// frameBytes is the full framebuffer size: 64 pixels x 2 bytes (RGB565 LE).
const frameBytes = 64 * 2

// statusColor is a solid RGB color plus its name for logging.
type statusColor struct {
	r, g, b byte
	label   string
}

var statusColors = map[string]statusColor{
	"working": {0, 200, 0, "green"},
	"done":    {0, 102, 255, "blue"},
	"idle":    {255, 140, 0, "amber"},
	"blocked": {220, 0, 0, "red"},
}

var (
	startupColor = statusColor{16, 16, 16, "dim gray"}
	lostColor    = statusColor{48, 48, 48, "dim gray"}
)

// rgb565 packs an 8-bit RGB triple into RGB565 as a little-endian byte pair.
func rgb565(r, g, b byte) (lo, hi byte) {
	u16 := (uint16(r>>3) << 11) | (uint16(g>>2) << 5) | uint16(b>>3)
	return byte(u16 & 0xFF), byte(u16 >> 8)
}

// matrix writes solid-color frames to the Sense HAT framebuffer.
type matrix struct {
	mu   sync.Mutex
	fb   *os.File
	buf  [frameBytes]byte
	last string
}

// paintLocked writes one full-frame solid color. Caller must hold mu.
func (m *matrix) paintLocked(c statusColor) error {
	lo, hi := rgb565(c.r, c.g, c.b)
	for i := 0; i < frameBytes; i += 2 {
		m.buf[i], m.buf[i+1] = lo, hi
	}
	if _, err := m.fb.WriteAt(m.buf[:], 0); err != nil {
		return fmt.Errorf("writing frame: %w", err)
	}
	if err := m.fb.Sync(); err != nil {
		return fmt.Errorf("syncing frame: %w", err)
	}
	return nil
}

// paint renders a solid color without touching the last-status bookkeeping.
func (m *matrix) paint(c statusColor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paintLocked(c)
}

// applyStatus renders the status color and logs accepted state changes.
func (m *matrix) applyStatus(name string, c statusColor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.paintLocked(c); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-sense: rendering status %s: %v\n", name, err)
		return
	}
	if m.last != name {
		fmt.Fprintf(os.Stderr, "herdr-sense: status=%s color=%s\n", name, c.label)
	}
	m.last = name
}

// blank sets the whole matrix to black (all zeros).
func (m *matrix) blank() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.buf[:])
	if _, err := m.fb.WriteAt(m.buf[:], 0); err != nil {
		return fmt.Errorf("clearing frame: %w", err)
	}
	if err := m.fb.Sync(); err != nil {
		return fmt.Errorf("syncing cleared frame: %w", err)
	}
	m.last = ""
	return nil
}

// findSenseFB scans /sys/class/graphics/fb*/name for the Sense HAT
// framebuffer ("RPi-Sense FB") and returns the matching /dev/fbN path.
func findSenseFB() (string, error) {
	matches, err := filepath.Glob("/sys/class/graphics/fb*/name")
	if err != nil {
		return "", fmt.Errorf("scanning framebuffer names: %w", err)
	}
	for _, nameFile := range matches {
		data, err := os.ReadFile(nameFile)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != "RPi-Sense FB" {
			continue
		}
		devPath := filepath.Join("/dev", filepath.Base(filepath.Dir(nameFile)))
		if _, err := os.Stat(devPath); err != nil {
			return "", fmt.Errorf("found %s but %s is not accessible: %w", nameFile, devPath, err)
		}
		return devPath, nil
	}
	return "", fmt.Errorf("no framebuffer named RPi-Sense FB found under /sys/class/graphics (is the Sense HAT attached and its driver loaded?)")
}

// newStatusHandler builds the MQTT message handler that maps payloads to
// status colors. Unknown payloads are logged and ignored.
func newStatusHandler(m *matrix) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		payload := strings.TrimSpace(string(msg.Payload()))
		c, ok := statusColors[payload]
		if !ok {
			fmt.Fprintf(os.Stderr, "herdr-sense: ignoring unknown payload %q\n", payload)
			return
		}
		m.applyStatus(payload, c)
	}
}

// subscribeOnConnect subscribes from within OnConnect so that every
// reconnect re-subscribes; the broker then delivers the retained last
// status automatically.
func subscribeOnConnect(client mqtt.Client, topic string, m *matrix) {
	fmt.Fprintf(os.Stderr, "herdr-sense: connected, subscribing to %s\n", topic)
	if token := client.Subscribe(topic, 0, newStatusHandler(m)); token.Wait() && token.Error() != nil {
		fmt.Fprintf(os.Stderr, "herdr-sense: subscribing to %s: %v\n", topic, token.Error())
	}
}

func run(broker, topic, clientID string) error {
	fbPath, err := findSenseFB()
	if err != nil {
		return fmt.Errorf("locating sense hat framebuffer: %w", err)
	}
	fb, err := os.OpenFile(fbPath, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("opening framebuffer %s (root or video group required): %w", fbPath, err)
	}
	defer fb.Close()

	m := &matrix{fb: fb}
	if err := m.paint(startupColor); err != nil {
		return fmt.Errorf("painting startup color: %w", err)
	}

	opts := mqtt.NewClientOptions().
		AddBroker("tcp://" + broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectTimeout(10 * time.Second)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		subscribeOnConnect(c, topic, m)
	})
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		fmt.Fprintf(os.Stderr, "herdr-sense: connection lost: %v\n", err)
		if err := m.paint(lostColor); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-sense: painting connection-lost color: %v\n", err)
		}
	})
	client := mqtt.NewClient(opts)

	// Single exit path: the signal goroutine disconnects and closes done;
	// main returns through run() so cleanup (blank + fb.Close) runs here.
	done := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		fmt.Fprintf(os.Stderr, "herdr-sense: received %s, shutting down\n", sig)
		client.Disconnect(250)
		close(done)
	}()

	// Connect asynchronously: with ConnectRetry the client keeps retrying
	// in the background until the broker is reachable.
	go func() {
		if token := client.Connect(); token.Wait() && token.Error() != nil {
			fmt.Fprintf(os.Stderr, "herdr-sense: connect failed, will keep retrying: %v\n", token.Error())
		}
	}()

	<-done

	if err := m.blank(); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-sense: clearing matrix on shutdown: %v\n", err)
	}
	return nil
}

func main() {
	broker := flag.String("broker", "localhost:1883", "MQTT broker as host:port (dialled via tcp://)")
	topic := flag.String("topic", "herdr/agent/status", "MQTT topic carrying the agent status")
	clientID := flag.String("client-id", "herdr-sense", "MQTT client ID")
	flag.Parse()

	// Validate the broker address before touching hardware. If this passes an
	// unedited "<HOST:PORT>" placeholder through, paho's AddBroker URL parse
	// fails silently, no server gets registered and the client never retries:
	// the app would look healthy with a blank matrix. Fail loudly instead.
	host, port, err := net.SplitHostPort(*broker)
	if err != nil || host == "" || strings.ContainsAny(host, "<>") {
		fmt.Fprintf(os.Stderr, "herdr-sense: invalid -broker %q: must be host:port (did you forget to edit deploy/herdr-sense.service?)\n", *broker)
		os.Exit(1)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		fmt.Fprintf(os.Stderr, "herdr-sense: invalid -broker %q: port must be a number between 1 and 65535\n", *broker)
		os.Exit(1)
	}

	if err := run(*broker, *topic, *clientID); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-sense: %v\n", err)
		os.Exit(1)
	}
}
