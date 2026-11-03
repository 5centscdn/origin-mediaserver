package rtmp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/config"
	"github.com/5centscdn/origin-mediaserver/internal/db"
	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// ─── Per-RTMP-message-stream state ────────────────────────────────────────────
//
// RTMP allows multiple logical streams (identified by a Message Stream ID) to
// exist within one TCP connection.  FMLE uses exactly this mechanism for
// multi-bitrate publishing: it calls createStream() once per bitrate variant,
// getting back a unique MSID for each, then calls publish() on every MSID.
//
// Most RTMP server implementations only track a single "current" stream per
// connection and ignore additional createStream/publish pairs — which is why
// FMLE multi-bitrate support is rare.  We track every MSID in a map.

type msgStreamState struct {
	msid      uint32
	name      string       // set when publish command arrives
	published bool
	stream    *media.Stream
	abrMatch  *ABRMatch    // non-nil if this variant is part of an ABR group
}

// ─── Connection ───────────────────────────────────────────────────────────────

// conn handles one RTMP TCP connection from handshake through streaming.
type conn struct {
	nc        net.Conn
	bufReader *bufio.Reader
	demux     *ChunkDemuxer

	// Acknowledgements (see maybeAck). in counts every byte read after the
	// handshake; ackWindow is the peer's Window Acknowledgement Size.
	in        *countingReader
	ackWindow uint32
	lastAck   uint64

	// Configuration
	cfg      *config.Config
	db       *db.DB
	app      *config.RTMPApp // non-nil when app came from config (legacy path)
	dbApp    *db.AppConfig   // non-nil when app came from the DB (preferred path)
	abrMgr   *ABRSessionManager
	registry *media.Registry
	srv      *Server // back-pointer for OnPublish/OnUnpublish callbacks

	// RTMP negotiation state
	appName       string
	sendChunkSize uint32

	// Per-message-stream states (MSID → state).
	// KEY DESIGN: this map is what makes FMLE %i work.
	// One TCP connection → many MSIDs → many parallel publish streams.
	msgStreams map[uint32]*msgStreamState
	nextMSID  uint32

	log *slog.Logger
}

func newConn(
	nc net.Conn,
	cfg *config.Config,
	database *db.DB,
	abrMgr *ABRSessionManager,
	registry *media.Registry,
	srv *Server,
) *conn {
	in := &countingReader{r: nc}
	br := bufio.NewReaderSize(in, 4096)
	return &conn{
		nc:            nc,
		bufReader:     br,
		in:            in,
		ackWindow:     defaultAckWindow,
		demux:         newChunkDemuxer(br),
		cfg:           cfg,
		db:            database,
		abrMgr:        abrMgr,
		registry:      registry,
		srv:           srv,
		sendChunkSize: 4096,
		msgStreams:    make(map[uint32]*msgStreamState),
		nextMSID:      1,
		log:           slog.With("remote", nc.RemoteAddr()),
	}
}

// serve is the main loop for one RTMP connection.
// It runs in its own goroutine.
func (c *conn) serve() {
	defer c.cleanup()

	// ── Step 1: RTMP handshake ───────────────────────────────────────────────
	if err := doHandshake(c.nc); err != nil {
		c.log.Debug("handshake failed", "err", err)
		return
	}

	// Tell the client to use larger chunks for efficiency.
	if err := c.sendSetChunkSize(c.sendChunkSize); err != nil {
		return
	}

	// ── Step 2: Message loop ─────────────────────────────────────────────────
	idleDeadline := time.Duration(c.cfg.RTMP.IdleTimeoutMs) * time.Millisecond

	for {
		// The idle timeout guards the handshake/connect phase and live
		// publishing, but not a connected encoder that hasn't started (or
		// has stopped) publishing: FMLE's "Connect" button opens the session
		// and sends nothing until "Start", and FMS/Wowza keep it open
		// indefinitely. Dead peers are still caught by TCP keepalive, which
		// net.Listen enables by default.
		if idleDeadline > 0 && (c.appName == "" || c.isPublishing()) {
			_ = c.nc.SetReadDeadline(time.Now().Add(idleDeadline))
		} else {
			_ = c.nc.SetReadDeadline(time.Time{})
		}

		msg, err := c.demux.ReadMessage()
		if err != nil {
			if !isClosedErr(err) {
				c.log.Info("connection closed", "err", err)
			}
			return
		}
		if err := c.maybeAck(); err != nil {
			return
		}

		if err := c.handleMessage(msg); err != nil {
			var ar authRejected
			if errors.As(err, &ar) && !ar.failed {
				c.log.Info("connection closed", "reason", err)
			} else {
				c.log.Warn("connection rejected", "err", err)
			}
			return
		}
	}
}

