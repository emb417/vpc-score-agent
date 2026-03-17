package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	vpxPluginWSURL      = "ws://127.0.0.1:3123"
	vpcAPIBase          = "https://virtualpinballchat.com"
	vpcEventsPath       = "/vpc/api/v2/events"
	reconnectDelay      = 5 * time.Second
	bufferFlushInterval = 10 * time.Second
	maxBufferSize       = 500
	httpTimeoutSeconds  = 10 * time.Second
	stopFileName        = "vpc-score-agent.stop"
)

// ---------------------------------------------------------------------------
// CLI args
// ---------------------------------------------------------------------------

type Config struct {
	Stop     bool
	VpsID    string
	Version  string
	ROM      string
	UserID   string
	Username string
}

func parseArgs() Config {
	cfg := Config{}
	flag.BoolVar(&cfg.Stop, "stop", false, "Signal a running agent to stop")
	flag.StringVar(&cfg.VpsID, "vpsId", "", "VPS table ID (PinUp Popper CUSTOM2)")
	flag.StringVar(&cfg.Version, "version", "", "Table version number (PinUp Popper GAMEVER)")
	flag.StringVar(&cfg.ROM, "rom", "", "ROM name")
	flag.StringVar(&cfg.UserID, "userId", "", "Discord user ID")
	flag.StringVar(&cfg.Username, "username", "", "Discord username")
	flag.Parse()
	return cfg
}

// ---------------------------------------------------------------------------
// Event types
// ---------------------------------------------------------------------------

// RawEvent is a parsed event from the VPX plugin WebSocket.
type RawEvent struct {
	Type          string       `json:"type"`
	Timestamp     string       `json:"timestamp"`
	ROM           string       `json:"rom"`
	Players       int          `json:"players"`
	CurrentPlayer int          `json:"current_player"`
	CurrentBall   int          `json:"current_ball"`
	Scores        []ScoreEntry `json:"scores"`
	GameDuration  int          `json:"game_duration"`
}

type ScoreEntry struct {
	Player string `json:"player"`
	Score  string `json:"score"`
}

// EnrichedEvent is what we POST to vpc-data.
type EnrichedEvent struct {
	UserID        string          `json:"userId"`
	Username      string          `json:"username"`
	VpsID         string          `json:"vpsId,omitempty"`
	VersionNumber string          `json:"versionNumber,omitempty"`
	ROM           string          `json:"rom,omitempty"`
	Event         string          `json:"event"`
	Timestamp     string          `json:"timestamp"`
	Payload       json.RawMessage `json:"payload"`
}

// ---------------------------------------------------------------------------
// Event buffer
// ---------------------------------------------------------------------------

type Buffer struct {
	mu    sync.Mutex
	items []EnrichedEvent
}

func (b *Buffer) Push(e EnrichedEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) >= maxBufferSize {
		// Drop oldest
		b.items = b.items[1:]
	}
	b.items = append(b.items, e)
}

func (b *Buffer) Drain() []EnrichedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return nil
	}
	drained := make([]EnrichedEvent, len(b.items))
	copy(drained, b.items)
	b.items = b.items[:0]
	return drained
}

func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// ---------------------------------------------------------------------------
// Event enrichment
// ---------------------------------------------------------------------------

func enrich(cfg Config, raw RawEvent) EnrichedEvent {
	ts := raw.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}

	payload := buildPayload(raw)

	return EnrichedEvent{
		UserID:        cfg.UserID,
		Username:      cfg.Username,
		VpsID:         cfg.VpsID,
		VersionNumber: cfg.Version,
		ROM:           cfg.ROM,
		Event:         raw.Type,
		Timestamp:     ts,
		Payload:       payload,
	}
}

