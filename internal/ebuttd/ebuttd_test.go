package ebuttd

import (
	"strings"
	"testing"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
)

func TestRender_BasicCue(t *testing.T) {
	cues := []captiondecode.Cue{
		{StartPTS: 90000, EndPTS: 270000, Text: "Hello, world"}, // 1.0s -> 3.0s absolute
	}
	// Segment starts at 90000 (1.0s absolute), 6s long (540000 ticks).
	doc := Render(cues, 90000, 6*90000)
	s := string(doc)

	if !strings.Contains(s, `xmlns="http://www.w3.org/ns/ttml"`) {
		t.Error("missing required TTML namespace")
	}
	if !strings.Contains(s, "Hello, world") {
		t.Error("cue text missing from rendered document")
	}
	if !strings.Contains(s, `begin="00:00:00.000"`) {
		t.Errorf("expected begin relative to segment start (00:00:00.000), got: %s", s)
	}
	if !strings.Contains(s, `end="00:00:02.000"`) {
		t.Errorf("expected end at 2s relative to segment start, got: %s", s)
	}
}

// TestRender_DefinesAndReferencesRegion is a regression test for a real
// live-playback bug: dash.js's TTML renderer (via imscJS's generateISD)
// only ever surfaces a cue whose computed ISD has non-empty region
// contents. Without any <region> defined in <head><layout> and referenced
// by each <p>, imscJS never assigns rendered text to a region, so every
// cue silently produces empty region contents and never renders — no
// error, no warning anywhere in the pipeline, regardless of correct
// timing. Confirmed live: dash.js fetched and buffered every text fragment
// correctly, yet zero cues ever appeared in the player, with or without an
// app-attached TTML rendering div, until a region was added.
func TestRender_DefinesAndReferencesRegion(t *testing.T) {
	cues := []captiondecode.Cue{
		{StartPTS: 0, EndPTS: 90000, Text: "Region test"},
	}
	doc := Render(cues, 0, 4*90000)
	s := string(doc)

	if !strings.Contains(s, "<layout>") || !strings.Contains(s, "<region ") {
		t.Errorf("expected a <region> defined in <head><layout>, got:\n%s", s)
	}
	if !strings.Contains(s, `xml:id="region0"`) {
		t.Errorf("expected the region to declare xml:id=\"region0\", got:\n%s", s)
	}
	if !strings.Contains(s, `region="region0"`) {
		t.Errorf("expected the <p> to reference region=\"region0\", got:\n%s", s)
	}
}

func TestRender_EmptyCuesStillProducesValidDocument(t *testing.T) {
	doc := Render(nil, 0, 6*90000)
	s := string(doc)
	if !strings.Contains(s, "<tt") || !strings.Contains(s, "<body") {
		t.Errorf("expected a valid empty document, got: %s", s)
	}
}

func TestRender_ClipsToSegmentBoundaries(t *testing.T) {
	// Cue starts before the segment and ends after it — should be clipped
	// to exactly the segment's own window, not the cue's full real span.
	cues := []captiondecode.Cue{
		{StartPTS: 0, EndPTS: 1000000, Text: "spans boundary"},
	}
	doc := Render(cues, 90000, 90000) // segment: [90000, 180000)
	s := string(doc)
	if !strings.Contains(s, `begin="00:00:00.000"`) {
		t.Errorf("expected clipped begin at segment start, got: %s", s)
	}
	if !strings.Contains(s, `end="00:00:01.000"`) {
		t.Errorf("expected clipped end at segment's own 1s duration, got: %s", s)
	}
}

func TestRender_ExcludesNonOverlappingCues(t *testing.T) {
	cues := []captiondecode.Cue{
		{StartPTS: 0, EndPTS: 90000, Text: "before segment"},
		{StartPTS: 1000000, EndPTS: 1100000, Text: "after segment"},
	}
	doc := Render(cues, 200000, 90000) // segment: [200000, 290000)
	s := string(doc)
	if strings.Contains(s, "before segment") || strings.Contains(s, "after segment") {
		t.Errorf("non-overlapping cues should be excluded, got: %s", s)
	}
}
