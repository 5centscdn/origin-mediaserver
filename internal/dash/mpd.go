// Package dash provides the DASH packager: it reads from a MediaBus and
// produces per-track fMP4 init/media segments plus an MPD manifest served by
// the HTTP layer.
package dash

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fixed bandwidth estimates. No per-stream bitrate measurement exists
// anywhere in this codebase today (the HLS output lives with the same gap
// for its BANDWIDTH attribute) — Representation/@bandwidth is a required,
// non-zero MPD attribute, so these documented constants stand in for a real
// measurement, which is out of scope for this stage.
const (
	estimatedVideoBandwidth = 2_000_000
	estimatedAudioBandwidth = 128_000
)

// ManifestParams carries everything needed to render one stream's MPD.
type ManifestParams struct {
	AvailabilityStart time.Time
	SegmentDurationS  int

	// MinBufferTimeS backs the MPD's @minBufferTime — a REQUIRED attribute
	// per the DASH MPD schema (ISO/IEC 23009-1), not merely informational;
	// omitting it produces a non-conformant manifest even though most
	// players tolerate it silently. Falls back to SegmentDurationS when <=0
	// (e.g. unconfigured) — see config.DASHConfig.MinBufferTimeS.
	MinBufferTimeS int

	// DVRWindowS, when > 0, advertises timeShiftBufferDepth so compliant
	// DASH clients know they may seek backward this many seconds using
	// ordinary live $Number$ SegmentTemplate addressing — see
	// Packager.cleanupOldSegments, which is what actually keeps segments
	// on disk that long instead of being cleaned up immediately.
	DVRWindowS int

	HasVideo                bool
	VideoWidth, VideoHeight int
	VideoCodecs             string

	HasAudio                       bool
	AudioCodecs                    string
	AudioSampleRate, AudioChannels int

	// HasText enables the text/subtitle AdaptationSet (codecs="stpp" or
	// "wvtt" depending on DVBProfile), written by Packager's text track.
	HasText bool
	// TeletextSourced is true when the caption content actually comes from
	// converted DVB Teletext (internal/teletext) rather than CEA-608/708
	// SEI data (internal/captiondecode) — selects a matching Accessibility
	// scheme instead of unconditionally claiming CEA-708 regardless of the
	// real source, which a DVB-DASH validator flagged as inconsistent
	// signaling for a teletext-converted track.
	TeletextSourced bool

	// DVBProfile enables full DVB-DASH profile signaling: the MPD@profiles
	// URN list, an UTCTiming element, a BaseURL with dvb:priority/weight,
	// and suppresses suggestedPresentationDelay (deliberately omitted under
	// DVB-DASH, unlike normal DASH practice — see config.DASHConfig.DVB).
	DVBProfile bool
	// UTCTimingURL is the self-hosted endpoint DVBProfile's UTCTiming
	// element points at (see internal/api's UTC-time route) — required
	// when DVBProfile is set.
	UTCTimingURL string
	// BaseURL is this stream's own base URL (e.g.
	// "https://host/dash_prefix/app/stream/"), used for DVBProfile's
	// BaseURL element — required when DVBProfile is set.
	BaseURL string

	// VideoTimeline/AudioTimeline/TextTimeline, when non-empty, render a real
	// <SegmentTimeline> (90kHz ticks, exact) instead of a flat nominal
	// @duration — see SegmentTimelineEntry's doc comment for why the flat
	// form drifts and eventually breaks playback on a long-running stream.
	// Empty is a valid, intentional fallback (e.g. the very first manifest
	// write, before any segment has been cut yet): RenderMPD falls back to
	// the flat-duration form in that case.
	VideoTimeline, AudioTimeline, TextTimeline []SegmentTimelineEntry
}

