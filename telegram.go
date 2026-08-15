package main

import (
	"context"
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

// Dialled through the tunnel's own stack rather than s.client: that client's
// transport ignores the requested address and always dials metaDialAddr().
func (s *ipStack) telegramRTT(ctx context.Context, timeout time.Duration) (time.Duration, bool) {
	return firstReachable(ctx, func(ctx context.Context, addr string) (net.Conn, error) {
		return s.tnet.DialContext(ctx, "tcp", addr)
	}, telegramDCs, timeout)
}
