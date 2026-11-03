package rtmp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// pullChunkSize is the outbound chunk size the puller announces right after
// the handshake, so the tiny connect/createStream/play command payloads
// never need multi-chunk splitting.
const pullChunkSize = 4096

// Pull dials rawURL as an RTMP client, connects, creates a stream, plays it,
// and publishes decoded packets into bus until ctx is cancelled or the
// connection ends. It blocks until the pull ends — run it in a goroutine.
//
// rawURL is a standard rtmp://host[:port]/app/streamName URL. streamName is
// stamped onto published packets (Packet.StreamID); callers may pass a
// different name than the URL's own stream name (failover keeps the
// primary's logical name regardless of what the backup calls it).
//
// Only plain rtmp:// is supported — no RTMPS, no other pull protocols.
func Pull(ctx context.Context, rawURL, streamName string, bus *media.Bus) error {
	appName, playName, addr, err := parsePullURL(rawURL)
	if err != nil {
		return err
	}

	log := slog.With("component", "rtmp-pull", "url", rawURL)

	dialer := net.Dialer{Timeout: 10 * time.Second}
	nc, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer nc.Close()

	// Unblock any in-flight read/write when ctx is cancelled — net.Conn has
	// no context-aware Read, so closing the connection is how we interrupt it.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			nc.Close()
		case <-stopWatch:
		}
	}()

	if err := clientHandshake(nc); err != nil {
		return fmt.Errorf("handshake with %s: %w", addr, err)
	}

	br := bufio.NewReaderSize(nc, 4096)
	demux := newChunkDemuxer(br)

	sizeBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeBytes, pullChunkSize)
	if err := writeChunk(nc, 128, 2, 0, msgSetChunkSize, 0, sizeBytes); err != nil {
		return fmt.Errorf("send Set Chunk Size: %w", err)
	}

	if err := sendConnect(nc, demux, addr, appName); err != nil {
		return err
	}

	msid, err := sendCreateStream(nc, demux)
	if err != nil {
		return err
	}

	if err := sendPlay(nc, msid, playName); err != nil {
		return err
	}

	log.Info("RTMP pull connected", "app", appName, "stream", playName)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		msg, err := demux.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		switch msg.TypeID {
		case msgSetChunkSize:
			if len(msg.Data) >= 4 {
				demux.SetChunkSize(binary.BigEndian.Uint32(msg.Data) & 0x7FFFFFFF)
			}

		case msgUserControl:
			// Keepalive from the source server (e.g. Zixi); see pingResponse.
			if resp := pingResponse(msg.Data); resp != nil {
				if err := writeChunk(nc, pullChunkSize, 2, 0, msgUserControl, 0, resp); err != nil {
					return fmt.Errorf("send ping response: %w", err)
				}
			}

		case msgVideo:
			bus.AddBytesIn(int64(len(msg.Data)))
			pkt, newCI := decodeVideoTag(streamName, bus.CodecInfo(), msg)
			if newCI != nil {
				bus.SetCodecInfo(newCI)
			}
			if pkt != nil {
				bus.Publish(pkt)
			}

		case msgAudio:
			bus.AddBytesIn(int64(len(msg.Data)))
			pkt, newCI := decodeAudioTag(streamName, bus.CodecInfo(), msg)
			if newCI != nil {
				bus.SetCodecInfo(newCI)
			}
			if pkt != nil {
				bus.Publish(pkt)
			}

		case msgCommandAMF0, msgCommandAMF3, msgDataAMF0:
			data := msg.Data
			if msg.TypeID == msgCommandAMF3 && len(data) > 0 {
				data = data[1:]
			}
			if vals, err := DecodeAMF0All(data); err == nil && len(vals) > 0 {
				log.Debug("backup pull server message", "command", vals[0])
			}
		}
	}
}

