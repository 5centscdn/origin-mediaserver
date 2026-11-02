// Package ebuttd renders EBU-TT-D documents (EBU Tech 3380) — the DVB-DASH
// profile's standard subtitle format, a simplified TTML profile designed
// for streaming. Cue text comes from internal/captiondecode's CEA-608/708
// decode; this package only renders already-decoded text + timing as XML,
// no caption parsing of its own.
package ebuttd

import (
	"encoding/xml"
	"fmt"

	"github.com/5centscdn/origin-mediaserver/internal/captiondecode"
)

// ─── XML model ──────────────────────────────────────────────────────────────
// Matches internal/dash/mpd.go's convention of encoding/xml structs over raw
// string building.

type ttXML struct {
	XMLName  xml.Name `xml:"tt"`
	Xmlns    string   `xml:"xmlns,attr"`
	XmlnsTTP string   `xml:"xmlns:ttp,attr"`
	XmlnsTTS string   `xml:"xmlns:tts,attr"`
	XmlnsEBU string   `xml:"xmlns:ebuttm,attr"`
	XmlnsEBS string   `xml:"xmlns:ebutts,attr"`
	XmlLang  string   `xml:"xml:lang,attr"`
	TimeBase string   `xml:"ttp:timeBase,attr"`
	Head     headXML  `xml:"head"`
	Body     bodyXML  `xml:"body"`
}

type headXML struct {
	Metadata metadataXML `xml:"metadata"`
	Layout   layoutXML   `xml:"layout"`
}

// layoutXML/regionXML define the single region every <p> references (see
// Render's doc comment on regionID for why this is required, not
// decorative). A bottom-anchored, centered safe-area box — the conventional
// subtitle placement.
type layoutXML struct {
	Region regionXML `xml:"region"`
}

type regionXML struct {
	ID           string `xml:"xml:id,attr"`
	Origin       string `xml:"tts:origin,attr"`
	Extent       string `xml:"tts:extent,attr"`
	DisplayAlign string `xml:"tts:displayAlign,attr"`
	TextAlign    string `xml:"tts:textAlign,attr"`
}

type metadataXML struct {
	DocumentMetadata documentMetadataXML `xml:"ebuttm:documentMetadata"`
}

type documentMetadataXML struct {
	ConformsTo string `xml:"ebuttm:conformsToStandard"`
}

type bodyXML struct {
	Div divXML `xml:"div"`
}

type divXML struct {
	Paragraphs []paragraphXML `xml:"p"`
}

type paragraphXML struct {
	Begin  string `xml:"begin,attr"`
	End    string `xml:"end,attr"`
	Region string `xml:"region,attr"`
	Text   string `xml:",chardata"`
}

// regionID is the single region every rendered <p> references — required,
// not decorative: dash.js's TTML renderer (via imscJS's generateISD) only
// ever surfaces a cue whose computed ISD has non-empty region contents.
// Without any <region> defined and referenced, imscJS never assigns our
// text to a region, so every cue silently produces an empty ISD and never
// renders — no error, no warning, just nothing, regardless of correct
// timing. Confirmed live: dash.js fetched and buffered every text fragment
// correctly, yet zero cues ever appeared, with or without an app-attached
// TTML rendering div.
const regionID = "region0"

// Render produces one EBU-TT-D document containing every cue that overlaps
// [segStartPTS, segStartPTS+segDurationTicks) (90kHz ticks — the same unit
// as media.Packet.PTS/DTS), with begin/end times expressed relative to the
// segment's own start (ttp:timeBase="media" — the segment's own first
// sample is 00:00:00.000, per EBU-TT-D/DASH convention). Cues are clipped to
// the segment window, not merely filtered by full containment, so a caption
// spanning a segment boundary still appears (correctly truncated) in both
// segments it overlaps.
//
// Always produces a valid, well-formed document — including when cues is
// empty — since stpp readers expect one parseable XML sample per segment,
// never a missing or empty-body one.
func Render(cues []captiondecode.Cue, segStartPTS int64, segDurationTicks int64) []byte {
	segEndPTS := segStartPTS + segDurationTicks

	doc := ttXML{
		Xmlns:    "http://www.w3.org/ns/ttml",
		XmlnsTTP: "http://www.w3.org/ns/ttml#parameter",
		XmlnsTTS: "http://www.w3.org/ns/ttml#styling",
		XmlnsEBU: "urn:ebu:tt:metadata",
		XmlnsEBS: "urn:ebu:tt:style",
		XmlLang:  "en",
		TimeBase: "media",
		Head: headXML{
			Metadata: metadataXML{DocumentMetadata: documentMetadataXML{
				ConformsTo: "urn:ebu:tt:distribution:2018-05",
			}},
			Layout: layoutXML{Region: regionXML{
				ID:           regionID,
				Origin:       "10% 80%",
				Extent:       "80% 15%",
				DisplayAlign: "after",
				TextAlign:    "center",
			}},
		},
	}

	for _, c := range cues {
		start := c.StartPTS
		end := c.EndPTS
		if end <= segStartPTS || start >= segEndPTS {
			continue // no overlap with this segment at all
		}
		if start < segStartPTS {
			start = segStartPTS
		}
		if end > segEndPTS {
			end = segEndPTS
		}
		doc.Body.Div.Paragraphs = append(doc.Body.Div.Paragraphs, paragraphXML{
			Begin:  ttmlTimestamp(start - segStartPTS),
			End:    ttmlTimestamp(end - segStartPTS),
			Region: regionID,
			Text:   c.Text,
		})
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		// doc is built entirely from this function's own well-formed
		// literals plus plain cue text (marshaled as escaped chardata) —
		// encoding/xml cannot fail on that shape.
		panic(fmt.Sprintf("ebuttd: unexpected marshal error: %v", err))
	}
	return append([]byte(xml.Header), body...)
}

// ttmlTimestamp formats a 90kHz-tick offset as TTML's clock-time syntax
// (HH:MM:SS.mmm), matching internal/dash's own tick convention.
func ttmlTimestamp(ticks int64) string {
	if ticks < 0 {
		ticks = 0
	}
	ms := (ticks * 1000) / 90000
	s := ms / 1000
	ms %= 1000
	m := s / 60
	s %= 60
	h := m / 60
	m %= 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
