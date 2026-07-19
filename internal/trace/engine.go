package trace

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// IANA protocol numbers for ParseMessage.
const (
	protoICMP4 = 1
	protoICMP6 = 58
)

// lateGrace is how long after being sent a probe can still be credited by a
// straggling reply: the sweeper marks it Lost at the timeout (so the UI shows
// the loss promptly) but keeps it matchable, and a reply inside the grace flips
// it back to OK and repairs the stats. Kept well under the ~18-minute uint16
// sequence-wrap horizon.
const lateGrace = 60 * time.Second

// resolveRetryInterval is how often hops with an unresolved hostname or ASN get
// their lookups re-kicked — a lookup that failed transiently (e.g. DNS still
// down as a dropout recovers) would otherwise leave the hop unlabeled forever.
const resolveRetryInterval = 30 * time.Second

// Options configures a traceroute run.
type Options struct {
	Interval  time.Duration // delay between probe rounds
	Timeout   time.Duration // how long to wait before declaring a probe lost
	MaxHops   int           // ceiling on TTL
	Force4    bool          // only trace IPv4
	Force6    bool          // only trace IPv6
	Raw       bool          // use raw ICMP sockets (needs root) instead of datagram
	LookupASN bool          // annotate each hop with its origin AS (Team Cymru)
}

// DefaultOptions mirrors mtr's defaults, except a snappier 0.5s interval.
func DefaultOptions() Options {
	return Options{
		Interval: 500 * time.Millisecond,
		Timeout:  2 * time.Second,
		MaxHops:  30,
		Force4:   false,
		Force6:   false,
	}
}

// engine drives one Session over one raw ICMP socket.
type engine struct {
	s    *Session
	opts Options

	conn     *icmp.PacketConn
	p4       *ipv4.PacketConn
	p6       *ipv6.PacketConn
	id       int
	echoType icmp.Type
	proto    int
	dst      net.Addr

	seq uint16
}

// Start resolves the target, then launches a traceroute per available address
// family. It returns one Session per family (IPv4 first when both exist).
//
// onResolve, if non-nil, is called once before each resolution attempt with the
// 1-based attempt number and the previous attempt's error (nil on the first), so
// the caller can show progress while DNS is unreachable (e.g. Starlink booting).
func Start(ctx context.Context, target string, opts Options, onResolve func(attempt int, prev error)) ([]*Session, error) {
	ip4, ip6, err := resolveRetry(ctx, target, opts, onResolve)
	if err != nil {
		return nil, err
	}

	var sessions []*Session
	if ip4 != nil {
		s := newSession("IPv4", target, ip4, opts.MaxHops)
		if err := startEngine(ctx, s, opts); err != nil {
			return nil, fmt.Errorf("IPv4: %w", err)
		}
		sessions = append(sessions, s)
	}
	if ip6 != nil {
		s := newSession("IPv6", target, ip6, opts.MaxHops)
		if err := startEngine(ctx, s, opts); err != nil {
			if len(sessions) == 0 {
				return nil, fmt.Errorf("IPv6: %w", err)
			}
			// IPv4 already running; don't fail the whole run on v6 socket errors.
		} else {
			sessions = append(sessions, s)
		}
	}
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no usable address family for %q", target)
	}
	return sessions, nil
}

