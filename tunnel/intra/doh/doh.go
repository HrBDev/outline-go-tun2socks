// Copyright 2019 The Outline Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package doh

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"time"

	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/dnsx"
	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/doh/ipmap"
	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/split"
	"github.com/Jigsaw-Code/outline-go-tun2socks/tunnel/intra/xdns"
	"github.com/eycorsican/go-tun2socks/common/log"
)

const (
	// Complete : Transaction completed successfully
	Complete = iota
	// SendFailed : Failed to send query
	SendFailed
	// HTTPError : Got a non-200 HTTP status
	HTTPError
	// BadQuery : Malformed input
	BadQuery
	// BadResponse : Response was invalid
	BadResponse
	// InternalError : This should never happen
	InternalError
)

// Summary is a summary of a DNS transaction, reported when it is complete.
type Summary struct {
	Latency    float64 // Response (or failure) latency in seconds
	Query      []byte
	Response   []byte
	Server     string
	Status     int
	HTTPStatus int    // Zero unless Status is Complete or HTTPError
	Blocklists string // csv separated list of blocklists names, if any.
}

// A Token is an opaque handle used to match responses to queries.
type Token interface{}

// Listener receives Summaries.
type Listener interface {
	OnQuery(url string) Token
	OnResponse(Token, *Summary)
}

// Transport represents a DNS query transport.  This interface is exported by gobind,
// so it has to be very simple.
type Transport interface {
	// Given a DNS query (including ID), returns a DNS response with matching
	// ID, or an error if no response was received.
	Query(q []byte) ([]byte, error)
	// Return the server URL used to initialize this transport.
	GetURL() string
	// SetBraveDNS sets bravedns variable
	SetBraveDNS(dnsx.BraveDNS)
}

// TODO: Keep a context here so that queries can be canceled.
type transport struct {
	Transport
	url      string
	hostname string
	port     int
	ips      ipmap.IPMap
	client   http.Client
	dialer   *net.Dialer
	listener Listener
	bravedns dnsx.BraveDNS
}

// Wait up to three seconds for the TCP handshake to complete.
const tcpTimeout time.Duration = 3 * time.Second

func (t *transport) dial(network, addr string) (net.Conn, error) {
	log.Debugf("Dialing %s", addr)
	domain, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	tcpaddr := func(ip net.IP) *net.TCPAddr {
		return &net.TCPAddr{IP: ip, Port: port}
	}

	// TODO: Improve IP fallback strategy with parallelism and Happy Eyeballs.
	var conn net.Conn
	ips := t.ips.Get(domain)
	confirmed := ips.Confirmed()
	if confirmed != nil {
		log.Debugf("Trying confirmed IP %s for addr %s", confirmed.String(), addr)
		if conn, err = split.DialWithSplitRetry(t.dialer, tcpaddr(confirmed), nil); err == nil {
			log.Infof("Confirmed IP %s worked", confirmed.String())
			return conn, nil
		}
		log.Debugf("Confirmed IP %s failed with err %v", confirmed.String(), err)
		ips.Disconfirm(confirmed)
	}

	log.Debugf("Trying all IPs")
	for _, ip := range ips.GetAll() {
		if ip.Equal(confirmed) {
			// Don't try this IP twice.
			continue
		}
		if conn, err = split.DialWithSplitRetry(t.dialer, tcpaddr(ip), nil); err == nil {
			log.Infof("Found working IP: %s", ip.String())
			return conn, nil
		}
	}
	return nil, err
}

