package mpegts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// Fixture: a real 3-frame H.264 Annex-B elementary stream encoded by
// libx264 (baseline profile, 64x64) — same fixture as
// internal/webrtc/manager_test.go, duplicated here since there's no shared
// fixture package and it's a small, self-contained constant.
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

// buildADTS wraps rawAAC in a 7-byte ADTS header (no CRC) with the given
// sampling-frequency-index and channel configuration.
func buildADTS(rawAAC []byte, sfIndex, channelConfig byte) []byte {
	frameLength := 7 + len(rawAAC)
	header := []byte{
		0xFF, 0xF1,
		(1 << 6) | (sfIndex << 2) | (channelConfig >> 2),
		(channelConfig&0x03)<<6 | byte(frameLength>>11),
		byte(frameLength >> 3),
		byte(frameLength<<5) | 0x1F,
		0xFC,
	}
	return append(header, rawAAC...)
}

// TestDemuxer_RoundTripFromTSMuxer feeds the demuxer real MPEG-TS bytes
// produced by this codebase's own mux.TSMuxer (the encode-side counterpart)
// carrying a real H.264 fixture (SPS/PPS/IDR/delta) and a synthetic ADTS AAC
// frame, and verifies it recovers the original access units, timestamps,
// and codec parameters — a real cross-component round trip, not just
// hand-crafted bytes matching the parser's own assumptions.
func TestDemuxer_RoundTripFromTSMuxer(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)
	rawAAC := []byte{0x01, 0x02, 0x03, 0x04, 0x05} // content is arbitrary — only ADTS header fields + framing are under test

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)

	const sfIndex, channelConfig = 4, 2 // 44100Hz, stereo
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo idr: %v", err)
	}
	if err := tsMuxer.WriteAudio(buildADTS(rawAAC, sfIndex, channelConfig), 1000); err != nil {
		t.Fatalf("WriteAudio: %v", err)
	}
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	d := NewDemuxer(bus)
	d.feedPackets(tsBuf.Bytes())
	d.Flush()

	var videoPkts, audioPkts []*media.Packet
drain:
	for {
		select {
		case pkt := <-sub.Chan():
			switch pkt.Codec {
			case media.CodecH264:
				videoPkts = append(videoPkts, pkt)
			case media.CodecAAC:
				audioPkts = append(audioPkts, pkt)
			}
		default:
			break drain
		}
	}

	if len(videoPkts) != 2 {
		t.Fatalf("got %d video packets, want 2 (idr+delta)", len(videoPkts))
	}
	if !videoPkts[0].IsKeyframe {
		t.Error("first video packet should be a keyframe")
	}
	if videoPkts[1].IsKeyframe {
		t.Error("second video packet should not be a keyframe")
	}
	for _, p := range videoPkts {
		if len(p.Data) < 5 || p.Data[0] != 0 || p.Data[1] != 0 || p.Data[2] != 0 || p.Data[3] != 1 {
			end := len(p.Data)
			if end > 8 {
				end = 8
			}
			t.Errorf("video packet Data missing Annex-B start code: % x", p.Data[:end])
			continue
		}
		if nalType := p.Data[4] & 0x1F; nalType == 7 || nalType == 8 {
			t.Errorf("video packet Data should not be SPS/PPS, got NAL type %d", nalType)
		}
	}

	if len(audioPkts) != 1 {
		t.Fatalf("got %d audio packets, want 1", len(audioPkts))
	}
	if !bytes.Equal(audioPkts[0].Data, rawAAC) {
		t.Errorf("audio packet Data = % x, want raw AAC % x (ADTS header should be stripped)", audioPkts[0].Data, rawAAC)
	}
	if audioPkts[0].PTS != 1000 {
		t.Errorf("audio PTS = %d, want 1000", audioPkts[0].PTS)
	}

	// Guards a real bug found while verifying this live: DVR's segment-cut
	// timer computes elapsed wall-clock time from pkt.WallClock — a
	// zero-value time.Time (from forgetting to set it) makes that timer
	// permanently stuck, so DVR recording starts but never actually writes
	// a segment. media.Packet.WallClock must be set on every packet.
	for _, p := range append(append([]*media.Packet{}, videoPkts...), audioPkts...) {
		if p.WallClock.IsZero() {
			t.Error("packet WallClock is zero — DVR's segment-cut timer would never elapse")
		}
	}

	ci := bus.CodecInfo()
	if ci == nil {
		t.Fatal("expected CodecInfo to be populated")
	}
	if ci.VideoCodec != media.CodecH264 || ci.Width != 64 || ci.Height != 64 {
		t.Errorf("video CodecInfo = %+v, want H264 64x64", ci)
	}
	if ci.AudioCodec != media.CodecAAC {
		t.Errorf("AudioCodec = %v, want CodecAAC", ci.AudioCodec)
	}
	if got := mux.ParseAACSampleRate(ci.AACConfig); got != 44100 {
		t.Errorf("ParseAACSampleRate(AACConfig) = %d, want 44100", got)
	}
	if got := mux.ParseAACChannels(ci.AACConfig); got != 2 {
		t.Errorf("ParseAACChannels(AACConfig) = %d, want 2", got)
	}
}