// SegmentTimelineEntry describes one already-produced segment's real,
// as-muxed start time and duration (90kHz ticks — the same clock as the
// fMP4 tracks' own tfdt/trun values).
//
// Why this matters: Packager.closeSegment cuts each segment at the first
// keyframe *at or after* the configured target duration, so real segment
// durations are always >= the nominal target and vary with GOP length —
// never exactly the flat duration a plain $Number$-addressed SegmentTemplate
// declares. Without a SegmentTimeline, a client has no way to know this and
// must extrapolate segment N's wall-clock position as
// availabilityStartTime + N*nominalDuration; on a long-running stream that
// extrapolation drifts further from the segment's real, as-muxed timestamps
// with every segment, until a strict client's own timestamp-offset/append-
// window bookkeeping — built on that wrong extrapolation — conflicts badly
// enough with the segment's real internal tfdt to break decode (confirmed:
// raw MSE appendBuffer of the exact same bytes, with no timestamp
// remapping, decodes cleanly).
type SegmentTimelineEntry struct {
	Number        int
	StartTicks    int64
	DurationTicks int64
}

// ─── XML model ──────────────────────────────────────────────────────────────

type mpdXML struct {
	XMLName xml.Name `xml:"MPD"`
	Xmlns   string   `xml:"xmlns,attr"`
	// XmlnsDVB declares the "dvb" namespace prefix used by BaseURL's
	// dvb:priority/dvb:weight attributes below — present only when
	// DVBProfile is set.
	XmlnsDVB              string `xml:"xmlns:dvb,attr,omitempty"`
	Profiles              string `xml:"profiles,attr"`
	Type                  string `xml:"type,attr"`
	AvailabilityStartTime string `xml:"availabilityStartTime,attr"`
	MinimumUpdatePeriod   string `xml:"minimumUpdatePeriod,attr"`
	// MinBufferTime is REQUIRED by the DASH MPD schema — see
	// ManifestParams.MinBufferTimeS's doc comment.
	MinBufferTime string `xml:"minBufferTime,attr"`
	// SuggestedPresentationDelay is omitted entirely under DVB-DASH — a
	// deliberate deviation from normal DASH practice (see ManifestParams.
	// DVBProfile's doc comment) — hence omitempty rather than an
	// unconditional attribute.
	SuggestedPresentationDelay string         `xml:"suggestedPresentationDelay,attr,omitempty"`
	TimeShiftBufferDepth       string         `xml:"timeShiftBufferDepth,attr,omitempty"`
	UTCTiming                  *utcTimingXML  `xml:"UTCTiming,omitempty"`
	BaseURL                    *mpdBaseURLXML `xml:"BaseURL,omitempty"`
	Period                     periodXML      `xml:"Period"`
}

// utcTimingXML is DVB-DASH's required UTCTiming element — points at a
// self-hosted endpoint (see internal/api's UTC-time route) rather than a
// third-party time service, consistent with this project's "a node works
// standalone, no required external services" principle.
type utcTimingXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

// mpdBaseURLXML is the MPD-level BaseURL DVB-DASH expects, with its
// priority/weight attributes — single-origin today (this node has no real
// multi-CDN failover to declare), present only for DVB-DASH conformance
// syntax.
type mpdBaseURLXML struct {
	URL      string `xml:",chardata"`
	Priority int    `xml:"dvb:priority,attr"`
	Weight   int    `xml:"dvb:weight,attr"`
}

type periodXML struct {
	Start          string             `xml:"start,attr"`
	AdaptationSets []adaptationSetXML `xml:"AdaptationSet"`
}

type adaptationSetXML struct {
	MimeType         string `xml:"mimeType,attr"`
	SegmentAlignment bool   `xml:"segmentAlignment,attr"`
	// ContentType and Lang are set only on the text AdaptationSet (real
	// live-playback bug fix: with mimeType="application/mp4" alone —
	// ambiguous across video/audio/text in a fragmented context — dash.js
	// classified the track as text via its codecs="stpp" sniffing (visible
	// in getTracksFor's isText:true), but its live TTML-rendering wiring
	// silently never activated without this explicit hint. Confirmed by
	// diffing against dash.js's own reference EBU-TT-D sample stream, whose
	// text AdaptationSet declares both.
	ContentType     string              `xml:"contentType,attr,omitempty"`
	Lang            string              `xml:"lang,attr,omitempty"`
	Role            *roleXML            `xml:"Role,omitempty"`
	Accessibility   *roleXML            `xml:"Accessibility,omitempty"`
	SegmentTemplate segmentTemplateXML  `xml:"SegmentTemplate"`
	Representations []representationXML `xml:"Representation"`
}