// pushCredentials returns the application's push login and password
// (panel app first, then config.toml), or an empty login when the app
// doesn't require one.
func (c *conn) pushCredentials() (login, password string) {
	if c.dbApp != nil && c.dbApp.PushLogin != "" {
		return c.dbApp.PushLogin, c.dbApp.PushPassword
	}
	if c.app != nil {
		return c.app.PushLogin, c.app.PushPassword
	}
	return "", ""
}

// authRejected ends a connection after an Adobe-auth _error. Steps 1 and 2
// of the exchange are rejections by design (the client reconnects with
// more), so only a failed response is worth a warning.
type authRejected struct {
	failed bool
	user   string
}

func (e authRejected) Error() string {
	if e.failed {
		return fmt.Sprintf("push login failed for user %q", e.user)
	}
	return "push login required, challenge sent"
}

// isPublishing reports whether any message stream on this connection is
// currently publishing.
func (c *conn) isPublishing() bool {
	for _, ms := range c.msgStreams {
		if ms.published {
			return true
		}
	}
	return false
}

// handleMessage dispatches a fully reassembled RTMP message.
func (c *conn) handleMessage(msg *Message) error {
	switch msg.TypeID {

	// ── RTMP control messages ────────────────────────────────────────────────
	case msgSetChunkSize:
		if len(msg.Data) < 4 {
			return nil
		}
		size := binary.BigEndian.Uint32(msg.Data) & 0x7FFFFFFF
		c.demux.SetChunkSize(size)

	case msgWindowAckSize:
		if len(msg.Data) >= 4 {
			if w := binary.BigEndian.Uint32(msg.Data); w > 0 {
				c.ackWindow = w
			}
		}

	case msgSetPeerBandwidth, msgAck:
		// Nothing to act on: we never throttle our own (small) sends.

	case msgUserControl:
		// Only PingRequest needs an answer; see pingResponse.
		if resp := pingResponse(msg.Data); resp != nil {
			return c.sendControlMessage(msgUserControl, resp)
		}

	// ── AMF0 commands ────────────────────────────────────────────────────────
	case msgCommandAMF0, msgDataAMF0:
		return c.handleAMF0Command(msg)

	// ── AMF3 commands (some encoders use AMF3) ───────────────────────────────
	case msgCommandAMF3:
		// AMF3 command: skip the first byte (0x00) then decode as AMF0.
		if len(msg.Data) < 1 {
			return nil
		}
		msg.Data = msg.Data[1:]
		return c.handleAMF0Command(msg)

	// ── Audio data ───────────────────────────────────────────────────────────
	case msgAudio:
		return c.handleAudioData(msg)

	// ── Video data ───────────────────────────────────────────────────────────
	case msgVideo:
		return c.handleVideoData(msg)
	}

	return nil
}

// ─── AMF0 command dispatch ───────────────────────────────────────────────────

func (c *conn) handleAMF0Command(msg *Message) error {
	vals, err := DecodeAMF0All(msg.Data)
	if err != nil || len(vals) == 0 {
		return nil
	}

	cmdName, ok := vals[0].(AMF0String)
	if !ok {
		return nil
	}

	transactionID := 0.0
	if len(vals) > 1 {
		if n, ok := vals[1].(AMF0Number); ok {
			transactionID = float64(n)
		}
	}

	switch string(cmdName) {
	case "connect":
		return c.handleConnect(transactionID, vals)

	case "createStream":
		return c.handleCreateStream(transactionID)

	case "publish":
		return c.handlePublish(msg.MessageStreamID, vals)

	case "deleteStream":
		return c.handleDeleteStream(vals)

	case "closeStream":
		return c.handleCloseStream(msg.MessageStreamID)

	case "releaseStream", "FCPublish", "FCUnpublish", "ping", "FCSubscribe", "_checkbw":
		// These are encoder-side bookkeeping commands; respond with _result.
		return c.sendResult(transactionID, AMF0Null{}, AMF0Null{})

	case "getStreamLength":
		// A duration, not null: 0 means "live". Zixi waits for this number
		// before going further and reconnects in a loop without it.
		return c.sendResult(transactionID, AMF0Null{}, AMF0Number(0))

	case "play":
		// Playback is via HLS/DASH/WHEP; we don't support RTMP play.
		return c.sendError(transactionID, "NetStream.Play.Failed",
			"Use HLS or DASH for playback")

	case "onFCPublish", "onStatus", "_result", "_error":
		// Responses to our own commands — ignore.
	}

	return nil
}

