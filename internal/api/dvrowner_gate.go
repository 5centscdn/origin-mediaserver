package api

import (
	"net/http"

	"github.com/5centscdn/origin-mediaserver/internal/dvr"
	"github.com/5centscdn/origin-mediaserver/internal/dvrowner"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// nDVR cluster ownership gates (internal/dvrowner). Each returns true when
// the handler should go on serving. With multinode off s.dvrOwner is nil
// and every gate is a nil check; for streams without nDVR it's one cached
// lookup.

// SetDVROwner enables nDVR cluster ownership checks (multinode only).
func (s *Server) SetDVROwner(t *dvrowner.Tracker) { s.dvrOwner = t }

// ownerGate guards an nDVR stream's live-edge playlists: a non-owner
// answers 404 so the CDN moves on to the next origin, and an undecidable
// owner answers 503. Neither may be cached.
func (s *Server) ownerGate(w http.ResponseWriter, r *http.Request, app, stream string) bool {
	if s.dvrOwner == nil {
		return true
	}
	return s.writeDecision(w, s.dvrOwner.Check(r.Context(), app, stream))
}

// groupGate is ownerGate for an ABR group's master: the variants'
// decisions combined, so master and variants resolve to the same node.
func (s *Server) groupGate(w http.ResponseWriter, r *http.Request, tracks []abrTrack) bool {
	if s.dvrOwner == nil {
		return true
	}
	ds := make([]dvrowner.Decision, len(tracks))
	for i, t := range tracks {
		ds[i] = s.dvrOwner.Check(r.Context(), t.app, t.stream)
	}
	return s.writeDecision(w, dvrowner.Combine(ds))
}

// segmentGate keeps live segments pinned to the owner: a known non-owner
// answers 404 (a segment of the same name here would be from a different
// feed). While ownership is undecidable a segment that exists is still
// served; the playlist gate already stops new players.
func (s *Server) segmentGate(w http.ResponseWriter, r *http.Request, app, stream string) bool {
	if s.dvrOwner == nil {
		return true
	}
	d := s.dvrOwner.Check(r.Context(), app, stream)
	if d.Verdict != dvrowner.NotOwner {
		return true
	}
	return s.writeDecision(w, d)
}

// dvrGate guards a rewind playlist. Window and timeshift playlists follow
// the live edge, so they're owned like live playlists. A time range is an
// archive request: any node whose own recording overlaps it serves it, so
// recordings on a node that lost ownership stay reachable.
func (s *Server) dvrGate(w http.ResponseWriter, r *http.Request, app, stream string, req dvr.Request) bool {
	if s.dvrOwner == nil {
		return true
	}
	if req.Kind != dvr.KindRange {
		return s.ownerGate(w, r, app, stream)
	}
	if !s.dvrOwner.Eligible(r.Context(), media.StreamKey{App: app, Name: stream}) ||
		s.dvrOwner.CoversRange(app, stream, req.FromMS, req.FromMS+req.LengthS*1000) {
		return true
	}
	return s.writeDecision(w, dvrowner.Decision{Verdict: dvrowner.NotOwner, Reason: "no recording of this range on this node"})
}

// dvrGroupGate is dvrGate for an ABR group's rewind master.
func (s *Server) dvrGroupGate(w http.ResponseWriter, r *http.Request, tracks []abrTrack, req dvr.Request) bool {
	if s.dvrOwner == nil {
		return true
	}
	if req.Kind != dvr.KindRange {
		return s.groupGate(w, r, tracks)
	}
	anyEligible := false
	for _, t := range tracks {
		if !s.dvrOwner.Eligible(r.Context(), media.StreamKey{App: t.app, Name: t.stream}) {
			continue
		}
		anyEligible = true
		if s.dvrOwner.CoversRange(t.app, t.stream, req.FromMS, req.FromMS+req.LengthS*1000) {
			return true
		}
	}
	if !anyEligible {
		return true
	}
	return s.writeDecision(w, dvrowner.Decision{Verdict: dvrowner.NotOwner, Reason: "no recording of this range on this node"})
}

// writeDecision answers a NotOwner/Unknown decision and returns false, or
// returns true for Bypass/Owner. The X-Origin-Owner header names the
// owning node (its configured name only, never its URL) to help debug CDN
// routing.
func (s *Server) writeDecision(w http.ResponseWriter, d dvrowner.Decision) bool {
	switch d.Verdict {
	case dvrowner.Bypass, dvrowner.Owner:
		return true
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-Origin-Ownership", d.Verdict.String())
	if d.Owner != "" {
		h.Set("X-Origin-Owner", d.Owner)
	}
	if d.Verdict == dvrowner.Unknown {
		h.Set("Retry-After", "2")
		http.Error(w, "stream ownership unknown: "+d.Reason, http.StatusServiceUnavailable)
		return false
	}
	http.Error(w, "not served by this origin: "+d.Reason, http.StatusNotFound)
	return false
}

// invalidateDVROwnership clears the tracker's nDVR eligibility cache after
// an application/stream config write.
func (s *Server) invalidateDVROwnership(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		s.dvrOwner.InvalidateEligibility()
	})
}
