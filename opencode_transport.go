package main

// Raw OpenCode transport.
//
// The free tier is guarded by a Cloudflare check that only serves the real
// OpenCode CLI. Two things have to match, and neither is optional:
//
//  1. The TLS ClientHello. Verified by capture: the CLI sends a BoringSSL hello
//     with a fixed cipher order, status_request + SCT extensions, ALPN
//     http/1.1 only and no GREASE. curl/OpenSSL, Go's crypto/tls, every stock
//     uTLS browser preset and even real Chrome are all answered with
//     `429 FreeUsageLimitError`; the spec below is a byte-structural clone of
//     the CLI hello (only the random session id and key shares differ) and is
//     served 200.
//  2. The HTTP header order (JA4H). Go's net/http (and Bun's fetch) sort
//     headers alphabetically, so the request line is written by hand in the
//     CLI's order over the uTLS connection instead of using http.Client.
//
// Session ids are shaped by genSessionIDLocked (`ses_` + descending-time hex
// + 14 base62) and request ids by genRequestID (`msg_` + ascending-time hex +
// 14 base62).

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

// opencodeHeaderOrder is the exact header order the CLI emits.
var opencodeHeaderOrder = []string{
	"Authorization",
	"Content-Type",
	"User-Agent",
	"x-opencode-client",
	"x-opencode-project",
	"x-opencode-request",
	"x-opencode-session",
	"x-opencode-session-id",
	"Connection",
	"Accept",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

// cliClientHelloSpec clones the CLI ClientHello byte-structurally.
func cliClientHelloSpec(host string) *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
			0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
		},
		CompressionMethods: []uint8{0},
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{ServerName: host},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.CurveID(0x11ec), utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.CurveID(0x11ec)},
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
		},
	}
}

// dialCLITLS opens a TCP (optionally CONNECT-tunnelled) connection and performs
// the CLI handshake on it.
func dialCLITLS(ctx context.Context, addr string, proxy *url.URL, insecureSkipVerify bool) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	if proxy != nil {
		conn, err = d.DialContext(ctx, "tcp", proxy.Host)
		if err != nil {
			return nil, err
		}
		var req strings.Builder
		req.WriteString("CONNECT ")
		req.WriteString(addr)
		req.WriteString(" HTTP/1.1\r\nHost: ")
		req.WriteString(addr)
		req.WriteString("\r\n")
		if proxy.User != nil {
			creds := base64.StdEncoding.EncodeToString([]byte(proxy.User.String()))
			req.WriteString("Proxy-Authorization: Basic ")
			req.WriteString(creds)
			req.WriteString("\r\n")
		}
		req.WriteString("\r\n")
		if _, err = conn.Write([]byte(req.String())); err != nil {
			conn.Close()
			return nil, err
		}
		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if !strings.Contains(line, " 200") {
			conn.Close()
			return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(line))
		}
		for {
			l, err := br.ReadString('\n')
			if err != nil {
				conn.Close()
				return nil, err
			}
			if l == "\r\n" || l == "\n" {
				break
			}
		}
		if br.Buffered() > 0 {
			buffered, _ := br.Peek(br.Buffered())
			conn = &prefixConn{Conn: conn, prefix: append([]byte(nil), buffered...)}
		}
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &utls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: insecureSkipVerify,
	}
	uconn := utls.UClient(conn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(cliClientHelloSpec(host)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("utls preset: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return uconn, nil
}

// prefixConn replays bytes a bufio.Reader consumed while parsing the CONNECT reply.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// openCodeRawDo performs a POST with a fixed header order and returns the
// response with a streaming body (chunked-encoded and compressed replies are decoded).
func openCodeRawDo(ctx context.Context, targetURL string, hdr http.Header, body []byte, proxy *url.URL, headerTimeout time.Duration) (*upstreamResponse, error) {
	return openCodeRawDoWithConfig(ctx, targetURL, hdr, body, proxy, headerTimeout, false)
}

func headerValue(hdr http.Header, name string) string {
	if v := hdr.Get(name); v != "" {
		return v
	}
	if vals := hdr[name]; len(vals) > 0 && vals[0] != "" {
		return vals[0]
	}
	for k, vals := range hdr {
		if strings.EqualFold(k, name) && len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return ""
}

func openCodeRawDoWithConfig(ctx context.Context, targetURL string, hdr http.Header, body []byte, proxy *url.URL, headerTimeout time.Duration, insecureSkipVerify bool) (*upstreamResponse, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "443")
	}

	conn, err := dialCLITLS(ctx, addr, proxy, insecureSkipVerify)
	if err != nil {
		return nil, err
	}

	var sb strings.Builder
	sb.WriteString("POST ")
	sb.WriteString(u.RequestURI())
	sb.WriteString(" HTTP/1.1\r\n")
	for _, name := range opencodeHeaderOrder {
		var val string
		switch {
		case strings.EqualFold(name, "Content-Length"):
			val = strconv.Itoa(len(body))
		case strings.EqualFold(name, "Host"):
			val = headerValue(hdr, "Host")
			if val == "" {
				val = u.Host
			}
		case strings.EqualFold(name, "Connection"):
			val = headerValue(hdr, "Connection")
			if val == "" {
				val = "keep-alive"
			}
		default:
			val = headerValue(hdr, name)
		}
		if val != "" {
			sb.WriteString(name)
			sb.WriteString(": ")
			sb.WriteString(val)
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\r\n")

	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			conn.Close()
			return nil, err
		}
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	if headerTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(headerTimeout))
	}
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		conn.Close()
		return nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bad status %q", statusLine)
	}
	respHeader := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.Index(line, ":"); i > 0 {
			respHeader.Add(strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]))
		}
	}
	_ = conn.SetReadDeadline(time.Time{})

	var reader io.Reader = br
	if strings.Contains(strings.ToLower(respHeader.Get("Transfer-Encoding")), "chunked") {
		reader = newChunkedReader(br)
	} else if cl := respHeader.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			reader = io.LimitReader(br, n)
		}
	}

	decodedBody, err := wrapDecodedBody(reader, respHeader.Get("Content-Encoding"), conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return &upstreamResponse{
		status: status,
		header: respHeader,
		body:   decodedBody,
	}, nil
}

