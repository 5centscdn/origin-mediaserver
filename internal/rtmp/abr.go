package rtmp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// ABRMatch is the result of matching a stream name against a wildcard pattern.
type ABRMatch struct {
	GroupName    string
	VariantIndex int // the integer captured from %i
	Label        string
	Bandwidth    int
	Height       int
}

// matchABRPattern tests streamName against one ABRPattern.
//
// The %i token is replaced with a regex group that captures one or more digits.
// Examples:
//
//	pattern="camera_%i", streamName="camera_3"  → ABRMatch{VariantIndex:3}
//	pattern="live%i",    streamName="live2"      → ABRMatch{VariantIndex:2}
//	pattern="stream",    streamName="stream"     → no match (no %i)
func matchABRPattern(pat config.ABRPattern, streamName string) (ABRMatch, bool) {
	if !strings.Contains(pat.Pattern, "%i") {
		return ABRMatch{}, false
	}

	// Build a full-match regex from the pattern.
	regexStr := "^" + regexp.QuoteMeta(pat.Pattern) + "$"
	regexStr = strings.ReplaceAll(regexStr, "%i", `([0-9]+)`)

	re, err := regexp.Compile(regexStr)
	if err != nil {
		return ABRMatch{}, false
	}

	m := re.FindStringSubmatch(streamName)
	if m == nil {
		return ABRMatch{}, false
	}

	idx, err := strconv.Atoi(m[1])
	if err != nil {
		return ABRMatch{}, false
	}

	match := ABRMatch{
		GroupName:    pat.GroupName,
		VariantIndex: idx,
	}

	// Look up optional hint for this index.
	for _, hint := range pat.Variants {
		if hint.Index == idx {
			match.Label     = hint.Label
			match.Bandwidth = hint.Bandwidth
			match.Height    = hint.Height
			break
		}
	}

	if match.Label == "" {
		match.Label = fmt.Sprintf("variant_%d", idx)
	}

	return match, true
}

// ─── ABRSession ──────────────────────────────────────────────────────────────

// abrVariant is one live rendition within an ABR session.
type abrVariant struct {
	index      int
	streamName string
	label      string
	bandwidth  int
	height     int
	stream     *media.Stream
}

// abrSession groups multiple variant streams published on the same (or
// different) RTMP connections into a single ABR rendition set.
//
// FMLE opens ONE TCP connection and publishes camera_1, camera_2, camera_3
// on different RTMP message stream IDs within that single TCP connection.
// Our connection handler calls Join() for each one; the session accumulates
// them and notifies downstream when ≥2 variants are live.
type abrSession struct {
	mu        sync.Mutex
	groupName string
	appName   string
	variants  []*abrVariant

	// onUpdate is called each time a variant joins or leaves.
	// The HLS packager listens here to rebuild the master playlist.
	onUpdate func(variants []*abrVariant)
}

func newABRSession(groupName, appName string, onUpdate func([]*abrVariant)) *abrSession {
	return &abrSession{
		groupName: groupName,
		appName:   appName,
		onUpdate:  onUpdate,
	}
}

// Join registers a new variant.  Safe for concurrent use.
func (s *abrSession) Join(match ABRMatch, stream *media.Stream) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Deduplicate: if a variant with this index already exists, replace it.
	for i, v := range s.variants {
		if v.index == match.VariantIndex {
			s.variants[i].stream = stream
			s.notify()
			return
		}
	}

	s.variants = append(s.variants, &abrVariant{
		index:      match.VariantIndex,
		streamName: stream.Key.Name,
		label:      match.Label,
		bandwidth:  match.Bandwidth,
		height:     match.Height,
		stream:     stream,
	})

	// Sort by variant index (insertion sort is fine for small N).
	for i := len(s.variants) - 1; i > 0; i-- {
		if s.variants[i].index < s.variants[i-1].index {
			s.variants[i], s.variants[i-1] = s.variants[i-1], s.variants[i]
		}
	}

	s.notify()
}

// Leave removes the variant identified by stream name.
func (s *abrSession) Leave(streamName string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, v := range s.variants {
		if v.streamName == streamName {
			s.variants = append(s.variants[:i], s.variants[i+1:]...)
			s.notify()
			return
		}
	}
}

// Variants returns a snapshot of current live variants.
func (s *abrSession) Variants() []*abrVariant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*abrVariant, len(s.variants))
	copy(out, s.variants)
	return out
}

