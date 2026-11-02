// Package webrtc implements WHIP (WebRTC-HTTP Ingestion Protocol) and WHEP
// (WebRTC-HTTP Egress Protocol) — sub-second-latency publishing and playback
// of live streams via WebRTC, sharing the same media.Registry/Bus pipeline
// every other ingest/output path uses.
package webrtc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	pionwebrtc "github.com/pion/webrtc/v4"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// ErrStreamNotLive is returned by CreateSession when the requested app/stream
// has no live source registered.
var ErrStreamNotLive = errors.New("webrtc: stream not live")

// ErrUnsupportedCodec is returned by CreateSession when the live stream's
// video codec isn't H.264 — the only codec this stage's RTP packetization
// supports.
var ErrUnsupportedCodec = errors.New("webrtc: only H.264 video sources are supported")

// ErrStreamAlreadyLive is returned by CreatePublishSession when the
// requested app/stream already has a live source registered — WHIP ingest
// doesn't preempt an existing publisher (RTMP's Registry.Register has the
// same reject-not-replace behavior for non-failover sources).
var ErrStreamAlreadyLive = errors.New("webrtc: stream already publishing")

// rtcSession is the common contract Manager.sessions holds — WHEP's Session
// (a viewer) and WHIP's PublishSession (a publisher) close very differently
// (a viewer just tears down its PeerConnection; a publisher also unregisters
// the source Stream) but DeleteSession doesn't need to know which.
type rtcSession interface {
	close()
}

// Manager tracks active WHIP/WHEP sessions, keyed by resource ID, and
// creates new ones from an SDP offer.
type Manager struct {
	cfg      *config.Config
	registry *media.Registry
	metrics  *metrics.Metrics

	// OnPublish fires once a WHIP-ingested stream is registered (before any
	// RTP has necessarily arrived) — wired by cmd/server/main.go to
	// startStreamOutputs, exactly like rtmp.Server.OnPublish/pull.Manager.OnLive.
	OnPublish func(*media.Stream)

	mu       sync.Mutex
	sessions map[string]rtcSession
}

// NewManager creates a WHIP/WHEP session manager.
func NewManager(cfg *config.Config, registry *media.Registry, m *metrics.Metrics) *Manager {
	return &Manager{
		cfg:      cfg,
		registry: registry,
		metrics:  m,
		sessions: make(map[string]rtcSession),
	}
}

// CreateSession looks up the live stream for app/streamName, negotiates a
// PeerConnection against offerSDP (non-trickle ICE — waits for full candidate
// gathering before returning), and starts forwarding the stream's video
// packets to the browser as RTP. Returns the SDP answer and a resource ID for
// the later DELETE.
func (mgr *Manager) CreateSession(ctx context.Context, app, streamName, offerSDP string) (answerSDP, resourceID string, err error) {
	stream := mgr.registry.Get(media.StreamKey{App: app, Name: streamName})
	if stream == nil {
		return "", "", ErrStreamNotLive
	}

	ci := stream.Bus.CodecInfo()
	if ci == nil || ci.VideoCodec != media.CodecH264 {
		return "", "", ErrUnsupportedCodec
	}

	pc, err := newPeerConnection(mgr.cfg)
	if err != nil {
		return "", "", fmt.Errorf("create peer connection: %w", err)
	}

	videoTrack, err := pionwebrtc.NewTrackLocalStaticSample(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeH264, ClockRate: 90000},
		"video", streamName)
	if err != nil {
		pc.Close()
		return "", "", fmt.Errorf("create video track: %w", err)
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		return "", "", fmt.Errorf("add video track: %w", err)
	}

	// Forward-compat hook: no ingest path produces Opus yet (WHIP isn't
	// implemented), so this branch is currently unreachable — but once WHIP
	// ships an Opus-sourced stream, WHEP starts including audio with no
	// further changes.
	var audioTrack *pionwebrtc.TrackLocalStaticSample
	if ci.AudioCodec == media.CodecOpus {
		audioTrack, err = pionwebrtc.NewTrackLocalStaticSample(
			pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			"audio", streamName)
		if err != nil {
			pc.Close()
			return "", "", fmt.Errorf("create audio track: %w", err)
		}
		if _, err := pc.AddTrack(audioTrack); err != nil {
			pc.Close()
			return "", "", fmt.Errorf("add audio track: %w", err)
		}
	}

	if err := pc.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		pc.Close()
		return "", "", fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return "", "", fmt.Errorf("create answer: %w", err)
	}

	gatherComplete := pionwebrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return "", "", fmt.Errorf("set local description: %w", err)
	}
	<-gatherComplete // non-trickle ICE: wait for all candidates before responding

	resourceID = uuid.NewString()
	sessionCtx, cancel := context.WithCancel(context.Background())
	sess := &Session{
		id:         resourceID,
		pc:         pc,
		stream:     stream,
		videoTrack: videoTrack,
		audioTrack: audioTrack,
		sps:        ci.SPSNALu,
		pps:        ci.PPSNALu,
		cancel:     cancel,
		metrics:    mgr.metrics,
	}

	mgr.mu.Lock()
	mgr.sessions[resourceID] = sess
	mgr.mu.Unlock()
	if mgr.metrics != nil {
		mgr.metrics.WHEPSessions.Inc()
	}

	pc.OnConnectionStateChange(func(s pionwebrtc.PeerConnectionState) {
		switch s {
		case pionwebrtc.PeerConnectionStateFailed, pionwebrtc.PeerConnectionStateClosed, pionwebrtc.PeerConnectionStateDisconnected:
			mgr.DeleteSession(resourceID)
		}
	})

	go sess.run(sessionCtx, mgr)

	local := pc.LocalDescription()
	return local.SDP, resourceID, nil
}