// NewTransport returns a DoH DNSTransport, ready for use.
// This is a POST-only DoH implementation, so the DoH template should be a URL.
// `rawurl` is the DoH template in string form.
// `addrs` is a list of domains or IP addresses to use as fallback, if the hostname
//   lookup fails or returns non-working addresses.
// `dialer` is the dialer that the transport will use.  The transport will modify the dialer's
//   timeout but will not mutate it otherwise.
// `listener` will receive the status of each DNS query when it is complete.
func NewTransport(rawurl string, addrs []string, dialer *net.Dialer,
	listener Listener) (Transport, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	parsedurl, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	if parsedurl.Scheme != "https" {
		return nil, fmt.Errorf("Bad scheme: %s", parsedurl.Scheme)
	}
	// Resolve the hostname and put those addresses first.
	portStr := parsedurl.Port()
	var port int
	if len(portStr) > 0 {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
	} else {
		port = 443
	}

	t := &transport{
		url:      rawurl,
		hostname: parsedurl.Hostname(),
		port:     port,
		listener: listener,
		dialer:   dialer,
		ips:      ipmap.NewIPMap(dialer.Resolver),
	}
	ips := t.ips.Get(t.hostname)
	for _, addr := range addrs {
		ips.Add(addr)
	}
	if ips.Empty() {
		return nil, fmt.Errorf("No IP addresses for %s", t.hostname)
	}

	// Override the dial function.
	t.client.Transport = &http.Transport{
		Dial:                  t.dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second, // Same value as Android DNS-over-TLS
	}
	return t, nil
}

type queryError struct {
	status int
	err    error
}

func (e *queryError) Error() string {
	return e.err.Error()
}

func (e *queryError) Unwrap() error {
	return e.err
}

type httpError struct {
	status int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP request failed: %d", e.status)
}

// Given a raw DNS query (including the query ID), this function sends the
// query.  If the query is successful, it returns the response and a nil qerr.  Otherwise,
// it returns a nil response and a qerr with a status value indicating the cause.
// Independent of the query's success or failure, this function also returns the
// address of the server on a best-effort basis, or nil if the address could not
// be determined.
func (t *transport) doQuery(q []byte) (response []byte, blocklists string, server *net.TCPAddr, elapsed time.Duration, qerr error) {
	if len(q) < 2 {
		qerr = &queryError{BadQuery, fmt.Errorf("Query length is %d", len(q))}
		return
	}

	start := time.Now()
	if err := t.prepareOnDeviceBlock(); err == nil {
		response, blocklists, err = t.applyBlocklists(q)
		elapsed = time.Since(start)
		if err == nil { // blocklist applied only when err is nil
			return
		}
		log.Infof("skipping local block for %s with err %s", blocklists, err)
		// skipping block because err
	} else {
		log.Debugf("forward query: no local block")
	}

	// Add padding to the raw query
	q, err := AddEdnsPadding(q)
	if err != nil {
		elapsed = time.Since(start)
		qerr = &queryError{InternalError, err}
		return
	}

	// Zero out the query ID.
	id := binary.BigEndian.Uint16(q)
	binary.BigEndian.PutUint16(q, 0)
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewBuffer(q))
	if err != nil {
		elapsed = time.Since(start)
		qerr = &queryError{InternalError, err}
		return
	}

	hostname := t.hostname

	// The connection used for this request.  If the request fails, we will close
	// this socket, in case it is no longer functioning.
	var conn net.Conn

	// Error cleanup function.  If the query fails, this function will close the
	// underlying socket and disconfirm the server IP.  Empirically, sockets often
	// become unresponsive after a network change, causing timeouts on all requests.
	defer func() {
		if qerr == nil {
			return
		}
		log.Infof("%d Query failed: %v", id, qerr)
		if server != nil {
			log.Debugf("%d Disconfirming %s", id, server.IP.String())
			t.ips.Get(hostname).Disconfirm(server.IP)
		}
		if conn != nil {
			log.Infof("%d Closing failing DoH socket", id)
			conn.Close()
		}
	}()

	// Add a trace to the request in order to expose the server's IP address.
	// Only GotConn performs any action; the other methods just provide debug logs.
	// GotConn runs before client.Do() returns, so there is no data race when
	// reading the variables it has set.
	trace := httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			log.Debugf("%d GetConn(%s)", id, hostPort)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			log.Debugf("%d GotConn(%v)", id, info)
			if info.Conn == nil {
				return
			}
			conn = info.Conn
			// info.Conn is a DuplexConn, so RemoteAddr is actually a TCPAddr.
			server = conn.RemoteAddr().(*net.TCPAddr)
		},
		PutIdleConn: func(err error) {
			log.Debugf("%d PutIdleConn(%v)", id, err)
		},
		GotFirstResponseByte: func() {
			log.Debugf("%d GotFirstResponseByte()", id)
		},
		Got100Continue: func() {
			log.Debugf("%d Got100Continue()", id)
		},
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			log.Debugf("%d Got1xxResponse(%d, %v)", id, code, header)
			return nil
		},
		DNSStart: func(info httptrace.DNSStartInfo) {
			log.Debugf("%d DNSStart(%v)", id, info)
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			log.Debugf("%d, DNSDone(%v)", id, info)
		},
		ConnectStart: func(network, addr string) {
			log.Debugf("%d ConnectStart(%s, %s)", id, network, addr)
		},
		ConnectDone: func(network, addr string, err error) {
			log.Debugf("%d ConnectDone(%s, %s, %v)", id, network, addr, err)
		},
		TLSHandshakeStart: func() {
			log.Debugf("%d TLSHandshakeStart()", id)
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			log.Debugf("%d TLSHandshakeDone(%v, %v)", id, state, err)
		},
		WroteHeaders: func() {
			log.Debugf("%d WroteHeaders()", id)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			log.Debugf("%d WroteRequest(%v)", id, info)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &trace))

	const mimetype = "application/dns-message"
	req.Header.Set("Content-Type", mimetype)
	req.Header.Set("Accept", mimetype)
	req.Header.Set("User-Agent", "Intra")
	log.Debugf("%d Sending query", id)
	start = time.Now() // re...start
	httpResponse, err := t.client.Do(req)
	if err != nil {
		elapsed = time.Since(start)
		qerr = &queryError{SendFailed, err}
		return
	}
	log.Debugf("%d Got response", id)

	response, err = ioutil.ReadAll(httpResponse.Body)

	elapsed = time.Since(start)

	if err != nil {
		qerr = &queryError{BadResponse, err}
		return
	}
	httpResponse.Body.Close()
	log.Debugf("%d Closed response", id)

	// Update the hostname, which could have changed due to a redirect.
	hostname = httpResponse.Request.URL.Hostname()

	if httpResponse.StatusCode != http.StatusOK {
		reqBuf := new(bytes.Buffer)
		req.Write(reqBuf)
		respBuf := new(bytes.Buffer)
		httpResponse.Write(respBuf)
		log.Debugf("%d request: %s\nresponse: %s", id, reqBuf.String(), respBuf.String())

		qerr = &queryError{HTTPError, &httpError{httpResponse.StatusCode}}
		return
	}

	// Restore the query ID.
	binary.BigEndian.PutUint16(q, id)
	if len(response) >= 2 {
		binary.BigEndian.PutUint16(response, id)
		var r []byte
		blocklists, r = t.resolveBlock(q, httpResponse, response)
		// overwrite response when blocked
		if len(blocklists) > 0 && r != nil {
			response = r
		}
	} else {
		qerr = &queryError{BadResponse, fmt.Errorf("Response length is %d", len(response))}
		return
	}
	if server != nil {
		// Record a working IP address for this server
		t.ips.Get(hostname).Confirm(server.IP)
	}
	return
}

