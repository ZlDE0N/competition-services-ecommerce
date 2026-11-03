package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SSRF protection for every fetch whose URL comes from outside the service
// (search results, user-supplied image/PDF/DOCX links).
//
// The check runs inside the dialer, on the IP the name actually resolved to, so
// it also covers redirects and DNS-rebinding tricks that a pre-flight check of
// the host name would miss.

var errBlockedAddress = errors.New("destination address is not allowed")

// blockedNets lists ranges a public fetch must never reach.
var blockedNets = mustParseCIDRs(
	"0.0.0.0/8",      // "this" network
	"10.0.0.0/8",     // private
	"100.64.0.0/10",  // carrier-grade NAT
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local, includes cloud metadata 169.254.169.254
	"172.16.0.0/12",  // private
	"192.0.0.0/24",   // IETF protocol assignments
	"192.168.0.0/16", // private
	"198.18.0.0/15",  // benchmarking
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved
	"::/128",         // unspecified
	"::1/128",        // loopback
	"64:ff9b::/96",   // NAT64
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("bad CIDR %q: %v", c, err))
		}
		out = append(out, n)
	}
	return out
}

// isBlockedIP reports whether ip is in a range the service must not call.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil { // also unwraps IPv4-mapped IPv6 (::ffff:a.b.c.d)
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validatePublicURL checks scheme and host before any request is built.
func validatePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("URL must not carry credentials")
	}
	return u, nil
}

// safeDialContext resolves the host and refuses blocked addresses.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip.IP) {
			return nil, fmt.Errorf("%w: %s", errBlockedAddress, host)
		}
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// newSafeHTTPClient returns a client that cannot reach internal addresses and
// follows at most three redirects.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil, // a proxy would hide the real destination from the dialer
			DialContext:           safeDialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          20,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("stopped after 3 redirects")
			}
			if _, err := validatePublicURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}

// safeGet validates the URL and fetches it with the safe client.
func safeGet(client *http.Client, raw string) (*http.Response, error) {
	u, err := validatePublicURL(raw)
	if err != nil {
		return nil, err
	}
	return client.Get(u.String())
}

// Limits for downloaded content (applied while reading, not after).
const (
	maxImageDownloadBytes = 10 * 1024 * 1024
	maxDocDownloadBytes   = 30 * 1024 * 1024
	maxImagePixels        = 40_000_000 // 40 megapixels: guards against decompression bombs
)

var errTooLarge = errors.New("response exceeds the size limit")

// readLimited reads at most max bytes and fails if the body is larger.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errTooLarge
	}
	return data, nil
}
