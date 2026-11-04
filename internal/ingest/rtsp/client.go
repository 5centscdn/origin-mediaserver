// Package rtsp implements a pure-Go RTSP pull client: it connects to an
// RTSP source (e.g. an IP camera), negotiates H.264 video and/or G.711
// audio over TCP-interleaved RTP, and publishes depacketized media.Packets
// on a media.Bus — the ingest-side counterpart to internal/webrtc's WHIP
// path, reusing the same pion/rtp depacketization approach.
package rtsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

const dialTimeout = 10 * time.Second

// Pull connects to rawURL, negotiates whatever supported tracks (H.264
// video, PCMU/PCMA audio) its SDP describes, and publishes depacketized
// media.Packets on bus until ctx is done or the connection ends. Mirrors
// rtmp.Pull's blocking contract for internal/pull.Manager's supervisor
// loop.
//
// Scope: TCP-interleaved RTP delivery only (RTSP's "RTP/AVP/TCP" transport)
// — avoids needing separate UDP ports/NAT traversal, at the cost of not
// supporting servers that only offer UDP transport. RTCP is not sent or
// processed. The session is kept alive instead with GET_PARAMETER (OPTIONS
// if the server rejects that) every half of the timeout the server gives
// in its Session header (60 s if none): Dahua devices close a session that
// hears nothing for its timeout, which showed as every camera on one
// device dropping with EOF every ~90 s.
func Pull(ctx context.Context, rawURL string, bus *media.Bus) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse rtsp url: %w", err)
	}
	if u.Scheme != "rtsp" {
		return fmt.Errorf("not an rtsp:// url: %s", rawURL)
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "554")
	}

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("dial %s: %w", host, err)
	}
	defer conn.Close()

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopWatch:
		}
	}()

	// Auth credentials travel in the URL (rtsp://user:pass@host/path) per
	// convention; stripped from the URL used in RTSP request lines/SDP
	// resolution, same as every other tool that accepts RTSP URLs.
	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	u.User = nil
	baseURL := u.String()

	c := &session{
		conn:     bufio.NewReader(conn),
		w:        conn,
		cseq:     0,
		username: username,
		password: password,
	}

	sdpBody, err := c.describe(baseURL)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	video, audio, err := parseSDP(sdpBody, baseURL)
	if err != nil {
		return err
	}

	var videoDepacketizer *videoTrack
	var audioDepacketizer *audioTrack

	if video != nil {
		if err := c.setup(video.controlURL, 0); err != nil {
			return fmt.Errorf("setup video: %w", err)
		}
		videoDepacketizer = newVideoTrack(bus, video.spropParameterSets)
	}
	if audio != nil {
		if err := c.setup(audio.controlURL, 2); err != nil {
			return fmt.Errorf("setup audio: %w", err)
		}
		audioDepacketizer = newAudioTrack(bus, audio.codec, audio.clockRate)
	}

	if err := c.play(baseURL); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	// Keepalive: from here on the reader below owns the connection's read
	// side, so keepalive requests are only written, and their responses
	// are consumed by readInterleavedFrame between RTP frames.
	kaStop := make(chan struct{})
	kaDone := make(chan struct{})
	go func() {
		defer close(kaDone)
		t := time.NewTicker(c.keepaliveInterval())
		defer t.Stop()
		for {
			select {
			case <-kaStop:
				return
			case <-t.C:
				method := "GET_PARAMETER"
				if c.noGetParameter.Load() {
					method = "OPTIONS"
				}
				if err := c.send(method, baseURL, nil); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(kaStop)
		<-kaDone
		c.teardown(baseURL)
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		channel, payload, err := c.readInterleavedFrame()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read stream: %w", err)
		}
		bus.AddBytesIn(int64(len(payload)))

		switch channel {
		case 0:
			if videoDepacketizer != nil {
				videoDepacketizer.handleRTP(payload)
			}
		case 2:
			if audioDepacketizer != nil {
				audioDepacketizer.handleRTP(payload)
			}
		// odd channels (1, 3) are RTCP — ignored, see doc comment above.
		default:
		}
	}
}

// session holds one RTSP control-connection's state: the request/response
// framing (CSeq, Session ID, auth) shared across DESCRIBE/SETUP/PLAY/
// TEARDOWN, plus the buffered reader also used to read interleaved binary
// RTP data once PLAY starts — RTSP multiplexes both onto the same TCP
// stream, so they must share one reader.
type session struct {
	conn *bufio.Reader
	w    io.Writer

	cseq      int
	sessionID string

	username, password string

	// authScheme/digestRealm/digestNonce are populated once by challenge()
	// after the first 401, then reused to build a fresh per-request
	// Authorization header in authorizationHeader — Digest's response hash
	// depends on the request's own method+URI, so it can't be precomputed
	// once and cached verbatim like Basic's can.
	authScheme  string // "" (not yet challenged) | "basic" | "digest"
	digestRealm string
	digestNonce string

	// timeoutS is the session timeout from SETUP's Session header
	// (";timeout=N"), 0 if none was given.
	timeoutS int
	// noGetParameter is set once the server rejects a GET_PARAMETER
	// keepalive (405/501); later keepalives use OPTIONS.
	noGetParameter atomic.Bool
	// wmu serializes writes: after PLAY the keepalive goroutine writes
	// while the read loop reads, and teardown writes after it stops.
	wmu sync.Mutex
}