func (t *transport) Query(q []byte) ([]byte, error) {
	var token Token
	if t.listener != nil {
		token = t.listener.OnQuery(t.url)
	}

	response, blocklists, server, elapsed, err := t.doQuery(q)

	if t.listener != nil {
		latency := elapsed
		status := Complete
		httpStatus := http.StatusOK
		var qerr *queryError
		if errors.As(err, &qerr) {
			status = qerr.status
			httpStatus = 0

			var herr *httpError
			if errors.As(qerr.err, &herr) {
				httpStatus = herr.status
			}
		}

		var ip string
		if server != nil {
			ip = server.IP.String()
		}

		t.listener.OnResponse(token, &Summary{
			Latency:    latency.Seconds(),
			Query:      q,
			Response:   response,
			Server:     ip,
			Status:     status,
			HTTPStatus: httpStatus,
			Blocklists: blocklists,
		})
	}
	return response, err
}

func (t *transport) GetURL() string {
	return t.url
}

func (t *transport) SetBraveDNS(b dnsx.BraveDNS) {
	t.bravedns = b
}

func (t *transport) prepareOnDeviceBlock() error {
	b := t.bravedns
	u := t.url

	if b == nil || len(u) <= 0 {
		return errors.New("t.url or dnsx.bravedns nil")
	}

	if !b.OnDeviceBlock() {
		return errors.New("on device block not set")
	}

	return nil
}