// handleConnect processes the NetConnection.connect() command.
func (c *conn) handleConnect(txID float64, vals []AMF0Value) error {
	// vals: ["connect", txID, {app: "live", ...}]
	if len(vals) < 3 {
		return nil
	}

	info, _ := vals[2].(AMF0Object)
	rawApp, _ := info["app"].(AMF0String)
	tcURL, _ := info["tcUrl"].(AMF0String)
	// Authentication parameters ride in the app name's query
	// ("live?authmod=adobe&user=…"); they are not part of the app.
	appName := stripQuery(string(rawApp))

	if appName == "" {
		return fmt.Errorf("connect: missing app name")
	}

	// DB apps take precedence for publish-key/allow-any-stream checks below.
	appFound := false
	if c.db != nil {
		if dbApp, err := c.db.GetApplicationByName(context.Background(), appName); err == nil && dbApp != nil {
			if !dbApp.Enabled {
				return fmt.Errorf("connect: app %q is disabled", appName)
			}
			c.dbApp = dbApp
			appFound = true
		}
	}
	// Config-based ABR %i patterns are config-only (never persisted to the
	// DB — see MatchABRPatterns/ABRPattern) and are resolved independently of
	// c.dbApp, so making an app DB-managed (required to toggle out_hls/
	// out_dash from the panel) doesn't silently disable its FMLE %i ABR
	// grouping just because a same-named config.toml entry also exists.
	if appCfg := c.cfg.RTMP.AppByName(appName); appCfg != nil {
		c.app = appCfg
		appFound = true
	}
	if !appFound {
		return fmt.Errorf("connect: unknown app %q", appName)
	}

	c.appName = appName
	c.log = c.log.With("app", appName)

	if login, password := c.pushCredentials(); login != "" {
		params := adobeAuthParams(string(rawApp))
		if params == nil {
			params = adobeAuthParams(string(tcURL))
		}
		if res := c.srv.authKey.checkAdobeAuth(params, login, password, time.Now()); !res.ok {
			if err := c.sendError(txID, "NetConnection.Connect.Rejected", res.description); err != nil {
				return err
			}
			if params["response"] != "" {
				// The exchange's own values (hashes, nonces), never the
				// password: enough to see why an encoder's response differs.
				c.log.Info("push login response", "app_param", string(rawApp), "tcUrl", string(tcURL))
			}
			return authRejected{failed: params["response"] != "", user: params["user"]}
		}
	}

	// Acknowledge window size and peer bandwidth first.
	if err := c.sendWindowAckSize(2500000); err != nil {
		return err
	}
	if err := c.sendSetPeerBandwidth(2500000, 2); err != nil {
		return err
	}
	// Set our chunk size on the sender side.
	if err := c.sendSetChunkSize(c.sendChunkSize); err != nil {
		return err
	}

	// Send _result for the connect command.
	props := AMF0Object{
		"fmsVer":       AMF0String("FMS/3,5,5,2004"),
		"capabilities": AMF0Number(31),
		"mode":         AMF0Number(1),
	}
	info2 := AMF0Object{
		"level":          AMF0String("status"),
		"code":           AMF0String("NetConnection.Connect.Success"),
		"description":    AMF0String("Connection succeeded"),
		"objectEncoding": AMF0Number(0),
	}

	c.log.Info("client connected")
	if err := c.sendResult(txID, props, info2); err != nil {
		return err
	}
	// FMS follows the connect _result with onBWDone, and Flash-era
	// encoders (FMLE) wait for it before reporting the connection as up.
	// Other clients ignore it.
	bw, err := EncodeAMF0(AMF0String("onBWDone"), AMF0Number(0), AMF0Null{})
	if err != nil {
		return err
	}
	return c.sendAMF0Command(0, bw)
}

