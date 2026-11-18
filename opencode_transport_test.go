//go:build cgo

package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost", "opencode.ai"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
}

func TestOpenCodeRawDoHeaderOrderAndEncodings(t *testing.T) {
	cert := selfSignedCert(t)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"},
	}

	cases := []struct {
		name     string
		encoding string
		chunked  bool
		compress func([]byte) []byte
	}{
		{name: "plain", encoding: "", chunked: false, compress: func(b []byte) []byte { return b }},
		{name: "chunked_plain", encoding: "", chunked: true, compress: func(b []byte) []byte { return b }},
		{
			name:     "gzip",
			encoding: "gzip",
			chunked:  true,
			compress: func(b []byte) []byte {
				var buf bytes.Buffer
				w := gzip.NewWriter(&buf)
				_, _ = w.Write(b)
				_ = w.Close()
				return buf.Bytes()
			},
		},
		{
			name:     "deflate",
			encoding: "deflate",
			chunked:  false,
			compress: func(b []byte) []byte {
				var buf bytes.Buffer
				w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
				_, _ = w.Write(b)
				_ = w.Close()
				return buf.Bytes()
			},
		},
		{
			name:     "br",
			encoding: "br",
			chunked:  true,
			compress: func(b []byte) []byte {
				var buf bytes.Buffer
				w := brotli.NewWriter(&buf)
				_, _ = w.Write(b)
				_ = w.Close()
				return buf.Bytes()
			},
		},
		{
			name:     "zstd",
			encoding: "zstd",
			chunked:  false,
			compress: func(b []byte) []byte {
				var buf bytes.Buffer
				w, _ := zstd.NewWriter(&buf)
				_, _ = w.Write(b)
				_ = w.Close()
				return buf.Bytes()
			},
		},
	}

	const wantReply = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
			if err != nil {
				t.Fatalf("tls listen: %v", err)
			}
			defer ln.Close()

			gotOrderCh := make(chan []string, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				br := bufio.NewReader(conn)
				reqLine, _ := br.ReadString('\n')
				if !strings.HasPrefix(reqLine, "POST /inference/openai/v1/chat/completions HTTP/1.1") {
					return
				}
				var order []string
				cl := 0
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" || line == "\n" {
						break
					}
					line = strings.TrimRight(line, "\r\n")
					if idx := strings.IndexByte(line, ':'); idx > 0 {
						name := line[:idx]
						order = append(order, name)
						if strings.EqualFold(name, "Content-Length") {
							cl, _ = strconv.Atoi(strings.TrimSpace(line[idx+1:]))
						}
					}
				}
				if cl > 0 {
					_, _ = io.CopyN(io.Discard, br, int64(cl))
				}
				gotOrderCh <- order

				payload := tc.compress([]byte(wantReply))
				var resp strings.Builder
				resp.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n")
				if tc.encoding != "" {
					resp.WriteString("Content-Encoding: " + tc.encoding + "\r\n")
				}
				if tc.chunked {
					resp.WriteString("Transfer-Encoding: chunked\r\n\r\n")
					_, _ = conn.Write([]byte(resp.String()))
					mid := len(payload) / 2
					for _, part := range [][]byte{payload[:mid], payload[mid:]} {
						if len(part) == 0 {
							continue
						}
						_, _ = fmt.Fprintf(conn, "%x\r\n", len(part))
						_, _ = conn.Write(part)
						_, _ = conn.Write([]byte("\r\n"))
					}
					_, _ = conn.Write([]byte("0\r\n\r\n"))
				} else {
					resp.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload)))
					_, _ = conn.Write([]byte(resp.String()))
					_, _ = conn.Write(payload)
				}
			}()

			hdr := http.Header{
				"Authorization":         []string{"Bearer public"},
				"Content-Type":          []string{"application/json"},
				"User-Agent":            []string{opencodeClientUA},
				"x-opencode-client":     []string{"cli"},
				"x-opencode-project":    []string{"global"},
				"x-opencode-request":    []string{genRequestID()},
				"x-opencode-session":    []string{genSessionID()},
				"x-opencode-session-id": []string{genSessionID()},
				"Connection":            []string{"keep-alive"},
				"Accept":                []string{"*/*"},
				"Host":                  []string{"opencode.ai"},
				"Accept-Encoding":       []string{"gzip, deflate, br, zstd"},
			}
			targetURL := "https://" + ln.Addr().String() + "/inference/openai/v1/chat/completions"
			res, err := openCodeRawDoWithConfig(context.Background(), targetURL, hdr, []byte(`{"model":"big-pickle"}`), nil, 5*time.Second, true)
			if err != nil {
				t.Fatalf("openCodeRawDoWithConfig failed: %v", err)
			}
			defer res.body.Close()

			body, err := io.ReadAll(res.body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != wantReply {
				t.Fatalf("decoded body mismatch:\ngot:  %q\nwant: %q", string(body), wantReply)
			}

			gotOrder := <-gotOrderCh
			if len(gotOrder) != len(opencodeHeaderOrder) {
				t.Fatalf("header count=%d want %d (%v)", len(gotOrder), len(opencodeHeaderOrder), gotOrder)
			}
			for i, want := range opencodeHeaderOrder {
				if gotOrder[i] != want {
					t.Errorf("header[%d] = %q, want %q (all=%v)", i, gotOrder[i], want, gotOrder)
				}
			}
		})
	}
}

