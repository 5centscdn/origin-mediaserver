// Package captiondecode extracts plain caption TEXT and TIMING from
// CEA-608/CEA-708 closed-caption data already embedded in H.264 SEI NALs —
// the first real decode of caption bytes anywhere in this codebase.
// internal/media's HasCEA608708/UsesCEA708DTVCCService (used for HLS's
// #EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS signaling) only ever classify whether
// caption data is present, never decode the actual characters.
//
// Deliberately scoped to text + timing only — no window positioning, pen
// color/style/font, or multi-window overlap. A full CEA-708 decoder (8
// windows, styling, multiple simultaneous services) is broadcast-decoder-
// scale complexity; this package extracts what a viewer would read, not how
// a broadcast receiver would render it. Only CEA-708 Service 1 (the
// near-universal default caption service) is decoded — services 2-63 are
// ignored.
package captiondecode

// Cue is one caption event: text visible from StartPTS until EndPTS, in the
// same 90kHz-tick unit as media.Packet.PTS/DTS (deliberately not importing
// internal/media here — this package operates on raw SEI/cc_data bytes, not
// media.Packet, keeping it usable from any caller that has NAL bytes and a
// timestamp).
type Cue struct {
	StartPTS int64
	EndPTS   int64
	Text     string
}
