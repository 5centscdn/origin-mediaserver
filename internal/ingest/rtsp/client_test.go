package rtsp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestPull_EndToEnd drives the real client (handshake, SDP parsing, SETUP/
// PLAY, interleaved RTP reading, depacketization) against a minimal fake
// RTSP server built from the RTSP wire protocol directly — mirroring
// internal/rtmp/puller_test.go's fakeRTMPPlayServer, since this codebase
// has no real RTSP server of its own to test against either.
func TestPull_EndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go fakeRTSPServer(t, ln)

	bus := media.NewBus()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub := bus.Subscribe(ctx)
	defer sub.Close()

	pullErrCh := make(chan error, 1)
	go func() {
		pullErrCh <- Pull(ctx, "rtsp://"+ln.Addr().String()+"/stream1", bus)
	}()

	var sawVideoKeyframe, sawAudio bool
	deadline := time.After(4 * time.Second)
collect:
	for !(sawVideoKeyframe && sawAudio) {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecH264 && pkt.IsKeyframe {
				sawVideoKeyframe = true
			}
			if pkt.Codec == media.CodecPCMU {
				sawAudio = true
			}
		case err := <-pullErrCh:
			t.Fatalf("Pull returned early: %v", err)
		case <-deadline:
			break collect
		}
	}

	if !sawVideoKeyframe {
		t.Error("never received a decoded H.264 keyframe")
	}
	if !sawAudio {
		t.Error("never received a decoded PCMU audio packet")
	}

	ci := bus.CodecInfo()
	if ci == nil || ci.Width != 64 || ci.Height != 64 {
		t.Errorf("CodecInfo = %+v, want 64x64", ci)
	}

	cancel()
	select {
	case <-pullErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Pull did not return after context cancellation")
	}
}

// fakeRTSPServer implements just enough of RTSP (DESCRIBE/SETUP/PLAY) to
// drive a real client through a full session, then streams real H.264 (the
// same fixture used throughout this package's tests) and synthetic PCMU
// audio as TCP-interleaved RTP.
func fakeRTSPServer(t *testing.T, ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	sps := mustDecode(fixtureSPS)
	pps := mustDecode(fixturePPS)
	idr := mustDecode(fixtureIDR)
	sprop := base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)

	sdp := fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=Fake\r\nt=0 0\r\n"+
			"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 sprop-parameter-sets=%s\r\na=control:trackID=1\r\n"+
			"m=audio 0 RTP/AVP 0\r\na=control:trackID=2\r\n",
		sprop)

	for {
		req, err := readTestRequest(r)
		if err != nil {
			return
		}

		switch req.method {
		case "DESCRIBE":
			writeTestResponse(conn, req.cseq, 200, map[string]string{"Content-Type": "application/sdp"}, []byte(sdp))
		case "SETUP":
			writeTestResponse(conn, req.cseq, 200, map[string]string{
				"Transport": req.headers["transport"],
				"Session":   "FAKESESSION1",
			}, nil)
		case "PLAY":
			writeTestResponse(conn, req.cseq, 200, nil, nil)
			go streamFakeMedia(conn, idr)
		case "TEARDOWN":
			writeTestResponse(conn, req.cseq, 200, nil, nil)
			return
		default:
			writeTestResponse(conn, req.cseq, 501, nil, nil)
		}
	}
}

// streamFakeMedia sends one H.264 keyframe (as a single-NAL RTP packet on
// interleaved channel 0) and a few PCMU audio packets (channel 2) after
// PLAY — enough for the client to depacketize a real keyframe and at least
// one audio packet.
func streamFakeMedia(conn net.Conn, idr []byte) {
	// Two video packets at different timestamps: the samplebuilder only
	// releases a timestamp's sample once it observes the next one arrive
	// (regardless of the marker bit), so a single packet would sit in the
	// jitter buffer forever with nothing to flush it.
	for i, ts := range []uint32{0, 3000} {
		videoPkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Timestamp: ts, Marker: true},
			Payload: idr,
		}
		writeInterleaved(conn, 0, videoPkt)
	}

	for i := 0; i < 3; i++ {
		audioPkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(i), Timestamp: uint32(i * 160)},
			Payload: []byte{0xFF, 0xFF, 0xFF, 0xFF},
		}
		writeInterleaved(conn, 2, audioPkt)
		time.Sleep(20 * time.Millisecond)
	}
}

func writeInterleaved(conn net.Conn, channel byte, pkt *rtp.Packet) {
	data, err := pkt.Marshal()
	if err != nil {
		return
	}
	header := []byte{'$', channel, byte(len(data) >> 8), byte(len(data))}
	conn.Write(header)
	conn.Write(data)
}

func mustDecode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

type testRequest struct {
	method  string
	uri     string
	cseq    int
	headers map[string]string
}

func readTestRequest(r *bufio.Reader) (*testRequest, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed request line: %q", line)
	}
	req := &testRequest{method: parts[0], uri: parts[1], headers: make(map[string]string)}

	for {
		hLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hLine = strings.TrimRight(hLine, "\r\n")
		if hLine == "" {
			break
		}
		if k, v, ok := strings.Cut(hLine, ":"); ok {
			req.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if cseq, ok := req.headers["cseq"]; ok {
		req.cseq, _ = strconv.Atoi(cseq)
	}
	return req, nil
}

func writeTestResponse(conn net.Conn, cseq, status int, headers map[string]string, body []byte) {
	statusText := "OK"
	if status != 200 {
		statusText = "Error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RTSP/1.0 %d %s\r\n", status, statusText)
	fmt.Fprintf(&b, "CSeq: %d\r\n", cseq)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	conn.Write([]byte(b.String()))
	if len(body) > 0 {
		conn.Write(body)
	}
}