// TestDemuxer_SEIPrecedingSliceSurvivesIntoSamePacket is the regression
// test for a real bug: CEA-608/708 closed captions are carried in a real
// H.264 stream as an SEI NAL unit (type 6, ITU-T T.35 registered user
// data) immediately preceding the slice NAL of the same access unit in
// decode order — real encoders always emit it that way. flushVideo's
// NAL-type switch had no case for type 6 at all, so it was silently
// dropped: an incoming SEI NAL never became part of any media.Packet,
// meaning any closed captions already present in the source could never
// reach HLS/DASH/raw-TS output no matter what those downstream stages did
// (they never even saw it). Feeds a keyframe whose Annex-B payload is a
// real SEI NAL immediately followed by the IDR slice — exactly the shape
// a captioned encoder produces — and confirms the resulting Packet.Data
// still contains the SEI bytes (start-code-prefixed) ahead of the slice.
func TestDemuxer_SEIPrecedingSliceSurvivesIntoSamePacket(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)

	// A minimal but structurally real SEI NAL: type 6, payload type 4
	// (user_data_registered_itu_t_t35), itu_t_t35_country_code 0xB5
	// (ATSC), then a fake CEA-608/708 payload — the exact bytes after the
	// type/country-code header don't matter for this test, which is only
	// verifying the NAL survives byte-for-byte, not decoding its content.
	sei := []byte{0x06, 0x04, 0x07, 0xB5, 0x00, 0x31, 0x47, 0x41, 0x39, 0x80}
	seiWithStart := withStartCode(sei)
	idrWithStart := withStartCode(idr)
	keyframePayload := append(append([]byte{}, seiWithStart...), idrWithStart...)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(keyframePayload, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}
	// A second frame is required to flush the demuxer's PES reassembly
	// buffer — same requirement noted by other tests in this file/package.
	delta := decodeFixture(t, fixtureDelta)
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	d := NewDemuxer(bus)
	d.feedPackets(tsBuf.Bytes())
	d.Flush()

	var videoPkts []*media.Packet
drain:
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecH264 {
				videoPkts = append(videoPkts, pkt)
			}
		default:
			break drain
		}
	}

	if len(videoPkts) == 0 {
		t.Fatal("got no video packets")
	}
	got := videoPkts[0].Data
	want := append(append([]byte{}, seiWithStart...), idrWithStart...)
	if !bytes.Equal(got, want) {
		t.Errorf("keyframe packet Data = % x\nwant (SEI then IDR, both start-coded) = % x", got, want)
	}

	// The fixture SEI above is payload_type=4 (0x04) with
	// itu_t_t35_country_code 0xB5 — a real CEA-608/708 caption signal — so
	// CodecInfo must reflect that, driving HLS's CLOSED-CAPTIONS signaling.
	ci := bus.CodecInfo()
	if ci == nil || !ci.HasCEA608708 {
		t.Errorf("CodecInfo.HasCEA608708 = %v, want true", ci != nil && ci.HasCEA608708)
	}
}

// realATSCSEINALHex is the same real-encoder-captured CEA-708 DTVCC SEI NAL
// used by internal/media/sei_test.go's UsesCEA708DTVCCService tests — full
// ATSC A/53 "GA94" cc_data() framing with cc_type 2/3 (708) triplets, not
// plain legacy CEA-608 field pairs (cc_type 0/1). See that file's doc
// comment for how this was confirmed against a real live stream.
const realATSCSEINALHex = "0604" + "39" +
	"b5" + "0031" + "47413934" + "03" +
	"5f" + "ff" +
	"ff223f" + "fe8c01" + "fe8098" + "fe204b" + "fe3203" + "fe1f00" + "fe9700" + "fe0000" +
	"fe0091" + "fe3f00" + "fe0092" + "fe0000" + "fe2841" + "fe6e64" + "fe2079" + "fe6f"

