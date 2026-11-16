// Package server runs the DNS listeners (UDP + TCP) and ties together
// resolution, logging, truncation and zone transfers.
package server

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"

	"gyndns/internal/dnsmsg"
	"gyndns/internal/qlog"
	"gyndns/internal/xfr"
	"gyndns/internal/zone"
)

// Resolver resolves one query.
type Resolver interface {
	Resolve(qname string, qtype uint16, client net.IP) *zone.Result
}

// Server is the DNS front end.
type Server struct {
	Store Resolver
	Log   *qlog.Log
	// Master handles AXFR/IXFR (nil on slaves).
	Master *xfr.Master
	// Slave handles NOTIFY (nil on masters).
	Slave *xfr.Slave
	// MaxUDP caps UDP responses even when the client advertises more.
	MaxUDP int
}

// New creates a server.
func New(store Resolver, log *qlog.Log) *Server {
	return &Server{Store: store, Log: log, MaxUDP: 1232}
}

// Answer builds the response message for one query. proto is "udp" or "tcp".
// The resolution result is returned alongside for logging.
func (s *Server) Answer(q *dnsmsg.Message, client net.IP, proto string) (*dnsmsg.Message, *zone.Result) {
	if q.Opcode == dnsmsg.OpcodeNotify && s.Slave != nil {
		return s.Slave.HandleNotify(q), nil
	}
	if len(q.Questions) == 0 {
		r := dnsmsg.NewReply(q)
		r.RCode = dnsmsg.RcodeFormatError
		return r, nil
	}
	qu := q.Questions[0]
	if (qu.Type == dnsmsg.TypeAXFR || qu.Type == dnsmsg.TypeIXFR) && s.Master != nil {
		if proto != "tcp" {
			r := dnsmsg.NewReply(q)
			r.RCode = dnsmsg.RcodeRefused // transfers are TCP only
			return r, nil
		}
		return s.Master.HandleTransfer(q), nil
	}

	res := s.Store.Resolve(qu.Name, qu.Type, client)
	r := dnsmsg.NewReply(q)
	r.RCode = res.RCode
	r.Answers = res.Answers
	r.Authorities = res.Authorities
	if q.HasEDNS() {
		r.SetEDNS(uint16(s.MaxUDP))
	}
	return r, res
}

func (s *Server) log(q *dnsmsg.Message, client net.IP, proto string, resp *dnsmsg.Message, res *zone.Result, start time.Time) {
	if s.Log == nil || len(q.Questions) == 0 || res == nil {
		return
	}
	qu := q.Questions[0]
	answers := make([]string, 0, len(resp.Answers))
	for _, a := range resp.Answers {
		answers = append(answers, a.String())
	}
	s.Log.Add(qlog.Entry{
		Time:      time.Now(),
		Client:    ipStr(client),
		Proto:     proto,
		QName:     qu.Name,
		QType:     dnsmsg.TypeString(qu.Type),
		RCode:     dnsmsg.RcodeString(resp.RCode),
		Zone:      res.Zone,
		Answers:   answers,
		Trace:     res.Trace,
		Latency:   float64(time.Since(start).Microseconds()) / 1000.0,
		Truncated: resp.TC,
	})
}

// ServeUDP listens on addr until ctx is cancelled.
func (s *Server) ServeUDP(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		pc.Close()
	}()
	buf := make([]byte, 65535)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		raw := make([]byte, n)
		copy(raw, buf[:n])
		go s.handleUDP(pc, src, raw)
	}
}

func (s *Server) handleUDP(pc net.PacketConn, src net.Addr, raw []byte) {
	start := time.Now()
	q, err := dnsmsg.Unpack(raw)
	if err != nil {
		// reflect the ID with FORMERR if at least the header parsed
		if len(raw) >= 12 {
			resp := make([]byte, 12)
			copy(resp, raw[:12])
			resp[2] = 0x80 | (raw[2] & 0x78) // QR + opcode
			resp[3] = dnsmsg.RcodeFormatError
			pc.WriteTo(resp, src)
		}
		return
	}
	client := clientIP(q, src)
	resp, res := s.Answer(q, client, "udp")
	limit := q.UDPSize()
	if s.MaxUDP > 0 && limit > s.MaxUDP {
		limit = s.MaxUDP
	}
	out, err := resp.Pack(limit)
	if err != nil {
		return
	}
	pc.WriteTo(out, src)
	if len(out) > 2 && out[2]&0x02 != 0 { // TC bit on the wire
		resp.TC = true
	}
	s.log(q, client, "udp", resp, res, start)
}

// ServeTCP listens on addr until ctx is cancelled.
func (s *Server) ServeTCP(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleTCP(conn)
	}
}

func (s *Server) handleTCP(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	for {
		var lbuf [2]byte
		if _, err := io.ReadFull(conn, lbuf[:]); err != nil {
			return
		}
		rlen := int(binary.BigEndian.Uint16(lbuf[:]))
		raw := make([]byte, rlen)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		start := time.Now()
		q, err := dnsmsg.Unpack(raw)
		if err != nil {
			return
		}
		client := clientIP(q, conn.RemoteAddr())
		resp, res := s.Answer(q, client, "tcp")
		out, err := resp.Pack(0) // TCP: no truncation
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(lbuf[:], uint16(len(out)))
		if _, err := conn.Write(append(lbuf[:], out...)); err != nil {
			return
		}
		s.log(q, client, "tcp", resp, res, start)
	}
}

// clientIP prefers the EDNS client subnet over the transport source.
func clientIP(q *dnsmsg.Message, src net.Addr) net.IP {
	if cs := q.ClientSubnet(); cs != nil {
		return cs.IP
	}
	if a, ok := src.(*net.UDPAddr); ok {
		return a.IP
	}
	if a, ok := src.(*net.TCPAddr); ok {
		return a.IP
	}
	host, _, err := net.SplitHostPort(src.String())
	if err == nil {
		return net.ParseIP(host)
	}
	return nil
}

func ipStr(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
