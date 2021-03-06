// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux netbsd openbsd

// DNS client: see RFC 1035.
// Has to be linked into package net for Dial.

// TODO(rsc):
//	Check periodically whether /etc/resolv.conf has changed.
//	Could potentially handle many outstanding lookups faster.
//	Could have a small cache.
//	Random UDP source port (net.Dial should do that for us).
//	Random request IDs.

package dns

import (
	"github.com/op/go-logging"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"
)

var noDeadline = time.Time{}
var log = logging.MustGetLogger("")

func check_black(name, server string, msg *dnsMsg, qtype uint16) bool {
	if qtype != dnsTypeA {
		return false
	}
	if len(cfg.servers) == 0 {
		return false
	}
	cname, addrs, err := answer(name, server, msg, qtype)
	if err != nil {
		return false
	}
	if cname != name {
		return false
	}
	if len(addrs) == 0 {
		log.Debug("no such host recved")
		return true
	}
	// all dns type A?
	ips := convertRR_A(addrs)
	if cfg.CheckBlack(ips) {
		log.Debug("fake dns resolv hited.")
		return true
	}
	return false
}

// Send a request on the connection and hope for a reply.
// Up to cfg.attempts attempts.
func exchange(cfg *dnsConfig, c net.Conn, name string, qtype uint16) (*dnsMsg, error) {
	_, useTCP := c.(*net.TCPConn)
	if len(name) >= 256 {
		return nil, &DNSError{Err: "name too long", Name: name}
	}
	out := new(dnsMsg)
	out.id = uint16(rand.Int()) ^ uint16(time.Now().UnixNano())
	out.question = []dnsQuestion{
		{name, qtype, dnsClassINET},
	}
	out.recursion_desired = true
	msg, ok := out.Pack()
	if !ok {
		return nil, &DNSError{Err: "internal error - cannot pack message", Name: name}
	}
	if useTCP {
		mlen := uint16(len(msg))
		msg = append([]byte{byte(mlen >> 8), byte(mlen)}, msg...)
	}
	var server string
	if a := c.RemoteAddr(); a != nil {
		server = a.String()
	}
	for attempt := 0; attempt < cfg.attempts; attempt++ {
		n, err := c.Write(msg)
		if err != nil {
			return nil, err
		}

		if cfg.timeout == 0 {
			c.SetReadDeadline(noDeadline)
		} else {
			c.SetReadDeadline(time.Now().Add(time.Duration(cfg.timeout) * time.Second))
		}

	Reread:
		buf := make([]byte, 2000)
		if useTCP {
			n, err = io.ReadFull(c, buf[:2])
			if err != nil {
				if e, ok := err.(net.Error); ok && e.Timeout() {
					continue
				}
			}
			mlen := int(buf[0])<<8 | int(buf[1])
			if mlen > len(buf) {
				buf = make([]byte, mlen)
			}
			n, err = io.ReadFull(c, buf[:mlen])
		} else {
			n, err = c.Read(buf)
		}
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			return nil, err
		}
		buf = buf[:n]
		in := new(dnsMsg)
		if !in.Unpack(buf) || in.id != out.id {
			continue
		}

		if check_black(name, server, in, qtype) {
			goto Reread
		}
		return in, nil
	}
	return nil, &DNSError{Err: "no answer from server", Name: name, Server: server, IsTimeout: true}
}

// Do a lookup for a single name, which must be rooted
// (otherwise answer will not find the answers).
func tryOneName(cfg *dnsConfig, name string, qtype uint16) (cname string, addrs []dnsRR, err error) {
	if len(cfg.servers) == 0 {
		return "", nil, &DNSError{Err: "no DNS servers", Name: name}
	}
	for i := 0; i < len(cfg.servers); i++ {
		// Calling Dial here is scary -- we have to be sure
		// not to dial a name that will require a DNS lookup,
		// or Dial will call back here to translate it.
		// The DNS config parser has already checked that
		// all the cfg.servers[i] are IP addresses, which
		// Dial will use without a DNS lookup.
		server := cfg.servers[i] + ":53"
		c, cerr := net.Dial("udp", server)
		if cerr != nil {
			err = cerr
			continue
		}
		msg, merr := exchange(cfg, c, name, qtype)
		c.Close()
		if merr != nil {
			err = merr
			continue
		}
		if msg.truncated { // see RFC 5966
			c, cerr = net.Dial("tcp", server)
			if cerr != nil {
				err = cerr
				continue
			}
			msg, merr = exchange(cfg, c, name, qtype)
			c.Close()
			if merr != nil {
				err = merr
				continue
			}
		}
		cname, addrs, err = answer(name, server, msg, qtype)
		if err == nil || err.(*DNSError).Err == noSuchHost {
			break
		}
	}
	return
}

