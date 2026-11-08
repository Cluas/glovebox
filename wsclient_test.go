package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsPeer is the server side of a test WebSocket
type wsPeer struct {
	c  net.Conn
	br *bufio.Reader
}

// wsAccept upgrades a request the way a WebSocket server does
func wsAccept(w http.ResponseWriter, r *http.Request) (*wsPeer, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "not a websocket request", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	brw.Flush()
	return &wsPeer{c: c, br: brw.Reader}, nil
}

// writeFrame sends an unmasked frame, as servers do
func (p *wsPeer) writeFrame(fin bool, op byte, data []byte) error {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	switch n := len(data); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = binary.BigEndian.AppendUint16(append(hdr, 126), uint16(n))
	default:
		hdr = binary.BigEndian.AppendUint64(append(hdr, 127), uint64(n))
	}
	_, err := p.c.Write(append(hdr, data...))
	return err
}

// readFrame reads a frame from the client, which must mask it
func (p *wsPeer) readFrame() (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(p.br, h[:]); err != nil {
		return 0, nil, err
	}
	if h[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame not masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var l [2]byte
		io.ReadFull(p.br, l[:])
		n = uint64(binary.BigEndian.Uint16(l[:]))
	case 127:
		var l [8]byte
		io.ReadFull(p.br, l[:])
		n = binary.BigEndian.Uint64(l[:])
	}
	var mask [4]byte
	io.ReadFull(p.br, mask[:])
	data := make([]byte, n)
	if _, err := io.ReadFull(p.br, data); err != nil {
		return 0, nil, err
	}
	for i := range data {
		data[i] ^= mask[i&3]
	}
	return h[0] & 0x0f, data, nil
}

func wsTestServer(t *testing.T, serve func(p *wsPeer)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := wsAccept(w, r)
		if err != nil {
			return
		}
		defer p.c.Close()
		serve(p)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func wsTestClient() *http.Client { return &http.Client{Transport: newWSTransport()} }

func TestWSMessages(t *testing.T) {
	got := make(chan [][]byte, 1)
	u := wsTestServer(t, func(p *wsPeer) {
		// a message in two fragments with a ping between them
		p.writeFrame(false, wsText, []byte("hel"))
		p.writeFrame(true, wsPing, []byte("p1"))
		p.writeFrame(true, 0, []byte("lo"))
		var in [][]byte
		for range 4 {
			_, data, err := p.readFrame()
			if err != nil {
				break
			}
			in = append(in, data)
		}
		got <- in
		p.writeFrame(true, wsClose, []byte{0x03, 0xe8})
		p.readFrame()
	})
	c, err := wsDial(context.Background(), wsTestClient(), u)
	if err != nil {
		t.Fatal(err)
	}
	op, msg, err := c.ReadMessage()
	if err != nil || op != wsText || string(msg) != "hello" {
		t.Fatalf("read %d %q %v", op, msg, err)
	}
	// the three length encodings
	big := bytes.Repeat([]byte("x"), 70000)
	for _, m := range [][]byte{[]byte("short"), bytes.Repeat([]byte("y"), 300), big} {
		if err := c.WriteMessage(wsBinary, m); err != nil {
			t.Fatal(err)
		}
	}
	in := <-got
	if len(in) != 4 || string(in[0]) != "p1" || string(in[1]) != "short" || len(in[2]) != 300 || !bytes.Equal(in[3], big) {
		t.Fatalf("server got %d frames", len(in))
	}
	if _, _, err := c.ReadMessage(); !errors.Is(err, errWSClosed) {
		t.Fatalf("after close: %v", err)
	}
	c.Close()
}

func TestWSRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Handshake-Msg", "app not found")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := wsDial(context.Background(), wsTestClient(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	var he *wsHandshakeError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Header.Get("Handshake-Msg") != "app not found" {
		t.Fatalf("error %v", err)
	}
	if _, err := wsDial(context.Background(), wsTestClient(), "https://example.com"); err == nil {
		t.Error("https URL accepted")
	}
}

func TestWSBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, brw, _ := http.NewResponseController(w).Hijack()
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
		brw.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	if _, err := wsDial(context.Background(), wsTestClient(), "ws"+strings.TrimPrefix(srv.URL, "http")); err == nil || !strings.Contains(err.Error(), "Accept") {
		t.Fatalf("error %v", err)
	}
}

func TestWSTooLarge(t *testing.T) {
	u := wsTestServer(t, func(p *wsPeer) {
		var hdr []byte
		hdr = binary.BigEndian.AppendUint64(append(hdr, 0x80|wsBinary, 127), wsMaxMsg+1)
		p.c.Write(hdr)
		time.Sleep(100 * time.Millisecond)
	})
	c, err := wsDial(context.Background(), wsTestClient(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.ReadMessage(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error %v", err)
	}
}

func TestFeishuFrame(t *testing.T) {
	f := feishuFrame{SeqID: 1, LogID: 2, Service: 3, Method: feishuData,
		Headers: []feishuHeader{{"type", "event"}}, Payload: []byte("{}")}
	// written by hand from the proto definition: every field, strings even when empty
	want := []byte{0x08, 1, 0x10, 2, 0x18, 3, 0x20, 1,
		0x2a, 13, 0x0a, 4, 't', 'y', 'p', 'e', 0x12, 5, 'e', 'v', 'e', 'n', 't',
		0x32, 0, 0x3a, 0, 0x42, 2, '{', '}', 0x4a, 0}
	if got := f.marshal(); !bytes.Equal(got, want) {
		t.Fatalf("marshal\n%x\nwant\n%x", got, want)
	}
	back, err := unmarshalFeishuFrame(want)
	if err != nil || back.SeqID != 1 || back.Service != 3 || back.Method != 1 || back.header("type") != "event" || string(back.Payload) != "{}" {
		t.Fatalf("unmarshal %+v %v", back, err)
	}
	// large values, unknown fields and every wire type are read past
	f = feishuFrame{SeqID: 1 << 40, Service: -1, Payload: bytes.Repeat([]byte("z"), 300), LogIDNew: "x"}
	b := append(f.marshal(), 0x50, 0x96, 0x01, 0x59, 1, 2, 3, 4, 5, 6, 7, 8, 0x65, 1, 2, 3, 4)
	back, err = unmarshalFeishuFrame(b)
	if err != nil || back.SeqID != 1<<40 || back.Service != -1 || len(back.Payload) != 300 || back.LogIDNew != "x" {
		t.Fatalf("round trip %+v %v", back, err)
	}
	for _, bad := range [][]byte{{0x08}, {0x42, 5, 1}, {0x0b}, {0x80}} {
		if _, err := unmarshalFeishuFrame(bad); err == nil {
			t.Errorf("%x decoded", bad)
		}
	}
}
