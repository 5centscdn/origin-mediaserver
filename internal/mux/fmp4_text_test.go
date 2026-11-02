package mux

import (
	"bytes"
	"testing"
)

// TestWriteInitSegment_TextTrack_STPPSampleEntry verifies a TrackText's init
// segment carries a well-formed 'stpp' (XMLSubtitleSampleEntry) sample
// entry with a 'subt' handler and 'nmhd' media header box — structurally
// correct per ISO/IEC 14496-30/14496-12 box layout, not just self-consistent
// with this muxer's own reader (reuses the readBox/findChildBox helpers
// from fmp4_test.go's TrunDataOffset test).
func TestWriteInitSegment_TextTrack_STPPSampleEntry(t *testing.T) {
	track := &FMP4Track{
		Type:      TrackText,
		TrackID:   3,
		Timescale: 90000,
		Namespace: "http://www.w3.org/ns/ttml",
	}
	muxer := NewFMP4Muxer(track)

	var buf bytes.Buffer
	if err := muxer.WriteInitSegment(&buf); err != nil {
		t.Fatalf("WriteInitSegment: %v", err)
	}
	out := buf.Bytes()

	ftypType, _, ftypTotal := readBox(t, out)
	if ftypType != "ftyp" {
		t.Fatalf("expected ftyp first, got %q", ftypType)
	}

	moovType, moovBody, _ := readBox(t, out[ftypTotal:])
	if moovType != "moov" {
		t.Fatalf("expected moov after ftyp, got %q", moovType)
	}

	trakBody := findChildBox(t, moovBody, "trak")
	mdiaBody := findChildBox(t, trakBody, "mdia")

	hdlrBody := findChildBox(t, mdiaBody, "hdlr")
	if len(hdlrBody) < 12 {
		t.Fatalf("hdlr body too short: %d bytes", len(hdlrBody))
	}
	handlerType := string(hdlrBody[8:12]) // version+flags(4) + pre_defined(4) + handler_type(4)
	if handlerType != "subt" {
		t.Errorf("hdlr handler_type = %q, want %q", handlerType, "subt")
	}

	minfBody := findChildBox(t, mdiaBody, "minf")
	var mediaHeaderBox string
	pos := 0
	for pos < len(minfBody) {
		bt, _, total := readBox(t, minfBody[pos:])
		if bt == "nmhd" || bt == "vmhd" || bt == "smhd" {
			mediaHeaderBox = bt
			break
		}
		pos += total
	}
	if mediaHeaderBox != "nmhd" {
		t.Errorf("media header box = %q, want nmhd (Null Media Header) for a text track", mediaHeaderBox)
	}

	stblBody := findChildBox(t, minfBody, "stbl")
	stsdBody := findChildBox(t, stblBody, "stsd")
	if len(stsdBody) < 8 {
		t.Fatalf("stsd body too short: %d bytes", len(stsdBody))
	}
	stppType, stppBody, _ := readBox(t, stsdBody[8:]) // skip version+flags(4)+entry_count(4)
	if stppType != "stpp" {
		t.Fatalf("expected stpp sample entry, got %q", stppType)
	}

	if len(stppBody) < 8 {
		t.Fatalf("stpp body too short: %d bytes", len(stppBody))
	}
	nsBytes := stppBody[8:] // skip reserved(6)+data_reference_index(2)
	nul := bytes.IndexByte(nsBytes, 0)
	if nul < 0 {
		t.Fatalf("stpp namespace field is not null-terminated")
	}
	if got := string(nsBytes[:nul]); got != track.Namespace {
		t.Errorf("stpp namespace = %q, want %q", got, track.Namespace)
	}
}

// TestWriteInitSegment_TextTrack_WVTTSampleEntry verifies a TrackText with
// TextFormat="wvtt" carries a well-formed 'wvtt' (WVTTSampleEntry) sample
// entry with a 'vttC' config box — the WebVTT-in-ISOBMFF path used outside
// the DVB profile (see internal/webvtt).
func TestWriteInitSegment_TextTrack_WVTTSampleEntry(t *testing.T) {
	track := &FMP4Track{
		Type:       TrackText,
		TrackID:    3,
		Timescale:  90000,
		TextFormat: "wvtt",
	}
	muxer := NewFMP4Muxer(track)

	var buf bytes.Buffer
	if err := muxer.WriteInitSegment(&buf); err != nil {
		t.Fatalf("WriteInitSegment: %v", err)
	}
	out := buf.Bytes()

	_, _, ftypTotal := readBox(t, out)
	moovType, moovBody, _ := readBox(t, out[ftypTotal:])
	if moovType != "moov" {
		t.Fatalf("expected moov after ftyp, got %q", moovType)
	}

	trakBody := findChildBox(t, moovBody, "trak")
	mdiaBody := findChildBox(t, trakBody, "mdia")
	minfBody := findChildBox(t, mdiaBody, "minf")
	stblBody := findChildBox(t, minfBody, "stbl")
	stsdBody := findChildBox(t, stblBody, "stsd")
	if len(stsdBody) < 8 {
		t.Fatalf("stsd body too short: %d bytes", len(stsdBody))
	}
	wvttType, wvttBody, _ := readBox(t, stsdBody[8:]) // skip version+flags(4)+entry_count(4)
	if wvttType != "wvtt" {
		t.Fatalf("expected wvtt sample entry, got %q", wvttType)
	}

	if len(wvttBody) < 8 {
		t.Fatalf("wvtt body too short: %d bytes", len(wvttBody))
	}
	vttCType, vttCBody, _ := readBox(t, wvttBody[8:]) // skip reserved(6)+data_reference_index(2)
	if vttCType != "vttC" {
		t.Fatalf("expected vttC config box inside wvtt, got %q", vttCType)
	}
	if string(vttCBody) != "WEBVTT\n" {
		t.Errorf("vttC body = %q, want %q", vttCBody, "WEBVTT\n")
	}
}
