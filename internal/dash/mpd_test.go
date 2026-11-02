package dash

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestRenderMPD_VideoAndAudio(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoWidth:        1280,
		VideoHeight:       720,
		VideoCodecs:       "avc1.64001f",
		HasAudio:          true,
		AudioCodecs:       "mp4a.40.2",
		AudioSampleRate:   44100,
		AudioChannels:     2,
	}

	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}

	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}

	if m.Type != "dynamic" {
		t.Errorf("Type = %q, want dynamic", m.Type)
	}
	if m.AvailabilityStartTime != "2026-01-01T00:00:00Z" {
		t.Errorf("AvailabilityStartTime = %q", m.AvailabilityStartTime)
	}
	if len(m.Period.AdaptationSets) != 2 {
		t.Fatalf("got %d AdaptationSets, want 2 (video+audio)", len(m.Period.AdaptationSets))
	}

	video := m.Period.AdaptationSets[0]
	if video.MimeType != "video/mp4" {
		t.Errorf("video MimeType = %q", video.MimeType)
	}
	if video.SegmentTemplate.Initialization != "init-video.mp4" {
		t.Errorf("video init = %q", video.SegmentTemplate.Initialization)
	}
	if video.SegmentTemplate.Media != "seg-video-$Number$.m4s" {
		t.Errorf("video media = %q", video.SegmentTemplate.Media)
	}
	if video.SegmentTemplate.Duration != 4000 {
		t.Errorf("video segment duration = %d, want 4000ms", video.SegmentTemplate.Duration)
	}
	if len(video.Representations) != 1 {
		t.Fatalf("got %d video Representations, want 1", len(video.Representations))
	}
	vr := video.Representations[0]
	if vr.Codecs != "avc1.64001f" || vr.Width != 1280 || vr.Height != 720 || vr.Bandwidth == 0 {
		t.Errorf("video representation = %+v", vr)
	}

	audio := m.Period.AdaptationSets[1]
	if audio.MimeType != "audio/mp4" {
		t.Errorf("audio MimeType = %q", audio.MimeType)
	}
	if audio.SegmentTemplate.Initialization != "init-audio.mp4" {
		t.Errorf("audio init = %q", audio.SegmentTemplate.Initialization)
	}
	if len(audio.Representations) != 1 {
		t.Fatalf("got %d audio Representations, want 1", len(audio.Representations))
	}
	ar := audio.Representations[0]
	if ar.Codecs != "mp4a.40.2" || ar.AudioSamplingRate != 44100 || ar.Bandwidth == 0 {
		t.Errorf("audio representation = %+v", ar)
	}
	if ar.AudioChannelConf == nil || ar.AudioChannelConf.Value != "2" {
		t.Errorf("audio channel config = %+v", ar.AudioChannelConf)
	}
}

func TestRenderMPD_VideoOnly(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoWidth:        1920,
		VideoHeight:       1080,
		VideoCodecs:       "avc1.64002a",
		HasAudio:          false,
	}

	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}

	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}

	if len(m.Period.AdaptationSets) != 1 {
		t.Fatalf("got %d AdaptationSets, want 1 (video-only)", len(m.Period.AdaptationSets))
	}
	if m.Period.AdaptationSets[0].MimeType != "video/mp4" {
		t.Errorf("MimeType = %q, want video/mp4", m.Period.AdaptationSets[0].MimeType)
	}
}

func TestRenderGroupMPD_MultiBitrate(t *testing.T) {
	params := GroupManifestParams{
		AvailabilityStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SegmentDurationS:  4,
		PathPrefix:        "/dash",
		Variants: []GroupVariantParams{
			{
				App: "live", Stream: "camera_1", Bandwidth: 800_000,
				HasVideo: true, VideoWidth: 426, VideoHeight: 240, VideoCodecs: "avc1.42001f",
				HasAudio: true, AudioCodecs: "mp4a.40.2", AudioSampleRate: 44100, AudioChannels: 2,
			},
			{
				App: "live", Stream: "camera_2", // no Bandwidth hint — must fall back to the fixed estimate
				HasVideo: true, VideoWidth: 854, VideoHeight: 480, VideoCodecs: "avc1.64001f",
				HasAudio: true, AudioCodecs: "mp4a.40.2", AudioSampleRate: 44100, AudioChannels: 2,
			},
		},
	}

	data, err := RenderGroupMPD(params)
	if err != nil {
		t.Fatalf("RenderGroupMPD: %v", err)
	}

	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}

	if len(m.Period.AdaptationSets) != 2 {
		t.Fatalf("got %d AdaptationSets, want 2 (video+audio)", len(m.Period.AdaptationSets))
	}

	video := m.Period.AdaptationSets[0]
	if len(video.Representations) != 2 {
		t.Fatalf("got %d video Representations, want 2", len(video.Representations))
	}
	v1, v2 := video.Representations[0], video.Representations[1]
	if v1.ID != "camera_1" || v1.Bandwidth != 800_000 || v1.BaseURL != "/dash/live/camera_1/" {
		t.Errorf("camera_1 representation = %+v", v1)
	}
	if v2.ID != "camera_2" || v2.Bandwidth <= 0 || v2.BaseURL != "/dash/live/camera_2/" {
		t.Errorf("camera_2 representation = %+v", v2)
	}
	if v1.Width != 426 || v2.Width != 854 {
		t.Errorf("expected distinct resolutions per variant, got %d and %d", v1.Width, v2.Width)
	}

	audio := m.Period.AdaptationSets[1]
	if len(audio.Representations) != 2 {
		t.Fatalf("got %d audio Representations, want 2", len(audio.Representations))
	}
	if audio.Representations[0].ID != "camera_1-audio" || audio.Representations[0].BaseURL != "/dash/live/camera_1/" {
		t.Errorf("camera_1 audio representation = %+v", audio.Representations[0])
	}
}

