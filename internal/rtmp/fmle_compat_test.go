package rtmp

import (
	"bufio"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestConn_FMLEConnectThenIdle covers FMLE's two-step workflow: "Connect"
// opens the session and then sends nothing until the operator presses
// "Start". The idle timeout used to close that silent connection after
// idle_timeout_ms, so FMLE never got to publish. It also checks that
// connect is followed by onBWDone, which FMLE waits for, and that the
// encoder parameters FMLE appends to the stream name are not part of it.
func TestConn_FMLEConnectThenIdle(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTMP.IdleTimeoutMs = 100
	cfg.RTMP.Applications = []config.RTMPApp{{Name: "live"}}
	registry := media.NewRegistry()
	srv := NewServer(cfg, registry, nil, nil)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(nc)
		}
	}()

	nc, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := clientHandshake(nc); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	demux := newChunkDemuxer(bufio.NewReader(nc))
	if err := sendConnect(nc, demux, l.Addr().String(), "live"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	vals, err := nextCommand(demux)
	if err != nil || vals[0] != AMF0String("onBWDone") {
		t.Fatalf("after connect = %v, %v; want onBWDone", vals, err)
	}

	// Sit idle well past idle_timeout_ms, as FMLE does before "Start".
	time.Sleep(400 * time.Millisecond)

	msid, err := sendCreateStream(nc, demux)
	if err != nil {
		t.Fatalf("createStream after idle: %v", err)
	}
	payload, err := EncodeAMF0(AMF0String("publish"), AMF0Number(0), AMF0Null{}, AMF0String("cam?videoKeyframeFrequency=5&totalDatarate=700"), AMF0String("live"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeChunk(nc, pullChunkSize, 8, 0, msgCommandAMF0, msid, payload); err != nil {
		t.Fatalf("send publish: %v", err)
	}
	vals, err = nextCommand(demux)
	if err != nil {
		t.Fatalf("read publish status: %v", err)
	}
	info, _ := vals[len(vals)-1].(AMF0Object)
	if vals[0] != AMF0String("onStatus") || info["code"] != AMF0String("NetStream.Publish.Start") {
		t.Fatalf("publish reply = %#v, want onStatus NetStream.Publish.Start", vals)
	}
	if registry.Get(media.StreamKey{App: "live", Name: "cam"}) == nil {
		t.Fatalf("stream live/cam not registered; FMLE parameters left in the name?")
	}
}

func TestValidatePublishKey(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"secret", true},
		{"cam?secret", true},
		{"cam?secret&videoKeyframeFrequency=5&totalDatarate=700", true},
		{"cam?secret?videoKeyframeFrequency=5&totalDatarate=700", true},
		{"cam?videoKeyframeFrequency=5&totalDatarate=700", false},
		{"cam?wrong", false},
		{"camsecret", false},
		{"cam?xsecret", false},
	} {
		if got := validatePublishKey(c.name, "secret"); got != c.want {
			t.Errorf("validatePublishKey(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	if got := stripQuery("cam?secret&totalDatarate=700"); got != "cam" {
		t.Errorf("stripQuery = %q, want cam", got)
	}
}

func TestMatchStreamConfigABR(t *testing.T) {
	for _, c := range []struct {
		pattern, name, group string
		idx                  int
		ok                   bool
	}{
		{"test%i", "test2", "test", 2, true},
		{"camera_%i", "camera_10", "camera", 10, true},
		{"test%i", "test", "", 0, false},
		{"%i", "1", "", 0, false},
		{"cam*", "cam1", "", 0, false},
	} {
		m, ok := matchStreamConfigABR(c.pattern, c.name)
		if ok != c.ok || m.GroupName != c.group || m.VariantIndex != c.idx {
			t.Errorf("matchStreamConfigABR(%q, %q) = %+v %v, want group %q idx %d %v",
				c.pattern, c.name, m, ok, c.group, c.idx, c.ok)
		}
	}
	if got := fmleTotalDatarate("test1?videoKeyframeFrequency=5&totalDatarate=700"); got != 700000 {
		t.Errorf("fmleTotalDatarate = %d, want 700000", got)
	}
	if got := fmleTotalDatarate("test1"); got != 0 {
		t.Errorf("fmleTotalDatarate(no query) = %d, want 0", got)
	}
}

// TestConn_SendsAcknowledgements checks the server acknowledges received
// bytes per the peer's Window Acknowledgement Size. FMLE stops sending
// when acks don't come, which stalled every FMLE publish after ~50 s.
func TestConn_SendsAcknowledgements(t *testing.T) {
	cfg := &config.Config{}
	cfg.RTMP.Applications = []config.RTMPApp{{Name: "live"}}
	srv := NewServer(cfg, media.NewRegistry(), nil, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if nc, err := l.Accept(); err == nil {
			srv.handleConn(nc)
		}
	}()
	client, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	if err := clientHandshake(client); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	demux := newChunkDemuxer(bufio.NewReader(client))
	if err := sendConnect(client, demux, l.Addr().String(), "live"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	window := make([]byte, 4)
	binary.BigEndian.PutUint32(window, 1000)
	if err := writeChunk(client, 128, 2, 0, msgWindowAckSize, 0, window); err != nil {
		t.Fatal(err)
	}
	// 3 KB of data the server ignores (no publish yet).
	if err := writeChunk(client, 128, 6, 0, msgDataAMF0, 1, make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}

	for {
		msg, err := demux.ReadMessage()
		if err != nil {
			t.Fatalf("no acknowledgement received: %v", err)
		}
		if msg.TypeID == msgAck {
			if seq := binary.BigEndian.Uint32(msg.Data); seq < 1000 {
				t.Fatalf("ack sequence = %d, want >= 1000", seq)
			}
			return
		}
	}
}