// resolveRetry resolves the target, retrying until it succeeds or ctx is
// cancelled so launching during a dropout doesn't fail hard — a Starlink cold
// boot can leave DNS unreachable for minutes. Each attempt is capped at 5s and
// they're paced 1s apart. A literal IP resolves on the first attempt. onResolve,
// if non-nil, fires before each attempt so the caller can report progress; a real
// error (a typo'd host) surfaces through it as a repeating message the user can
// Ctrl-C out of.
func resolveRetry(ctx context.Context, target string, opts Options, onResolve func(attempt int, prev error)) (ip4, ip6 net.IP, err error) {
	for attempt := 1; ; attempt++ {
		if onResolve != nil {
			onResolve(attempt, err)
		}
		rc, cancel := context.WithTimeout(ctx, 5*time.Second)
		ip4, ip6, err = resolve(rc, target, opts)
		cancel()
		if err == nil {
			return ip4, ip6, nil
		}
		var pe permanentError
		if errors.As(err, &pe) {
			return nil, nil, err // retrying can never fix this — fail fast
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// permanentError marks a resolution failure that retrying can never fix (e.g. a
// family flag contradicting a literal IP); resolveRetry fails fast on it.
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

func resolve(ctx context.Context, target string, opts Options) (ip4, ip6 net.IP, err error) {
	if ip := net.ParseIP(target); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			if opts.Force6 {
				return nil, nil, permanentError{fmt.Errorf("-6 was given but %q is an IPv4 address", target)}
			}
			return v4, nil, nil
		}
		if opts.Force4 {
			return nil, nil, permanentError{fmt.Errorf("-4 was given but %q is an IPv6 address", target)}
		}
		return nil, ip, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", target)
	if err != nil {
		return nil, nil, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			if ip4 == nil && !opts.Force6 {
				ip4 = v4
			}
		} else if ip6 == nil && !opts.Force4 {
			ip6 = ip
		}
	}
	if ip4 == nil && ip6 == nil {
		return nil, nil, fmt.Errorf("could not resolve %q", target)
	}
	return ip4, ip6, nil
}

func startEngine(ctx context.Context, s *Session, opts Options) error {
	e := &engine{s: s, opts: opts, id: os.Getpid() & 0xffff}

	if s.Family == "IPv4" {
		network := "udp4"
		if opts.Raw {
			network = "ip4:icmp"
		}
		conn, err := icmp.ListenPacket(network, "0.0.0.0")
		if err != nil {
			return err
		}
		e.conn = conn
		e.p4 = conn.IPv4PacketConn()
		e.echoType = ipv4.ICMPTypeEcho
		e.proto = protoICMP4
	} else {
		network := "udp6"
		if opts.Raw {
			network = "ip6:ipv6-icmp"
		}
		conn, err := icmp.ListenPacket(network, "::")
		if err != nil {
			return err
		}
		e.conn = conn
		e.p6 = conn.IPv6PacketConn()
		e.echoType = ipv6.ICMPTypeEchoRequest
		e.proto = protoICMP6
	}

	// Datagram ICMP sockets: the kernel rewrites the echo ID to the socket's
	// port (in both the reply and the packet quoted inside Time Exceeded), so
	// adopt the port as our identifier and address the target as a UDP endpoint.
	if opts.Raw {
		e.dst = &net.IPAddr{IP: s.TargetIP}
	} else {
		e.dst = &net.UDPAddr{IP: s.TargetIP}
		if ua, ok := e.conn.LocalAddr().(*net.UDPAddr); ok {
			e.id = ua.Port & 0xffff
		}
	}

	go e.recvLoop(ctx)
	go e.sendLoop(ctx)
	go e.sweepLoop(ctx)
	go e.resolveLoop(ctx)
	return nil
}

// sendLoop probes one hop at a time, spacing a full cycle (TTL 1..maxHops)
// evenly across the interval. Pacing the probes — rather than bursting them all
// at once — means each row's reply lands at a different moment, so rows update
// live as results arrive instead of all flipping together once per round. The
// ceiling is always the whole TTL range — never clamped to the destination —
// so the path tracks route changes live (Snapshot hides hops past the target).
func (e *engine) sendLoop(ctx context.Context) {
	defer e.conn.Close()

	// Probe the whole TTL range every cycle. Snapshot derives the displayed path
	// length from which hop answers as the target, so probing past it costs a few
	// extra echoes but lets the path track route changes live (grow and shrink).
	ceiling := e.s.maxHops
	ttl := 1
	var round int
	for {
		if ttl == 1 {
			e.s.mu.Lock()
			e.s.roundNo++
			round = e.s.roundNo
			e.s.mu.Unlock()
		}

		e.sendProbe(ttl, round)

		ttl++
		if ttl > ceiling {
			ttl = 1
		}

		delay := e.opts.Interval / time.Duration(ceiling)
		if delay < time.Millisecond {
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (e *engine) sendProbe(ttl, round int) {
	e.s.mu.Lock()
	e.seq++
	seq := e.seq
	now := time.Now()
	sm := &sample{seq: seq, round: round, sentAt: now, state: Pending}
	h := e.s.getHop(ttl)
	h.samples = append(h.samples, sm)
	if len(h.samples) > sampleCap {
		h.samples = h.samples[len(h.samples)-sampleCap:]
	}
	e.s.pending[seq] = &pendingProbe{ttl: ttl, s: sm, sentAt: now}
	e.s.mu.Unlock()

	msg := icmp.Message{
		Type: e.echoType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   e.id,
			Seq:  int(seq),
			Data: []byte("tracer-probe----"),
		},
	}
	b, err := msg.Marshal(nil)
	if err != nil {
		return
	}
	// Set the hop limit via the socket option rather than a per-write control
	// message: datagram ICMP sockets ignore the control message. Safe because
	// every probe is sent from this one goroutine.
	if e.p4 != nil {
		_ = e.p4.SetTTL(ttl)
		_, _ = e.p4.WriteTo(b, nil, e.dst)
	} else {
		_ = e.p6.SetHopLimit(ttl)
		_, _ = e.p6.WriteTo(b, nil, e.dst)
	}
}

func (e *engine) recvLoop(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = e.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, peer, err := e.conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// A transient socket error (network down mid-outage, e.g. a Starlink
			// dropout) must not kill the receiver — back off briefly and keep
			// listening so monitoring resumes when the link returns.
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		m, err := icmp.ParseMessage(e.proto, buf[:n])
		if err != nil {
			continue
		}
		e.handle(m, peer)
	}
}

func (e *engine) handle(m *icmp.Message, peer net.Addr) {
	switch body := m.Body.(type) {
	case *icmp.Echo:
		// An Echo Reply comes only from the target itself.
		if m.Type == ipv4.ICMPTypeEchoReply || m.Type == ipv6.ICMPTypeEchoReply {
			if body.ID == e.id {
				e.recordReply(uint16(body.Seq), peer)
			}
		}
	case *icmp.TimeExceeded:
		if id, seq, ok := extractEcho(body.Data, e.proto); ok && int(id) == e.id {
			e.recordReply(seq, peer)
		}
	case *icmp.DstUnreach:
		if id, seq, ok := extractEcho(body.Data, e.proto); ok && int(id) == e.id {
			// Record a Destination Unreachable only when it comes from the target
			// itself (that hop genuinely reached the destination). From an
			// intermediate router it means the path broke there (common during a
			// Starlink dropout) — ignore it so it doesn't mislabel that TTL's hop.
			if src := addrIP(peer); src != nil && src.Equal(e.s.TargetIP) {
				e.recordReply(seq, peer)
			}
		}
	}
}

// extractEcho pulls the original ICMP id and sequence out of the packet quoted
// inside a Time Exceeded / Destination Unreachable message. It only matches when
// the quoted packet is an echo request, so quoted traffic from some other
// program that happens to collide on id can't be mistaken for one of our probes.
func extractEcho(data []byte, proto int) (id, seq uint16, ok bool) {
	var ipHdrLen int
	var echoRequest byte
	switch proto {
	case protoICMP4:
		if len(data) < 1 {
			return 0, 0, false
		}
		ipHdrLen = int(data[0]&0x0f) * 4
		echoRequest = 8
	case protoICMP6:
		ipHdrLen = 40
		echoRequest = 128
	default:
		return 0, 0, false
	}
	// After the inner IP header: type(1) code(1) checksum(2) id(2) seq(2).
	if len(data) < ipHdrLen+8 {
		return 0, 0, false
	}
	if data[ipHdrLen] != echoRequest {
		return 0, 0, false
	}
	id = binary.BigEndian.Uint16(data[ipHdrLen+4 : ipHdrLen+6])
	seq = binary.BigEndian.Uint16(data[ipHdrLen+6 : ipHdrLen+8])
	return id, seq, true
}

func (e *engine) recordReply(seq uint16, peer net.Addr) {
	addr := addrIP(peer)

	e.s.mu.Lock()
	pp := e.s.pending[seq]
	if pp == nil {
		e.s.mu.Unlock()
		return
	}
	delete(e.s.pending, seq)
	rtt := time.Since(pp.sentAt)
	wasLost := pp.s.state == Lost
	pp.s.rtt = rtt
	pp.s.state = OK

	h := e.s.getHop(pp.ttl)
	if wasLost {
		// The sweeper already counted this probe lost; the reply straggled in
		// inside the grace window, so credit it back and repair the stats.
		h.creditOK(rtt, pp.sentAt)
	} else {
		h.noteOK(rtt, pp.sentAt)
	}
	newAddr := addr != nil && (h.addr == nil || !h.addr.Equal(addr))
	if addr != nil {
		h.addr = addr
		h.addrs[addr.String()] = true
	}
	if newAddr {
		// The hostname and AS label belong to the previous address; drop them so
		// a flapping/rerouted hop can't keep a stale label (e.g. a STARLINK tag
		// left on a CGNAT IP). They re-resolve below for the new address.
		h.host = ""
		h.asn = ""
		h.hostDone = false
		h.asnDone = false
	}
	ttl := pp.ttl
	e.s.mu.Unlock()

	if newAddr {
		go e.resolveHost(ttl, addr)
		if e.opts.LookupASN {
			go e.resolveASN(ttl, addr)
		}
	}
}

func (e *engine) resolveASN(ttl int, addr net.IP) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	label, settled := lookupASN(ctx, addr)

	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	if h := e.s.hops[ttl]; h != nil && h.addr != nil && h.addr.Equal(addr) {
		if label != "" {
			h.asn = label
		}
		h.asnDone = settled // a transient failure leaves it false for the retry loop
	}
}

func (e *engine) resolveHost(ttl int, addr net.IP) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, addr.String())

	// Settled means retrying is pointless: the lookup answered (even with a
	// bogus name) or the name definitively does not exist. Transient failures
	// (timeout, servfail — e.g. DNS still down during a dropout) stay unsettled
	// so resolveLoop retries them.
	var dnsErr *net.DNSError
	settled := err == nil || (errors.As(err, &dnsErr) && dnsErr.IsNotFound)
	name := ""
	if err == nil && len(names) > 0 {
		if n := strings.TrimSuffix(names[0], "."); !isBogusPTR(n) {
			name = n // a bogus PTR leaves host empty; the view falls back to the raw IP
		}
	}

	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	if h := e.s.hops[ttl]; h != nil && h.addr != nil && h.addr.Equal(addr) {
		if name != "" {
			h.host = name
		}
		h.hostDone = settled
	}
}