func TestRenderGroupMPD_VideoOnlyVariant(t *testing.T) {
	params := GroupManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		PathPrefix:        "/dash",
		Variants: []GroupVariantParams{
			{App: "live", Stream: "camera_1", HasVideo: true, VideoWidth: 640, VideoHeight: 360, VideoCodecs: "avc1.42001f"},
		},
	}

	data, err := RenderGroupMPD(params)
	if err != nil {
		t.Fatalf("RenderGroupMPD: %v", err)
	}

	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}

	if len(m.Period.AdaptationSets) != 1 {
		t.Fatalf("got %d AdaptationSets, want 1 (video-only, no audio anywhere in the group)", len(m.Period.AdaptationSets))
	}
}

func TestRenderMPD_TimeShiftBufferDepth(t *testing.T) {
	base := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoWidth:        640, VideoHeight: 360, VideoCodecs: "avc1.42001f",
	}

	withDVR := base
	withDVR.DVRWindowS = 120
	data, err := RenderMPD(withDVR)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}
	if m.TimeShiftBufferDepth != "PT120S" {
		t.Errorf("TimeShiftBufferDepth = %q, want %q", m.TimeShiftBufferDepth, "PT120S")
	}

	withoutDVR := base // DVRWindowS left at zero value
	data, err = RenderMPD(withoutDVR)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	if strings.Contains(string(data), "timeShiftBufferDepth") {
		t.Errorf("expected no timeShiftBufferDepth attribute when DVRWindowS is 0, got:\n%s", data)
	}
}

func TestRenderGroupMPD_TimeShiftBufferDepth(t *testing.T) {
	params := GroupManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		DVRWindowS:        60,
		PathPrefix:        "/dash",
		Variants: []GroupVariantParams{
			{App: "live", Stream: "camera_1", HasVideo: true, VideoWidth: 640, VideoHeight: 360, VideoCodecs: "avc1.42001f"},
		},
	}
	data, err := RenderGroupMPD(params)
	if err != nil {
		t.Fatalf("RenderGroupMPD: %v", err)
	}
	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}
	if m.TimeShiftBufferDepth != "PT60S" {
		t.Errorf("TimeShiftBufferDepth = %q, want %q", m.TimeShiftBufferDepth, "PT60S")
	}
}

// TestRenderMPD_TextAdaptationSet covers the DVB-profile case (EBU-TT-D,
// codecs="stpp" — DVB-DASH mandates it). See
// TestRenderMPD_TextAdaptationSet_NonDVBUsesWebVTT for the non-DVB case.
func TestRenderMPD_TextAdaptationSet(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  6,
		HasVideo:          true,
		VideoWidth:        640, VideoHeight: 360, VideoCodecs: "avc1.42001f",
		HasText:    true,
		DVBProfile: true,
	}
	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}
	if len(m.Period.AdaptationSets) != 2 {
		t.Fatalf("got %d AdaptationSets, want 2 (video+text)", len(m.Period.AdaptationSets))
	}
	text := m.Period.AdaptationSets[1]
	if text.MimeType != "application/mp4" {
		t.Errorf("text.MimeType = %q, want application/mp4", text.MimeType)
	}
	if len(text.Representations) != 1 || text.Representations[0].Codecs != "stpp" {
		t.Errorf("text representation codecs = %+v, want exactly one with codecs=stpp under the DVB profile", text.Representations)
	}
	if text.SegmentTemplate.Initialization != "init-text.mp4" || text.SegmentTemplate.Media != "seg-text-$Number$.m4s" {
		t.Errorf("text SegmentTemplate = %+v, wrong filenames", text.SegmentTemplate)
	}
	if text.Role == nil || text.Role.Value != "caption" {
		t.Errorf("text.Role = %+v, want value=caption", text.Role)
	}
	if text.Accessibility == nil || text.Accessibility.SchemeIDURI != "urn:scte:dash:cc:cea-708:2015" {
		t.Errorf("text.Accessibility = %+v, want the CEA-708 SCTE scheme", text.Accessibility)
	}

	// HasText=false (the default) must not add a third AdaptationSet.
	params.HasText = false
	data, err = RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	if strings.Contains(string(data), "stpp") {
		t.Errorf("expected no stpp AdaptationSet when HasText is false, got:\n%s", data)
	}
}

