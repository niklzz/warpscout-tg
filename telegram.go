package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"net"
	"strings"
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

// allDCs is the reachedDCs mask with every entry of telegramDCs set.
var allDCs = uint8(1<<len(telegramDCs)) - 1

// Every address at once under one deadline. Walking them in turn would cost
// len(addrs) x timeout on a blocked exit, where each dial runs the clock out
// in full - a scan pays that per endpoint.
//
// An account lives on one DC and the client cannot pick another, so an exit
// where only some DCs answer is an exit where some accounts never connect
// (seen live: WARP exits in FRA reached DC1/3/5 while DC2/4 never answered,
// and Telegram Desktop sat in "connecting..."). reached is therefore a bitmask
// (bit i = addrs[i]) and the caller wants all of it set; rtt is the slowest
// answer, since the DC a client sits on may well be that one.
func reachedDCs(ctx context.Context, dial dialFunc, addrs []string, timeout time.Duration) (rtt time.Duration, reached uint8) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		i   int
		rtt time.Duration
		ok  bool
	}
	res := make(chan result, len(addrs))
	start := time.Now()
	for i, addr := range addrs {
		go func(i int, addr string) {
			conn, err := dial(ctx, addr)
			if err != nil {
				res <- result{i: i}
				return
			}
			conn.Close()
			res <- result{i, time.Since(start), true}
		}(i, addr)
	}

	for range addrs {
		r := <-res // every dial honours ctx, so this returns by the deadline
		if r.ok {
			reached |= 1 << r.i
			rtt = max(rtt, r.rtt)
		}
	}
	return rtt, reached
}

// tgPartialStr renders a mask short of allDCs for the TG column.
func tgPartialStr(reached uint8) string {
	if reached == 0 {
		return "blocked"
	}
	return fmt.Sprintf("%d/%d", bits.OnesCount8(reached), len(telegramDCs))
}

// dcNames lists the DCs in mask as "DC2, DC4".
func dcNames(mask uint8) string {
	var names []string
	for i := range telegramDCs {
		if mask&(1<<i) != 0 {
			names = append(names, fmt.Sprintf("DC%d", i+1))
		}
	}
	return strings.Join(names, ", ")
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
// A dial only counts once the DC has answered an MTProto request, so the
// reported RTT is time-to-response, not time-to-SYN-ACK. reached is the mask
// of DCs that answered; the row is Telegram-ok only when it equals allDCs.
func (s *ipStack) telegramRTT(ctx context.Context, timeout time.Duration) (rtt time.Duration, reached uint8) {
	return reachedDCs(ctx, func(ctx context.Context, addr string) (net.Conn, error) {
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
