package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// Telegram's MTProto data centres. The addresses are hardcoded on purpose: a
// client dials them directly, so resolving a name first would test a path
// Telegram itself never takes.
//
// ponytail: IPv4 only. The tunnel carries both families whatever -6 selects, so
// a v6 scan reaches these as well; add the v6 DCs if a network turns up where it
// does not.
var telegramDCs = []string{
	"149.154.175.50:443",  // DC1
	"149.154.167.51:443",  // DC2
	"149.154.175.100:443", // DC3
	"149.154.167.91:443",  // DC4
	"91.108.56.130:443",   // DC5
}

type dialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Every address at once under one deadline, first answer wins. Walking them in
// turn would cost len(addrs) x timeout on a blocked exit, where each dial runs
// the clock out in full - a scan pays that per endpoint. One reachable DC is
// also all a client needs: it finds the rest through that one.
func firstReachable(ctx context.Context, dial dialFunc, addrs []string, timeout time.Duration) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		rtt time.Duration
		ok  bool
	}
	res := make(chan result, len(addrs))
	start := time.Now()
	for _, addr := range addrs {
		go func(addr string) {
			conn, err := dial(ctx, addr)
			if err != nil {
				res <- result{}
				return
			}
			conn.Close()
			res <- result{time.Since(start), true}
		}(addr)
	}

	for range addrs {
		select {
		case r := <-res:
			if r.ok {
				return r.rtt, true
			}
		case <-ctx.Done():
			return 0, false
		}
	}
	return 0, false
}

// mtprotoProbe speaks just enough MTProto to make a DC answer: the
// intermediate-transport magic plus an unencrypted req_pq_multi. A bare TCP
// connect proves nothing — DPI boxes complete the handshake toward Telegram
// and silently drop the payload, so connect-only checks pass everywhere.
// Only response bytes framed by the transport mean the DC really talked back.
func mtprotoProbe(ctx context.Context, conn net.Conn) error {
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
		defer conn.SetDeadline(time.Time{})
	}

	msg := make([]byte, 0, 4+4+40)
	msg = append(msg, 0xee, 0xee, 0xee, 0xee)                                  // intermediate transport
	msg = binary.LittleEndian.AppendUint32(msg, 40)                            // frame: auth_key_id + msg_id + len + req_pq_multi
	msg = append(msg, make([]byte, 8)...)                                      // auth_key_id = 0, plaintext message
	msg = binary.LittleEndian.AppendUint64(msg, uint64(time.Now().Unix())<<32) // msg_id: unixtime<<32, %4 == 0 as clients must
	msg = binary.LittleEndian.AppendUint32(msg, 20)                            // message_data_length
	msg = binary.LittleEndian.AppendUint32(msg, 0xbe7e8ef1)                    // req_pq_multi
	var nonce [16]byte
	rand.Read(nonce[:])
	msg = append(msg, nonce[:]...)

	if _, err := conn.Write(msg); err != nil {
		return err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	// res_pq is ~84 bytes; anything outside a sane frame is not Telegram.
	if n := binary.LittleEndian.Uint32(hdr[:]); n == 0 || n > 1<<16 {
		return fmt.Errorf("mtproto: implausible frame length %d", n)
	}
	return nil
}

// Dialled through the tunnel's own stack rather than s.client: that client's
// transport ignores the requested address and always dials metaDialAddr().
// The dial only counts once the DC has answered an MTProto request, so the
// reported RTT is time-to-first-response, not time-to-SYN-ACK.
func (s *ipStack) telegramRTT(ctx context.Context, timeout time.Duration) (time.Duration, bool) {
	return firstReachable(ctx, func(ctx context.Context, addr string) (net.Conn, error) {
		conn, err := s.tnet.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		if err := mtprotoProbe(ctx, conn); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}, telegramDCs, timeout)
}