// TestRenderMPD_TextAdaptationSet_NonDVBUsesWebVTT is a regression test: outside
// the DVB profile, the text Representation must advertise codecs="wvtt", not
// "stpp" — live EBU-TT-D/fragmented-TTML rendering was found unreliable
// across web DASH clients (see internal/webvtt's doc comment), so
// Packager.ensureInit muxes WebVTT samples instead whenever DVBProfile is
// off; the manifest must match what's actually on disk.
func TestRenderMPD_TextAdaptationSet_NonDVBUsesWebVTT(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoWidth:        640, VideoHeight: 360, VideoCodecs: "avc1.42001f",
		HasText:    true,
		DVBProfile: false,
	}
	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}
	if len(m.Period.AdaptationSets) != 2 {
		t.Fatalf("got %d AdaptationSets, want 2 (video+text)", len(m.Period.AdaptationSets))
	}
	text := m.Period.AdaptationSets[1]
	if len(text.Representations) != 1 || text.Representations[0].Codecs != "wvtt" {
		t.Errorf("text representation codecs = %+v, want exactly one with codecs=wvtt outside the DVB profile", text.Representations)
	}
	if strings.Contains(string(data), "stpp") {
		t.Errorf("expected no stpp anywhere in a non-DVB-profile manifest, got:\n%s", data)
	}
}

func TestRenderMPD_DVBProfile(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoWidth:        640, VideoHeight: 360, VideoCodecs: "avc1.42001f",
		DVBProfile:   true,
		UTCTimingURL: "https://example.com/dash-utc-time",
		BaseURL:      "https://example.com/dash/live/stream1/",
	}
	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	var m mpdXML
	if err := xml.Unmarshal(data, &m); err != nil {
		t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
	}

	if !strings.Contains(m.Profiles, "urn:dvb:dash:profile:dvb-dash:2014") {
		t.Errorf("Profiles = %q, missing DVB-DASH profile URN", m.Profiles)
	}
	if m.SuggestedPresentationDelay != "" {
		t.Errorf("SuggestedPresentationDelay = %q, want empty (deliberately omitted under DVB-DASH)", m.SuggestedPresentationDelay)
	}
	if m.UTCTiming == nil || m.UTCTiming.Value != "https://example.com/dash-utc-time" {
		t.Errorf("UTCTiming = %+v, want the configured UTC endpoint", m.UTCTiming)
	}
	if m.UTCTiming != nil && m.UTCTiming.SchemeIDURI != "urn:mpeg:dash:utc:http-xsdate:2014" {
		t.Errorf("UTCTiming.SchemeIDURI = %q, want the http-xsdate scheme", m.UTCTiming.SchemeIDURI)
	}
	if m.BaseURL == nil || m.BaseURL.URL != "https://example.com/dash/live/stream1/" {
		t.Errorf("BaseURL = %+v, want the configured URL", m.BaseURL)
	}
	// encoding/xml's Unmarshal doesn't reliably round-trip a "prefix:local"
	// struct tag back into the same field on decode (a known stdlib
	// asymmetry — Marshal writes it correctly; verified separately by
	// printing raw output), so check the actual bytes directly for the
	// dvb:priority/weight attributes rather than relying on the decoded
	// struct field.
	if !strings.Contains(string(data), `dvb:priority="1"`) || !strings.Contains(string(data), `dvb:weight="1"`) {
		t.Errorf("expected dvb:priority=\"1\" dvb:weight=\"1\" in raw output, got:\n%s", data)
	}

	// Non-DVB-profile output must retain the normal suggestedPresentationDelay
	// and must NOT include any DVB-only elements.
	params.DVBProfile = false
	data, err = RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "suggestedPresentationDelay") {
		t.Error("expected suggestedPresentationDelay present when DVBProfile is false")
	}
	if strings.Contains(s, "dvb:") || strings.Contains(s, "UTCTiming") {
		t.Errorf("expected no DVB-DASH elements when DVBProfile is false, got:\n%s", s)
	}
}

