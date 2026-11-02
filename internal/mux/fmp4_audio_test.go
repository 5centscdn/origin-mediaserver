package mux

import (
	"bytes"
	"testing"
)

// findEsdsObjectType walks a real init segment down to the mp4a sample
// entry's esds box and returns the objectTypeIndication byte, plus whether
// a DecoderSpecificInfo (tag 0x05) is present.
func findEsdsObjectType(t *testing.T, initSeg []byte) (objectType byte, hasDSI bool) {
	t.Helper()
	_, _, ftypTotal := readBox(t, initSeg)
	_, moovBody, _ := readBox(t, initSeg[ftypTotal:])
	trakBody := findChildBox(t, moovBody, "trak")
	mdiaBody := findChildBox(t, trakBody, "mdia")
	minfBody := findChildBox(t, mdiaBody, "minf")
	stblBody := findChildBox(t, minfBody, "stbl")
	stsdBody := findChildBox(t, stblBody, "stsd")
	if len(stsdBody) < 8 {
		t.Fatalf("stsd body too short: %d bytes", len(stsdBody))
	}
	mp4aType, mp4aBody, _ := readBox(t, stsdBody[8:])
	if mp4aType != "mp4a" {
		t.Fatalf("expected mp4a sample entry, got %q", mp4aType)
	}
	if len(mp4aBody) < 28 {
		t.Fatalf("mp4a body too short: %d bytes", len(mp4aBody))
	}
	esdsType, esdsBody, _ := readBox(t, mp4aBody[28:]) // skip the fixed AudioSampleEntry fields
	if esdsType != "esds" {
		t.Fatalf("expected esds box, got %q", esdsType)
	}
	// esds body layout (offsets from 0): version+flags(4) | ES_DescrTag(1)
	// len(1) ES_ID(2) flags(1) | DecoderConfigDescriptor tag(1) len(1)
	// objectTypeIndication(1) streamType(1) bufferSizeDB(3) maxBitrate(4)
	// avgBitrate(4) | optional DecoderSpecificInfo tag(1) len(1) data(...) |
	// SLConfigDescriptor(3).
	const objectTypeOffset = 4 + 5 + 2                              // = 11
	const afterFixedDecoderConfigFields = objectTypeOffset + 1 + 12 // = 24: where a DSI tag would start
	if len(esdsBody) <= objectTypeOffset {
		t.Fatalf("esds body too short: %d bytes", len(esdsBody))
	}
	objectType = esdsBody[objectTypeOffset]
	// A DSI tag is present iff the esds body extends past the fixed
	// DecoderConfigDescriptor fields plus the trailing 3-byte
	// SLConfigDescriptor — i.e. there's a tag+len+data block in between.
	hasDSI = len(esdsBody) > afterFixedDecoderConfigFields+3
	return objectType, hasDSI
}

// TestWriteInitSegment_AudioTrack_AACObjectType is a regression test: the
// default (AudioObjectType left zero) must still produce AAC's 0x40 with a
// DecoderSpecificInfo carrying AACConfig — unchanged behavior from before
// MP2 support was added.
func TestWriteInitSegment_AudioTrack_AACObjectType(t *testing.T) {
	track := &FMP4Track{
		Type:       TrackAudio,
		TrackID:    2,
		Timescale:  44100,
		AACConfig:  []byte{0x12, 0x10},
		SampleRate: 44100,
		Channels:   2,
	}
	muxer := NewFMP4Muxer(track)
	var buf bytes.Buffer
	if err := muxer.WriteInitSegment(&buf); err != nil {
		t.Fatalf("WriteInitSegment: %v", err)
	}
	objectType, hasDSI := findEsdsObjectType(t, buf.Bytes())
	if objectType != AACAudioObjectType {
		t.Errorf("objectTypeIndication = %#x, want %#x (AAC)", objectType, AACAudioObjectType)
	}
	if !hasDSI {
		t.Error("expected a DecoderSpecificInfo tag (0x05) carrying AACConfig, found none")
	}
}

// TestWriteInitSegment_AudioTrack_MP2ObjectType is a regression test for
// real MP2 (MPEG-1/2 Audio Layer II) passthrough support: the sample entry
// must advertise objectTypeIndication 0x6B (ISO/IEC 11172-3) and must NOT
// include a DecoderSpecificInfo tag — MP2 has no AudioSpecificConfig-style
// out-of-band config; every frame's own header self-describes sample
// rate/channels (see internal/ingest/mpegts/mp2.go).
func TestWriteInitSegment_AudioTrack_MP2ObjectType(t *testing.T) {
	track := &FMP4Track{
		Type:            TrackAudio,
		TrackID:         2,
		Timescale:       48000,
		SampleRate:      48000,
		Channels:        2,
		AudioObjectType: MPEG1AudioObjectType,
	}
	muxer := NewFMP4Muxer(track)
	var buf bytes.Buffer
	if err := muxer.WriteInitSegment(&buf); err != nil {
		t.Fatalf("WriteInitSegment: %v", err)
	}
	objectType, hasDSI := findEsdsObjectType(t, buf.Bytes())
	if objectType != MPEG1AudioObjectType {
		t.Errorf("objectTypeIndication = %#x, want %#x (MPEG-1 Audio)", objectType, MPEG1AudioObjectType)
	}
	if hasDSI {
		t.Error("expected no DecoderSpecificInfo tag for MP2 (no AudioSpecificConfig equivalent), found one")
	}
}
