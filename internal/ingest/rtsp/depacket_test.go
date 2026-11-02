package rtsp

import (
	"encoding/base64"
	"testing"

	"github.com/pion/rtp"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// Fixture: a real 3-frame H.264 Annex-B elementary stream encoded by
// libx264 (baseline profile, 64x64) — same fixture as
// internal/webrtc/manager_test.go and internal/ingest/mpegts's tests.
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

// singleNALPacket builds a real RTP packet carrying one complete NAL unit
// as its payload — RFC 6184's "single NAL unit packet" mode, valid for any
// NAL small enough to not need FU-A fragmentation.
func singleNALPacket(seq uint16, ts uint32, marker bool, nal []byte) []byte {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			Marker:         marker,
		},
		Payload: nal,
	}
	data, err := pkt.Marshal()
	if err != nil {
		panic(err)
	}
	return data
}

func TestVideoTrack_SpropParameterSetsPreseedsCodecInfo(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	sprop := base64.StdEncoding.EncodeToString(sps) + "," + base64.StdEncoding.EncodeToString(pps)

	bus := media.NewBus()
	newVideoTrack(bus, sprop)

	ci := bus.CodecInfo()
	if ci == nil {
		t.Fatal("expected CodecInfo to be populated from sprop-parameter-sets alone, before any RTP arrives")
	}
	if ci.VideoCodec != media.CodecH264 || ci.Width != 64 || ci.Height != 64 {
		t.Errorf("CodecInfo = %+v, want H264 64x64", ci)
	}
}

func TestVideoTrack_DepacketizesRealH264(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	bus := media.NewBus()
	tr := newVideoTrack(bus, "") // no SDP sprop — parameter sets arrive in-band, like many real cameras

	sub := bus.Subscribe(t.Context())
	defer sub.Close()

	// SPS and PPS as their own single-NAL RTP packets (real cameras
	// commonly send these in-band before each keyframe), then the IDR,
	// then a delta frame at the next RTP timestamp, then one more packet
	// at a third timestamp purely to flush the delta frame out of the
	// samplebuilder's jitter buffer — it only releases a timestamp's
	// packets once it observes the next one arrive, so the very last
	// timestamp sent is always held back with nothing to release it.
	tr.handleRTP(singleNALPacket(0, 0, false, sps))
	tr.handleRTP(singleNALPacket(1, 0, false, pps))
	tr.handleRTP(singleNALPacket(2, 0, true, idr))
	tr.handleRTP(singleNALPacket(3, 3000, true, delta))
	tr.handleRTP(singleNALPacket(4, 6000, true, delta))

	// handleRTP publishes synchronously (no goroutines involved), so both
	// released samples are already sitting in the subscription's buffered
	// channel — the samplebuilder releases a timestamp's sample once it
	// sees the NEXT timestamp arrive, so of the 3 timestamps sent (0,
	// 3000, 6000) only the first two are released; 6000 stays pending
	// with nothing after it to release it.
	var gotKeyframe, gotDelta *media.Packet
	for i := 0; i < 2; i++ {
		select {
		case pkt := <-sub.Chan():
			if pkt.IsKeyframe {
				gotKeyframe = pkt
			} else {
				gotDelta = pkt
			}
		default:
			t.Fatalf("expected 2 published packets, only got %d", i)
		}
	}
	if gotKeyframe == nil || gotDelta == nil {
		t.Fatal("did not receive both a keyframe and a delta packet")
	}
	if len(gotKeyframe.Data) < 5 || gotKeyframe.Data[0] != 0 || gotKeyframe.Data[3] != 1 {
		t.Errorf("keyframe Data missing Annex-B start code: % x", gotKeyframe.Data[:8])
	}

	ci := bus.CodecInfo()
	if ci == nil || ci.Width != 64 || ci.Height != 64 {
		t.Errorf("CodecInfo not correctly populated from in-band SPS: %+v", ci)
	}
}

func TestAudioTrack_PCMUPassthrough(t *testing.T) {
	bus := media.NewBus()
	tr := newAudioTrack(bus, "PCMU", 8000)

	sub := bus.Subscribe(t.Context())
	defer sub.Close()

	payload := []byte{0x11, 0x22, 0x33, 0x44}
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, Timestamp: 8000},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal rtp packet: %v", err)
	}
	tr.handleRTP(raw)

	select {
	case got := <-sub.Chan():
		if got.Codec != media.CodecPCMU {
			t.Errorf("Codec = %v, want CodecPCMU", got.Codec)
		}
		if string(got.Data) != string(payload) {
			t.Errorf("Data = % x, want % x (G.711 payload should pass through unchanged)", got.Data, payload)
		}
	default:
		t.Fatal("expected a published audio packet")
	}

	ci := bus.CodecInfo()
	if ci == nil || ci.AudioCodec != media.CodecPCMU || ci.SampleRate != 8000 || ci.Channels != 1 {
		t.Errorf("CodecInfo = %+v, want PCMU/8000/mono", ci)
	}
}

func TestAudioTrack_TimestampRescaledTo90kHz(t *testing.T) {
	bus := media.NewBus()
	tr := newAudioTrack(bus, "PCMA", 8000)
	sub := bus.Subscribe(t.Context())
	defer sub.Close()

	// Second packet's RTP timestamp is 1 second (8000 ticks at 8kHz) after
	// the first — must become exactly 90000 ticks at 90kHz.
	for _, ts := range []uint32{0, 8000} {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(ts), Timestamp: ts}, Payload: []byte{0x00}}
		raw, _ := pkt.Marshal()
		tr.handleRTP(raw)
	}

	first := <-sub.Chan()
	second := <-sub.Chan()
	if first.DTS != 0 {
		t.Errorf("first packet DTS = %d, want 0", first.DTS)
	}
	if second.DTS != 90000 {
		t.Errorf("second packet DTS = %d, want 90000 (1s at 8kHz rescaled to 90kHz)", second.DTS)
	}
}