// sweepLoop marks probes that outlive the timeout as lost.
func (e *engine) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.sweep(time.Now())
		}
	}
}

// sweep decides overdue probes: past the timeout a probe is marked Lost (so the
// UI shows the loss promptly) but stays matchable so a straggling reply can
// still credit it; past the grace window it is pruned for good.
func (e *engine) sweep(now time.Time) {
	grace := lateGrace
	if g := 2 * e.opts.Timeout; g > grace {
		grace = g // an unusually long -t must not out-live its own grace
	}
	e.s.mu.Lock()
	// The lost-timeout adapts to the measured path RTT, capped by the user's -t.
	rto := e.s.rtoLocked(e.opts.Timeout)
	for seq, pp := range e.s.pending {
		age := now.Sub(pp.sentAt)
		switch {
		case age > grace:
			delete(e.s.pending, seq)
		case pp.s.state == Pending && age > rto:
			pp.s.state = Lost
			e.s.getHop(pp.ttl).noteLost()
		}
	}
	e.s.updateOutagesLocked(now)
	e.s.mu.Unlock()
}

// resolveLoop periodically re-kicks reverse-DNS and ASN lookups for hops still
// missing them — a transient failure at discovery time (DNS still down while a
// dropout recovers) must not leave a hop unlabeled for the rest of the run.
func (e *engine) resolveLoop(ctx context.Context) {
	ticker := time.NewTicker(resolveRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		type job struct {
			ttl        int
			addr       net.IP
			host, asnq bool
		}
		var jobs []job
		e.s.mu.Lock()
		for ttl, h := range e.s.hops {
			if h.addr == nil {
				continue
			}
			needHost := !h.hostDone
			needASN := e.opts.LookupASN && !h.asnDone
			if needHost || needASN {
				jobs = append(jobs, job{ttl, h.addr, needHost, needASN})
			}
		}
		e.s.mu.Unlock()
		for _, j := range jobs {
			if j.host {
				go e.resolveHost(j.ttl, j.addr)
			}
			if j.asnq {
				go e.resolveASN(j.ttl, j.addr)
			}
		}
	}
}

// isBogusPTR rejects reverse-DNS names that carry no useful information so the
// display falls back to the raw IP (e.g. "undefined.hostname.localhost").
func isBogusPTR(name string) bool {
	if name == "" || !strings.Contains(name, ".") {
		return true
	}
	lower := strings.ToLower(name)
	for _, junk := range []string{"localhost", "undefined", "unknown", "invalid", "in-addr.arpa", "ip6.arpa"} {
		if strings.Contains(lower, junk) {
			return true
		}
	}
	return false
}

func addrIP(peer net.Addr) net.IP {
	switch a := peer.(type) {
	case *net.IPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	default:
		return nil
	}
}