// roleXML renders both <Role> and <Accessibility> (identical
// schemeIdUri/value attribute shape per the DASH schema — reused for both).
type roleXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

type segmentTemplateXML struct {
	Timescale       int                 `xml:"timescale,attr"`
	Duration        int                 `xml:"duration,attr,omitempty"`
	StartNumber     int                 `xml:"startNumber,attr"`
	Initialization  string              `xml:"initialization,attr"`
	Media           string              `xml:"media,attr"`
	SegmentTimeline *segmentTimelineXML `xml:"SegmentTimeline,omitempty"`
}

// segmentTimelineXML/sTimelineEntryXML render <SegmentTimeline>/<S> — see
// SegmentTimelineEntry's doc comment for why this exists instead of a flat
// @duration.
type segmentTimelineXML struct {
	S []sTimelineEntryXML `xml:"S"`
}

type sTimelineEntryXML struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	// R is the DASH spec's "additional repeat count": R=n means n+1 total
	// consecutive segments of duration D starting at T. Only set when
	// buildSegmentTemplate finds truly contiguous, equal-duration runs.
	R int `xml:"r,attr,omitempty"`
}

// buildSegmentTemplate renders one SegmentTemplate. With a non-empty
// timeline it emits a real <SegmentTimeline> in 90kHz ticks (exact, no
// rounding) and derives @startNumber from the timeline's own oldest
// retained entry — decoupling it from whatever segment numbers have aged
// out of this in-memory window vs. disk, rather than assuming "1". With an
// empty timeline (e.g. the very first manifest write, before any segment
// exists yet) it falls back to the flat nominal-duration form.
func buildSegmentTemplate(entries []SegmentTimelineEntry, fallbackDurMs int, init, media string) segmentTemplateXML {
	if len(entries) == 0 {
		return segmentTemplateXML{
			Timescale:      1000,
			Duration:       fallbackDurMs,
			StartNumber:    1,
			Initialization: init,
			Media:          media,
		}
	}

	var s []sTimelineEntryXML
	var expectNextT int64 = -1
	for _, e := range entries {
		if n := len(s); n > 0 && s[n-1].D == e.DurationTicks && e.StartTicks == expectNextT {
			s[n-1].R++
		} else {
			s = append(s, sTimelineEntryXML{T: e.StartTicks, D: e.DurationTicks})
		}
		expectNextT = e.StartTicks + e.DurationTicks
	}

	return segmentTemplateXML{
		Timescale:       90000,
		StartNumber:     entries[0].Number,
		Initialization:  init,
		Media:           media,
		SegmentTimeline: &segmentTimelineXML{S: s},
	}
}

type representationXML struct {
	ID                string                        `xml:"id,attr"`
	Codecs            string                        `xml:"codecs,attr"`
	Bandwidth         int                           `xml:"bandwidth,attr"`
	Width             int                           `xml:"width,attr,omitempty"`
	Height            int                           `xml:"height,attr,omitempty"`
	AudioSamplingRate int                           `xml:"audioSamplingRate,attr,omitempty"`
	AudioChannelConf  *audioChannelConfigurationXML `xml:"AudioChannelConfiguration"`

	// BaseURL, if set, overrides the Period/AdaptationSet BaseURL for this
	// Representation only — used by RenderGroupMPD to point each ABR
	// variant's Representation at its own independently-packaged segment
	// directory, since each variant runs its own single-rendition
	// dash.Packager writing into its own app/stream directory.
	BaseURL string `xml:"BaseURL,omitempty"`
}

type audioChannelConfigurationXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