// TestRenderMPD_SegmentTimeline is a regression test for a real live-
// playback bug: closeSegment cuts each segment at the first keyframe *at or
// after* the configured target duration (see its own doc comment), so real
// segment durations are always >= the nominal target and vary with GOP
// length. Before this fix, RenderMPD always declared a flat nominal
// @duration with no <SegmentTimeline>, forcing every DASH client to
// extrapolate each segment's wall-clock position as
// availabilityStartTime + N*nominalDuration — an approximation that drifts
// further from reality every segment and, confirmed live, eventually broke
// decode on a long-running stream even though the segment bytes themselves
// were byte-correct. This verifies VideoTimeline entries render as a real,
// exact <SegmentTimeline>, with contiguous equal-duration runs compressed
// via @r and @startNumber taken from the timeline's own oldest entry
// (rather than hardcoded "1") so an aged-out-of-window start is handled
// correctly too.
func TestRenderMPD_SegmentTimeline(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoCodecs:       "avc1.64001f",
		VideoTimeline: []SegmentTimelineEntry{
			{Number: 50, StartTicks: 18935130, DurationTicks: 416970},
			{Number: 51, StartTicks: 19352100, DurationTicks: 416970}, // contiguous, same duration -> merges via r
			{Number: 52, StartTicks: 19769070, DurationTicks: 396000}, // different duration -> new <S>
		},
	}

	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	s := string(data)

	if strings.Contains(s, `duration="`) {
		t.Errorf("expected no flat @duration when a SegmentTimeline is present, got:\n%s", s)
	}
	if !strings.Contains(s, `timescale="90000"`) {
		t.Errorf("expected timescale=90000 (exact 90kHz ticks) with a SegmentTimeline, got:\n%s", s)
	}
	if !strings.Contains(s, `startNumber="50"`) {
		t.Errorf(`expected startNumber="50" (the timeline's oldest entry), got:%s`, s)
	}
	if !strings.Contains(s, `<S t="18935130" d="416970" r="1"></S>`) {
		t.Errorf("expected the two contiguous equal-duration segments merged via r=1, got:\n%s", s)
	}
	if !strings.Contains(s, `<S t="19769070" d="396000"></S>`) {
		t.Errorf("expected the differing-duration segment as its own <S> with no r, got:\n%s", s)
	}
}

// TestRenderMPD_SegmentTimeline_FallsBackWhenEmpty verifies the pre-fix flat-
// duration form is still used when no timeline data exists yet (e.g. the
// very first manifest write, before any segment has been cut) — the fix
// must not regress that case.
func TestRenderMPD_SegmentTimeline_FallsBackWhenEmpty(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		SegmentDurationS:  4,
		HasVideo:          true,
		VideoCodecs:       "avc1.64001f",
	}

	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	s := string(data)

	if !strings.Contains(s, `duration="4000"`) {
		t.Errorf(`expected flat duration="4000" fallback with no timeline data, got:%s`, s)
	}
	if strings.Contains(s, "SegmentTimeline") {
		t.Errorf("expected no SegmentTimeline element with no timeline data, got:\n%s", s)
	}
	if !strings.Contains(s, `startNumber="1"`) {
		t.Errorf(`expected startNumber="1" fallback, got:%s`, s)
	}
}

// TestRenderMPD_MinBufferTime is a regression test: @minBufferTime is
// REQUIRED by the DASH MPD schema (ISO/IEC 23009-1), not merely
// informational — a real DVB-DASH validator flagged its absence as an MPD
// signalling defect. Falls back to SegmentDurationS when unconfigured.
func TestRenderMPD_MinBufferTime(t *testing.T) {
	params := ManifestParams{
		AvailabilityStart: time.Now(),
		SegmentDurationS:  4,
		MinBufferTimeS:    8,
		HasVideo:          true,
		VideoCodecs:       "avc1.64001f",
	}
	data, err := RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	if !strings.Contains(string(data), `minBufferTime="PT8S"`) {
		t.Errorf(`expected minBufferTime="PT8S", got:%s`, data)
	}

	// Unconfigured (<=0) falls back to SegmentDurationS rather than
	// omitting the required attribute or emitting a degenerate PT0S.
	params.MinBufferTimeS = 0
	data, err = RenderMPD(params)
	if err != nil {
		t.Fatalf("RenderMPD: %v", err)
	}
	if !strings.Contains(string(data), `minBufferTime="PT4S"`) {
		t.Errorf(`expected minBufferTime="PT4S" fallback to SegmentDurationS, got:%s`, data)
	}
}