func convertRR_A(records []dnsRR) []net.IP {
	addrs := make([]net.IP, len(records))
	for i, rr := range records {
		a := rr.(*dnsRR_A).A
		addrs[i] = net.IPv4(byte(a>>24), byte(a>>16), byte(a>>8), byte(a))
	}
	return addrs
}

func convertRR_AAAA(records []dnsRR) []net.IP {
	addrs := make([]net.IP, len(records))
	for i, rr := range records {
		a := make(net.IP, net.IPv6len)
		copy(a, rr.(*dnsRR_AAAA).AAAA[:])
		addrs[i] = a
	}
	return addrs
}

var cfg *dnsConfig
var dnserr error

func loadConfig() { cfg, dnserr = dnsReadConfig("/etc/goproxy/resolv.conf") }

var onceLoadConfig sync.Once

func lookup(name string, qtype uint16) (cname string, addrs []dnsRR, err error) {
	if !isDomainName(name) {
		return name, nil, &DNSError{Err: "invalid domain name", Name: name}
	}
	onceLoadConfig.Do(loadConfig)
	if dnserr != nil || cfg == nil {
		err = dnserr
		return
	}
	// If name is rooted (trailing dot) or has enough dots,
	// try it by itself first.
	rooted := len(name) > 0 && name[len(name)-1] == '.'
	if rooted || count(name, '.') >= cfg.ndots {
		rname := name
		if !rooted {
			rname += "."
		}
		// Can try as ordinary name.
		cname, addrs, err = tryOneName(cfg, rname, qtype)
		if err == nil {
			return
		}
	}
	if rooted {
		return
	}

	// Otherwise, try suffixes.
	for i := 0; i < len(cfg.search); i++ {
		rname := name + "." + cfg.search[i]
		if rname[len(rname)-1] != '.' {
			rname += "."
		}
		cname, addrs, err = tryOneName(cfg, rname, qtype)
		if err == nil {
			return
		}
	}

	// Last ditch effort: try unsuffixed.
	rname := name
	if !rooted {
		rname += "."
	}
	cname, addrs, err = tryOneName(cfg, rname, qtype)
	if err == nil {
		return
	}
	if e, ok := err.(*DNSError); ok {
		// Show original name passed to lookup, not suffixed one.
		// In general we might have tried many suffixes; showing
		// just one is misleading. See also golang.org/issue/6324.
		e.Name = name
	}
	return
}

// goLookupIP is the native Go implementation of LookupIP.
// Used only if cgoLookupIP refuses to handle the request
// (that is, only if cgoLookupIP is the stub in cgo_stub.go).
// Normally we let cgo use the C library resolver instead of
// depending on our lookup code, so that Go and C get the same
// answers.
func goLookupIP(name string) (addrs []net.IP, err error) {
	onceLoadConfig.Do(loadConfig)
	if dnserr != nil || cfg == nil {
		err = dnserr
		return
	}
	var records []dnsRR
	var cname string
	var err4, err6 error
	cname, records, err4 = lookup(name, dnsTypeA)
	addrs = convertRR_A(records)
	if cname != "" {
		name = cname
	}
	_, records, err6 = lookup(name, dnsTypeAAAA)
	if err4 != nil && err6 == nil {
		// Ignore A error because AAAA lookup succeeded.
		err4 = nil
	}
	if err6 != nil && len(addrs) > 0 {
		// Ignore AAAA error because A lookup succeeded.
		err6 = nil
	}
	if err4 != nil {
		return nil, err4
	}
	if err6 != nil {
		return nil, err6
	}

	addrs = append(addrs, convertRR_AAAA(records)...)
	return addrs, nil
}
