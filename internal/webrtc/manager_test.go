package webrtc

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// Fixture: a real 3-frame H.264 Annex-B elementary stream encoded by
// libx264 (baseline profile, 64x64), split into its NAL units. Captured once
// via `ffmpeg -f lavfi -i testsrc=size=64x64:rate=25 -frames:v 3 -c:v libx264
// -profile:v baseline -pix_fmt yuv420p -x264-params keyint=3 -f h264 ...`.
const (
	fixtureSPS   = "Z0LACtkEJsBEAAADAAQAAAMAyDxImSA="
	fixturePPS   = "aMuDyyA="
	fixtureIDR   = "ZYiECX/h8h+KAAIAPigAEuAH2lobkDHXABZWvWasHLOP10A6RkCEPw4gARSIAAIA4AxgTGbDY47YaAEeJIp1IQIQIkiGQhA5XEpSlDDUUAAhBy8or2PABv2adUIaNDDhCgAYAGCAAQABAXHHCkXUC++B0Btor51f80cK9wHTPCxLD8Q8P+GoDgnHES38GCBsiEzx4QgAECgACBCEAAiBFSAIjJYBuYphA2zz9fLAOookUwDfPP+YfWKU88AAlgBkqoXd6zxQACX8YBF78P91vwCJajL06g9By1ZEt/MX/9eEAELIQEAAIA4AAgK0DG6HJhvavW2/Ff4vh+ezvgwGVrI5i6oHZmIO0PwvACXinpN//ZX9b22PH//GnMclRE+Vf4C8SRLyzTg3+DDpZnFtlP/Sn0iw56IAGxIml0mP3zQFmi7PRLDb7fVOdqP//v9vNYfhD//BBA4EB3oMAatYlYrFAp6vencU+uXS7///swQ4DgIxrkyAL8K7WLZc99sQx4YKAcRAAuDo/wdLp4B+Qg9OBXao/KoC8Afrp+GUfgYAAyAwDwwADwAOAHApkiDVywEzTeyqEh4oYPkVOAK3TNGCUdX5H+QAoAg8Q8V6roVh0WIFoRs3NAPQh5bAEYV5HNP/9vCACAAIAKDQgBAFgCQoyEwDQbLIAyOj4Fa7DW1aCgZGeXfYQKxM8IWcv/wxC7we+Dw/g8PiT8BZPAqAWe6PB0xyWxXy1BBEAAQHnAAEBQIAA2cAB8JxkXxpASGlgDI98IE3e+EA8SA8OmcsAZF/wgj099hh2coAHti+/eAER+U3H5ruKngB4OmM0s++BoCDBAgMF+P8UDnBtrfc4YoDPwM72OeYxAIx/xEilw=="
	fixtureDelta = "QZo4FOPRPonw"
)

func decodeFixture(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

// containsNALType reports whether an Annex-B byte stream contains a NAL unit
// of the given type (start-code-prefixed).
func containsNALType(data []byte, nalType byte) bool {
	for i := 0; i+4 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			if data[i+4]&0x1F == nalType {
				return true
			}
		}
	}
	return false
}