// RenderMPD renders an MPD manifest for a single-rendition live DASH stream.
func RenderMPD(p ManifestParams) ([]byte, error) {
	segDurMs := p.SegmentDurationS * 1000
	minBufferTimeS := p.MinBufferTimeS
	if minBufferTimeS <= 0 {
		minBufferTimeS = p.SegmentDurationS
	}

	m := mpdXML{
		Xmlns:                 "urn:mpeg:dash:schema:mpd:2011",
		Profiles:              "urn:mpeg:dash:profile:isoff-live:2011",
		Type:                  "dynamic",
		AvailabilityStartTime: p.AvailabilityStart.UTC().Format(time.RFC3339),
		MinimumUpdatePeriod:   fmt.Sprintf("PT%dS", p.SegmentDurationS),
		MinBufferTime:         fmt.Sprintf("PT%dS", minBufferTimeS),
		Period: periodXML{
			Start: "PT0S",
		},
	}
	if p.DVBProfile {
		// suggestedPresentationDelay deliberately omitted under DVB-DASH —
		// see ManifestParams.DVBProfile's doc comment.
		m.Profiles = "urn:mpeg:dash:profile:isoff-live:2011,urn:dvb:dash:profile:dvb-dash:2014,urn:dvb:dash:profile:dvb-dash:isoff-ext-live:2014"
		m.XmlnsDVB = "urn:dvb:dash-extensions:2014"
		m.UTCTiming = &utcTimingXML{
			SchemeIDURI: "urn:mpeg:dash:utc:http-xsdate:2014",
			Value:       p.UTCTimingURL,
		}
		m.BaseURL = &mpdBaseURLXML{URL: p.BaseURL, Priority: 1, Weight: 1}
	} else {
		m.SuggestedPresentationDelay = fmt.Sprintf("PT%dS", 3*p.SegmentDurationS)
	}
	if p.DVRWindowS > 0 {
		m.TimeShiftBufferDepth = fmt.Sprintf("PT%dS", p.DVRWindowS)
	}

	if p.HasVideo {
		m.Period.AdaptationSets = append(m.Period.AdaptationSets, adaptationSetXML{
			MimeType:         "video/mp4",
			SegmentAlignment: true,
			SegmentTemplate:  buildSegmentTemplate(p.VideoTimeline, segDurMs, "init-video.mp4", "seg-video-$Number$.m4s"),
			Representations: []representationXML{{
				ID:        "video",
				Codecs:    p.VideoCodecs,
				Bandwidth: estimatedVideoBandwidth,
				Width:     p.VideoWidth,
				Height:    p.VideoHeight,
			}},
		})
	}

	if p.HasAudio {
		m.Period.AdaptationSets = append(m.Period.AdaptationSets, adaptationSetXML{
			MimeType:         "audio/mp4",
			SegmentAlignment: true,
			SegmentTemplate:  buildSegmentTemplate(p.AudioTimeline, segDurMs, "init-audio.mp4", "seg-audio-$Number$.m4s"),
			Representations: []representationXML{{
				ID:                "audio",
				Codecs:            p.AudioCodecs,
				Bandwidth:         estimatedAudioBandwidth,
				AudioSamplingRate: p.AudioSampleRate,
				AudioChannelConf: &audioChannelConfigurationXML{
					SchemeIDURI: "urn:mpeg:dash:23003:3:audio_channel_configuration:2011",
					Value:       fmt.Sprintf("%d", p.AudioChannels),
				},
			}},
		})
	}

	if p.HasText {
		// EBU-TT-D (stpp) under the DVB profile — DVB-DASH mandates it;
		// WebVTT (wvtt) otherwise — dash.js/browsers' more reliable text
		// path for live streams (see internal/webvtt's doc comment and
		// Packager.ensureInit, which mux the matching format).
		textCodecs := "stpp"
		if !p.DVBProfile {
			textCodecs = "wvtt"
		}
		// The Accessibility scheme must match the track's real origin — a
		// DVB-DASH validator flagged the CEA-708 SCTE scheme as
		// inconsistent when the actual content is converted DVB Teletext
		// (internal/teletext), not CEA-608/708 SEI data
		// (internal/captiondecode). TVA's generic AudioPurposeCS "hard of
		// hearing" classification (value "2") is the scheme the reference
		// EBU-TT-D-over-DVB sample stream this project verified against
		// (rdmedia.bbc.co.uk) itself uses for exactly this case.
		accessibility := &roleXML{SchemeIDURI: "urn:scte:dash:cc:cea-708:2015", Value: "1=lang:eng"}
		if p.TeletextSourced {
			accessibility = &roleXML{SchemeIDURI: "urn:tva:metadata:cs:AudioPurposeCS:2007", Value: "2"}
		}
		m.Period.AdaptationSets = append(m.Period.AdaptationSets, adaptationSetXML{
			MimeType:         "application/mp4",
			ContentType:      "text",
			Lang:             "eng",
			SegmentAlignment: true,
			Role:             &roleXML{SchemeIDURI: "urn:mpeg:dash:role:2011", Value: "caption"},
			Accessibility:    accessibility,
			SegmentTemplate:  buildSegmentTemplate(p.TextTimeline, segDurMs, "init-text.mp4", "seg-text-$Number$.m4s"),
			Representations: []representationXML{{
				ID:        "text",
				Codecs:    textCodecs,
				Bandwidth: 1000, // nominal — subtitle bitrate is negligible and unmeasured, same gap as video/audio's own fixed estimates
			}},
		})
	}

	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal mpd: %w", err)
	}
	out := append([]byte(xml.Header), body...)
	return out, nil
}

