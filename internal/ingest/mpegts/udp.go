package mpegts

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/5centscdn/origin-mediaserver/internal/media"
)

// ListenUDP binds a UDP socket at listenAddr (host:port, optionally prefixed
// with a "udp://" scheme to match how other pull_sources URLs look; a
// multicast group address joins that group) and demuxes incoming MPEG-TS
// datagrams onto bus until ctx is done or a fatal socket error occurs.
// Mirrors rtmp.Pull's blocking, run-until-ended contract, so
// internal/pull.Manager's existing supervisor loop (reconnect/backoff/
// status) works unchanged for this protocol too — "reconnect" here just
// means re-binding the socket.
func ListenUDP(ctx context.Context, listenAddr string, bus *media.Bus) error {
	listenAddr = strings.TrimPrefix(listenAddr, "udp://")

	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", listenAddr, err)
	}

	var conn *net.UDPConn
	if udpAddr.IP != nil && udpAddr.IP.IsMulticast() {
		conn, err = net.ListenMulticastUDP("udp", nil, udpAddr)
	} else {
		conn, err = net.ListenUDP("udp", udpAddr)
	}
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer conn.Close()

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopWatch:
		}
	}()

	d := NewDemuxer(bus)
	d.OnBytes = func(n int) { bus.AddBytesIn(int64(n)) }
	defer d.Flush()

	// A UDP datagram typically carries a whole number of 188-byte TS
	// packets (commonly 7 per 1316-byte payload). Each Read is one
	// datagram, not a continuous byte stream — feedPackets processes
	// exactly the packets it contains, with no cross-datagram buffering.
	buf := make([]byte, 65536)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read udp: %w", err)
		}
		d.feedPackets(buf[:n])
	}
}
