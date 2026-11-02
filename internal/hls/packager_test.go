package hls

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// TestHandleAudio_SkipsUnsupportedCodec guards against the TS-container
// corruption bug: the PMT's audio stream_type is fixed at construction
// (AAC unless MP3/AC3, see initMuxer), so writing an unsupported codec's raw
// bytes (e.g. Opus, from a WHIP-ingested stream) into the audio PES would
// have a demuxer parse them as ADTS AAC and choke. handleAudio must skip
// entirely for codecs the TS muxer wasn't built to describe.
func TestHandleAudio_SkipsUnsupportedCodec(t *testing.T) {
	cfg := &config.Config{}
	cfg.HLS.Container = "ts"
	cfg.HLS.SegmentDurationS = 6

	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	ci := &media.CodecInfo{VideoCodec: media.CodecH264, AudioCodec: media.CodecOpus}
	stream.Bus.SetCodecInfo(ci)

	p := &Packager{cfg: cfg, stream: stream, outDir: t.TempDir(), log: slog.Default()}
	p.initMuxer(ci)
	if p.segMuxer == nil {
		t.Fatal("segMuxer not initialized")
	}
	p.segStartDTS = 1 // pretend a segment is already open

	before := p.segBuf.Len()
	if err := p.handleAudio(&media.Packet{Type: media.PacketMedia, Codec: media.CodecOpus, PTS: 100, Data: []byte{1, 2, 3, 4}}); err != nil {
		t.Fatalf("handleAudio returned error: %v", err)
	}
	if p.segBuf.Len() != before {
		t.Errorf("segBuf grew from %d to %d bytes — Opus audio was not skipped", before, p.segBuf.Len())
	}

	// Sanity check: AAC through the same path does write bytes, so the
	// assertion above is actually exercising the skip and not just a no-op
	// segMuxer.
	if err := p.handleAudio(&media.Packet{Type: media.PacketMedia, Codec: media.CodecAAC, PTS: 100, Data: []byte{1, 2, 3, 4}}); err != nil {
		t.Fatalf("handleAudio (AAC) returned error: %v", err)
	}
	if p.segBuf.Len() == before {
		t.Error("segBuf did not grow for AAC audio — sanity check failed, WriteAudio path itself may be broken")
	}
}

// TestHandleVideo_LazyInitsFromCodecInfoWithoutConfigPacket guards against a
// real bug found while verifying MPEG-TS ingest live: sources with only
// in-band parameter sets (WHIP, MPEG-TS — SPS/PPS folded into CodecInfo
// directly, no separate out-of-band config record) never send a
// media.PacketConfig packet, so handleVideo's old "only initMuxer on
// PacketConfig" gate meant the TS muxer never got built at all for such
// sources — every real video packet was silently dropped forever, and the
// TS-container HLS output (config.toml's default container="ts") stayed
// permanently empty. handleVideo must also lazily init from CodecInfo once
// it's populated, mirroring handleVideoFMP4/ensureInit's existing lazy
// pattern.
func TestHandleVideo_LazyInitsFromCodecInfoWithoutConfigPacket(t *testing.T) {
	cfg := &config.Config{}
	cfg.HLS.Container = "ts"
	cfg.HLS.SegmentDurationS = 6

	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	sps := []byte{0x67, 0x42, 0xC0, 0x1E} // enough for initMuxer's SPS/PPS wiring; resolution parsing isn't under test here
	ci := &media.CodecInfo{VideoCodec: media.CodecH264, SPSNALu: sps, PPSNALu: []byte{0x68, 0xCE}}
	stream.Bus.SetCodecInfo(ci)

	p := &Packager{cfg: cfg, stream: stream, outDir: t.TempDir(), log: slog.Default()}

	// No PacketConfig ever arrives — only a keyframe, exactly like WHIP/
	// MPEG-TS ingest. DTS is deliberately nonzero: segStartDTS uses <=0 as
	// its own "segment not started" sentinel, so a literal DTS of 0 hits a
	// separate, narrower pre-existing collision unrelated to what's under
	// test here.
	if err := p.handleVideo(&media.Packet{Type: media.PacketKeyframe, Codec: media.CodecH264, IsKeyframe: true, DTS: 1000, PTS: 1000, Data: []byte{0, 0, 0, 1, 0x65, 1, 2, 3}}); err != nil {
		t.Fatalf("handleVideo returned error: %v", err)
	}
	if p.segMuxer == nil {
		t.Fatal("segMuxer was never lazily initialized from CodecInfo — video packets would be silently dropped forever")
	}
	if p.segBuf.Len() == 0 {
		t.Error("segBuf is empty — the keyframe itself wasn't muxed after lazy init")
	}
}

