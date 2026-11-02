package srt

import (
	"bytes"
	"context"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"

	"github.com/5centscdn/origin-mediaserver/internal/media"
	"github.com/5centscdn/origin-mediaserver/internal/mux"
)

// TestPull_EndToEnd drives Pull against a real gosrt.Listen "remote SRT
// source" (a bare listener, not Server — Pull is the caller/dial side, the
// opposite role from Server's listener/callee side) carrying real MPEG-TS
// bytes from this codebase's own mux.TSMuxer, and confirms a correctly
// decoded H.264 keyframe reaches the bus — the same standard
// TestServer_EndToEnd uses for the listen side, so both SRT modes are held
// to the same real-bytes-through-both-real-ends bar.
func TestPull_EndToEnd(t *testing.T) {
	srtCfg := gosrt.DefaultConfig()
	ln, err := gosrt.Listen("srt", "127.0.0.1:18856", srtCfg)
	if err != nil {
		t.Fatalf("gosrt.Listen: %v", err)
	}
	defer ln.Close()

	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo idr: %v", err)
	}
	// A second frame is required to flush the demuxer's PES reassembly
	// buffer — see TestServer_EndToEnd's identical note.
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	// Act as the "remote SRT source": accept one connection and write the
	// pre-built TS bytes into it, exactly what a real upstream SRT encoder
	// would do once Pull dials in. The connection is kept open after the
	// write (SRT is UDP-based, and closing immediately risks truncating
	// data still in flight) — srcConnCh hands it back so the test can close
	// it explicitly once done, since Demuxer.Run's io.ReadFull only notices
	// ctx cancellation between reads, not while blocked inside one; without
	// an explicit close/EOF here, Pull would never return after cancel().
	srcConnCh := make(chan gosrt.Conn, 1)
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			t.Logf("Accept2: %v", err)
			return
		}
		conn, err := req.Accept()
		if err != nil {
			t.Logf("req.Accept: %v", err)
			return
		}
		n, err := conn.Write(tsBuf.Bytes())
		t.Logf("wrote %d bytes, err=%v", n, err)
		srcConnCh <- conn
	}()
	time.Sleep(200 * time.Millisecond) // let the accept goroutine start listening for a connection

	bus := media.NewBus()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Subscribe BEFORE starting Pull: media.Bus.Publish is non-blocking and
	// drops packets with no subscriber registered yet — Pull's demuxer could
	// otherwise parse and publish the keyframe before this goroutine gets to
	// Subscribe, losing it.
	sub := bus.Subscribe(ctx)
	defer sub.Close()

	pullErrCh := make(chan error, 1)
	go func() { pullErrCh <- Pull(ctx, "127.0.0.1:18856", bus, "", 0) }()

	var gotKeyframe bool
	waitDeadline := time.After(5 * time.Second)
	for !gotKeyframe {
		select {
		case pkt := <-sub.Chan():
			t.Logf("got packet: codec=%v keyframe=%v", pkt.Codec, pkt.IsKeyframe)
			if pkt.Codec == media.CodecH264 && pkt.IsKeyframe {
				gotKeyframe = true
			}
		case err := <-pullErrCh:
			t.Fatalf("Pull returned early: %v", err)
		case <-waitDeadline:
			t.Fatal("never received a decoded H.264 keyframe over SRT Pull")
		}
	}

	select {
	case srcConn := <-srcConnCh:
		srcConn.Close() // forces EOF on Pull's read side so demux.Run actually returns
	case <-time.After(2 * time.Second):
	}
	cancel()
	<-pullErrCh
}

// TestPull_DialFailureReturnsError guards against Pull silently succeeding
// (or hanging) when there's nothing listening at url.
func TestPull_DialFailureReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := Pull(ctx, "127.0.0.1:1", media.NewBus(), "", 0) // nothing listens on port 1
	if err == nil {
		t.Fatal("Pull against an unreachable address returned nil error, want an error")
	}
}

// TestPull_URLWithSchemeIsAccepted guards a real bug found testing this
// against a live pull source: gosrt.Dial's address argument must be a bare
// host:port (network is the separate "srt" argument), but the panel's Pull
// Sources URL field naturally invites "srt://host:port" like every other
// protocol's URL field there — Pull must strip the scheme rather than
// failing with "too many colons in address".
func TestPull_URLWithSchemeIsAccepted(t *testing.T) {
	srtCfg := gosrt.DefaultConfig()
	ln, err := gosrt.Listen("srt", "127.0.0.1:18859", srtCfg)
	if err != nil {
		t.Fatalf("gosrt.Listen: %v", err)
	}
	defer ln.Close()

	acceptedCh := make(chan struct{}, 1)
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		conn, err := req.Accept()
		if err != nil {
			return
		}
		acceptedCh <- struct{}{}
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pullErrCh := make(chan error, 1)
	go func() { pullErrCh <- Pull(ctx, "srt://127.0.0.1:18859", media.NewBus(), "", 0) }()

	select {
	case <-acceptedCh:
	case err := <-pullErrCh:
		t.Fatalf("Pull with a scheme-prefixed URL failed to dial: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Pull never connected")
	}
	cancel()
}