func (t *transport) applyBlocklists(q []byte) (response []byte, blocklists string, err error) {
	bravedns := t.bravedns
	if bravedns == nil {
		errors.New("bravedns is nil")
		return
	}
	blocklists, err = bravedns.BlockRequest(q)
	if err != nil {
		return
	}
	if len(blocklists) <= 0 {
		err = errors.New("no blocklist applies")
		return
	}

	ans, err := xdns.BlockResponseFromMessage(q)
	if err != nil {
		return
	}

	response, err = ans.Pack()
	return
}

func (t *transport) resolveBlock(q []byte, res *http.Response, ans []byte) (blocklistNames string, blockedResponse []byte) {
	bravedns := t.bravedns
	if bravedns == nil {
		return
	}

	var err error
	blocklistNames = t.blocklistsFromHeader(bravedns, res)
	if len(blocklistNames) > 0 || bravedns.OnDeviceBlock() == false {
		return
	}

	if blocklistNames, err = bravedns.BlockResponse(ans); err != nil {
		log.Debugf("response not blocked %v", err)
		return
	}

	if len(blocklistNames) <= 0 {
		log.Debugf("query not blocked blocklist empty")
		return
	}

	msg, err := xdns.BlockResponseFromMessage(q)
	if err != nil {
		log.Warnf("could not pack blocked dns ans %v", err)
		return
	}

	blockedResponse, err = msg.Pack()
	return
}

func (t *transport) blocklistsFromHeader(bravedns dnsx.BraveDNS, res *http.Response) (blocklistNames string) {
	blocklistStamp := res.Header.Get(bravedns.GetBlocklistStampHeaderKey())
	log.Debugf("header", res.Header)
	log.Debugf("st", blocklistStamp)
	if len(blocklistStamp) <= 0 {
		return
	}
	var err error
	blocklistNames, err = bravedns.StampToNames(blocklistStamp)
	if err != nil {
		log.Errorf("could not resolve blocklist-stamp %v", err)
		return
	}
	log.Debugf(blocklistNames)
	return
}

// Perform a query using the transport, and send the response to the writer.
func forwardQuery(t Transport, q []byte, c io.Writer) error {
	resp, err := t.Query(q)
	if err != nil {
		return err
	}
	rlen := len(resp)
	if rlen > math.MaxUint16 {
		return fmt.Errorf("Oversize response: %d", rlen)
	}
	// Use a combined write to ensure atomicity.  Otherwise, writes from two
	// responses could be interleaved.
	rlbuf := make([]byte, rlen+2)
	binary.BigEndian.PutUint16(rlbuf, uint16(rlen))
	copy(rlbuf[2:], resp)
	n, err := c.Write(rlbuf)
	if err != nil {
		return err
	}
	if int(n) != len(rlbuf) {
		return fmt.Errorf("Incomplete response write: %d < %d", n, len(rlbuf))
	}
	return nil
}

// Perform a query using the transport, send the response to the writer,
// and close the writer if there was an error.
func forwardQueryAndCheck(t Transport, q []byte, c io.WriteCloser) {
	if err := forwardQuery(t, q, c); err != nil {
		log.Warnf("Query forwarding failed: %v", err)
		c.Close()
	}
}

// Accept a DNS-over-TCP socket from a stub resolver, and connect the socket
// to this DNSTransport.
func Accept(t Transport, c io.ReadWriteCloser) {
	qlbuf := make([]byte, 2)
	for {
		n, err := c.Read(qlbuf)
		if n == 0 {
			log.Debugf("TCP query socket clean shutdown")
			break
		}
		if err != nil {
			log.Warnf("Error reading from TCP query socket: %v", err)
			break
		}
		if n < 2 {
			log.Warnf("Incomplete query length")
			break
		}
		qlen := binary.BigEndian.Uint16(qlbuf)
		q := make([]byte, qlen)
		n, err = c.Read(q)
		if err != nil {
			log.Warnf("Error reading query: %v", err)
			break
		}
		if n != int(qlen) {
			log.Warnf("Incomplete query: %d < %d", n, qlen)
			break
		}
		go forwardQueryAndCheck(t, q, c)
	}
	// TODO: Cancel outstanding queries at this point.
	c.Close()
}