func TestDemuxer_CEA708DTVCCServiceDetected(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)

	sei, err := hex.DecodeString(realATSCSEINALHex)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	keyframePayload := append(append([]byte{}, withStartCode(sei)...), withStartCode(idr)...)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(keyframePayload, 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}
	delta := decodeFixture(t, fixtureDelta)
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	bus := media.NewBus()
	d := NewDemuxer(bus)
	d.feedPackets(tsBuf.Bytes())
	d.Flush()

	ci := bus.CodecInfo()
	if ci == nil || !ci.HasCEA608708 {
		t.Fatalf("CodecInfo.HasCEA608708 = %v, want true", ci != nil && ci.HasCEA608708)
	}
	if !ci.CEA708DTVCCService {
		t.Error("CodecInfo.CEA708DTVCCService = false for a real CEA-708 DTVCC SEI, want true — HLS would wrongly signal INSTREAM-ID=\"CC1\" instead of \"SERVICE1\"")
	}
}

func TestSplitADTS_MultipleFramesInOnePayload(t *testing.T) {
	frame1 := buildADTS([]byte{0xAA, 0xBB}, 4, 2)
	frame2 := buildADTS([]byte{0xCC, 0xDD, 0xEE}, 4, 2)
	payload := append(append([]byte{}, frame1...), frame2...)

	frames := splitADTS(payload)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0].data, []byte{0xAA, 0xBB}) {
		t.Errorf("frame 0 data = % x", frames[0].data)
	}
	if !bytes.Equal(frames[1].data, []byte{0xCC, 0xDD, 0xEE}) {
		t.Errorf("frame 1 data = % x", frames[1].data)
	}
}

// TestDemuxer_MultipleADTSFramesPerPESGetDistinctPTS guards a real bug
// found while live-verifying SRT ingest against real ffmpeg output: ffmpeg's
// mpegts muxer routinely batches many AAC frames (14+ in the observed live
// capture) into a single PES packet to cut overhead. flushAudio previously
// stamped every frame split out of that one PES with the PES's single PTS
// verbatim — real downstream demuxers/decoders (ffprobe/ffmpeg) saw this as
// non-monotonically-increasing DTS and failed to decode the audio track
// entirely. Each split frame must get its own correctly-advanced PTS.
func TestDemuxer_MultipleADTSFramesPerPESGetDistinctPTS(t *testing.T) {
	const sfIndex, channelConfig = 4, 2 // 44100Hz, stereo -> nominal 2088-tick frames
	frame1 := buildADTS([]byte{0x01}, sfIndex, channelConfig)
	frame2 := buildADTS([]byte{0x02}, sfIndex, channelConfig)
	frame3 := buildADTS([]byte{0x03}, sfIndex, channelConfig)
	payload := append(append(append([]byte{}, frame1...), frame2...), frame3...)

	idr := decodeFixture(t, fixtureIDR)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	// A keyframe is what actually triggers PAT/PMT — needed before the
	// demuxer can discover the audio PID at all.
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}
	if err := tsMuxer.WriteAudio(payload, 1000); err != nil {
		t.Fatalf("WriteAudio: %v", err)
	}

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	d := NewDemuxer(bus)
	d.feedPackets(tsBuf.Bytes())
	d.Flush()

	var pkts []*media.Packet
drain:
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecAAC {
				pkts = append(pkts, pkt)
			}
		default:
			break drain
		}
	}

	if len(pkts) != 3 {
		t.Fatalf("got %d audio packets, want 3", len(pkts))
	}
	if pkts[0].PTS != 1000 {
		t.Errorf("frame 0 PTS = %d, want 1000 (the real PES timestamp)", pkts[0].PTS)
	}
	for i := 1; i < len(pkts); i++ {
		if pkts[i].PTS <= pkts[i-1].PTS {
			t.Fatalf("frame %d PTS = %d, not strictly greater than frame %d's PTS %d", i, pkts[i].PTS, i-1, pkts[i-1].PTS)
		}
	}
	// 90000*1024/44100 = 2089.795..., so consecutive frames should advance
	// by 2089 or 2090 ticks (the exact-rounding accumulator), never by 0.
	for i := 1; i < len(pkts); i++ {
		delta := pkts[i].PTS - pkts[i-1].PTS
		if delta != 2089 && delta != 2090 {
			t.Errorf("frame %d-%d PTS delta = %d, want 2089 or 2090", i-1, i, delta)
		}
	}
}