// TestPackager_SplitAudioTrack_WritesIndependentOutputs drives a real
// Packager end-to-end (handleVideoFMP4/handleAudioFMP4/flushSegment, not
// closeSegmentSplit directly) with hls.separate_audio_track enabled, and
// verifies it produces two fully independent playlists + init segments +
// media segments — video's own directory and a new audio/ subdirectory —
// rather than one combined .m4s.
func TestPackager_SplitAudioTrack_WritesIndependentOutputs(t *testing.T) {
	cfg := &config.Config{}
	cfg.HLS.Container = "fmp4"
	cfg.HLS.SegmentDurationS = 2
	cfg.HLS.PlaylistWindow = 5
	cfg.HLS.SeparateAudioTrack = true

	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s1"}, Bus: media.NewBus()}
	sps := []byte{0x67, 0x42, 0xC0, 0x1E}
	pps := []byte{0x68, 0xCE}
	aacConfig := []byte{0x12, 0x10} // AAC-LC, 44100Hz, stereo
	stream.Bus.SetCodecInfo(&media.CodecInfo{
		VideoCodec: media.CodecH264, AudioCodec: media.CodecAAC,
		Width: 64, Height: 64, SPSNALu: sps, PPSNALu: pps, AACConfig: aacConfig,
	})

	baseDir := t.TempDir()
	p, err := NewPackager(cfg, stream, baseDir, nil)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}
	if !p.useSplitAudio() {
		t.Fatal("expected useSplitAudio() to be true")
	}

	// DTS is deliberately nonzero: segStartDTS uses <=0 as its own "segment
	// not started" sentinel (see the note in
	// TestHandleVideo_LazyInitsFromCodecInfoWithoutConfigPacket above).
	if err := p.handleVideoFMP4(&media.Packet{Type: media.PacketKeyframe, Codec: media.CodecH264, IsKeyframe: true, DTS: 1000, PTS: 1000, Data: []byte{0, 0, 0, 1, 0x65, 1, 2, 3}}); err != nil {
		t.Fatalf("handleVideoFMP4 keyframe: %v", err)
	}
	if err := p.handleAudioFMP4(&media.Packet{Type: media.PacketMedia, Codec: media.CodecAAC, DTS: 1000, PTS: 1000, Data: []byte{1, 2, 3, 4}}); err != nil {
		t.Fatalf("handleAudioFMP4: %v", err)
	}
	if err := p.flushSegment(true); err != nil {
		t.Fatalf("flushSegment: %v", err)
	}

	streamDir := filepath.Join(baseDir, "hls", "test", "s1")
	for _, path := range []string{
		filepath.Join(streamDir, "init.mp4"),
		filepath.Join(streamDir, "seg000000.m4s"),
		filepath.Join(streamDir, "audio", "init.mp4"),
		filepath.Join(streamDir, "audio", "seg000000.m4s"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", path, err)
		}
	}

	videoPlaylist := p.playlist.Render()
	if strings.Contains(videoPlaylist, "audio/") {
		t.Errorf("video playlist should not reference the audio subdirectory, got:\n%s", videoPlaylist)
	}
	if !strings.Contains(videoPlaylist, "seg000000.m4s") {
		t.Errorf("video playlist missing its own segment, got:\n%s", videoPlaylist)
	}

	audioPlaylist := p.audioPlaylist.Render()
	if !strings.Contains(audioPlaylist, "seg000000.m4s") {
		t.Errorf("audio playlist missing its segment, got:\n%s", audioPlaylist)
	}
}