// handleCreateStream allocates a new message stream ID and returns it to the client.
//
// This is called once per variant by FMLE.  A single RTMP TCP connection can
// have many MSIDs active simultaneously.
func (c *conn) handleCreateStream(txID float64) error {
	msid := c.nextMSID
	c.nextMSID++

	c.msgStreams[msid] = &msgStreamState{msid: msid}

	c.log.Debug("createStream", "msid", msid)
	return c.sendResult(txID, AMF0Null{}, AMF0Number(float64(msid)))
}

// handlePublish processes the publish() command on a specific MSID.
//
// This is the critical entry point for FMLE multi-bitrate.
// FMLE calls createStream → publish for EACH bitrate variant, all on the
// same TCP connection.  When "camera_1", "camera_2", "camera_3" all arrive
// here and match the "camera_%i" ABR pattern, they are grouped together.
func (c *conn) handlePublish(msid uint32, vals []AMF0Value) error {
	if c.appName == "" {
		return fmt.Errorf("publish before connect")
	}

	// vals: ["publish", txID, null, streamName, publishType]
	streamName := ""
	if len(vals) > 3 {
		if s, ok := vals[3].(AMF0String); ok {
			streamName = string(s)
		}
	}
	if streamName == "" {
		return fmt.Errorf("publish: empty stream name")
	}
	rawName := streamName

	// Determine effective publish key (DB app takes precedence over config).
	publishKey := ""
	if c.dbApp != nil {
		publishKey = c.dbApp.PublishKey
	} else if c.app != nil {
		publishKey = c.app.PublishKey
	}

	if publishKey != "" {
		if !validatePublishKey(streamName, publishKey) {
			_ = c.sendStatusMessage(msid, "error", "NetStream.Publish.BadName",
				"Invalid stream key")
			return fmt.Errorf("publish: bad key for stream %q", streamName)
		}
	}
	streamName = stripQuery(streamName)

	// For DB-managed apps: if allow_any_stream is false, the stream name must
	// match a configured stream_configs pattern.
	var dbStream *db.StreamConfig
	if c.dbApp != nil {
		dbStream, _ = c.db.GetStreamConfig(context.Background(), c.dbApp.ID, streamName)
		if dbStream == nil && !c.dbApp.AllowAnyStream {
			_ = c.sendStatusMessage(msid, "error", "NetStream.Publish.BadName",
				"Stream not configured for this application")
			return fmt.Errorf("publish: stream %q not allowed in app %q", streamName, c.appName)
		}
	}

	key := media.StreamKey{App: c.appName, Name: streamName}

	// Reject if already publishing with this exact name — unless the
	// existing registration is a failover backup pull. A real encoder
	// reconnecting always preempts a backup: this is what lets failover
	// switch back to primary automatically once it recovers.
	if existing := c.registry.Get(key); existing != nil {
		if existing.SourceType != "failover_backup" {
			_ = c.sendStatusMessage(msid, "error", "NetStream.Publish.BadName",
				"Stream already publishing")
			return fmt.Errorf("stream %s already active", key)
		}
		c.log.Info("primary reconnected, evicting failover backup", "stream", key)
		c.registry.Unregister(existing) // also calls existing.StopFunc, tearing down the backup pull + outputs
	}

	stream := &media.Stream{
		Key:        key,
		SourceType: "rtmp",
		SourceAddr: c.nc.RemoteAddr().String(),
		StartedAt:  time.Now(),
	}

	// ── ABR pattern matching ─────────────────────────────────────────────────
	// Config apps group by their [[rtmp.applications.abr_patterns]]; panel
	// apps group by the %i stream config the name matched (see
	// matchStreamConfigABR). This MUST happen before Register:
	// Registry.Register only buckets a stream into its ABR group map if
	// ABRGroup is already set at registration time.
	var match ABRMatch
	var isABR bool
	if c.app != nil {
		match, isABR = MatchABRPatterns(c.app, streamName)
	}
	if !isABR && dbStream != nil && dbStream.IsWildcard {
		match, isABR = matchStreamConfigABR(dbStream.NamePattern, streamName)
	}
	if !isABR && dbStream != nil && !dbStream.IsWildcard && c.dbApp != nil {
		// An exact config (e.g. test1 created on the Streams page) won the
		// settings lookup; the %i pattern still decides the ABR group.
		if pat, _ := c.db.GetABRPatternConfig(context.Background(), c.dbApp.ID, streamName); pat != nil {
			match, isABR = matchStreamConfigABR(pat.NamePattern, streamName)
		}
	}
	if isABR {
		if match.Bandwidth == 0 {
			match.Bandwidth = fmleTotalDatarate(rawName)
		}
		stream.ABRGroup = match.GroupName
		stream.ABRVariantIndex = match.VariantIndex
		stream.ABRLabel = match.Label
		stream.ABRBandwidthHint = match.Bandwidth
		stream.ABRHeightHint = match.Height
	}

	if err := c.registry.Register(stream); err != nil {
		return err
	}

	if isABR {
		c.abrMgr.Join(c.appName, match, stream)
	}

	ms := c.msgStreams[msid]
	if ms == nil {
		// Some encoders publish without calling createStream first — handle it.
		ms = &msgStreamState{msid: msid}
		c.msgStreams[msid] = ms
	}
	ms.name = streamName
	ms.published = true
	ms.stream = stream
	if isABR {
		ms.abrMatch = &match
	}

	if isABR {
		c.log.Info("ABR variant publishing",
			"stream", streamName,
			"group", match.GroupName,
			"variant_index", match.VariantIndex,
			"label", match.Label,
		)
	} else {
		c.log.Info("stream publishing", "stream", streamName)
	}

	// Notify the server so it can start HLS/DASH packagers and DVR.
	if c.srv.OnPublish != nil {
		go c.srv.OnPublish(stream)
	}

	return c.sendStatusMessage(msid, "status", "NetStream.Publish.Start",
		streamName+" is now published")
}