// TestManager_CreateSession_DeliversH264OverRTP drives a real Manager.CreateSession
// call with a real pion client PeerConnection (in-process, no HTTP layer, no
// real network — loopback ICE candidates only), and verifies the client
// actually receives RTP that reassembles into valid Annex-B H.264 containing
// an in-band SPS before the keyframe's slice data, matching the SPS/PPS
// prepend behavior in session.go.
func TestManager_CreateSession_DeliversH264OverRTP(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	registry := media.NewRegistry()
	key := media.StreamKey{App: "test", Name: "stream1"}
	stream := &media.Stream{Key: key, Bus: media.NewBus(), StartedAt: time.Now()}
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264,
		Width:      64,
		Height:     64,
		SPSNALu:    sps,
		PPSNALu:    pps,
	})
	if err := registry.Register(stream); err != nil {
		t.Fatalf("register stream: %v", err)
	}
	defer registry.Unregister(stream)

	mgr := NewManager(&config.Config{}, registry, metrics.New())

	clientPC, err := pionwebrtc.NewPeerConnection(pionwebrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	defer clientPC.Close()

	if _, err := clientPC.AddTransceiverFromKind(pionwebrtc.RTPCodecTypeVideo, pionwebrtc.RTPTransceiverInit{
		Direction: pionwebrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}
	clientPC.OnConnectionStateChange(func(s pionwebrtc.PeerConnectionState) {
		t.Logf("client connection state: %v", s)
	})
	clientPC.OnICEConnectionStateChange(func(s pionwebrtc.ICEConnectionState) {
		t.Logf("client ICE connection state: %v", s)
	})

	sb := samplebuilder.New(50, &codecs.H264Packet{}, 90000)
	sampleCh := make(chan []byte, 10)
	clientPC.OnTrack(func(track *pionwebrtc.TrackRemote, _ *pionwebrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				sb.Push(pkt)
				for {
					s := sb.Pop()
					if s == nil {
						break
					}
					sampleCh <- s.Data
				}
			}
		}()
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

	answerSDP, resourceID, err := mgr.CreateSession(ctx, "test", "stream1", clientPC.LocalDescription().SDP)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if resourceID == "" {
		t.Fatal("CreateSession returned an empty resource ID")
	}

	if err := clientPC.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		t.Fatalf("client set remote description: %v", err)
	}

	// The session's forwarding goroutine subscribes to the Bus asynchronously
	// (started via `go sess.run(...)` inside CreateSession) — wait for the
	// subscription to actually exist before publishing, since the Bus has no
	// packet history/backlog for late subscribers.
	waitUntil := time.Now().Add(5 * time.Second)
	for stream.Bus.SubscriberCount() < 1 {
		if time.Now().After(waitUntil) {
			t.Fatal("timed out waiting for the WHEP session to subscribe to the stream bus")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Also wait for the actual PeerConnection (ICE+DTLS+SRTP) to be ready —
	// samples written before that are silently dropped.
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
	dts := int64(0)

	var data []byte
waitForSample:
	for {
		select {
		case data = <-sampleCh:
			break waitForSample
		case <-publishTicker.C:
			stream.Bus.Publish(&media.Packet{
				Type: media.PacketKeyframe, Codec: media.CodecH264,
				DTS: dts, PTS: dts, IsKeyframe: true, Data: idr,
			})
			stream.Bus.Publish(&media.Packet{
				Type: media.PacketMedia, Codec: media.CodecH264,
				DTS: dts + 3000, PTS: dts + 3000, IsKeyframe: false, Data: delta,
			})
			dts += 6000
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a reassembled RTP sample")
		}
	}

	if len(data) < 5 || data[0] != 0 || data[1] != 0 || data[2] != 0 || data[3] != 1 {
		t.Fatalf("reassembled sample doesn't look like Annex-B: % x", data)
	}
	if !containsNALType(data, 7) {
		t.Errorf("expected an in-band SPS (NAL type 7) prepended before the keyframe, got % x", data)
	}
	if !containsNALType(data, 8) {
		t.Errorf("expected an in-band PPS (NAL type 8) prepended before the keyframe, got % x", data)
	}

	if err := mgr.DeleteSession(resourceID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		switch clientPC.ConnectionState() {
		case pionwebrtc.PeerConnectionStateClosed, pionwebrtc.PeerConnectionStateFailed, pionwebrtc.PeerConnectionStateDisconnected:
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client connection state did not close after DeleteSession: %v", clientPC.ConnectionState())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestManager_CreateSession_StreamNotLive(t *testing.T) {
	registry := media.NewRegistry()
	mgr := NewManager(&config.Config{}, registry, metrics.New())

	_, _, err := mgr.CreateSession(context.Background(), "nope", "nope", "")
	if err != ErrStreamNotLive {
		t.Fatalf("got err = %v, want ErrStreamNotLive", err)
	}
}