// TestPackager_SplitAudioTrack_RejectedWithLLHLS guards the documented
// scope limit: separate_audio_track + ll_hls.enabled together is not yet
// supported (LL-HLS parts across two independent playlists is a distinct
// future item) — NewPackager must silently fall back to combined mode
// rather than attempting an unsupported combination.
func TestPackager_SplitAudioTrack_RejectedWithLLHLS(t *testing.T) {
	cfg := &config.Config{}
	cfg.HLS.Container = "fmp4"
	cfg.HLS.SegmentDurationS = 2
	cfg.HLS.SeparateAudioTrack = true
	cfg.HLS.LLHls.Enabled = true
	cfg.HLS.LLHls.PartDurationMs = 200

	stream := &media.Stream{Key: media.StreamKey{App: "test", Name: "s2"}, Bus: media.NewBus()}
	p, err := NewPackager(cfg, stream, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}
	if p.useSplitAudio() {
		t.Error("expected separate_audio_track to be ignored when ll_hls.enabled is also set")
	}
}

// TestHandleAudioOnlyTS_IcecastMP3ProducesRealSegments drives the
// audio-only TS branch (Stream.SourceType == "icecast") end-to-end through
// a real Packager and confirms it actually cuts multiple real segment
// files by wall-clock/DTS duration alone — there's no video keyframe here
// to drive segment cuts the way every other source does.
func TestHandleAudioOnlyTS_IcecastMP3ProducesRealSegments(t *testing.T) {
	cfg := &config.Config{}
	cfg.HLS.Container = "ts"
	cfg.HLS.SegmentDurationS = 1

	stream := &media.Stream{
		Key:        media.StreamKey{App: "test", Name: "radio"},
		Bus:        media.NewBus(),
		SourceType: "icecast",
	}
	stream.Bus.SetCodecInfo(&media.CodecInfo{AudioCodec: media.CodecMP3, SampleRate: 44100, Channels: 2})

	baseDir := t.TempDir()
	p, err := NewPackager(cfg, stream, baseDir, nil)
	if err != nil {
		t.Fatalf("NewPackager: %v", err)
	}

	// ~2352 ticks (90kHz) per 1152-sample MPEG1 Layer III frame at 44.1kHz.
	// 50 frames covers a bit over 1 real second, enough to force at least
	// one 1s segment cut. pts starts at 1, not 0 — a literal 0 would hit
	// segStartDTS's own <=0 "not open" sentinel collision, the same
	// footgun internal/icecast.newSource's doc comment avoids for exactly
	// this reason.
	const ticksPerFrame = 90000 * 1152 / 44100
	pts := int64(1)
	for i := 0; i < 50; i++ {
		pkt := &media.Packet{
			Type:  media.PacketMedia,
			Codec: media.CodecMP3,
			DTS:   pts,
			PTS:   pts,
			Data:  []byte{0xFF, 0xFB, 0x90, 0x00, byte(i)}, // opaque payload — content doesn't matter for muxing
		}
		if err := p.handleAudio(pkt); err != nil {
			t.Fatalf("handleAudio frame %d: %v", i, err)
		}
		pts += ticksPerFrame
	}
	if err := p.flushSegment(true); err != nil {
		t.Fatalf("flushSegment: %v", err)
	}

	streamDir := filepath.Join(baseDir, "hls", "test", "radio")
	entries, err := os.ReadDir(streamDir)
	if err != nil {
		t.Fatalf("read stream dir: %v", err)
	}
	var segFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ts") {
			segFiles = append(segFiles, e.Name())
			info, err := e.Info()
			if err != nil || info.Size() == 0 {
				t.Errorf("segment %s is empty or unreadable: %v", e.Name(), err)
			}
		}
	}
	if len(segFiles) < 2 {
		t.Fatalf("got %d .ts segment(s), want at least 2 — segment-cut-by-duration for an audio-only source isn't working: %v", len(segFiles), segFiles)
	}
}