// handleDeleteStream handles deleteStream(msid) — encoder teardown per variant.
func (c *conn) handleDeleteStream(vals []AMF0Value) error {
	if len(vals) < 4 {
		return nil
	}
	n, ok := vals[3].(AMF0Number)
	if !ok {
		return nil
	}
	msid := uint32(n)
	c.unpublishMSID(msid)
	return nil
}

func (c *conn) handleCloseStream(msid uint32) error {
	c.unpublishMSID(msid)
	return nil
}

func (c *conn) unpublishMSID(msid uint32) {
	ms, ok := c.msgStreams[msid]
	if !ok || !ms.published {
		return
	}

	c.log.Info("stream unpublished", "stream", ms.name)

	if c.srv.OnUnpublish != nil && ms.stream != nil {
		go c.srv.OnUnpublish(ms.stream)
	}

	if ms.abrMatch != nil {
		c.abrMgr.Leave(c.appName, ms.abrMatch.GroupName, ms.name)
	}

	if ms.stream != nil {
		c.registry.Unregister(ms.stream)
	}
	delete(c.msgStreams, msid)
}

// ─── Audio / Video data ───────────────────────────────────────────────────────

// handleVideoData routes a video message to the appropriate stream's MediaBus.
// The message stream ID (msg.MessageStreamID) identifies which ABR variant
// this data belongs to.
func (c *conn) handleVideoData(msg *Message) error {
	ms := c.msgStreams[msg.MessageStreamID]
	if ms == nil || !ms.published || ms.stream == nil {
		return nil
	}

	if len(msg.Data) < 2 {
		return nil
	}

	ms.stream.Bus.AddBytesIn(int64(len(msg.Data)))
	if c.srv.metrics != nil {
		c.srv.metrics.RTMPBytesIn.WithLabelValues(c.appName, ms.name).Add(float64(len(msg.Data)))
	}

	pkt, newCI := decodeVideoTag(ms.name, ms.stream.Bus.CodecInfo(), msg)
	if newCI != nil {
		ms.stream.Bus.SetCodecInfo(newCI)
	}
	if pkt == nil {
		return nil
	}

	ms.stream.Bus.Publish(pkt)
	return nil
}