// GroupVariantParams describes one ABR variant's contribution to a
// multi-bitrate MPD. Each variant runs its own independent, single-rendition
// dash.Packager writing into its own <data_dir>/dash/<App>/<Stream>
// directory — RenderGroupMPD never writes segments itself, it only builds a
// manifest whose Representations point at those already-existing variant
// directories via a per-Representation BaseURL.
type GroupVariantParams struct {
	App, Stream string
	// Bandwidth is the variant's real bitrate hint (media.Stream.ABRBandwidthHint,
	// sourced from config's ABR pattern) — falls back to the same fixed
	// estimate RenderMPD uses when unset, since no per-stream bitrate
	// measurement exists anywhere in this codebase.
	Bandwidth int

	HasVideo                bool
	VideoWidth, VideoHeight int
	VideoCodecs             string

	HasAudio                       bool
	AudioCodecs                    string
	AudioSampleRate, AudioChannels int
}

// GroupManifestParams carries everything needed to render a multi-bitrate
// MPD for an ABR group formed by RTMP config %i patterns — one video
// AdaptationSet with one Representation per variant that has video, and one
// audio AdaptationSet with one Representation per variant that has audio.
// Mirrors hls.RenderMasterPlaylist's per-variant listing, just expressed in
// DASH's AdaptationSet/Representation model instead of HLS's flat
// #EXT-X-STREAM-INF list.
type GroupManifestParams struct {
	AvailabilityStart time.Time
	SegmentDurationS  int
	// MinBufferTimeS — see ManifestParams.MinBufferTimeS.
	MinBufferTimeS int
	// DVRWindowS, when > 0, advertises timeShiftBufferDepth — see
	// ManifestParams.DVRWindowS.
	DVRWindowS int
	// PathPrefix is the DASH HTTP mount point (e.g. "/dash", from
	// config.DASHConfig.PathPrefix) — used to build each Representation's
	// BaseURL so a client resolves seg-*.m4s against the right variant.
	PathPrefix string
	Variants   []GroupVariantParams

	// HasText/DVBProfile mirror ManifestParams' fields of the same name —
	// present here for a future stage (per-variant or shared EBU-TT-D text
	// tracks across an ABR group is explicitly out of scope for the pass
	// that added these; see internal/captiondecode). DVBProfile/
	// UTCTimingURL/BaseURL are already wired into this function's own MPD
	// profile signaling below (cheap and consistent to add now), even
	// though no caller sets HasText for a group MPD yet.
	HasText      bool
	DVBProfile   bool
	UTCTimingURL string
	BaseURL      string
}

