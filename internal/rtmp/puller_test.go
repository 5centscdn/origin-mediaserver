package rtmp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestPull_EndToEnd verifies the outbound RTMP client end to end: handshake,
// connect/createStream/play, and decoding received audio/video into a Bus.
//
// This project's own RTMP server deliberately rejects "play" (RTMP playback
// was never implemented here — see connection.go's handleAMF0Command,
// "Use HLS or DASH for playback"), so Pull can't be exercised against this
// codebase's own server. Real backup sources are a genuine third-party RTMP
// server that does implement play. This test stands in for that with a
// minimal fake RTMP play server built from the same primitives (doHandshake,
// writeChunk, EncodeAMF0) the real server itself already uses.
func TestPull_EndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go fakeRTMPPlayServer(t, ln)

	bus := media.NewBus()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub := bus.Subscribe(ctx)
	defer sub.Close()

	pullErrCh := make(chan error, 1)
	go func() {
		pullErrCh <- Pull(ctx, "rtmp://"+ln.Addr().String()+"/live/test", "backup1", bus)
	}()

	var pkts []*media.Packet
	var sawVideo, sawAudio bool
	deadline := time.After(4 * time.Second)
collect:
	for !(sawVideo && sawAudio) {
		select {
		case pkt := <-sub.Chan():
			pkts = append(pkts, pkt)
			if pkt.StreamID != "backup1" {
				t.Errorf("packet StreamID = %q, want %q", pkt.StreamID, "backup1")
			}
			if pkt.Codec.IsVideo() {
				sawVideo = true
			}
			if pkt.Codec.IsAudio() {
				sawAudio = true
			}
		case <-deadline:
			break collect
		}
	}

	if !sawVideo {
		t.Errorf("never received a video packet (got %d packets total)", len(pkts))
	}
	if !sawAudio {
		t.Errorf("never received an audio packet (got %d packets total)", len(pkts))
	}

	if ci := bus.CodecInfo(); ci == nil {
		t.Error("expected CodecInfo to be set on the bus")
	}

	cancel()
	<-pullErrCh
}

// fakeRTMPPlayServer accepts one connection, performs the server-side RTMP
// handshake, answers connect/createStream/play, then pushes one synthetic
// video sequence header + keyframe and one audio config + frame.
func fakeRTMPPlayServer(t *testing.T, ln net.Listener) {
	nc, err := ln.Accept()
	if err != nil {
		return
	}
	defer nc.Close()

	if err := doHandshake(nc); err != nil {
		t.Errorf("fake server handshake: %v", err)
		return
	}

	demux := newChunkDemuxer(nc)
	const msid = 1

	for {
		msg, err := demux.ReadMessage()
		if err != nil {
			return
		}
		if msg.TypeID != msgCommandAMF0 {
			continue
		}
		vals, err := DecodeAMF0All(msg.Data)
		if err != nil || len(vals) == 0 {
			continue
		}
		cmd, _ := vals[0].(AMF0String)
		txID := 0.0
		if len(vals) > 1 {
			if n, ok := vals[1].(AMF0Number); ok {
				txID = float64(n)
			}
		}

		switch cmd {
		case "connect":
			result, _ := EncodeAMF0(AMF0String("_result"), AMF0Number(txID),
				AMF0Object{}, AMF0Object{"level": AMF0String("status"), "code": AMF0String("NetConnection.Connect.Success")})
			if err := writeChunk(nc, 128, 3, 0, msgCommandAMF0, 0, result); err != nil {
				return
			}

		case "createStream":
			result, _ := EncodeAMF0(AMF0String("_result"), AMF0Number(txID), AMF0Null{}, AMF0Number(msid))
			if err := writeChunk(nc, 128, 3, 0, msgCommandAMF0, 0, result); err != nil {
				return
			}

		case "play":
			status, _ := EncodeAMF0(AMF0String("onStatus"), AMF0Number(0), AMF0Null{},
				AMF0Object{"level": AMF0String("status"), "code": AMF0String("NetStream.Play.Start")})
			writeChunk(nc, 128, 8, 0, msgCommandAMF0, msid, status)

			// Video sequence header (AVCDecoderConfigurationRecord).
			videoConfig := append([]byte{0x17, 0x00, 0x00, 0x00, 0x00}, minimalAVCDecoderConfig(0x64, 0x1F)...)
			writeChunk(nc, 128, 6, 0, msgVideo, msid, videoConfig)

			// Video keyframe: one AVCC length-prefixed NALU.
			videoFrame := append([]byte{0x17, 0x01, 0x00, 0x00, 0x00}, []byte{0, 0, 0, 4, 0x65, 0xAA, 0xBB, 0xCC}...)
			writeChunk(nc, 128, 6, 40, msgVideo, msid, videoFrame)

			// Audio: AudioSpecificConfig then one raw AAC frame.
			writeChunk(nc, 128, 6, 0, msgAudio, msid, []byte{0xAF, 0x00, 0x12, 0x10})
			writeChunk(nc, 128, 6, 40, msgAudio, msid, []byte{0xAF, 0x01, 0xDE, 0xAD, 0xBE, 0xEF})
			return
		}
	}
}