func wrapDecodedBody(r io.Reader, contentEncoding string, conn net.Conn) (io.ReadCloser, error) {
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch enc {
	case "", "identity":
		return &connReadCloser{Reader: r, conn: conn}, nil
	case "gzip":
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		return &connReadCloser{Reader: gr, extraClose: gr, conn: conn}, nil
	case "deflate":
		fr := flate.NewReader(r)
		return &connReadCloser{Reader: fr, extraClose: fr, conn: conn}, nil
	case "br":
		return &connReadCloser{Reader: brotli.NewReader(r), conn: conn}, nil
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("zstd reader: %w", err)
		}
		return &connReadCloser{Reader: zr, zstdClose: zr, conn: conn}, nil
	default:
		return &connReadCloser{Reader: r, conn: conn}, nil
	}
}

type connReadCloser struct {
	io.Reader
	extraClose io.Closer
	zstdClose  *zstd.Decoder
	conn       net.Conn
}

func (c *connReadCloser) Close() error {
	if c.zstdClose != nil {
		c.zstdClose.Close()
	}
	if c.extraClose != nil {
		_ = c.extraClose.Close()
	}
	return c.conn.Close()
}

// chunkedReader de-chunks an HTTP/1.1 chunked body.
type chunkedReader struct {
	br    *bufio.Reader
	left  int64
	err   error
	first bool
}

func newChunkedReader(br *bufio.Reader) *chunkedReader { return &chunkedReader{br: br, first: true} }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	for c.left == 0 {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.err = err
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" && c.first {
			continue
		}
		c.first = false
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			c.err = err
			return 0, err
		}
		if n == 0 {
			c.err = io.EOF
			return 0, io.EOF
		}
		c.left = n
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.br.Read(p)
	c.left -= int64(n)
	if c.left == 0 {
		// consume trailing CRLF
		_, _ = c.br.ReadString('\n')
	}
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}