func buildPayload(raw RawEvent) json.RawMessage {
	var p interface{}

	switch raw.Type {
	case "game_end":
		p = map[string]interface{}{
			"players":       raw.Players,
			"scores":        raw.Scores,
			"game_duration": raw.GameDuration,
		}
	case "current_scores":
		p = map[string]interface{}{
			"players":        raw.Players,
			"scores":         raw.Scores,
			"current_ball":   raw.CurrentBall,
			"current_player": raw.CurrentPlayer,
		}
	default:
		p = map[string]interface{}{}
	}

	b, _ := json.Marshal(p)
	return json.RawMessage(b)
}

// ---------------------------------------------------------------------------
// HTTP forwarding
// ---------------------------------------------------------------------------

func postEvent(client *http.Client, event EnrichedEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	resp, err := client.Post(
		vpcAPIBase+vpcEventsPath,
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// flushBuffer drains buffered events and attempts to POST each one.
// Failed events are re-buffered.
func flushBuffer(client *http.Client, buf *Buffer) {
	events := buf.Drain()
	if len(events) == 0 {
		return
	}
	log.Printf("Flushing %d buffered event(s)", len(events))
	var failed []EnrichedEvent
	for _, e := range events {
		if err := postEvent(client, e); err != nil {
			log.Printf("Flush failed for event %s: %v — re-buffering", e.Event, err)
			failed = append(failed, e)
		}
	}
	for _, e := range failed {
		buf.Push(e)
	}
}

// sendOrBuffer attempts to POST immediately; on failure, buffers the event.
func sendOrBuffer(client *http.Client, buf *Buffer, event EnrichedEvent) {
	if err := postEvent(client, event); err != nil {
		log.Printf("POST failed for %s: %v — buffering", event.Event, err)
		buf.Push(event)
	} else {
		log.Printf("Event sent: %s", event.Event)
	}
}

// ---------------------------------------------------------------------------
// Minimal WebSocket client (stdlib only)
// ---------------------------------------------------------------------------

type WSConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func wsConnect(url string) (*WSConn, error) {
	// Parse ws://host:port/path
	addr := strings.TrimPrefix(url, "ws://")
	host := addr
	path := "/"
	if idx := strings.Index(addr, "/"); idx != -1 {
		host = addr[:idx]
		path = addr[idx:]
	}

	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Generate Sec-WebSocket-Key
	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)

	// Send HTTP upgrade request
	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, host, key,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write upgrade: %w", err)
	}

	// Read and validate HTTP 101 response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 101 {
		conn.Close()
		return nil, fmt.Errorf("expected 101, got %d", resp.StatusCode)
	}

	// Validate Sec-WebSocket-Accept
	magic := "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New()
	h.Write([]byte(key + magic))
	expected := base64.StdEncoding.EncodeToString(h.Sum(nil))
	got := resp.Header.Get("Sec-Websocket-Accept")
	if got == "" {
		got = resp.Header.Get("Sec-WebSocket-Accept")
	}
	if got != expected {
		conn.Close()
		return nil, fmt.Errorf("bad Sec-WebSocket-Accept: got %q want %q", got, expected)
	}

	return &WSConn{conn: conn, br: br}, nil
}

// ReadMessage reads a single WebSocket text frame and returns the payload.
func (ws *WSConn) ReadMessage() ([]byte, error) {
	// Read 2-byte frame header
	header := make([]byte, 2)
	if _, err := io.ReadFull(ws.br, header); err != nil {
		return nil, err
	}

	// fin := header[0] & 0x80 != 0  // we don't use this but note it
	opcode := header[0] & 0x0F
	masked := header[1]&0x80 != 0
	payloadLen := int64(header[1] & 0x7F)

	// Extended payload length
	switch payloadLen {
	case 126:
		var ext uint16
		if err := binary.Read(ws.br, binary.BigEndian, &ext); err != nil {
			return nil, err
		}
		payloadLen = int64(ext)
	case 127:
		var ext uint64
		if err := binary.Read(ws.br, binary.BigEndian, &ext); err != nil {
			return nil, err
		}
		payloadLen = int64(ext)
	}

	// Masking key (server→client frames are never masked per spec, but handle it)
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(ws.br, maskKey[:]); err != nil {
			return nil, err
		}
	}

	// Read payload
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(ws.br, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	// Connection close frame
	if opcode == 0x8 {
		return nil, io.EOF
	}

	return payload, nil
}