// RenderGroupMPD renders a multi-bitrate MPD for an ABR group. Unlike
// RenderMPD (one Representation per AdaptationSet, written once to a
// variant's own directory), this is rendered fresh on every request — like
// hls.handlePlaylist's registry-first branch — since the group's variant
// membership and each variant's live CodecInfo can change between requests.
func RenderGroupMPD(p GroupManifestParams) ([]byte, error) {
	segDurMs := p.SegmentDurationS * 1000
	prefix := strings.TrimSuffix(p.PathPrefix, "/")
	minBufferTimeS := p.MinBufferTimeS
	if minBufferTimeS <= 0 {
		minBufferTimeS = p.SegmentDurationS
	}

	m := mpdXML{
		Xmlns:                 "urn:mpeg:dash:schema:mpd:2011",
		Profiles:              "urn:mpeg:dash:profile:isoff-live:2011",
		Type:                  "dynamic",
		AvailabilityStartTime: p.AvailabilityStart.UTC().Format(time.RFC3339),
		MinimumUpdatePeriod:   fmt.Sprintf("PT%dS", p.SegmentDurationS),
		MinBufferTime:         fmt.Sprintf("PT%dS", minBufferTimeS),
		Period: periodXML{
			Start: "PT0S",
		},
	}
	if p.DVBProfile {
		m.Profiles = "urn:mpeg:dash:profile:isoff-live:2011,urn:dvb:dash:profile:dvb-dash:2014,urn:dvb:dash:profile:dvb-dash:isoff-ext-live:2014"
		m.XmlnsDVB = "urn:dvb:dash-extensions:2014"
		m.UTCTiming = &utcTimingXML{
			SchemeIDURI: "urn:mpeg:dash:utc:http-xsdate:2014",
			Value:       p.UTCTimingURL,
		}
		m.BaseURL = &mpdBaseURLXML{URL: p.BaseURL, Priority: 1, Weight: 1}
	} else {
		m.SuggestedPresentationDelay = fmt.Sprintf("PT%dS", 3*p.SegmentDurationS)
	}
	if p.DVRWindowS > 0 {
		m.TimeShiftBufferDepth = fmt.Sprintf("PT%dS", p.DVRWindowS)
	}

	var videoReps, audioReps []representationXML
	for _, v := range p.Variants {
		baseURL := fmt.Sprintf("%s/%s/%s/", prefix, v.App, v.Stream)

		if v.HasVideo {
			bandwidth := v.Bandwidth
			if bandwidth <= 0 {
				bandwidth = estimatedVideoBandwidth
			}
			videoReps = append(videoReps, representationXML{
				ID:        v.Stream,
				Codecs:    v.VideoCodecs,
				Bandwidth: bandwidth,
				Width:     v.VideoWidth,
				Height:    v.VideoHeight,
				BaseURL:   baseURL,
			})
		}

		if v.HasAudio {
			audioReps = append(audioReps, representationXML{
				ID:                v.Stream + "-audio",
				Codecs:            v.AudioCodecs,
				Bandwidth:         estimatedAudioBandwidth,
				AudioSamplingRate: v.AudioSampleRate,
				AudioChannelConf: &audioChannelConfigurationXML{
					SchemeIDURI: "urn:mpeg:dash:23003:3:audio_channel_configuration:2011",
					Value:       fmt.Sprintf("%d", v.AudioChannels),
				},
				BaseURL: baseURL,
			})
		}
	}

	if len(videoReps) > 0 {
		m.Period.AdaptationSets = append(m.Period.AdaptationSets, adaptationSetXML{
			MimeType:         "video/mp4",
			SegmentAlignment: true,
			SegmentTemplate: segmentTemplateXML{
				Timescale:      1000,
				Duration:       segDurMs,
				StartNumber:    1,
				Initialization: "init-video.mp4",
				Media:          "seg-video-$Number$.m4s",
			},
			Representations: videoReps,
		})
	}

	if len(audioReps) > 0 {
		m.Period.AdaptationSets = append(m.Period.AdaptationSets, adaptationSetXML{
			MimeType:         "audio/mp4",
			SegmentAlignment: true,
			SegmentTemplate: segmentTemplateXML{
				Timescale:      1000,
				Duration:       segDurMs,
				StartNumber:    1,
				Initialization: "init-audio.mp4",
				Media:          "seg-audio-$Number$.m4s",
			},
			Representations: audioReps,
		})
	}

	body, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal group mpd: %w", err)
	}
	out := append([]byte(xml.Header), body...)
	return out, nil
}

// WriteMPD renders and atomically publishes manifest.mpd into outDir, using
// the same write-tmp-then-rename pattern as internal/hls/playlist.go's
// write() — readers never see a partial file.
func WriteMPD(outDir string, p ManifestParams) error {
	data, err := RenderMPD(p)
	if err != nil {
		return err
	}

	tmpPath := filepath.Join(outDir, ".manifest.mpd.tmp")
	finalPath := filepath.Join(outDir, "manifest.mpd")

	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write mpd tmp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename mpd: %w", err)
	}
	return nil
}