func (c *conn) handleAudioData(msg *Message) error {
	ms := c.msgStreams[msg.MessageStreamID]
	if ms == nil || !ms.published || ms.stream == nil {
		return nil
	}

	if len(msg.Data) < 2 {
		return nil
	}

	ms.stream.Bus.AddBytesIn(int64(len(msg.Data)))
	if c.srv.metrics != nil {
		c.srv.metrics.RTMPBytesIn.WithLabelValues(c.appName, ms.name).Add(float64(len(msg.Data)))
	}

	pkt, newCI := decodeAudioTag(ms.name, ms.stream.Bus.CodecInfo(), msg)
	if newCI != nil {
		ms.stream.Bus.SetCodecInfo(newCI)
	}
	if pkt == nil {
		return nil
	}

	ms.stream.Bus.Publish(pkt)
	return nil
}

// ─── Write helpers ────────────────────────────────────────────────────────────

func (c *conn) sendSetChunkSize(size uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, size&0x7FFFFFFF)
	return c.sendControlMessage(msgSetChunkSize, data)
}

// defaultAckWindow is the Window Acknowledgement Size used until the peer
// announces its own; the same value handleConnect announces to the peer.
const defaultAckWindow = 2500000

// maybeAck sends an Acknowledgement (sequence number = bytes received so
// far) each time another ackWindow bytes have arrived, as the RTMP spec
// requires of the receiving side. FMLE stops sending once a window's worth
// is unacknowledged, which showed up as every FMLE publish stalling after
// about 50 s and hitting the idle timeout. ffmpeg and OBS don't wait for
// acks, so they never exposed it.
func (c *conn) maybeAck() error {
	if c.in.n-c.lastAck < uint64(c.ackWindow) {
		return nil
	}
	c.lastAck = c.in.n
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, uint32(c.in.n)) // wraps, per spec
	return c.sendControlMessage(msgAck, data)
}

// countingReader counts the bytes read through it. Only the connection's
// own read loop uses it, so the count needs no locking.
type countingReader struct {
	r io.Reader
	n uint64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += uint64(n)
	return n, err
}

func (c *conn) sendWindowAckSize(size uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, size)
	return c.sendControlMessage(msgWindowAckSize, data)
}

func (c *conn) sendSetPeerBandwidth(size uint32, limitType byte) error {
	data := make([]byte, 5)
	binary.BigEndian.PutUint32(data, size)
	data[4] = limitType
	return c.sendControlMessage(msgSetPeerBandwidth, data)
}

func (c *conn) sendResult(txID float64, props, info AMF0Value) error {
	payload, err := EncodeAMF0(AMF0String("_result"), AMF0Number(txID), props, info)
	if err != nil {
		return err
	}
	return c.sendAMF0Command(0, payload)
}

func (c *conn) sendError(txID float64, code, description string) error {
	payload, err := EncodeAMF0(
		AMF0String("_error"),
		AMF0Number(txID),
		AMF0Null{},
		AMF0Object{
			"level":       AMF0String("error"),
			"code":        AMF0String(code),
			"description": AMF0String(description),
		},
	)
	if err != nil {
		return err
	}
	return c.sendAMF0Command(0, payload)
}

func (c *conn) sendStatusMessage(msid uint32, level, code, description string) error {
	payload, err := EncodeAMF0(
		AMF0String("onStatus"),
		AMF0Number(0),
		AMF0Null{},
		AMF0Object{
			"level":       AMF0String(level),
			"code":        AMF0String(code),
			"description": AMF0String(description),
		},
	)
	if err != nil {
		return err
	}
	return c.sendAMF0Command(msid, payload)
}

// sendControlMessage sends an RTMP control message on chunk stream 2, MSID 0.
func (c *conn) sendControlMessage(typeID uint8, data []byte) error {
	return c.writeChunk(2, 0, typeID, 0, data)
}

// sendAMF0Command sends an AMF0 command on chunk stream 3.
func (c *conn) sendAMF0Command(msid uint32, payload []byte) error {
	return c.writeChunk(3, 0, msgCommandAMF0, msid, payload)
}

// writeChunk writes a Type-0 chunk (full header) followed by payload chunks.
func (c *conn) writeChunk(csid, timestamp uint32, typeID uint8, msid uint32, data []byte) error {
	return writeChunk(c.nc, c.sendChunkSize, csid, timestamp, typeID, msid, data)
}

