package rtmp

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/metrics"
)

// Server accepts RTMP (and optionally RTMPS) connections and spawns a
// goroutine per connection.  All connections share the ABRSessionManager
// and StreamRegistry.
type Server struct {
	cfg      *config.Config
	db       *db.DB
	abrMgr   *ABRSessionManager
	registry *media.Registry
	metrics  *metrics.Metrics
	authKey  adobeAuthKey // signs Adobe-auth salts/opaques; see adobeauth.go

	// OnPublish is called in a new goroutine each time a stream starts publishing.
	// Wire this up in main.go to start HLS/DASH packagers and DVR recording.
	OnPublish func(stream *media.Stream)

	// OnUnpublish is called when a stream stops.
	OnUnpublish func(stream *media.Stream)

	// ingest counts publishing connections per application, for
	// [rtmp] max_connections_per_app (see acquireIngest).
	ingestMu sync.Mutex
	ingest   map[string]int
}

// acquireIngest takes one of app's publishing-connection slots, or reports
// false when max_connections_per_app (<= 0 = no limit) are already in use.
// A slot is per connection, not per stream: FMLE publishes all of its %i
// renditions on one connection and counts once. Pulls, failover backups
// and players never publish here, so they don't count.
func (s *Server) acquireIngest(app string) bool {
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	if s.ingest == nil {
		s.ingest = make(map[string]int)
	}
	if max := s.cfg.RTMP.MaxConnsPerApp; max > 0 && s.ingest[app] >= max {
		return false
	}
	s.ingest[app]++
	return true
}

// releaseIngest gives back a slot taken by acquireIngest.
func (s *Server) releaseIngest(app string) {
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()
	if s.ingest[app] <= 1 {
		delete(s.ingest, app)
	} else {
		s.ingest[app]--
	}
}

// NewServer creates an RTMP server.
func NewServer(cfg *config.Config, registry *media.Registry, database *db.DB, m *metrics.Metrics) *Server {
	s := &Server{
		cfg:      cfg,
		db:       database,
		registry: registry,
		metrics:  m,
		authKey:  newAdobeAuthKey(),
	}

	s.abrMgr = NewABRSessionManager(func(app, group string, variants []*abrVariant) {
		slog.Info("ABR group updated", "app", app, "group", group, "variants", len(variants))
	})

	return s
}

// ListenAndServe starts the RTMP listener.  It blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.RTMP.Listen)
	if err != nil {
		return fmt.Errorf("rtmp listen %s: %w", s.cfg.RTMP.Listen, err)
	}
	defer l.Close()

	slog.Info("RTMP listening", "addr", s.cfg.RTMP.Listen)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		nc, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			slog.Warn("rtmp accept", "err", err)
			continue
		}

		go s.handleConn(nc)
	}
}

// ListenAndServeTLS starts the RTMPS listener.
func (s *Server) ListenAndServeTLS(ctx context.Context, tlsCfg *tls.Config) error {
	l, err := tls.Listen("tcp", s.cfg.RTMP.TLSListen, tlsCfg)
	if err != nil {
		return fmt.Errorf("rtmps listen %s: %w", s.cfg.RTMP.TLSListen, err)
	}
	defer l.Close()

	slog.Info("RTMPS listening", "addr", s.cfg.RTMP.TLSListen)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		nc, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handleConn(nc)
	}
}

func (s *Server) handleConn(nc net.Conn) {
	if s.metrics != nil {
		s.metrics.RTMPConnections.Inc()
		defer s.metrics.RTMPConnections.Dec()
	}
	c := newConn(nc, s.cfg, s.db, s.abrMgr, s.registry, s)
	c.serve()
}
