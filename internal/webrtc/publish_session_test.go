package webrtc

import (
	"context"
	"testing"
	"time"

	pionwebrtc "github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// annexB concatenates NAL units into a single Annex-B byte stream with
// 4-byte start codes, matching what a real WebRTC H.264 sender emits for a
// keyframe (SPS+PPS+IDR in one sample).
func annexB(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// TestManager_CreatePublishSession_IngestsH264OverRTP drives a real WHIP
// publish flow: a real pion client PeerConnection acts as the publisher,
// sending real RTP-packetized H.264 (and Opus) samples in-process (loopback
// ICE, no HTTP layer) against Manager.CreatePublishSession, and verifies the
// resulting media.Stream's Bus receives depacketized H.264 NAL packets with
// correctly-merged CodecInfo, mirroring the existing WHEP test's approach
// for the opposite direction.
func TestManager_CreatePublishSession_IngestsH264OverRTP(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	registry := media.NewRegistry()
	mgr := NewManager(&config.Config{}, registry, metrics.New())

	clientPC, err := pionwebrtc.NewPeerConnection(pionwebrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	defer clientPC.Close()

	videoTrack, err := pionwebrtc.NewTrackLocalStaticSample(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeH264, ClockRate: 90000},
		"video", "whip-test")
	if err != nil {
		t.Fatalf("create video track: %v", err)
	}
	if _, err := clientPC.AddTrack(videoTrack); err != nil {
		t.Fatalf("add video track: %v", err)
	}

	clientPC.OnConnectionStateChange(func(s pionwebrtc.PeerConnectionState) {
		t.Logf("client connection state: %v", s)
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gatherComplete := pionwebrtc.GatheringCompletePromise(clientPC)
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	<-gatherComplete

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := media.StreamKey{App: "test", Name: "whipstream"}
	answerSDP, resourceID, err := mgr.CreatePublishSession(ctx, key.App, key.Name, clientPC.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("CreatePublishSession: %v", err)
	}
	if resourceID == "" {
		t.Fatal("CreatePublishSession returned an empty resource ID")
	}

	stream := registry.Get(key)
	if stream == nil {
		t.Fatal("expected stream to be registered immediately after CreatePublishSession")
	}
	defer registry.Unregister(stream)

	sub := stream.Bus.Subscribe(ctx)
	defer sub.Close()

	if err := clientPC.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		t.Fatalf("client set remote description: %v", err)
	}

	connDeadline := time.Now().Add(10 * time.Second)
	for clientPC.ConnectionState() != pionwebrtc.PeerConnectionStateConnected {
		if time.Now().After(connDeadline) {
			t.Fatalf("timed out waiting for PeerConnection to connect, state=%v", clientPC.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Publish a keyframe+delta pair repeatedly for a few seconds — RTP has no
	// retransmission for a one-shot write, so this decouples the assertion
	// from any single point-in-time race against the DTLS/SRTP session
	// becoming fully write-ready immediately after ConnectionStateConnected.
	publishTicker := time.NewTicker(200 * time.Millisecond)
	defer publishTicker.Stop()

	var keyframePkt *media.Packet
	var deltaPkt *media.Packet
waitForSamples:
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec != media.CodecH264 {
				continue
			}
			if pkt.IsKeyframe {
				keyframePkt = pkt
			} else {
				deltaPkt = pkt
			}
			if keyframePkt != nil && deltaPkt != nil {
				break waitForSamples
			}
		case <-publishTicker.C:
			if err := videoTrack.WriteSample(pionmedia.Sample{
				Data:     annexB(sps, pps, idr),
				Duration: 40 * time.Millisecond,
			}); err != nil {
				t.Logf("write keyframe sample: %v", err)
			}
			if err := videoTrack.WriteSample(pionmedia.Sample{
				Data:     annexB(delta),
				Duration: 40 * time.Millisecond,
			}); err != nil {
				t.Logf("write delta sample: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("timed out waiting for both a keyframe and a delta packet on the stream Bus")
		}
	}

	// The server must strip SPS/PPS out of the published packet — those go
	// into CodecInfo, never onto the Bus as data (see mergeVideoCodecInfo).
	if len(keyframePkt.Data) == 0 || keyframePkt.Data[0]&0x1F != 5 {
		t.Fatalf("expected keyframe packet data to be a bare IDR slice NAL, got NAL type %d", keyframePkt.Data[0]&0x1F)
	}
	if len(deltaPkt.Data) == 0 || deltaPkt.Data[0]&0x1F != 1 {
		t.Fatalf("expected delta packet data to be a bare non-IDR slice NAL, got NAL type %d", deltaPkt.Data[0]&0x1F)
	}

	ci := stream.Bus.CodecInfo()
	if ci == nil {
		t.Fatal("expected CodecInfo to be populated from the incoming SPS/PPS")
	}
	if ci.VideoCodec != media.CodecH264 {
		t.Errorf("got VideoCodec = %v, want CodecH264", ci.VideoCodec)
	}
	if ci.Width != 64 || ci.Height != 64 {
		t.Errorf("got resolution %dx%d, want 64x64 (from fixture SPS)", ci.Width, ci.Height)
	}
	if len(ci.SPSNALu) == 0 || len(ci.PPSNALu) == 0 {
		t.Error("expected SPSNALu/PPSNALu to be populated in CodecInfo")
	}

	if err := mgr.DeleteSession(resourceID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	if got := registry.Get(key); got != nil {
		t.Error("expected stream to be unregistered after DeleteSession")
	}
}

// TestManager_CreatePublishSession_RejectsDuplicatePublisher verifies WHIP
// ingest doesn't preempt an existing publisher at the same app/stream key —
// mirroring RTMP's Registry.Register reject-not-replace behavior.
func TestManager_CreatePublishSession_RejectsDuplicatePublisher(t *testing.T) {
	registry := media.NewRegistry()
	key := media.StreamKey{App: "test", Name: "dup"}
	existing := &media.Stream{Key: key, Bus: media.NewBus(), StartedAt: time.Now()}
	if err := registry.Register(existing); err != nil {
		t.Fatalf("register existing stream: %v", err)
	}
	defer registry.Unregister(existing)

	mgr := NewManager(&config.Config{}, registry, metrics.New())
	_, _, err := mgr.CreatePublishSession(context.Background(), key.App, key.Name, "")
	if err != ErrStreamAlreadyLive {
		t.Fatalf("got err = %v, want ErrStreamAlreadyLive", err)
	}
}