// DeleteSession tears down and removes the session (WHEP viewer or WHIP
// publisher) with the given resource ID. Safe to call more than once (e.g.
// once from an explicit DELETE and again from OnConnectionStateChange) —
// later calls are no-ops since the map lookup only succeeds once, and each
// session's own close() is additionally guarded by a sync.Once.
func (mgr *Manager) DeleteSession(resourceID string) error {
	mgr.mu.Lock()
	sess, ok := mgr.sessions[resourceID]
	if ok {
		delete(mgr.sessions, resourceID)
	}
	mgr.mu.Unlock()
	if !ok {
		return nil
	}

	sess.close()
	return nil
}

// CreatePublishSession negotiates a PeerConnection against offerSDP for a
// WHIP publisher, registers a new media.Stream once negotiation succeeds,
// and starts depacketizing incoming RTP into media.Packets on that stream's
// Bus. Returns the SDP answer and a resource ID for the later DELETE.
func (mgr *Manager) CreatePublishSession(ctx context.Context, app, streamName, offerSDP string) (answerSDP, resourceID string, err error) {
	key := media.StreamKey{App: app, Name: streamName}
	if mgr.registry.Get(key) != nil {
		return "", "", ErrStreamAlreadyLive
	}

	pc, err := newPeerConnection(mgr.cfg)
	if err != nil {
		return "", "", fmt.Errorf("create peer connection: %w", err)
	}

	_, cancel := context.WithCancel(context.Background())
	sess := &PublishSession{
		stream: &media.Stream{
			Key:        key,
			SourceType: "whip",
			StartedAt:  time.Now(),
		},
		pc:       pc,
		registry: mgr.registry,
		metrics:  mgr.metrics,
		mgr:      mgr,
		cancel:   cancel,
	}

	pc.OnTrack(func(track *pionwebrtc.TrackRemote, _ *pionwebrtc.RTPReceiver) {
		go sess.handleIncomingTrack(track)
	})

	if err := pc.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		cancel()
		pc.Close()
		return "", "", fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		cancel()
		pc.Close()
		return "", "", fmt.Errorf("create answer: %w", err)
	}

	gatherComplete := pionwebrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		cancel()
		pc.Close()
		return "", "", fmt.Errorf("set local description: %w", err)
	}
	<-gatherComplete // non-trickle ICE: wait for all candidates before responding

	// Register only once negotiation has actually succeeded, so a failed
	// offer never leaves a phantom stream in the registry.
	if err := mgr.registry.Register(sess.stream); err != nil {
		cancel()
		pc.Close()
		return "", "", fmt.Errorf("register stream: %w", err)
	}
	// StopFunc converges every teardown path (explicit WHIP DELETE, ICE
	// disconnect, or an operator's kick-stream API call — all of which end
	// up at Registry.Unregister, either directly or via sess.close()) onto
	// the same PeerConnection close. This must NOT be sess.close itself:
	// close() calls Unregister, and Unregister calls StopFunc — assigning
	// close() here would recurse back into Unregister from inside the
	// Unregister call that invoked it.
	sess.stream.StopFunc = sess.closePeerConnection

	resourceID = uuid.NewString()
	sess.id = resourceID

	mgr.mu.Lock()
	mgr.sessions[resourceID] = sess
	mgr.mu.Unlock()
	if mgr.metrics != nil {
		mgr.metrics.WHIPSessions.Inc()
	}

	pc.OnConnectionStateChange(func(s pionwebrtc.PeerConnectionState) {
		switch s {
		case pionwebrtc.PeerConnectionStateFailed, pionwebrtc.PeerConnectionStateClosed, pionwebrtc.PeerConnectionStateDisconnected:
			mgr.DeleteSession(resourceID)
		}
	})

	if mgr.OnPublish != nil {
		go mgr.OnPublish(sess.stream)
	}

	local := pc.LocalDescription()
	return local.SDP, resourceID, nil
}

// newPeerConnection builds a PeerConnection using cfg.WebRTC's ICE server
// list and, if configured, a restricted ephemeral UDP port range.
func newPeerConnection(cfg *config.Config) (*pionwebrtc.PeerConnection, error) {
	var iceServers []pionwebrtc.ICEServer
	if len(cfg.WebRTC.ICEServers) > 0 {
		iceServers = append(iceServers, pionwebrtc.ICEServer{URLs: cfg.WebRTC.ICEServers})
	}
	rtcConfig := pionwebrtc.Configuration{ICEServers: iceServers}

	if cfg.WebRTC.ICEUDPPortMin == 0 && cfg.WebRTC.ICEUDPPortMax == 0 {
		return pionwebrtc.NewPeerConnection(rtcConfig)
	}

	var se pionwebrtc.SettingEngine
	if err := se.SetEphemeralUDPPortRange(cfg.WebRTC.ICEUDPPortMin, cfg.WebRTC.ICEUDPPortMax); err != nil {
		return nil, fmt.Errorf("set ice udp port range: %w", err)
	}
	api := pionwebrtc.NewAPI(pionwebrtc.WithSettingEngine(se))
	return api.NewPeerConnection(rtcConfig)
}
