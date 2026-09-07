package fusiongate

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	wsOpcodeText  = 1
	wsOpcodeBin   = 2
	wsOpcodeClose = 8
	wsOpcodePing  = 9
	wsOpcodePong  = 10

	wsMaxFrameSize = 16 << 20
)

type wsConn struct {
	conn   net.Conn
	br     *bufio.Reader
	mu     sync.Mutex
	closed bool
}

func wsDialContext(ctx context.Context, rawURL string, headers http.Header) (*wsConn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}

	host := u.Host
	scheme := u.Scheme
	if scheme != "wss" && scheme != "ws" {
		return nil, nil, fmt.Errorf("unsupported scheme: %s", scheme)
	}

	port := u.Port()
	if port == "" {
		if scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	hostPort := net.JoinHostPort(u.Hostname(), port)

	var dialer net.Dialer
	var conn net.Conn

	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		dialer.Deadline = deadline
	}

	if scheme == "wss" {
		rawConn, dialErr := dialer.DialContext(ctx, "tcp", hostPort)
		if dialErr != nil {
			return nil, nil, fmt.Errorf("websocket tcp dial failed: %w", dialErr)
		}
		uconn := utls.UClient(rawConn, &utls.Config{ServerName: u.Hostname()}, utls.HelloChrome_Auto)
		if handshakeErr := uconn.HandshakeContext(ctx); handshakeErr != nil {
			rawConn.Close()
			return nil, nil, fmt.Errorf("websocket tls handshake failed: %w", handshakeErr)
		}
		conn = uconn
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", hostPort)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	wsKey := base64.StdEncoding.EncodeToString(keyBytes)

	reqPath := u.RequestURI()
	reqLines := []string{
		fmt.Sprintf("GET %s HTTP/1.1", reqPath),
		fmt.Sprintf("Host: %s", host),
		"Upgrade: websocket",
		"Connection: Upgrade",
		fmt.Sprintf("Sec-WebSocket-Key: %s", wsKey),
		"Sec-WebSocket-Version: 13",
	}
	for key, vals := range headers {
		for _, v := range vals {
			reqLines = append(reqLines, fmt.Sprintf("%s: %s", key, v))
		}
	}
	reqLines = append(reqLines, "", "")
	reqStr := strings.Join(reqLines, "\r\n")

	if hasDeadline {
		conn.SetWriteDeadline(deadline)
	}
	if _, err := conn.Write([]byte(reqStr)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("websocket handshake write failed: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("websocket handshake read failed: %w", err)
	}

	if resp.StatusCode != 101 {
		conn.Close()
		return nil, resp, fmt.Errorf("websocket handshake failed: %d", resp.StatusCode)
	}

	return &wsConn{conn: conn, br: br}, resp, nil
}

func (ws *wsConn) ReadMessage() (int, []byte, error) {
	for {
		opcode, data, err := ws.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case wsOpcodePing:
			ws.writeFrame(wsOpcodePong, data)
			continue
		case wsOpcodePong:
			continue
		case wsOpcodeClose:
			ws.writeFrame(wsOpcodeClose, nil)
			return wsOpcodeClose, data, io.EOF
		default:
			return opcode, data, nil
		}
	}
}

func (ws *wsConn) WriteMessage(opcode int, data []byte) error {
	return ws.writeFrame(opcode, data)
}

func (ws *wsConn) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.WriteMessage(wsOpcodeText, data)
}

func (ws *wsConn) SetReadDeadline(t time.Time) error {
	return ws.conn.SetReadDeadline(t)
}

func (ws *wsConn) Close() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closed {
		return nil
	}
	ws.closed = true
	return ws.conn.Close()
}

func (ws *wsConn) readFrame() (int, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(ws.br, header); err != nil {
		return 0, nil, err
	}

	opcode := int(header[0] & 0x0F)
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)

	switch {
	case length == 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(ws.br, ext); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext))
	case length == 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(ws.br, ext); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext))
	}

	if length > wsMaxFrameSize {
		return 0, nil, fmt.Errorf("websocket frame too large: %d", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(ws.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(ws.br, data); err != nil {
		return 0, nil, err
	}

	if masked {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}

	return opcode, data, nil
}

func (ws *wsConn) writeFrame(opcode int, data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.closed {
		return errors.New("websocket connection closed")
	}

	maskKey := make([]byte, 4)
	rand.Read(maskKey)

	length := len(data)
	var header []byte

	fin := byte(0x80)
	switch {
	case length <= 125:
		header = []byte{fin | byte(opcode), 0x80 | byte(length)}
	case length <= 65535:
		header = make([]byte, 4)
		header[0] = fin | byte(opcode)
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:], uint16(length))
	default:
		header = make([]byte, 10)
		header[0] = fin | byte(opcode)
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:], uint64(length))
	}

	buf := make([]byte, 0, len(header)+4+length)
	buf = append(buf, header...)
	buf = append(buf, maskKey...)

	masked := make([]byte, length)
	for i, b := range data {
		masked[i] = b ^ maskKey[i%4]
	}
	buf = append(buf, masked...)

	_, err := ws.conn.Write(buf)
	return err
}