// writeChunk serializes one RTMP message as a Type-0 chunk (full header)
// followed by Type-3 continuation chunks for any data beyond chunkSize.
// Shared by the inbound connection's reply writer (conn.writeChunk) and the
// outbound puller, which sends connect/createStream/play the same way.
func writeChunk(w io.Writer, chunkSize, csid, timestamp uint32, typeID uint8, msid uint32, data []byte) error {
	// Basic header (csid 2–63 fits in 1 byte)
	var buf bytes.Buffer

	offset := 0
	first := true

	for offset < len(data) {
		end := offset + int(chunkSize)
		if end > len(data) {
			end = len(data)
		}
		payload := data[offset:end]

		if first {
			// Type 0 header: fmt=0, csid, + 11-byte message header
			fmtCSID := byte(csid & 0x3F) // fmt=0 (bits 7-6 = 00)
			buf.WriteByte(fmtCSID)

			// Timestamp (3 bytes)
			ts := timestamp
			if ts >= 0xFFFFFF {
				buf.Write([]byte{0xFF, 0xFF, 0xFF})
			} else {
				buf.Write([]byte{byte(ts >> 16), byte(ts >> 8), byte(ts)})
			}

			// Message length (3 bytes)
			l := len(data)
			buf.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)})

			// Type ID (1 byte)
			buf.WriteByte(typeID)

			// Message stream ID (4 bytes, little-endian)
			msidBytes := make([]byte, 4)
			binary.LittleEndian.PutUint32(msidBytes, msid)
			buf.Write(msidBytes)

			// Extended timestamp if needed
			if ts >= 0xFFFFFF {
				extTS := make([]byte, 4)
				binary.BigEndian.PutUint32(extTS, ts)
				buf.Write(extTS)
			}

			first = false
		} else {
			// Type 3 header: fmt=3, same csid — no additional header bytes
			buf.WriteByte(byte(0xC0 | (csid & 0x3F)))
		}

		buf.Write(payload)
		offset = end
	}

	_, err := w.Write(buf.Bytes())
	return err
}

// ─── Cleanup ─────────────────────────────────────────────────────────────────

func (c *conn) cleanup() {
	c.nc.Close()

	// Unpublish all streams on this connection.
	for msid, ms := range c.msgStreams {
		if ms.published {
			c.unpublishMSID(msid)
		}
	}

	c.log.Debug("connection closed")
}

// ─── Codec helpers ───────────────────────────────────────────────────────────

// parseAVCDecoderConfig extracts SPS/PPS from an AVCDecoderConfigurationRecord.
// This is used to fill CodecInfo for the HLS init segment and master playlist.
func parseAVCDecoderConfig(data []byte) *media.CodecInfo {
	// AVCDecoderConfigurationRecord layout:
	//   configurationVersion (1)
	//   AVCProfileIndication  (1)
	//   profile_compatibility (1)
	//   AVCLevelIndication    (1)
	//   lengthSizeMinusOne    (1) — bottom 2 bits
	//   numSPS                (1) — bottom 5 bits
	//   [SPS length (2) + SPS data] × numSPS
	//   numPPS                (1)
	//   [PPS length (2) + PPS data] × numPPS

	if len(data) < 7 {
		return nil
	}

	ci := &media.CodecInfo{
		VideoCodec:           media.CodecH264,
		Profile:              data[1],
		ProfileCompatibility: data[2],
		Level:                data[3],
	}

	pos := 5
	numSPS := int(data[pos] & 0x1F)
	pos++

	for i := 0; i < numSPS && pos+2 <= len(data); i++ {
		spsLen := int(data[pos])<<8 | int(data[pos+1])
		pos += 2
		if pos+spsLen > len(data) {
			break
		}
		spsBytes := make([]byte, spsLen)
		copy(spsBytes, data[pos:pos+spsLen])
		ci.SPSNALu = spsBytes
		ci.Width, ci.Height = media.ParseSPSResolution(spsBytes)
		pos += spsLen
	}

	if pos < len(data) {
		numPPS := int(data[pos])
		pos++
		for i := 0; i < numPPS && pos+2 <= len(data); i++ {
			ppsLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2
			if pos+ppsLen > len(data) {
				break
			}
			ci.PPSNALu = data[pos : pos+ppsLen]
			pos += ppsLen
		}
	}

	return ci
}

