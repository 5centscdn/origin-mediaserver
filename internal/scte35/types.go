// Package scte35 parses and tracks SCTE-35 splice_info_section messages,
// independent of any particular transport (MPEG-TS/SRT today; UDP, HTTP,
// RIST or RTMP-carried MPEG-TS later can all feed the same Parse/
// StateMachine without change) or output format (MPEG-TS pass-through and
// HLS ad markers both consume the same Event/Action types).
package scte35

import "time"

// CommandType identifies the splice_command_type of a parsed SCTE-35
// message, using named constants rather than the raw wire byte so callers
// don't need to know the SCTE-35 spec's numeric values.
type CommandType uint8

const (
	CommandUnknown CommandType = iota
	CommandSpliceNull
	CommandSpliceSchedule
	CommandSpliceInsert
	CommandTimeSignal
	CommandBandwidthReservation
	CommandPrivate
)

func (c CommandType) String() string {
	switch c {
	case CommandSpliceNull:
		return "splice_null"
	case CommandSpliceSchedule:
		return "splice_schedule"
	case CommandSpliceInsert:
		return "splice_insert"
	case CommandTimeSignal:
		return "time_signal"
	case CommandBandwidthReservation:
		return "bandwidth_reservation"
	case CommandPrivate:
		return "private_command"
	default:
		return "unknown"
	}
}

// Event is the protocol-independent, parsed representation of one SCTE-35
// splice_info_section. RawSection is always populated verbatim — even when
// only a subset of the fields below could be parsed — so MPEG-TS
// pass-through never needs to rebuild a section from parsed fields; it just
// forwards RawSection unmodified (see mux.TSMuxer.WriteSCTE35).
type Event struct {
	// RawSection is the complete, original splice_info_section bytes
	// (CRC32 included), exactly as received. Never modified or rebuilt.
	RawSection []byte

	// PID is the MPEG-TS PID the section was carried on.
	PID uint16

	CommandType    CommandType
	RawCommandType uint8 // the wire splice_command_type value, preserved even when CommandType is CommandUnknown

	// PTS is the splice point's presentation time in 90kHz ticks, from the
	// command's splice_time()/pts_time when time_specified_flag was set.
	// Valid only if HasPTS.
	PTS    int64
	HasPTS bool

	// PTSAdjustment is splice_info_section's pts_adjustment field (90kHz
	// ticks), used by re-distributors to correct pts_time for a
	// retransmission delay.
	PTSAdjustment int64

	// splice_insert() fields.
	EventID      uint32
	HasEventID   bool
	OutOfNetwork bool

	// break_duration(), when duration_flag was set on a splice_insert.
	BreakDurationMS  int64
	HasBreakDuration bool
	AutoReturn       bool

	// segmentation_descriptor() fields, when one was present in the
	// descriptor loop (commonly paired with a time_signal command).
	SegmentationEventID     uint32
	HasSegmentation         bool
	SegmentationTypeID      uint8
	SegmentationDurationMS  int64
	HasSegmentationDuration bool
	UPID                    []byte
	UPIDType                uint8

	// RawDescriptors preserves every splice_descriptor() found in the
	// descriptor loop verbatim (tag + length + identifier + data),
	// including ones not specifically modeled above (e.g. avail_descriptor,
	// DTMF_descriptor, or any descriptor tag this parser doesn't recognize)
	// — nothing in the descriptor loop is ever dropped.
	RawDescriptors [][]byte

	// ReceivedAt is when this section was parsed, used as the ad-break
	// clock anchor when the command carries no explicit PTS.
	ReceivedAt time.Time

	// Source distinguishes an event demuxed from the incoming stream from
	// one the server generated itself (see generator.go), so a per-stream
	// scte35_mode of "insert" or "passthrough" can filter one or the other
	// out before it ever reaches MPEG-TS pass-through or HLS marker
	// generation — see internal/hls/admarkers.go.
	Source Source
}

// Source identifies where an Event came from.
type Source uint8

const (
	// SourceIngest is set by internal/ingest/mpegts on every section it
	// demuxes from the incoming transport.
	SourceIngest Source = iota
	// SourceInserted is set by generator.go's Build* functions on every
	// locally synthesized event.
	SourceInserted
)

func (s Source) String() string {
	if s == SourceInserted {
		return "inserted"
	}
	return "ingest"
}

// SourceAllowed reports whether an Event with the given Source should be
// processed under a stream's resolved scte35_mode
// ("disabled"|"passthrough"|"insert"|"hybrid"|""). Shared by
// internal/hls.Packager and internal/dvr's recorder so both MPEG-TS/HLS
// consumers gate incoming-vs-inserted events identically.
func SourceAllowed(mode string, source Source) bool {
	switch mode {
	case "passthrough":
		return source == SourceIngest
	case "insert":
		return source == SourceInserted
	case "hybrid":
		return true
	default: // "disabled", "", or anything unrecognized
		return false
	}
}
