package locks

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func partSum(t *testing.T, part string) (float64, uint64) {
	t.Helper()
	h, err := clientPart.GetMetricWithLabelValues(part)
	if err != nil {
		t.Fatalf("get %s: %v", part, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %s: %v", part, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

// slowServer answers the version handshake and then holds every command for
// hold before replying OK, so a caller occupies its pool slot for that long.
func slowServer(t *testing.T, hold time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				r := newReader(conn)
				for {
					fields, rerr := r.readFields()
					if rerr != nil {
						return
					}
					switch fields[0] {
					case cmdVersion:
						_ = writeFields(conn, cmdVersion, protocolVersion, respOK)
					default:
						time.Sleep(hold)
						_ = writeFields(conn, respOK, "lock-id")
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// The slot clock spans the wait for a pool connection.
//
// A pool of one and two callers: the second cannot start until the first is
// done, and that wait is inside Lock -- invisible to the attempt counter and to
// the server, which is why a service answering in milliseconds still produced
// acquisitions taking seconds (#1650). A clock placed after the select reports
// zero here, which is the mistake this test exists to make impossible: the same
// one made in #1648 and again in #1651.
func TestTheSlotClockSpansTheWaitForAConnection(t *testing.T) {
	const hold = 200 * time.Millisecond
	addr := slowServer(t, hold)

	c, err := NewClient(context.Background(), func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}, WithPoolSize(1))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close() //nolint:errcheck

	slotBefore, _ := partSum(t, "slot")
	rtBefore, _ := partSum(t, "roundtrip")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Lock(context.Background(), "mbox:u@x:INBOX", "owner", time.Minute)
		}()
	}
	wg.Wait()

	slot, _ := partSum(t, "slot")
	rt, _ := partSum(t, "roundtrip")

	// One of the two waited behind the other for most of a hold.
	if waited := slot - slotBefore; waited < hold.Seconds()/2 {
		t.Errorf("the slot clock recorded %.3fs while one caller queued behind a %v command: "+
			"it is not spanning the wait", waited, hold)
	}
	// And the exchange is timed too, or the split reports a wait against nothing.
	if spent := rt - rtBefore; spent < hold.Seconds() {
		t.Errorf("the exchange clock recorded %.3fs for two commands held %v each", spent, hold)
	}
}
