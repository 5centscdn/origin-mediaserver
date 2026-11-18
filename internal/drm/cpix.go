package drm

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
)

// Well-known DRM system IDs (DASH-IF registry). CPIX names systems only by
// these UUIDs; the names are for logs, the panel's "test" result and the
// MPD's ContentProtection@value.
const (
	SystemWidevine  = "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
	SystemPlayReady = "9a04f079-9840-4286-ab92-e65be0885f95"
	SystemFairPlay  = "94ce86fb-07ff-4f43-adb8-93d2fa968ca2"
)

// SystemName returns a display name for a DRM system ID ("" if unknown).
func SystemName(systemID string) string {
	switch strings.ToLower(systemID) {
	case SystemWidevine:
		return "Widevine"
	case SystemPlayReady:
		return "PlayReady"
	case SystemFairPlay:
		return "FairPlay"
	}
	return ""
}

// ContentKey is one CPIX content key. IV is the cbcs constant IV
// (CPIX explicitIV); when the provider sends none, the KID is used, so
// every origin in a cluster derives the same IV and writes identical
// segments.
type ContentKey struct {
	KID [16]byte
	Key [16]byte
	IV  [16]byte
}

// KIDString formats the KID as a lowercase UUID (DASH cenc:default_KID).
func (k ContentKey) KIDString() string { return uuidString(k.KID) }

// System is one DRMSystem entry: the signaling a DRM system needs for one
// KID, taken verbatim from CPIX so the license URL, PSSH and FairPlay skd://
// URI are exactly what DRMtoday expects.
type System struct {
	KID      [16]byte
	SystemID string // lowercase UUID
	// PSSH is a complete 'pssh' box for the init segment (may be nil:
	// FairPlay has none).
	PSSH []byte
	// ContentProtection is the inner XML of the MPD's ContentProtection
	// element for this system (typically <cenc:pssh>…</cenc:pssh>, plus
	// <mspr:pro> for PlayReady). Empty means no DASH signaling.
	ContentProtection string
	// HLSMedia / HLSMaster are complete tag lines (#EXT-X-KEY /
	// #EXT-X-SESSION-KEY) for the media and master playlists.
	HLSMedia  []string
	HLSMaster []string
}

// usageRule maps a KID onto the tracks it encrypts (CPIX
// ContentKeyUsageRule). DRMtoday uses it to give audio and each video
// resolution tier its own key.
type usageRule struct {
	kid       [16]byte
	audio     bool
	video     bool
	minPixels int64 // 0 = unbounded
	maxPixels int64 // 0 = unbounded
}

// KeySet is a parsed CPIX document for one asset.
type KeySet struct {
	AssetID string
	Keys    []ContentKey
	Systems []System
	rules   []usageRule
}

// VideoKey returns the key for a video track of width×height (CPIX
// VideoFilter by pixel count). With no matching rule it falls back to a
// key whose rule is video at all, then to the first key, because an
// unknown resolution (no SPS yet) must still be encrypted.
func (ks *KeySet) VideoKey(width, height int) ContentKey {
	px := int64(width) * int64(height)
	var anyVideo *ContentKey
	for _, r := range ks.rules {
		if !r.video {
			continue
		}
		k := ks.key(r.kid)
		if k == nil {
			continue
		}
		if anyVideo == nil {
			anyVideo = k
		}
		if px > 0 && (r.minPixels == 0 || px >= r.minPixels) && (r.maxPixels == 0 || px <= r.maxPixels) {
			return *k
		}
	}
	if anyVideo != nil {
		return *anyVideo
	}
	return ks.Keys[0]
}

// AudioKey returns the key for audio tracks (CPIX AudioFilter), falling
// back to the first key.
func (ks *KeySet) AudioKey() ContentKey {
	for _, r := range ks.rules {
		if r.audio {
			if k := ks.key(r.kid); k != nil {
				return *k
			}
		}
	}
	return ks.Keys[0]
}

func (ks *KeySet) key(kid [16]byte) *ContentKey {
	for i := range ks.Keys {
		if ks.Keys[i].KID == kid {
			return &ks.Keys[i]
		}
	}
	return nil
}

