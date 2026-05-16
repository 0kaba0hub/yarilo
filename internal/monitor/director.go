package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	directorProto = "yarilo-director"
	directorMajor = 1
	directorMinor = 0

	reconnectBase = 2 * time.Second
	reconnectMax  = 60 * time.Second
)

// BackendEvent describes a change in the director ring received by the monitor.
type BackendEvent struct {
	IP    string
	Port  int // populated from HOST handshake lines; 0 for RING-CHANGE events
	Tag   string
	Event string // "up" | "down" | "flush"
}

// DirectorClient maintains a persistent TCP connection to the director.
// It reads ring-change events (which drive the backend pool) and writes
// health reports (BACKEND-UP / BACKEND-FLUSH) on the same connection.
//
// Reconnects automatically with exponential backoff.
type DirectorClient struct {
	addr   string
	events chan BackendEvent

	wrMu sync.Mutex
	conn net.Conn
	wr   *bufio.Writer
}

// NewDirectorClient creates a client for the given director address.
func NewDirectorClient(addr string) *DirectorClient {
	return &DirectorClient{
		addr:   addr,
		events: make(chan BackendEvent, 64),
	}
}

// Events returns a channel that receives ring changes from the director.
// The channel is never closed; it blocks when nothing is happening.
func (d *DirectorClient) Events() <-chan BackendEvent { return d.events }

// Run connects to the director and processes events until ctx is cancelled.
// Reconnects with exponential backoff on failure.
func (d *DirectorClient) Run(ctx context.Context) {
	backoff := reconnectBase
	for {
		if err := d.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("monitor: director connection lost, reconnecting",
				"err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff *= 2
			if backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}
}

// runOnce opens one connection, reads events until it drops, and returns.
func (d *DirectorClient) runOnce(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", d.addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", d.addr, err)
	}
	defer func() {
		conn.Close()
		d.wrMu.Lock()
		d.conn = nil
		d.wr = nil
		d.wrMu.Unlock()
	}()

	conn.SetDeadline(time.Now().Add(15 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(conn)
	wr := bufio.NewWriter(conn)

	// Parse server handshake. Emit BackendEvent for every HOST line.
	if err := d.readHandshake(rd); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// Send client handshake.
	fmt.Fprintf(wr, "VERSION\t%s\t%d\t%d\n", directorProto, directorMajor, directorMinor) //nolint:errcheck
	fmt.Fprintf(wr, "ME\t127.0.0.1\t0\t0\n")                                              //nolint:errcheck
	fmt.Fprintf(wr, "DONE\n")                                                             //nolint:errcheck
	if err := wr.Flush(); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}

	conn.SetDeadline(time.Time{}) //nolint:errcheck

	d.wrMu.Lock()
	d.conn = conn
	d.wr = wr
	d.wrMu.Unlock()

	slog.Info("monitor: connected to director", "addr", d.addr)

	// Read event loop in a goroutine so ctx cancellation can close the conn.
	readErr := make(chan error, 1)
	go func() { readErr <- d.readLoop(rd) }()

	select {
	case <-ctx.Done():
		conn.Close()
		<-readErr
		return nil
	case err := <-readErr:
		return err
	}
}

// readHandshake consumes the server's VERSION / HOST-HAND-* / DONE sequence
// and emits BackendEvent{Event:"up"} for each live HOST line.
func (d *DirectorClient) readHandshake(rd *bufio.Reader) error {
	inHand := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\n\r")
		fields := strings.Split(line, "\t")

		switch fields[0] {
		case "VERSION":
			// ignore
		case "HOST-HAND-START":
			inHand = true
		case "HOST-HAND-END":
			inHand = false
		case "HOST":
			// HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{hostname}
			if inHand && len(fields) >= 4 {
				ev := parseHostLine(fields)
				if ev != nil {
					d.emit(*ev)
				}
			}
		case "DONE":
			return nil
		}
	}
}

// readLoop processes unsolicited pushes from the director after handshake.
func (d *DirectorClient) readLoop(rd *bufio.Reader) error {
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("connection closed")
			}
			return err
		}
		line = strings.TrimRight(line, "\n\r")
		fields := strings.Split(line, "\t")

		switch fields[0] {
		case "RING-CHANGE":
			// RING-CHANGE\t{ip}\t{event}\t{tag}
			if len(fields) >= 3 {
				d.emit(BackendEvent{
					IP:    fields[1],
					Tag:   safeGet(fields, 3),
					Event: fields[2],
				})
			}
		case "PING":
			d.sendRaw("PONG")
		case "OK":
			// response to our BACKEND-UP/FLUSH commands — ignore
		}
	}
}

// ReportUp sends BACKEND-UP\t{ip}\t{port}\t{tag} to the director.
func (d *DirectorClient) ReportUp(ip string, port int, tag string) {
	d.sendRaw(fmt.Sprintf("BACKEND-UP\t%s\t%d\t%s", ip, port, tag))
}

// ReportFlush sends BACKEND-FLUSH\t{ip} to mark the backend as temporarily unavailable.
func (d *DirectorClient) ReportFlush(ip string) {
	d.sendRaw(fmt.Sprintf("BACKEND-FLUSH\t%s", ip))
}

func (d *DirectorClient) sendRaw(line string) {
	d.wrMu.Lock()
	defer d.wrMu.Unlock()
	if d.wr == nil {
		return
	}
	fmt.Fprintln(d.wr, line) //nolint:errcheck
	d.wr.Flush()             //nolint:errcheck
}

func (d *DirectorClient) emit(ev BackendEvent) {
	select {
	case d.events <- ev:
	default:
		slog.Warn("monitor: event channel full, dropping", "event", ev.Event, "ip", ev.IP)
	}
}

// parseHostLine parses a HOST handshake line and returns an "up" event.
// HOST\t{ip}\t{port}\t{tag}\tD{down_ts}\tU{up_ts}\t{hostname}
func parseHostLine(fields []string) *BackendEvent {
	if len(fields) < 3 {
		return nil
	}
	var port int
	fmt.Sscanf(fields[2], "%d", &port)
	return &BackendEvent{
		IP:    fields[1],
		Port:  port,
		Tag:   safeGet(fields, 3),
		Event: "up",
	}
}

func safeGet(ss []string, i int) string {
	if i < len(ss) {
		return ss[i]
	}
	return ""
}
