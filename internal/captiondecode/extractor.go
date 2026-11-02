package captiondecode

import "github.com/5centscdn/origin-mediaserver/internal/media"

// Extractor ties raw H.264 SEI NAL parsing to the CEA-608 and CEA-708
// decoders, holding one instance of each for the life of a stream. Both run
// simultaneously rather than picking one upfront — whichever the source
// doesn't actually use simply never receives a matching triplet type and
// never produces cues, so no separate classification step (like
// media.UsesCEA708DTVCCService) is needed here.
type Extractor struct {
	d608 *CEA608Decoder
	d708 *CEA708Decoder
}

// NewExtractor creates an Extractor with fresh decoder state.
func NewExtractor() *Extractor {
	return &Extractor{
		d608: NewCEA608Decoder(),
		d708: NewCEA708Decoder(),
	}
}

// Feed processes one video frame's SEI NAL — type 6, 1-byte NAL header
// included, no Annex-B start code, emulation prevention bytes NOT yet
// removed (same contract as media.HasCEA608708; media.ExtractCCData handles
// both) — at presentation time pts (90kHz ticks, the same unit as
// media.Packet.PTS). Returns every Cue completed while processing this
// frame's caption data, across both decoders.
func (e *Extractor) Feed(seiNAL []byte, pts int64) []Cue {
	triplets, ok := media.ExtractCCData(seiNAL)
	if !ok {
		return nil
	}

	var cues []Cue
	for i := 0; i*3+3 <= len(triplets); i++ {
		b0 := triplets[i*3]
		ccValid := (b0>>2)&0x01 == 1
		ccType := b0 & 0x03
		b1 := triplets[i*3+1]
		b2 := triplets[i*3+2]

		switch ccType {
		case 0: // NTSC_CC_FIELD_1 — CEA-608, CC1
			if !ccValid {
				continue
			}
			if cue, ok := e.d608.Feed(b1, b2, pts); ok {
				cues = append(cues, cue)
			}
		case 2, 3: // DTVCC_PACKET_DATA / DTVCC_PACKET_START — CEA-708
			cues = append(cues, e.d708.Feed(ccValid, ccType, b1, b2, pts)...)
		}
		// ccType==1 (NTSC_CC_FIELD_2) — out of scope, discarded (see
		// CEA608Decoder's doc comment: CC1 only).
	}
	return cues
}

// Flush finalizes whatever cue is currently visible/accumulating in either
// decoder — call once at stream end so a final in-progress caption isn't
// silently dropped.
func (e *Extractor) Flush(pts int64) []Cue {
	var cues []Cue
	if cue, ok := e.d608.Flush(pts); ok {
		cues = append(cues, cue)
	}
	if cue, ok := e.d708.Flush(pts); ok {
		cues = append(cues, cue)
	}
	return cues
}