// parseHEVCDecoderConfig parses an HEVCDecoderConfigurationRecord (ISO/IEC
// 14496-15) — Enhanced RTMP's HEVC equivalent of AVCDecoderConfigurationRecord.
// Unlike AVC's fixed one-SPS/one-PPS layout, HEVC's record holds a variable
// number of "arrays," each tagged with the NAL unit type it contains (32=VPS,
// 33=SPS, 34=PPS, plus optional SEI arrays this codebase doesn't need) —
// arrays of other types are skipped.
func parseHEVCDecoderConfig(data []byte) *media.CodecInfo {
	// Fixed 23-byte header: configurationVersion(1) + general_profile_space/
	// tier/idc(1) + general_profile_compatibility_flags(4) +
	// general_constraint_indicator_flags(6) + general_level_idc(1) +
	// min_spatial_segmentation_idc(2) + parallelismType(1) +
	// chroma_format_idc(1) + bit_depth_luma_minus8(1) +
	// bit_depth_chroma_minus8(1) + avgFrameRate(2) + constantFrameRate/
	// numTemporalLayers/temporalIdNested/lengthSizeMinusOne(1) +
	// numOfArrays(1) = 23 bytes, before the array list.
	if len(data) < 23 {
		return nil
	}

	ci := &media.CodecInfo{VideoCodec: media.CodecH265}

	numArrays := int(data[22])
	pos := 23
	for a := 0; a < numArrays && pos+3 <= len(data); a++ {
		nalType := data[pos] & 0x3F
		numNalus := int(data[pos+1])<<8 | int(data[pos+2])
		pos += 3
		for n := 0; n < numNalus && pos+2 <= len(data); n++ {
			nalLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2
			if pos+nalLen > len(data) {
				break
			}
			nal := make([]byte, nalLen)
			copy(nal, data[pos:pos+nalLen])
			switch nalType {
			case 32: // VPS
				ci.VPSNALu = nal
			case 33: // SPS
				ci.SPSNALu = nal
				if w, h, ptl, ok := media.ParseHEVCSPS(nal); ok {
					ci.Width, ci.Height = w, h
					ci.HEVCProfileSpace = ptl.ProfileSpace
					ci.HEVCTierFlag = ptl.TierFlag
					ci.HEVCProfileIDC = ptl.ProfileIDC
					ci.HEVCProfileCompat = ptl.ProfileCompat
					ci.HEVCLevelIDC = ptl.LevelIDC
				}
			case 34: // PPS
				ci.PPSNALu = nal
			}
			pos += nalLen
		}
	}

	if ci.SPSNALu == nil {
		return nil // no SPS — nothing downstream can do anything useful with this
	}
	return ci
}

// convertAVCCToAnnexB converts AVCC-format NALUs (length-prefixed) to
// Annex-B format (start-code-prefixed).  RTMP carries H.264 in AVCC;
// MPEG-TS and HLS .ts segments need Annex-B.
func convertAVCCToAnnexB(data []byte) []byte {
	const startCode = "\x00\x00\x00\x01"

	var out []byte
	pos := 0
	for pos+4 <= len(data) {
		naluLen := int(binary.BigEndian.Uint32(data[pos:]))
		pos += 4
		if naluLen == 0 {
			continue
		}
		if pos+naluLen > len(data) {
			break
		}
		out = append(out, []byte(startCode)...)
		out = append(out, data[pos:pos+naluLen]...)
		pos += naluLen
	}
	return out
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

// validatePublishKey accepts the key as the whole stream name or as any
// ?/&-separated parameter after it ("cam?KEY"). FMLE appends its encoder
// settings to whatever stream name it is given
// ("cam?KEY&videoKeyframeFrequency=5&totalDatarate=700"), so the key is not
// necessarily last.
func validatePublishKey(streamName, key string) bool {
	if streamName == key {
		return true
	}
	_, query, ok := strings.Cut(streamName, "?")
	if !ok {
		return false
	}
	for _, p := range strings.FieldsFunc(query, func(r rune) bool { return r == '?' || r == '&' }) {
		if p == key {
			return true
		}
	}
	return false
}

// stripQuery drops everything from the first '?': the publish key and any
// encoder parameters (FMLE's videoKeyframeFrequency/totalDatarate) are not
// part of the stream name.
func stripQuery(name string) string {
	name, _, _ = strings.Cut(name, "?")
	return name
}

func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return s == "EOF" ||
		len(s) > 14 && s[len(s)-14:] == "use of closed network connection"
}