func (ws *WSConn) Close() {
	ws.conn.Close()
}

// ---------------------------------------------------------------------------
// Stop signal (close script support)
// ---------------------------------------------------------------------------

// writeStopFile creates a sentinel file that signals any running agent to stop.
func writeStopFile() {
	f, err := os.Create(stopFileName)
	if err != nil {
		log.Printf("Could not write stop file: %v", err)
		return
	}
	f.Close()
	log.Println("Stop file written — running agent will exit shortly")
}

// watchStopFile polls for the sentinel file and closes done when found.
func watchStopFile(done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
			if _, err := os.Stat(stopFileName); err == nil {
				log.Println("Stop file detected — shutting down")
				os.Remove(stopFileName)
				close(done)
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := parseArgs()

	// Logging
	logFile, err := os.OpenFile("vpc-score-agent.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		mw := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(mw)
		defer logFile.Close()
	}
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)

	// --stop: write sentinel file and exit
	if cfg.Stop {
		writeStopFile()
		return
	}

	log.Println("--- vpc-score-agent STARTED ---")
	log.Printf("vpsId=%s version=%s rom=%s userId=%s username=%s",
		cfg.VpsID, cfg.Version, cfg.ROM, cfg.UserID, cfg.Username)

	if cfg.UserID == "" || cfg.Username == "" {
		log.Fatal("--userId and --username are required")
	}

	buf := &Buffer{}
	client := &http.Client{Timeout: httpTimeoutSeconds}
	done := make(chan struct{})

	// OS signal handler (Ctrl+C / SIGTERM)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("OS signal received — shutting down")
		select {
		case <-done:
		default:
			close(done)
		}
	}()

	// Stop file watcher
	go watchStopFile(done)

	// Buffer flush ticker
	go func() {
		ticker := time.NewTicker(bufferFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				// Final flush on shutdown
				flushBuffer(client, buf)
				return
			case <-ticker.C:
				flushBuffer(client, buf)
			}
		}
	}()

	// WebSocket loop — reconnects until done
	for {
		select {
		case <-done:
			log.Println("--- vpc-score-agent STOPPED ---")
			return
		default:
		}

		log.Printf("Connecting to VPX plugin at %s", vpxPluginWSURL)
		ws, err := wsConnect(vpxPluginWSURL)
		if err != nil {
			log.Printf("WebSocket connect failed: %v — retrying in %s", err, reconnectDelay)
			select {
			case <-done:
				log.Println("--- vpc-score-agent STOPPED ---")
				return
			case <-time.After(reconnectDelay):
				continue
			}
		}

		log.Println("Connected to VPX plugin")

		// Set a read deadline so we can check done periodically
	readLoop:
		for {
			select {
			case <-done:
				ws.Close()
				break readLoop
			default:
			}

			ws.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			msg, err := ws.ReadMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected — just loop and check done
					continue
				}
				log.Printf("WebSocket read error: %v — reconnecting", err)
				ws.Close()
				break readLoop
			}

			// Parse raw event
			var raw RawEvent
			if err := json.Unmarshal(msg, &raw); err != nil {
				log.Printf("Failed to parse event: %v — skipping", err)
				continue
			}

			// Skip events we don't care about
			if raw.Type == "connected" {
				log.Printf("Plugin handshake: %s", string(msg))
				continue
			}

			log.Printf("Received event: %s", raw.Type)
			enriched := enrich(cfg, raw)
			sendOrBuffer(client, buf, enriched)
		}

		select {
		case <-done:
			log.Println("--- vpc-score-agent STOPPED ---")
			return
		case <-time.After(reconnectDelay):
			log.Println("Reconnecting...")
		}
	}
}