// SystemsFor returns the DRM systems that carry kid, ordered by system ID
// so playlists and MPDs render identically on every node.
func (ks *KeySet) SystemsFor(kid [16]byte) []System {
	var out []System
	for _, s := range ks.Systems {
		if s.KID == kid {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SystemID < out[j].SystemID })
	return out
}

// PSSHFor returns the pssh boxes for kid's systems (init segment moov).
func (ks *KeySet) PSSHFor(kid [16]byte) [][]byte {
	var out [][]byte
	for _, s := range ks.SystemsFor(kid) {
		if len(s.PSSH) > 0 {
			out = append(out, s.PSSH)
		}
	}
	return out
}

// HLSMediaLines returns the #EXT-X-KEY lines for kid, de-duplicated.
func (ks *KeySet) HLSMediaLines(kid [16]byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ks.SystemsFor(kid) {
		for _, l := range s.HLSMedia {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// HLSMasterLines returns the #EXT-X-SESSION-KEY lines for kids (all keys
// in the set when none are given), de-duplicated.
func (ks *KeySet) HLSMasterLines(kids ...[16]byte) []string {
	var out []string
	seen := map[string]bool{}
	sys := append([]System(nil), ks.Systems...)
	sort.SliceStable(sys, func(i, j int) bool {
		if c := bytes.Compare(sys[i].KID[:], sys[j].KID[:]); c != 0 {
			return c < 0
		}
		return sys[i].SystemID < sys[j].SystemID
	})
	for _, s := range sys {
		if len(kids) > 0 && !containsKID(kids, s.KID) {
			continue
		}
		for _, l := range s.HLSMaster {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// ── XML ──────────────────────────────────────────────────────────────────
// encoding/xml matches on local names when a tag has no namespace, which
// keeps the parser indifferent to the cpix:/pskc: prefixes providers pick.

type cpixDoc struct {
	XMLName   xml.Name          `xml:"CPIX"`
	ContentID string            `xml:"contentId,attr"`
	Keys      []cpixContentKey  `xml:"ContentKeyList>ContentKey"`
	Systems   []cpixDRMSystem   `xml:"DRMSystemList>DRMSystem"`
	Rules     []cpixContentRule `xml:"ContentKeyUsageRuleList>ContentKeyUsageRule"`
}

type cpixContentKey struct {
	KID        string `xml:"kid,attr"`
	ExplicitIV string `xml:"explicitIV,attr"`
	Scheme     string `xml:"commonEncryptionScheme,attr"`
	PlainValue string `xml:"Data>Secret>PlainValue"`
}

type cpixDRMSystem struct {
	KID                   string          `xml:"kid,attr"`
	SystemID              string          `xml:"systemId,attr"`
	PSSH                  string          `xml:"PSSH"`
	ContentProtectionData string          `xml:"ContentProtectionData"`
	HLSSignaling          []cpixHLSSignal `xml:"HLSSignalingData"`
}

type cpixHLSSignal struct {
	Playlist string `xml:"playlist,attr"`
	Value    string `xml:",chardata"`
}

type cpixContentRule struct {
	KID               string          `xml:"kid,attr"`
	IntendedTrackType string          `xml:"intendedTrackType,attr"`
	Video             []cpixVideoFilt `xml:"VideoFilter"`
	Audio             []struct{}      `xml:"AudioFilter"`
}

type cpixVideoFilt struct {
	MinPixels int64 `xml:"minPixels,attr"`
	MaxPixels int64 `xml:"maxPixels,attr"`
}

// ParseCPIX parses a CPIX 2.x response. Keys must be in the clear
// (PlainValue): DRMtoday's ingest endpoint returns them that way over TLS,
// and encrypted (DocumentKey) CPIX isn't supported.
func ParseCPIX(data []byte) (*KeySet, error) {
	var doc cpixDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cpix: %w", err)
	}
	ks := &KeySet{AssetID: doc.ContentID}
	for _, k := range doc.Keys {
		kid, err := parseUUID(k.KID)
		if err != nil {
			return nil, fmt.Errorf("cpix: content key kid: %w", err)
		}
		if s := strings.ToLower(k.Scheme); s != "" && s != "cbcs" {
			return nil, fmt.Errorf("cpix: key %s uses scheme %q, want cbcs", k.KID, k.Scheme)
		}
		key, err := decode16(k.PlainValue)
		if err != nil {
			return nil, fmt.Errorf("cpix: key %s: %w (encrypted CPIX isn't supported)", k.KID, err)
		}
		ck := ContentKey{KID: kid, Key: key, IV: kid}
		if k.ExplicitIV != "" {
			if ck.IV, err = decode16(k.ExplicitIV); err != nil {
				return nil, fmt.Errorf("cpix: key %s explicitIV: %w", k.KID, err)
			}
		}
		ks.Keys = append(ks.Keys, ck)
	}
	if len(ks.Keys) == 0 {
		return nil, fmt.Errorf("cpix: no content keys")
	}
	for _, s := range doc.Systems {
		kid, err := parseUUID(s.KID)
		if err != nil {
			return nil, fmt.Errorf("cpix: drm system kid: %w", err)
		}
		sysID := strings.ToLower(strings.TrimSpace(s.SystemID))
		sys := System{KID: kid, SystemID: sysID}
		if v := strings.TrimSpace(s.PSSH); v != "" {
			if sys.PSSH, err = base64.StdEncoding.DecodeString(v); err != nil {
				return nil, fmt.Errorf("cpix: %s PSSH: %w", sysID, err)
			}
		}
		if v := strings.TrimSpace(s.ContentProtectionData); v != "" {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, fmt.Errorf("cpix: %s ContentProtectionData: %w", sysID, err)
			}
			sys.ContentProtection = strings.TrimSpace(string(b))
		}
		for _, h := range s.HLSSignaling {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h.Value))
			if err != nil {
				return nil, fmt.Errorf("cpix: %s HLSSignalingData: %w", sysID, err)
			}
			master := strings.EqualFold(h.Playlist, "master")
			lines := hlsLines(string(b), master)
			if master {
				sys.HLSMaster = append(sys.HLSMaster, lines...)
			} else {
				sys.HLSMedia = append(sys.HLSMedia, lines...)
			}
		}
		ks.Systems = append(ks.Systems, sys)
	}
	for _, r := range doc.Rules {
		kid, err := parseUUID(r.KID)
		if err != nil {
			return nil, fmt.Errorf("cpix: usage rule kid: %w", err)
		}
		t := strings.ToUpper(r.IntendedTrackType)
		rule := usageRule{kid: kid, audio: len(r.Audio) > 0 || strings.HasPrefix(t, "AUDIO")}
		if len(r.Video) > 0 {
			rule.video = true
			rule.minPixels, rule.maxPixels = r.Video[0].MinPixels, r.Video[0].MaxPixels
		} else if t == "ALL" || strings.HasPrefix(t, "SD") || strings.HasPrefix(t, "HD") || strings.HasPrefix(t, "UHD") || strings.HasPrefix(t, "VIDEO") {
			rule.video = true
		}
		if t == "ALL" {
			rule.audio = true
		}
		ks.rules = append(ks.rules, rule)
	}
	return ks, nil
}

// hlsLines splits decoded HLSSignalingData into tag lines. CPIX carries the
// complete tag; a bare attribute list (some providers) gets the tag that
// fits the playlist prepended.
func hlsLines(s string, master bool) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "#") {
			if master {
				l = "#EXT-X-SESSION-KEY:" + l
			} else {
				l = "#EXT-X-KEY:" + l
			}
		}
		out = append(out, l)
	}
	return out
}

func decode16(s string) ([16]byte, error) {
	var out [16]byte
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return out, err
	}
	if len(b) != 16 {
		return out, fmt.Errorf("want 16 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

func parseUUID(s string) ([16]byte, error) {
	var out [16]byte
	h := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		return out, fmt.Errorf("bad uuid %q", s)
	}
	copy(out[:], b)
	return out, nil
}

func uuidString(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func containsKID(kids [][16]byte, k [16]byte) bool {
	for _, x := range kids {
		if x == k {
			return true
		}
	}
	return false
}