// TestPull_EncryptedEndToEnd is TestPull_EndToEnd's counterpart for a
// passphrase-encrypted upstream (the Nimble/Haivision "Incoming SRT stream"
// pull-source use case this test guards the regression for): the "remote
// source" listener requires AES encryption, and Pull must decrypt it with
// the matching passphrase and still decode a real H.264 keyframe.
func TestPull_EncryptedEndToEnd(t *testing.T) {
	const passphrase = "this-is-a-test-passphrase"

	srtCfg := gosrt.DefaultConfig()
	srtCfg.Passphrase = passphrase
	srtCfg.EnforcedEncryption = true
	ln, err := gosrt.Listen("srt", "127.0.0.1:18857", srtCfg)
	if err != nil {
		t.Fatalf("gosrt.Listen: %v", err)
	}
	defer ln.Close()

	sps := decodeFixture(t, fixtureSPS)
	pps := decodeFixture(t, fixturePPS)
	idr := decodeFixture(t, fixtureIDR)
	delta := decodeFixture(t, fixtureDelta)

	var tsBuf bytes.Buffer
	tsMuxer := mux.NewTSMuxer(&tsBuf, mux.StreamTypeH264, mux.StreamTypeAAC)
	tsMuxer.SetSPSPPS(sps, pps)
	if err := tsMuxer.WriteVideo(withStartCode(idr), 0, 0, true); err != nil {
		t.Fatalf("WriteVideo idr: %v", err)
	}
	if err := tsMuxer.WriteVideo(withStartCode(delta), 3000, 3000, false); err != nil {
		t.Fatalf("WriteVideo delta: %v", err)
	}

	srcConnCh := make(chan gosrt.Conn, 1)
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			t.Logf("Accept2: %v", err)
			return
		}
		// Accept2's per-request ConnRequest, unlike a plain Listen+Accept
		// loop, needs the passphrase set explicitly on the request before
		// Accept() — the listener-level config.Passphrase only seeds
		// IsEncrypted(), it doesn't auto-decrypt each connection.
		if req.IsEncrypted() {
			if err := req.SetPassphrase(passphrase); err != nil {
				t.Logf("SetPassphrase: %v", err)
				return
			}
		}
		conn, err := req.Accept()
		if err != nil {
			t.Logf("req.Accept: %v", err)
			return
		}
		n, err := conn.Write(tsBuf.Bytes())
		t.Logf("wrote %d bytes, err=%v", n, err)
		srcConnCh <- conn
	}()
	time.Sleep(200 * time.Millisecond)

	bus := media.NewBus()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub := bus.Subscribe(ctx)
	defer sub.Close()

	pullErrCh := make(chan error, 1)
	go func() { pullErrCh <- Pull(ctx, "127.0.0.1:18857", bus, passphrase, 0) }()

	var gotKeyframe bool
	waitDeadline := time.After(5 * time.Second)
	for !gotKeyframe {
		select {
		case pkt := <-sub.Chan():
			if pkt.Codec == media.CodecH264 && pkt.IsKeyframe {
				gotKeyframe = true
			}
		case err := <-pullErrCh:
			t.Fatalf("Pull returned early: %v", err)
		case <-waitDeadline:
			t.Fatal("never received a decoded H.264 keyframe over encrypted SRT Pull")
		}
	}

	select {
	case srcConn := <-srcConnCh:
		srcConn.Close()
	case <-time.After(2 * time.Second):
	}
	cancel()
	<-pullErrCh
}

// TestPull_WrongPassphraseFails guards against silently falling back to an
// unencrypted (or garbage-decrypted) connection when the configured
// passphrase doesn't match the upstream's — SRT's handshake must reject the
// connection outright.
func TestPull_WrongPassphraseFails(t *testing.T) {
	srtCfg := gosrt.DefaultConfig()
	srtCfg.Passphrase = "the-real-passphrase-value"
	srtCfg.EnforcedEncryption = true
	ln, err := gosrt.Listen("srt", "127.0.0.1:18858", srtCfg)
	if err != nil {
		t.Fatalf("gosrt.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		req.Accept()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = Pull(ctx, "127.0.0.1:18858", media.NewBus(), "the-wrong-passphrase-value", 0)
	if err == nil {
		t.Fatal("Pull with a mismatched passphrase returned nil error, want a dial/handshake error")
	}
}