func TestParsePAT_FindsPMTPID(t *testing.T) {
	payload := buildPAT(1, 0x1000)
	pmtPID, ok := parsePAT(payload)
	if !ok {
		t.Fatal("parsePAT: not ok")
	}
	if pmtPID != 0x1000 {
		t.Errorf("pmtPID = 0x%04X, want 0x1000", pmtPID)
	}
}

// buildPAT hand-builds a minimal single-program PAT section (with a
// pointer_field of 0) for testing parsePAT in isolation from a full TS
// packet — the CRC32 is left zeroed since parsePAT doesn't validate it.
func buildPAT(programNumber, pmtPID uint16) []byte {
	section := []byte{
		0x00,       // table_id
		0x00, 0x00, // section_length placeholder
		0x00, 0x01, // transport_stream_id
		0xC1, // version/current_next
		0x00, // section_number
		0x00, // last_section_number
		byte(programNumber >> 8), byte(programNumber),
		byte(0xE0 | (pmtPID >> 8)), byte(pmtPID),
		0x00, 0x00, 0x00, 0x00, // CRC32 placeholder
	}
	sectionLength := len(section) - 3 // everything after the length field
	section[1] = 0xB0 | byte(sectionLength>>8)
	section[2] = byte(sectionLength)
	return append([]byte{0x00}, section...) // pointer_field = 0
}

// buildSpliceNullSection hand-builds a minimal, CRC32-valid splice_null
// splice_info_section — used to test SCTE-35 PID detection and pass-through
// through the full TS packet layer, independent of internal/scte35's own
// (unexported, package-private) test fixtures.
func buildSpliceNullSection(t *testing.T) []byte {
	t.Helper()
	body := []byte{
		0x00,                         // protocol_version
		0x00, 0x00, 0x00, 0x00, 0x00, // encrypted_packet+algorithm+pts_adjustment
		0x00,             // cw_index
		0x00, 0x00, 0x00, // tier(12) + splice_command_length(12) = 0
		0x00,       // splice_command_type = splice_null
		0x00, 0x00, // descriptor_loop_length = 0
	}
	sectionLength := len(body) + 4 // + CRC32
	section := []byte{0xFC, 0x30 | byte(sectionLength>>8&0x0F), byte(sectionLength)}
	section = append(section, body...)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, mux.CRC32MPEG2(section))
	return append(section, crcBytes...)
}

// TestDemuxer_SCTE35RoundTrip feeds the demuxer real MPEG-TS bytes produced
// by mux.TSMuxer with SCTE-35 enabled (PMT advertises stream_type 0x86,
// WriteSCTE35 writes a real section) and verifies a CodecSCTE35 Packet
// comes out with both the exact original bytes (Data) and a parsed Event
// (SCTE35) — the ingest-side half of TEST 3 (MPEG-TS pass-through) and the
// PID-detection prerequisite for TEST 1/2 (HLS ad markers).
func TestDemuxer_SCTE35RoundTrip(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.EnableSCTE35()
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	section := buildSpliceNullSection(t)
	if err := tsMuxer.WriteSCTE35(section); err != nil {
		t.Fatalf("WriteSCTE35: %v", err)
	}

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()

	d := NewDemuxer(bus)
	var parseErrs int
	d.OnSCTE35Error = func(error) { parseErrs++ }
	d.feedPackets(tsBuf.Bytes())
	d.Flush()

	var got *media.Packet
drainSCTE35:
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecSCTE35 {
				got = pkt
			}
		default:
			break drainSCTE35
		}
	}

	if parseErrs != 0 {
		t.Errorf("OnSCTE35Error called %d times, want 0", parseErrs)
	}
	if got == nil {
		t.Fatal("no CodecSCTE35 packet received on the bus")
	}
	if !bytes.Equal(got.Data, section) {
		t.Errorf("SCTE35 packet Data = %x, want the exact original section %x", got.Data, section)
	}
	if got.SCTE35 == nil {
		t.Fatal("SCTE35 packet has a nil parsed Event")
	}
	if got.SCTE35.CommandType.String() != "splice_null" {
		t.Errorf("CommandType = %v, want splice_null", got.SCTE35.CommandType)
	}
	if got.WallClock.IsZero() {
		t.Error("SCTE35 packet WallClock is zero")
	}
}

