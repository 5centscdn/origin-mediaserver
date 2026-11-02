package rtmp

import (
	"bytes"
	"log/slog"
	"net"
	"testing"
)

// TestConn_ZixiCompat covers the two replies Zixi needs that lenient
// encoders (OBS, ffmpeg, VLC) never ask for: a PingResponse to its
// keepalive PingRequest (it resets the connection after ~20 s without one)
// and a numeric duration in getStreamLength's _result (it reconnects in a
// loop without one).
func TestConn_ZixiCompat(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c := &conn{nc: server, sendChunkSize: 128, log: slog.Default()}
	demux := newChunkDemuxer(client)

	handle := func(msg *Message) {
		go func() {
			if err := c.handleMessage(msg); err != nil {
				t.Errorf("handleMessage: %v", err)
			}
		}()
	}

	handle(&Message{TypeID: msgUserControl, Data: []byte{0, userCtrlPingRequest, 0x01, 0x02, 0x03, 0x04}})
	msg, err := demux.ReadMessage()
	if err != nil {
		t.Fatalf("read ping response: %v", err)
	}
	if want := []byte{0, userCtrlPingResponse, 0x01, 0x02, 0x03, 0x04}; msg.TypeID != msgUserControl || !bytes.Equal(msg.Data, want) {
		t.Fatalf("ping response = type %d % x, want type %d % x", msg.TypeID, msg.Data, msgUserControl, want)
	}

	payload, err := EncodeAMF0(AMF0String("getStreamLength"), AMF0Number(4), AMF0Null{}, AMF0String("cam"))
	if err != nil {
		t.Fatal(err)
	}
	handle(&Message{TypeID: msgCommandAMF0, Data: payload})
	vals, err := nextCommand(demux)
	if err != nil {
		t.Fatalf("read getStreamLength result: %v", err)
	}
	if len(vals) != 4 || vals[0] != AMF0String("_result") || vals[1] != AMF0Number(4) || vals[3] != AMF0Number(0) {
		t.Fatalf("getStreamLength reply = %#v, want [_result 4 null 0]", vals)
	}
}

func TestPingResponse_IgnoresOtherEvents(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		{0, 0, 0, 0, 0, 1},             // StreamBegin
		{0, userCtrlPingRequest, 0, 0}, // truncated
		{0, userCtrlPingResponse, 0, 0, 0, 1},
	} {
		if resp := pingResponse(data); resp != nil {
			t.Errorf("pingResponse(% x) = % x, want nil", data, resp)
		}
	}
}
