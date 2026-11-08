package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// A WebSocket client (RFC 6455), as much of it as Feishu's long connection needs. The handshake
// goes through net/http, so it uses the same proxies and TLS settings as every other request:
// after "101 Switching Protocols" the response body is the connection.

const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
	wsMaxMsg = 1 << 20
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	rw  io.ReadWriteCloser
	br  *bufio.Reader
	wmu sync.Mutex
}

// newWSTransport is an HTTP/1.1-only transport: the upgrade does not exist in HTTP/2
func newWSTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return tr
}

// wsDial opens a WebSocket on a ws:// or wss:// URL. hc must not have a timeout, which would
// cut the connection, and must not speak HTTP/2 (see newWSTransport). Cancelling ctx during the
// handshake aborts it; afterwards close the connection with Close.
func wsDial(ctx context.Context, hc *http.Client, rawURL string) (*wsConn, error) {
	u := rawURL
	switch {
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + u[len("wss://"):]
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + u[len("ws://"):]
	default:
		return nil, fmt.Errorf("websocket: unsupported URL scheme")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	key := base64.StdEncoding.EncodeToString(nonce)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, &wsHandshakeError{Status: resp.StatusCode, Header: resp.Header}
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("websocket: the connection cannot be written")
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		rw.Close()
		return nil, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	return &wsConn{rw: rw, br: bufio.NewReader(rw)}, nil
}

// wsHandshakeError is a refused upgrade; Feishu explains why in its headers
type wsHandshakeError struct {
	Status int
	Header http.Header
}

func (e *wsHandshakeError) Error() string {
	return fmt.Sprintf("websocket handshake: %d %s", e.Status, http.StatusText(e.Status))
}

// write sends one unfragmented frame, masked as clients must
func (c *wsConn) write(op byte, data []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(data); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	rand.Read(mask[:])
	buf := append(append(hdr, mask[:]...), data...)
	body := buf[len(hdr)+4:]
	for i := range body {
		body[i] ^= mask[i&3]
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(buf)
	return err
}

// WriteMessage sends a text or binary message; it is safe to call from several goroutines
func (c *wsConn) WriteMessage(op byte, data []byte) error {
	return c.write(op, data)
}

// ReadMessage returns the next text or binary message. It answers pings itself and skips pongs;
// a close frame from the peer ends with errWSClosed.
func (c *wsConn) ReadMessage() (byte, []byte, error) {
	var msg []byte
	var msgOp byte
	for {
		b0, err := c.br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		b1, err := c.br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		fin, op := b0&0x80 != 0, b0&0x0f
		n := uint64(b1 & 0x7f)
		switch n {
		case 126:
			var l [2]byte
			if _, err := io.ReadFull(c.br, l[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(l[:]))
		case 127:
			var l [8]byte
			if _, err := io.ReadFull(c.br, l[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(l[:])
		}
		var mask [4]byte
		masked := b1&0x80 != 0
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		if n > wsMaxMsg || uint64(len(msg))+n > wsMaxMsg {
			return 0, nil, errors.New("websocket: message too large")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.br, data); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range data {
				data[i] ^= mask[i&3]
			}
		}
		switch op {
		case wsPing:
			if err := c.write(wsPong, data); err != nil {
				return 0, nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			c.write(wsClose, data)
			return 0, nil, errWSClosed
		case wsText, wsBinary:
			if msg != nil {
				return 0, nil, errors.New("websocket: new message inside a fragmented one")
			}
			msgOp, msg = op, data
		case 0:
			if msg == nil {
				return 0, nil, errors.New("websocket: continuation without a message")
			}
			msg = append(msg, data...)
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
		if fin {
			return msgOp, msg, nil
		}
	}
}

// Close sends a close frame and closes the connection without waiting for the answer
func (c *wsConn) Close() error {
	c.write(wsClose, []byte{0x03, 0xe8}) // 1000, normal closure
	return c.rw.Close()
}
