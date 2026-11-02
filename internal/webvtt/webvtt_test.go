package webvtt

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
)

// readBox reads one box header at the start of b, returning its type and
// body.
func readBox(t *testing.T, b []byte) (boxType string, body []byte, total int) {
	t.Helper()
	if len(b) < 8 {
		t.Fatalf("buffer too short for a box header: %d bytes", len(b))
	}
	size := binary.BigEndian.Uint32(b[0:4])
	boxType = string(b[4:8])
	if int(size) > len(b) {
		t.Fatalf("box %q size %d exceeds buffer length %d", boxType, size, len(b))
	}
	return boxType, b[8:size], int(size)
}

func TestRenderSamples_SingleCueFillsWholeSegment(t *testing.T) {
	cues := []captiondecode.Cue{{StartPTS: 0, EndPTS: 90000, Text: "Hello"}}
	samples := RenderSamples(cues, 0, 4*90000)

	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2 (cue + trailing filler), samples=%+v", len(samples), samples)
	}

	// First sample: the cue itself, 0 -> 1s.
	if samples[0].DTS != 0 || samples[0].Duration != 90000 {
		t.Errorf("samples[0] = %+v, want DTS=0 Duration=90000", samples[0])
	}
	boxType, body, total := readBox(t, samples[0].Data)
	if boxType != "vttc" {
		t.Fatalf("samples[0] box type = %q, want vttc", boxType)
	}
	if total != len(samples[0].Data) {
		t.Errorf("vttc box size %d != actual data length %d", total, len(samples[0].Data))
	}
	paylType, paylBody, _ := readBox(t, body)
	if paylType != "payl" {
		t.Fatalf("expected payl child box, got %q", paylType)
	}
	if string(paylBody) != "Hello" {
		t.Errorf("payl text = %q, want %q", paylBody, "Hello")
	}

	// Second sample: filler covering the rest of the segment.
	if samples[1].DTS != 90000 || samples[1].Duration != 3*90000 {
		t.Errorf("samples[1] = %+v, want DTS=90000 Duration=270000", samples[1])
	}
	fillerType, fillerBody, _ := readBox(t, samples[1].Data)
	if fillerType != "vtte" {
		t.Fatalf("filler box type = %q, want vtte", fillerType)
	}
	if len(fillerBody) != 0 {
		t.Errorf("vtte body = %v, want empty", fillerBody)
	}
}

func TestRenderSamples_NoCuesProducesOneEmptySample(t *testing.T) {
	samples := RenderSamples(nil, 0, 4*90000)
	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(samples))
	}
	if samples[0].DTS != 0 || samples[0].Duration != 4*90000 {
		t.Errorf("samples[0] = %+v, want DTS=0 Duration=360000", samples[0])
	}
	boxType, _, _ := readBox(t, samples[0].Data)
	if boxType != "vtte" {
		t.Errorf("box type = %q, want vtte (empty)", boxType)
	}
}

func TestRenderSamples_GapBetweenCuesGetsFillerSample(t *testing.T) {
	cues := []captiondecode.Cue{
		{StartPTS: 90000, EndPTS: 180000, Text: "First"},   // 1s -> 2s
		{StartPTS: 270000, EndPTS: 360000, Text: "Second"}, // 3s -> 4s
	}
	samples := RenderSamples(cues, 0, 4*90000)

	// Expected: [filler 0-1s] [cue 1-2s] [filler 2-3s] [cue 3-4s]
	if len(samples) != 4 {
		t.Fatalf("got %d samples, want 4, samples=%+v", len(samples), samples)
	}
	wantDTS := []int64{0, 90000, 180000, 270000}
	wantDur := []int64{90000, 90000, 90000, 90000}
	for i, s := range samples {
		if s.DTS != wantDTS[i] || s.Duration != wantDur[i] {
			t.Errorf("samples[%d] = %+v, want DTS=%d Duration=%d", i, s, wantDTS[i], wantDur[i])
		}
	}
	if bt, _, _ := readBox(t, samples[0].Data); bt != "vtte" {
		t.Errorf("samples[0] type = %q, want vtte", bt)
	}
	if bt, _, _ := readBox(t, samples[1].Data); bt != "vttc" {
		t.Errorf("samples[1] type = %q, want vttc", bt)
	}
	if bt, _, _ := readBox(t, samples[2].Data); bt != "vtte" {
		t.Errorf("samples[2] type = %q, want vtte", bt)
	}
	if bt, _, _ := readBox(t, samples[3].Data); bt != "vttc" {
		t.Errorf("samples[3] type = %q, want vttc", bt)
	}
}

func TestRenderSamples_ClipsToSegmentBoundaries(t *testing.T) {
	// Cue spans from before the segment to after it; must be clipped to
	// exactly the segment window.
	cues := []captiondecode.Cue{{StartPTS: -90000, EndPTS: 5 * 90000, Text: "Spans"}}
	samples := RenderSamples(cues, 0, 4*90000)

	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1 (cue fills exactly the segment), samples=%+v", len(samples), samples)
	}
	if samples[0].DTS != 0 || samples[0].Duration != 4*90000 {
		t.Errorf("samples[0] = %+v, want DTS=0 Duration=360000 (clipped to segment)", samples[0])
	}
}

func TestRenderSamples_ExcludesNonOverlappingCues(t *testing.T) {
	cues := []captiondecode.Cue{
		{StartPTS: -2 * 90000, EndPTS: -90000, Text: "Before"},
		{StartPTS: 5 * 90000, EndPTS: 6 * 90000, Text: "After"},
	}
	samples := RenderSamples(cues, 0, 4*90000)

	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1 (all-empty segment), samples=%+v", len(samples), samples)
	}
	if bt, _, _ := readBox(t, samples[0].Data); bt != "vtte" {
		t.Errorf("type = %q, want vtte", bt)
	}
}

func TestEncodeCue_BoxSizeExact(t *testing.T) {
	data := encodeCue("hi")
	// vttc(8) + payl(8 + 2) = 18 bytes total.
	if len(data) != 18 {
		t.Fatalf("encodeCue length = %d, want 18", len(data))
	}
	if !bytes.Contains(data, []byte("payl")) || !bytes.Contains(data, []byte("vttc")) || !bytes.Contains(data, []byte("hi")) {
		t.Errorf("encoded cue missing expected box types/content: %x", data)
	}
}