// TestDemuxer_PMTDetectsSCTE35PID is a focused unit test on parsePMT/
// handlePMT's stream_type 0x86 branch, independent of the full TS-packet
// round trip above.
func TestDemuxer_PMTDetectsSCTE35PID(t *testing.T) {
	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.EnableSCTE35()
	if err := tsMuxer.WriteVideo(withStartCode(decodeFixture(t, fixtureIDR)), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo: %v", err)
	}

	d := NewDemuxer(media.NewBus())
	d.feedPackets(tsBuf.Bytes())

	if d.scte35PID != mux.PIDSCTE35 {
		t.Errorf("scte35PID = 0x%04X, want 0x%04X", d.scte35PID, mux.PIDSCTE35)
	}
}

// TestDemuxer_SlicesSplitAcrossPESBecomeOneAccessUnit reproduces a live
// IPTV source: a 1080p IDR is too big for one length-capped PES (64 KB), so
// the encoder sends its 4 slices in 4 PES packets with the same PTS/DTS.
// Each picture must still reach the bus as one Packet with all its slices;
// one Packet per slice gave the HLS packager four "keyframes" with the same
// timestamp and browsers couldn't play the stream.
func TestDemuxer_SlicesSplitAcrossPESBecomeOneAccessUnit(t *testing.T) {
	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	// Slice NALs whose header starts with first_mb_in_slice = 0, 1, 2, 3
	// (ue(v): 1, 010, 011, 00100), then filler.
	idrSlice := func(firstByte byte) []byte { return []byte{0x65, firstByte, 0xAA, 0xBB, 0xCC} }
	pSlice := func(firstByte byte) []byte { return []byte{0x41, firstByte, 0xAA, 0xBB, 0xCC} }
	idr := [][]byte{idrSlice(0xC0), idrSlice(0x50), idrSlice(0x70), idrSlice(0x24)}

	var ts bytes.Buffer
	m := mux.NewTSMuxer(&ts, mux.StreamTypeH264, mux.StreamTypeAAC)
	m.SetSPSPPS(sps, pps)
	for _, s := range idr { // one PES per slice, same timestamps
		if err := m.WriteVideo(withStartCode(s), 9000, 12600, true); err != nil {
			t.Fatal(err)
		}
	}
	// A P picture with both its slices in one PES.
	p := append(withStartCode(pSlice(0xC0)), withStartCode(pSlice(0x50))...)
	if err := m.WriteVideo(p, 12600, 16200, false); err != nil {
		t.Fatal(err)
	}
	// Next picture starts: completes the P picture.
	if err := m.WriteVideo(withStartCode(pSlice(0xC0)), 16200, 19800, false); err != nil {
		t.Fatal(err)
	}

	bus := media.NewBus()
	sub := bus.Subscribe(context.Background())
	defer sub.Close()
	d := NewDemuxer(bus)
	d.feedPackets(ts.Bytes())
	d.Flush()

	var video []*media.Packet
	for {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecH264 {
				video = append(video, pkt)
			}
			continue
		default:
		}
		break
	}
	if len(video) != 3 {
		t.Fatalf("got %d video packets, want 3 pictures", len(video))
	}
	if !video[0].IsKeyframe || video[0].DTS != 9000 || video[0].PTS != 12600 {
		t.Errorf("IDR packet: key=%v dts=%d pts=%d", video[0].IsKeyframe, video[0].DTS, video[0].PTS)
	}
	if n := bytes.Count(video[0].Data, []byte{0, 0, 0, 1, 0x65}); n != 4 {
		t.Errorf("IDR packet holds %d slices, want 4", n)
	}
	if n := bytes.Count(video[1].Data, []byte{0, 0, 0, 1, 0x41}); n != 2 || video[1].IsKeyframe {
		t.Errorf("P packet holds %d slices (key=%v), want 2", n, video[1].IsKeyframe)
	}
}