func (s *abrSession) notify() {
	if s.onUpdate != nil {
		cp := make([]*abrVariant, len(s.variants))
		copy(cp, s.variants)
		go s.onUpdate(cp) // async so we don't hold the mutex
	}
}

// ─── ABRSessionManager ───────────────────────────────────────────────────────

// ABRSessionManager maintains all active ABR sessions for all applications.
// It is shared across all RTMP connections on this node.
type ABRSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*abrSession // key: "appName/groupName"

	// Called when a session is created or updated.  The HLS packager attaches here.
	onSessionUpdate func(appName, groupName string, variants []*abrVariant)
}

// NewABRSessionManager creates a manager.
func NewABRSessionManager(onUpdate func(appName, groupName string, variants []*abrVariant)) *ABRSessionManager {
	return &ABRSessionManager{
		sessions:        make(map[string]*abrSession),
		onSessionUpdate: onUpdate,
	}
}

// MatchABRPatterns checks whether streamName matches any ABR pattern in the
// app config and returns the match metadata, with no side effects.
//
// Callers must copy the returned match's fields onto the media.Stream and
// call Registry.Register *before* calling ABRSessionManager.Join — Register
// only buckets a stream into the registry's ABR group map if ABRGroup is
// already set at registration time.
func MatchABRPatterns(app *config.RTMPApp, streamName string) (ABRMatch, bool) {
	for _, pat := range app.ABRPatterns {
		if match, ok := matchABRPattern(pat, streamName); ok {
			return match, true
		}
	}
	return ABRMatch{}, false
}

// Join adds an already-matched, already-registered stream to its live ABR
// session (creating the session on first join). Call after MatchABRPatterns
// + Registry.Register.
func (m *ABRSessionManager) Join(appName string, match ABRMatch, stream *media.Stream) {
	sessionKey := appName + "/" + match.GroupName

	m.mu.Lock()
	sess, exists := m.sessions[sessionKey]
	if !exists {
		groupName := match.GroupName
		onUpdate := func(variants []*abrVariant) {
			if m.onSessionUpdate != nil {
				m.onSessionUpdate(appName, groupName, variants)
			}
		}
		sess = newABRSession(match.GroupName, appName, onUpdate)
		m.sessions[sessionKey] = sess
	}
	m.mu.Unlock()

	sess.Join(match, stream)
}

// Leave removes a stream from its ABR session when it unpublishes.
func (m *ABRSessionManager) Leave(appName, groupName, streamName string) {
	sessionKey := appName + "/" + groupName
	m.mu.Lock()
	sess, ok := m.sessions[sessionKey]
	m.mu.Unlock()

	if ok {
		sess.Leave(streamName)
		// Clean up empty sessions.
		m.mu.Lock()
		if len(sess.Variants()) == 0 {
			delete(m.sessions, sessionKey)
		}
		m.mu.Unlock()
	}
}

// GetSession returns the ABR session for a group, or nil.
func (m *ABRSessionManager) GetSession(appName, groupName string) *abrSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[appName+"/"+groupName]
}

// matchStreamConfigABR groups a panel-app stream by the %i stream config it
// matched: "test%i" puts test1, test2, … in group "test", served at
// /{app}/test/playlist.m3u8 like a config abr_patterns group. Separators
// left next to the removed %i are trimmed ("camera_%i" → "camera"). A
// pattern without %i, or one that is only %i, is not a group.
func matchStreamConfigABR(pattern, streamName string) (ABRMatch, bool) {
	group := strings.Trim(strings.Replace(pattern, "%i", "", 1), "_-.")
	if group == "" {
		return ABRMatch{}, false
	}
	return matchABRPattern(config.ABRPattern{Pattern: pattern, GroupName: group}, streamName)
}

// fmleTotalDatarate returns the bitrate in bps that FMLE appends to each
// rendition's stream name ("test1?videoKeyframeFrequency=5&totalDatarate=700",
// in kbps), or 0. It is the only per-rendition bitrate an ABR master can
// advertise before any stats exist.
func fmleTotalDatarate(rawName string) int {
	_, query, ok := strings.Cut(rawName, "?")
	if !ok {
		return 0
	}
	for _, p := range strings.FieldsFunc(query, func(r rune) bool { return r == '?' || r == '&' }) {
		if v, ok := strings.CutPrefix(p, "totalDatarate="); ok {
			if kbps, err := strconv.Atoi(v); err == nil && kbps > 0 {
				return kbps * 1000
			}
		}
	}
	return 0
}
