package dash

import (
	"encoding/base64"
	"encoding/xml"
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/drm"
)

// MPD namespaces for DRM signaling.
const (
	NSCENC = "urn:mpeg:cenc:2013"
	NSMSPR = "urn:microsoft:playready"
)

// ContentProtection is one <ContentProtection> element. Inner is raw XML
// (cenc:pssh, mspr:pro) taken verbatim from DRMtoday's CPIX.
type ContentProtection struct {
	XMLName     xml.Name `xml:"ContentProtection"`
	SchemeIDURI string   `xml:"schemeIdUri,attr"`
	Value       string   `xml:"value,attr,omitempty"`
	DefaultKID  string   `xml:"cenc:default_KID,attr,omitempty"`
	Inner       string   `xml:",innerxml"`
}

// ProtectionFor builds the ContentProtection elements for a track
// encrypted with kid: the generic mp4protection element ("cbcs" +
// cenc:default_KID, which players use to match the init segment's tenc),
// then one per DRM system that has DASH signaling. A system with a PSSH
// box but no ContentProtectionData gets a <cenc:pssh> built from the box.
// FairPlay has neither and is HLS-only.
func ProtectionFor(ks *drm.KeySet, kid [16]byte) []ContentProtection {
	if ks == nil {
		return nil
	}
	k := drm.ContentKey{KID: kid}
	out := []ContentProtection{{
		SchemeIDURI: "urn:mpeg:dash:mp4protection:2011",
		Value:       "cbcs",
		DefaultKID:  k.KIDString(),
	}}
	for _, s := range ks.SystemsFor(kid) {
		inner, value := unwrapContentProtection(s.ContentProtection)
		if inner == "" && len(s.PSSH) > 0 {
			inner = "<cenc:pssh>" + base64.StdEncoding.EncodeToString(s.PSSH) + "</cenc:pssh>"
		}
		if inner == "" {
			continue
		}
		if value == "" {
			switch s.SystemID {
			case drm.SystemWidevine:
				value = "Widevine"
			case drm.SystemPlayReady:
				value = "MSPR 2.0"
			}
		}
		out = append(out, ContentProtection{
			SchemeIDURI: "urn:uuid:" + s.SystemID,
			Value:       value,
			Inner:       inner,
		})
	}
	return out
}

// unwrapContentProtection accepts CPIX ContentProtectionData either as the
// element's children (DASH-IF CPIX: "added under the ContentProtection
// element") or as a whole <ContentProtection> element, which some key
// servers send; the latter is unwrapped so the element isn't nested.
func unwrapContentProtection(data string) (inner, value string) {
	var cp struct {
		XMLName xml.Name
		Value   string `xml:"value,attr"`
		Inner   string `xml:",innerxml"`
	}
	if data != "" && xml.Unmarshal([]byte(data), &cp) == nil && cp.XMLName.Local == "ContentProtection" {
		return strings.TrimSpace(cp.Inner), cp.Value
	}
	return data, ""
}