func TestOpenCodeRawDoProxyConnectAuth(t *testing.T) {
	cert := selfSignedCert(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	upstreamLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstreamLn.Close()

	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" || l == "\n" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()

	authGot := make(chan string, 1)
	go func() {
		c, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n') // CONNECT
		var gotAuth string
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" || l == "\n" {
				break
			}
			if strings.HasPrefix(strings.ToLower(l), "proxy-authorization:") {
				gotAuth = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
			}
		}
		authGot <- gotAuth
		up, err := net.Dial("tcp", upstreamLn.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go io.Copy(up, br)
		io.Copy(c, up)
	}()

	proxyURL, _ := url.Parse("http://alice:s3cr3t@" + proxyLn.Addr().String())
	hdr := http.Header{
		"Authorization": []string{"Bearer public"},
		"Content-Type":  []string{"application/json"},
		"User-Agent":    []string{opencodeClientUA},
		"Accept":        []string{"*/*"},
	}
	res, err := openCodeRawDoWithConfig(context.Background(), "https://"+upstreamLn.Addr().String()+"/inference/openai/v1/chat/completions", hdr, []byte(`{}`), proxyURL, 5*time.Second, true)
	if err != nil {
		t.Fatalf("openCodeRawDo via proxy failed: %v", err)
	}
	defer res.body.Close()
	b, _ := io.ReadAll(res.body)
	if string(b) != "ok" {
		t.Fatalf("got body %q, want ok", string(b))
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	if got := <-authGot; got != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", got, wantAuth)
	}
}

func TestOpenCodeIDDirectionality(t *testing.T) {
	ses := genSessionID()
	req := genRequestID()
	if !strings.HasPrefix(ses, "ses_") || len(ses) != 4+12+14 {
		t.Fatalf("bad session id: %q", ses)
	}
	if !strings.HasPrefix(req, "msg_") || len(req) != 4+12+14 {
		t.Fatalf("bad request id: %q", req)
	}
	// ses_ uses descending time (~now*4096), msg_ uses ascending time (now*4096)
	sesHex, err1 := strconv.ParseUint(ses[4:16], 16, 64)
	reqHex, err2 := strconv.ParseUint(req[4:16], 16, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("non-hex id prefix: %v %v", err1, err2)
	}
	// Complementary prefixes should sum close to 0xFFFFFFFFFFFF (mod 2^48).
	sum := (sesHex + reqHex) & sessionPrefixMask
	dist := (sessionPrefixMask - sum) & sessionPrefixMask
	if dist > 0x100000 && sum > 0x100000 {
		t.Fatalf("ses (%012x) and msg (%012x) prefixes are not descending/ascending complements (sum=%012x)", sesHex, reqHex, sum)
	}
}

func TestCLIClientHelloSpecHasNoGREASE(t *testing.T) {
	spec := cliClientHelloSpec("opencode.ai")
	if len(spec.CipherSuites) != 17 {
		t.Fatalf("expected 17 cipher suites, got %d", len(spec.CipherSuites))
	}
	for _, cs := range spec.CipherSuites {
		if (cs & 0x0f0f) == 0x0a0a {
			t.Fatalf("CLI ClientHello must not include GREASE cipher 0x%04x", cs)
		}
	}
}
