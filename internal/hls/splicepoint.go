package hls

import (
	"encoding/base64"

	"github.com/5centscdn/origin-mediaserver/internal/scte35"
)

// buildSplicepoint returns the base64-encoded payload for
// #EXT-X-SPLICEPOINT-SCTE35 — the raw, complete, unmodified
// splice_info_section, base64-encoded, per the real-world convention this
// tag uses in practice (verified against public examples; see the plan's
// research note — this is distinct from #EXT-X-DATERANGE's SCTE35-* hex
// encoding below). Preserves the original SCTE-35 payload exactly, so
// downstream systems can recover the full signaling instead of only a
// generic CUE-OUT/CUE-IN indication.
func buildSplicepoint(ev *scte35.Event) string {
	if ev == nil || len(ev.RawSection) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(ev.RawSection)
}
