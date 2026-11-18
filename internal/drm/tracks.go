package drm

import (
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// Encryption returns the muxer settings that encrypt a track with k: the
// key, constant IV, the PSSH boxes of every DRM system that carries k, and
// for video the slice-header parser cbcs needs. Each track needs its own
// value (the parser keeps that track's SPS/PPS).
func (ks *KeySet) Encryption(k ContentKey, video bool) *mux.TrackEncryption {
	e := &mux.TrackEncryption{KID: k.KID, Key: k.Key, IV: k.IV, PSSH: ks.PSSHFor(k.KID)}
	if video {
		e.Params = media.NewH264ParamSets()
	}
	return e
}

// StreamKeys are the keys one stream's outputs use. Video is picked by
// resolution (CPIX usage rules); Audio is the audio key when audio has its
// own track/playlist (DASH, split-audio HLS, nDVR's per-track files), and
// Muxed is the key for a combined audio+video HLS segment, which uses the
// video key for both tracks so one #EXT-X-KEY covers the whole segment.
type StreamKeys struct {
	Set   *KeySet
	Video ContentKey
	Audio ContentKey
}

// KeysFor picks a stream's keys for its resolution.
func (ks *KeySet) KeysFor(width, height int) StreamKeys {
	return StreamKeys{Set: ks, Video: ks.VideoKey(width, height), Audio: ks.AudioKey()}
}

// KIDs lists the distinct KIDs in use.
func (sk StreamKeys) KIDs() [][16]byte {
	if sk.Video.KID == sk.Audio.KID {
		return [][16]byte{sk.Video.KID}
	}
	return [][16]byte{sk.Video.KID, sk.Audio.KID}
}