// parsePullURL splits a rtmp://host[:port]/app/streamName URL into its
// dial address, app name, and stream name.
func parsePullURL(rawURL string) (appName, streamName, addr string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse backup url: %w", err)
	}
	if u.Scheme != "rtmp" {
		return "", "", "", fmt.Errorf("unsupported backup url scheme %q (only rtmp:// pulls are supported)", u.Scheme)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "1935"
	}
	addr = net.JoinHostPort(host, port)

	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("backup url path must be /app/streamName, got %q", u.Path)
	}
	return parts[0], parts[1], addr, nil
}

// sendConnect sends the RTMP connect command and waits for its _result.
func sendConnect(nc net.Conn, demux *ChunkDemuxer, addr, appName string) error {
	tcURL := fmt.Sprintf("rtmp://%s/%s", addr, appName)
	payload, err := EncodeAMF0(
		AMF0String("connect"),
		AMF0Number(1),
		AMF0Object{
			"app":   AMF0String(appName),
			"type":  AMF0String("nonprivate"),
			"tcUrl": AMF0String(tcURL),
		},
	)
	if err != nil {
		return fmt.Errorf("encode connect: %w", err)
	}
	if err := writeChunk(nc, pullChunkSize, 3, 0, msgCommandAMF0, 0, payload); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}
	return waitForResult(demux, "connect")
}

// sendCreateStream sends createStream and returns the server-assigned MSID.
func sendCreateStream(nc net.Conn, demux *ChunkDemuxer) (uint32, error) {
	payload, err := EncodeAMF0(AMF0String("createStream"), AMF0Number(2), AMF0Null{})
	if err != nil {
		return 0, fmt.Errorf("encode createStream: %w", err)
	}
	if err := writeChunk(nc, pullChunkSize, 3, 0, msgCommandAMF0, 0, payload); err != nil {
		return 0, fmt.Errorf("send createStream: %w", err)
	}
	return waitForStreamID(demux)
}

// sendPlay sends play(streamName) on the given message stream ID.
func sendPlay(nc net.Conn, msid uint32, streamName string) error {
	payload, err := EncodeAMF0(AMF0String("play"), AMF0Number(0), AMF0Null{}, AMF0String(streamName))
	if err != nil {
		return fmt.Errorf("encode play: %w", err)
	}
	if err := writeChunk(nc, pullChunkSize, 8, 0, msgCommandAMF0, msid, payload); err != nil {
		return fmt.Errorf("send play: %w", err)
	}
	return nil
}

// nextCommand reads messages from demux — transparently applying any Set
// Chunk Size control message along the way — until it finds a decodable
// AMF0/AMF3 command message, and returns its decoded values.
func nextCommand(demux *ChunkDemuxer) ([]AMF0Value, error) {
	for {
		msg, err := demux.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch msg.TypeID {
		case msgSetChunkSize:
			if len(msg.Data) >= 4 {
				demux.SetChunkSize(binary.BigEndian.Uint32(msg.Data) & 0x7FFFFFFF)
			}
		case msgCommandAMF0, msgCommandAMF3:
			data := msg.Data
			if msg.TypeID == msgCommandAMF3 && len(data) > 0 {
				data = data[1:]
			}
			vals, err := DecodeAMF0All(data)
			if err != nil || len(vals) == 0 {
				continue
			}
			return vals, nil
		}
	}
}

func waitForResult(demux *ChunkDemuxer, action string) error {
	for {
		vals, err := nextCommand(demux)
		if err != nil {
			return fmt.Errorf("waiting for %s result: %w", action, err)
		}
		cmd, _ := vals[0].(AMF0String)
		switch cmd {
		case "_result":
			return nil
		case "_error":
			return fmt.Errorf("%s: server returned _error", action)
		}
	}
}

func waitForStreamID(demux *ChunkDemuxer) (uint32, error) {
	for {
		vals, err := nextCommand(demux)
		if err != nil {
			return 0, fmt.Errorf("waiting for createStream result: %w", err)
		}
		cmd, _ := vals[0].(AMF0String)
		if cmd == "_error" {
			return 0, fmt.Errorf("createStream: server returned _error")
		}
		if cmd != "_result" || len(vals) < 4 {
			continue
		}
		n, ok := vals[3].(AMF0Number)
		if !ok {
			continue
		}
		return uint32(n), nil
	}
}