// keepaliveInterval is half the server's session timeout (60 s when it
// gives none), at least 5 s.
func (c *session) keepaliveInterval() time.Duration {
	t := c.timeoutS
	if t <= 0 {
		t = 60
	}
	return max(time.Duration(t)*time.Second/2, keepaliveMin)
}

// keepaliveMin is the shortest keepalive interval (a variable for tests).
var keepaliveMin = 5 * time.Second

func (c *session) describe(uri string) ([]byte, error) {
	status, headers, body, err := c.roundTrip("DESCRIBE", uri, map[string]string{"Accept": "application/sdp"}, nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("status %d", status)
	}
	_ = headers
	return body, nil
}

func (c *session) setup(uri string, interleavedChannel int) error {
	headers := map[string]string{
		"Transport": fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", interleavedChannel, interleavedChannel+1),
	}
	status, respHeaders, _, err := c.roundTrip("SETUP", uri, headers, nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("status %d", status)
	}
	if sessionHeader, ok := respHeaders["session"]; ok {
		// Session header may carry ";timeout=N": the ID goes in later
		// requests, the timeout paces keepalives.
		id, params, _ := strings.Cut(sessionHeader, ";")
		c.sessionID = strings.TrimSpace(id)
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "timeout") {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
					c.timeoutS = n
				}
			}
		}
	}
	return nil
}

func (c *session) play(uri string) error {
	status, _, _, err := c.roundTrip("PLAY", uri, map[string]string{"Range": "npt=0.000-"}, nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("status %d", status)
	}
	return nil
}

func (c *session) teardown(uri string) {
	_, _, _, _ = c.roundTrip("TEARDOWN", uri, nil, nil)
}

// roundTrip sends one RTSP request and returns its response, transparently
// retrying once with credentials if challenged with a 401 (see auth.go).
func (c *session) roundTrip(method, uri string, headers map[string]string, body []byte) (status int, respHeaders map[string]string, respBody []byte, err error) {
	status, respHeaders, respBody, err = c.doRequest(method, uri, headers, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if status == 401 && c.username != "" {
		if err := c.challenge(respHeaders); err != nil {
			return status, respHeaders, respBody, nil // no usable challenge — return the 401 as-is
		}
		return c.doRequest(method, uri, headers, body)
	}
	return status, respHeaders, respBody, nil
}

func (c *session) doRequest(method, uri string, headers map[string]string, body []byte) (status int, respHeaders map[string]string, respBody []byte, err error) {
	if err := c.send(method, uri, headers, body...); err != nil {
		return 0, nil, nil, err
	}
	return c.readResponse()
}

// send writes one request without reading the response: after PLAY the
// media read loop consumes responses (see readInterleavedFrame).
func (c *session) send(method, uri string, headers map[string]string, body ...byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.cseq++

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", c.cseq)
	b.WriteString("User-Agent: origin-mediaserver\r\n")
	if c.sessionID != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", c.sessionID)
	}
	if auth := c.authorizationHeader(method, uri); auth != "" {
		fmt.Fprintf(&b, "Authorization: %s\r\n", auth)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")

	if _, err := io.WriteString(c.w, b.String()); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := c.w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// readResponse reads one RTSP status line + headers + (if Content-Length is
// present) body. RTSP responses use the same CRLF-terminated header format
// as HTTP/1.0.
func (c *session) readResponse() (status int, headers map[string]string, body []byte, err error) {
	statusLine, err := c.readLine()
	if err != nil {
		return 0, nil, nil, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, nil, nil, fmt.Errorf("malformed status line: %q", statusLine)
	}
	status, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("malformed status code: %q", statusLine)
	}

	headers = make(map[string]string)
	for {
		line, err := c.readLine()
		if err != nil {
			return 0, nil, nil, err
		}
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}

	if cl, ok := headers["content-length"]; ok {
		n, err := strconv.Atoi(cl)
		if err == nil && n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(c.conn, body); err != nil {
				return 0, nil, nil, err
			}
		}
	}

	return status, headers, body, nil
}

func (c *session) readLine() (string, error) {
	line, err := c.conn.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readInterleavedFrame reads one "$"-framed binary block per RFC 2326
// section 10.12: a literal '$' byte, a 1-byte channel number, a 2-byte
// big-endian length, then that many bytes of RTP/RTCP payload.
func (c *session) readInterleavedFrame() (channel int, payload []byte, err error) {
	for {
		// A response to a keepalive can arrive between RTP frames.
		if next, _ := c.conn.Peek(5); string(next) == "RTSP/" {
			status, _, _, err := c.readResponse()
			if err != nil {
				return 0, nil, err
			}
			if status == 405 || status == 501 {
				c.noGetParameter.Store(true)
			}
			continue
		}
		magic, err := c.conn.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		if magic != '$' {
			continue // resync: skip stray bytes until the next frame marker
		}
		header := make([]byte, 3)
		if _, err := io.ReadFull(c.conn, header); err != nil {
			return 0, nil, err
		}
		length := int(header[1])<<8 | int(header[2])
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.conn, payload); err != nil {
			return 0, nil, err
		}
		return int(header[0]), payload, nil
	}
}
