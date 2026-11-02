package dvr

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type hlsKind int

const (
	hlsLive  hlsKind = iota // sliding window, no ENDLIST
	hlsEvent                // fixed start, still growing
	hlsVOD                  // complete
)

// renderHLS builds a media playlist over segs (ascending msn). Each
// session gets its own EXT-X-MAP and starts with EXT-X-DISCONTINUITY, and
// EXT-X-DISCONTINUITY-SEQUENCE comes from the stored per-stream counter so
// it stays correct as old segments slide out of a live playlist.
func renderHLS(segs []segmentRow, kind hlsKind, prefix string, segDurS int) string {
	target := segDurS
	for _, s := range segs {
		if d := int(math.Ceil(float64(s.DurationMS) / 1000)); d > target {
			target = d
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	switch kind {
	case hlsVOD:
		b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	case hlsEvent:
		b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", segs[0].MSN)
	if kind == hlsLive {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", segs[0].DiscSeq)
	}
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for i, s := range segs {
		dir := fmt.Sprintf("%sdvr/%d/", prefix, s.SessionMS)
		if i == 0 || s.SessionMS != segs[i-1].SessionMS {
			if i > 0 {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%sinit.mp4\"\n", dir)
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				time.UnixMilli(s.StartMS).UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s%d.m4s\n", float64(s.DurationMS)/1000, dir, s.MSN)
	}
	if kind == hlsVOD {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}

type mpdOptions struct {
	Prefix   string
	Now      time.Time
	SegDurS  int
	Dynamic  bool  // live window / timeshift; false = static range
	ShiftS   int64 // timeshift delay (dynamic only)
	DepthS   int64 // timeShiftBufferDepth (dynamic only)
	Sessions map[int64]sessionRow
}

// mpdPeriod is a run of segments that share one session and have
// consecutive msn — what a single SegmentTemplate with $Number$ can
// address.
type mpdPeriod struct {
	sess sessionRow
	segs []segmentRow
}

func (p mpdPeriod) startTicks() int64 {
	if p.sess.HasVideo {
		return p.segs[0].VideoTfdt
	}
	return p.segs[0].AudioTfdt
}

// renderMPD builds a DASH MPD over segs. Video and audio are separate
// AdaptationSets addressing the two halves of the same recorded files
// (…_v.m4s / …_a.m4s, see ResolveFile).
//
// Every timestamp is on the recording timeline (90 kHz since dvrEpoch), so
// a dynamic MPD can simply use dvrEpoch as availabilityStartTime and each
// Period starts at its first tfdt — the player's live edge is then "now"
// with no extra bookkeeping. A timeshift moves availabilityStartTime
// forward by the delay, which moves the live edge back by the same amount.
func renderMPD(segs []segmentRow, o mpdOptions) string {
	var periods []mpdPeriod
	for i, s := range segs {
		if i == 0 || s.SessionMS != segs[i-1].SessionMS || s.MSN != segs[i-1].MSN+1 {
			periods = append(periods, mpdPeriod{sess: o.Sessions[s.SessionMS]})
		}
		p := &periods[len(periods)-1]
		p.segs = append(p.segs, s)
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011"`)
	if o.Dynamic {
		ast := time.UnixMilli(dvrEpochMS + o.ShiftS*1000).UTC().Format(time.RFC3339)
		if o.ShiftS == 0 {
			ast = dvrEpochXSD
		}
		fmt.Fprintf(&b, ` type="dynamic" availabilityStartTime="%s" publishTime="%s"`,
			ast, o.Now.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, ` minimumUpdatePeriod="PT%dS" timeShiftBufferDepth="PT%dS" suggestedPresentationDelay="PT%dS"`,
			o.SegDurS, o.DepthS, 3*o.SegDurS)
	} else {
		first := periods[0].startTicks()
		last := segs[len(segs)-1]
		end := max(last.VideoTfdt+last.VideoDur, last.AudioTfdt+last.AudioDur)
		fmt.Fprintf(&b, ` type="static" mediaPresentationDuration="%s"`, xsDuration(end-first))
	}
	b.WriteString(` minBufferTime="PT2S">` + "\n")

	var origin int64 // Period@start is relative to this
	if !o.Dynamic {
		origin = periods[0].startTicks()
	}
	for _, p := range periods {
		pto := p.startTicks()
		dir := fmt.Sprintf("%sdvr/%d/", o.Prefix, p.sess.SessionMS)
		fmt.Fprintf(&b, "  <Period id=\"%d-%d\" start=\"%s\">\n", p.sess.SessionMS, p.segs[0].MSN, xsDuration(pto-origin))

		if p.sess.HasVideo {
			var bytes, dur int64
			for _, s := range p.segs {
				bytes, dur = bytes+s.VideoBytes, dur+s.VideoDur
			}
			b.WriteString(`    <AdaptationSet id="1" contentType="video" mimeType="video/mp4" segmentAlignment="true" startWithSAP="1">` + "\n")
			fmt.Fprintf(&b, `      <Representation id="v" codecs="%s" bandwidth="%d"`, p.sess.VideoCodecs, bandwidth(bytes, dur))
			if p.sess.Width > 0 && p.sess.Height > 0 {
				fmt.Fprintf(&b, ` width="%d" height="%d"`, p.sess.Width, p.sess.Height)
			}
			b.WriteString(">\n")
			writeTemplate(&b, dir, "v", pto, p.segs, func(s segmentRow) (int64, int64) { return s.VideoTfdt, s.VideoDur })
			b.WriteString("      </Representation>\n    </AdaptationSet>\n")
		}
		if p.sess.HasAudio {
			var bytes, dur int64
			for _, s := range p.segs {
				bytes, dur = bytes+s.Size-s.VideoBytes, dur+s.AudioDur
			}
			b.WriteString(`    <AdaptationSet id="2" contentType="audio" mimeType="audio/mp4" lang="und" segmentAlignment="true">` + "\n")
			fmt.Fprintf(&b, `      <Representation id="a" codecs="%s" bandwidth="%d" audioSamplingRate="%d">`+"\n",
				p.sess.AudioCodecs, bandwidth(bytes, dur), p.sess.SampleRate)
			if p.sess.Channels > 0 {
				fmt.Fprintf(&b, `        <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="%d"/>`+"\n", p.sess.Channels)
			}
			writeTemplate(&b, dir, "a", pto, p.segs, func(s segmentRow) (int64, int64) { return s.AudioTfdt, s.AudioDur })
			b.WriteString("      </Representation>\n    </AdaptationSet>\n")
		}
		b.WriteString("  </Period>\n")
	}
	if o.Dynamic {
		b.WriteString(`  <UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-iso:2014" value="/dash-utc-time"/>` + "\n")
	}
	b.WriteString("</MPD>\n")
	return b.String()
}

func writeTemplate(b *strings.Builder, dir, track string, pto int64, segs []segmentRow, timing func(segmentRow) (t, d int64)) {
	fmt.Fprintf(b, `        <SegmentTemplate timescale="90000" presentationTimeOffset="%d" startNumber="%d" initialization="%sinit_%s.mp4" media="%s$Number$_%s.m4s">`+"\n",
		pto, segs[0].MSN, dir, track, dir, track)
	b.WriteString("          <SegmentTimeline>\n")
	var next int64 = -1
	for _, s := range segs {
		t, d := timing(s)
		if d <= 0 {
			d = s.DurationMS * 90 // track missing from this segment; keep numbering aligned
		}
		if t == next {
			fmt.Fprintf(b, "            <S d=\"%d\"/>\n", d)
		} else {
			fmt.Fprintf(b, "            <S t=\"%d\" d=\"%d\"/>\n", t, d)
		}
		next = t + d
	}
	b.WriteString("          </SegmentTimeline>\n        </SegmentTemplate>\n")
}

// bandwidth estimates a Representation's bits/s from bytes over 90 kHz dur.
func bandwidth(bytes, dur int64) int64 {
	if dur <= 0 {
		return 128000
	}
	return bytes * 8 * 90000 / dur
}

// xsDuration formats 90 kHz ticks as an xs:duration in seconds.
func xsDuration(ticks int64) string {
	return fmt.Sprintf("PT%.3fS", float64(ticks)/90000)
}
