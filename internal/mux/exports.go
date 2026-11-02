package mux

// Exported stream type constants for use by hls and dvr packages.
const (
	StreamTypeH264 = streamTypeH264
	StreamTypeH265 = streamTypeH265
	StreamTypeAV1  = streamTypeAV1
	StreamTypeAAC  = streamTypeAAC
	StreamTypeMP3  = streamTypeMP3
	StreamTypeAC3  = streamTypeAC3
	StreamTypeOpus = streamTypeOpus
)

// CRC32MPEG2 is the CRC-32/MPEG-2 checksum (poly 0x04C11DB7, init
// 0xFFFFFFFF, no final XOR) used by every MPEG-2 PSI/private section,
// including PAT/PMT (see writePAT/writePMT) and SCTE-35's
// splice_info_section. Exported so internal/scte35 doesn't need its own
// copy of the table.
var CRC32MPEG2 = crc32MPEG2
