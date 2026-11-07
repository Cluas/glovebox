package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// event is a health check state change or a lifecycle note. Events are kept in memory for the UI
// and appended to events.jsonl, so alerts and log shipping can catch up after a power cut.
type event struct {
	Seq   int64     `json:"seq"`
	Time  time.Time `json:"time"`
	Check string    `json:"check"`
	State string    `json:"state"`         // fail, ok or info
	Msg   string    `json:"msg,omitempty"` // error text of a failure, version for started
	// seconds: how long a recovered check was failing, the system uptime for started, the clock
	// jump for clock_fixed
	Dur int64    `json:"dur,omitempty"`
	Log []string `json:"log,omitempty"` // recent mihomo warnings attached to a failure
	// the message of an "sms" event; Msg is the sender. It is kept out of shipped logs.
	SMS *smsEvent `json:"sms,omitempty"`
}

const (
	eventKeep    = 200       // events kept in memory
	eventFileMax = 256 << 10 // events.jsonl is compacted beyond this size
)

type eventLog struct {
	path string // empty keeps events in memory only
	mu   sync.Mutex
	ring []event
	seq  int64
	size int64
	subs []chan struct{}
}

func openEventLog(path string) *eventLog {
	l := &eventLog{path: path}
	if path == "" {
		return l
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("events: %v", err)
		}
		return l
	}
	clean := len(b) == 0 || b[len(b)-1] == '\n'
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil || e.Seq <= l.seq {
			// a power cut can leave a run of NUL bytes or a half-written line
			clean = false
			continue
		}
		l.push(e)
	}
	l.size = int64(len(b))
	if !clean {
		l.compactLocked()
	}
	return l
}

func (l *eventLog) push(e event) {
	l.ring = append(l.ring, e)
	if len(l.ring) > eventKeep {
		l.ring = append(l.ring[:0:0], l.ring[len(l.ring)-eventKeep:]...)
	}
	l.seq = e.Seq
}

// add assigns the next sequence number (and the current time when unset), persists the event and
// wakes the subscribers
func (l *eventLog) add(e event) event {
	l.mu.Lock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Time = e.Time.Round(0)
	e.Seq = l.seq + 1
	l.push(e)
	l.appendLocked(e)
	subs := l.subs
	l.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	return e
}

func (l *eventLog) appendLocked(e event) {
	if l.path == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		log.Printf("events: %v", err)
		return
	}
	b = append(b, '\n')
	if l.size+int64(len(b)) > eventFileMax {
		l.compactLocked()
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("events: %v", err)
		return
	}
	defer f.Close()
	n, err := f.Write(b)
	l.size += int64(n)
	if err == nil {
		// the device loses power without shutting down
		err = f.Sync()
	}
	if err != nil {
		log.Printf("events: %v", err)
	}
}

// compactLocked rewrites the file with the newest events that fill half of eventFileMax, so it is
// not compacted again right away
func (l *eventLog) compactLocked() {
	if l.path == "" {
		return
	}
	var lines [][]byte
	size := 0
	for i := len(l.ring) - 1; i >= 0; i-- {
		b, _ := json.Marshal(l.ring[i])
		if size += len(b) + 1; size > eventFileMax/2 && len(lines) > 0 {
			break
		}
		lines = append(lines, b)
	}
	var buf bytes.Buffer
	for i := len(lines) - 1; i >= 0; i-- {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}
	if err := writeFileAtomic(l.path, buf.Bytes(), 0o600); err != nil {
		log.Printf("events: %v", err)
		return
	}
	l.size = int64(buf.Len())
}

// recent returns up to n events, newest first
func (l *eventLog) recent(n int) []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]event, 0, min(n, len(l.ring)))
	for i := len(l.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.ring[i])
	}
	return out
}

// since returns the events after seq that are still kept, oldest first
func (l *eventLog) since(seq int64) []event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []event
	for _, e := range l.ring {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

func (l *eventLog) lastSeq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// restamp shifts the time of the events from seq on by d: they were recorded while the wall clock
// was wrong
func (l *eventLog) restamp(seq int64, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for i := range l.ring {
		if l.ring[i].Seq >= seq {
			l.ring[i].Time = l.ring[i].Time.Add(d)
			changed = true
		}
	}
	if changed {
		l.compactLocked()
	}
}

// subscribe returns a channel that receives a value (coalesced) whenever an event is added
func (l *eventLog) subscribe() <-chan struct{} {
	c := make(chan struct{}, 1)
	l.mu.Lock()
	l.subs = append(l.subs, c)
	l.mu.Unlock()
	return c
}
