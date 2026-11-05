package js

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dop251/goja"

	"serbu/internal/metrics"
)

type statusRange struct{ min, max int }

// statusCallback menentukan status mana yang dianggap "berhasil" untuk http_req_failed.
type statusCallback struct{ ranges []statusRange }

func (c *statusCallback) expected(code int) bool {
	for _, r := range c.ranges {
		if code >= r.min && code <= r.max {
			return true
		}
	}
	return false
}

var defaultCallback = &statusCallback{ranges: []statusRange{{200, 399}}}

type fileData struct {
	Data        []byte `js:"data"`
	Filename    string `js:"filename"`
	ContentType string `js:"content_type"`
}

type reqSpec struct {
	method    string
	url       string
	body      []byte
	hasBody   bool
	headers   http.Header
	host      string
	tags      metrics.Tags
	name      string
	userName  bool
	timeout   time.Duration
	redirects int
	respType  string
	cookies   []*http.Cookie
	cb        *statusCallback
	noCb      bool
}

type timings struct {
	blocked, connecting, tls, sending, waiting, receiving, duration float64
}

type httpResult struct {
	spec       *reqSpec
	url        string
	status     int
	statusText string
	proto      string
	headers    http.Header
	body       []byte
	cookies    []*http.Cookie
	t          timings
	errMsg     string
	errCode    int
	remoteIP   string
	remotePort int
	reqMethod  string
	reqURL     string
	reqHeaders http.Header
	reqBody    []byte
}

func isNil(v goja.Value) bool { return v == nil || goja.IsUndefined(v) || goja.IsNull(v) }

// modHTTP = modul "k6/http".
func (vu *VU) modHTTP() goja.Value {
	o := vu.rt.NewObject()
	noBody := func(method string) func(goja.FunctionCall) goja.Value {
		return func(c goja.FunctionCall) goja.Value {
			return vu.request(vu.parseRequest(method, c.Argument(0), nil, c.Argument(1)))
		}
	}
	withBody := func(method string) func(goja.FunctionCall) goja.Value {
		return func(c goja.FunctionCall) goja.Value {
			return vu.request(vu.parseRequest(method, c.Argument(0), c.Argument(1), c.Argument(2)))
		}
	}
	_ = o.Set("get", noBody("GET"))
	_ = o.Set("head", noBody("HEAD"))
	_ = o.Set("post", withBody("POST"))
	_ = o.Set("put", withBody("PUT"))
	_ = o.Set("patch", withBody("PATCH"))
	_ = o.Set("del", withBody("DELETE"))
	_ = o.Set("options", withBody("OPTIONS"))
	_ = o.Set("request", func(c goja.FunctionCall) goja.Value {
		method := strings.ToUpper(c.Argument(0).String())
		return vu.request(vu.parseRequest(method, c.Argument(1), c.Argument(2), c.Argument(3)))
	})
	_ = o.Set("batch", vu.httpBatch)
	_ = o.Set("file", vu.httpFile)
	_ = o.Set("cookieJar", func(goja.FunctionCall) goja.Value { return vu.cookieJarObj() })
	_ = o.Set("expectedStatuses", vu.expectedStatuses)
	_ = o.Set("setResponseCallback", func(c goja.FunctionCall) goja.Value {
		a := c.Argument(0)
		vu.respCbSet = true
		if isNil(a) {
			vu.respCb = nil
			return goja.Undefined()
		}
		cb, ok := a.Export().(*statusCallback)
		if !ok {
			vu.throw("setResponseCallback() butuh hasil http.expectedStatuses(...) atau null")
		}
		vu.respCb = cb
		return goja.Undefined()
	})
	return o
}

func (vu *VU) request(s *reqSpec) goja.Value {
	vu.httpClient()
	return vu.responseObject(vu.doRequest(s))
}

func (vu *VU) parseRequest(method string, urlV, bodyV, paramsV goja.Value) *reqSpec {
	if vu.inInit {
		vu.throw("request HTTP tidak bisa dibuat di init context; pindahkan ke dalam function atau ke setup()")
	}
	if isNil(urlV) {
		vu.throw("URL request kosong")
	}
	s := &reqSpec{
		method:    method,
		url:       urlV.String(),
		headers:   http.Header{},
		redirects: vu.sh.Opts.MaxRedirects,
		timeout:   60 * time.Second,
		respType:  "text",
	}
	if vu.sh.Opts.DiscardResponseBodies {
		s.respType = "none"
	}
	if u, err := url.Parse(s.url); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		vu.throw("URL tidak valid: %q (harus diawali http:// atau https://)", s.url)
	}
	var userTags map[string]string
	if !isNil(paramsV) {
		po := paramsV.ToObject(vu.rt)
		for _, k := range po.Keys() {
			v := po.Get(k)
			if isNil(v) && k != "responseCallback" {
				continue
			}
			switch k {
			case "headers":
				ho := v.ToObject(vu.rt)
				for _, hk := range ho.Keys() {
					hv := ho.Get(hk)
					if isNil(hv) {
						continue
					}
					if strings.EqualFold(hk, "host") {
						s.host = hv.String()
					} else {
						s.headers.Set(hk, hv.String())
					}
				}
			case "tags":
				userTags = vu.exportTags(v)
			case "timeout":
				d, err := parseTimeout(v)
				if err != nil {
					vu.throw("%v", err)
				}
				s.timeout = d
			case "redirects":
				s.redirects = int(v.ToInteger())
			case "responseType":
				s.respType = v.String()
				if s.respType != "text" && s.respType != "binary" && s.respType != "none" {
					vu.throw("responseType harus 'text', 'binary', atau 'none'")
				}
			case "cookies":
				co := v.ToObject(vu.rt)
				for _, name := range co.Keys() {
					cv := co.Get(name)
					val := cv.String()
					if obj, ok := cv.(*goja.Object); ok && obj.ClassName() == "Object" {
						val = obj.Get("value").String()
					}
					s.cookies = append(s.cookies, &http.Cookie{Name: name, Value: val})
				}
			case "responseCallback":
				if isNil(v) {
					s.noCb = true
				} else if cb, ok := v.Export().(*statusCallback); ok {
					s.cb = cb
				}
			}
		}
	}
	if !isNil(bodyV) {
		s.hasBody = true
		var ctype string
		s.body, ctype = vu.encodeBody(bodyV)
		if ctype != "" && s.headers.Get("Content-Type") == "" {
			s.headers.Set("Content-Type", ctype)
		}
	}
	s.name = s.url
	if n, ok := userTags["name"]; ok && n != "" {
		s.name, s.userName = n, true
	}
	s.tags = vu.tags(userTags)
	if s.cb == nil && !s.noCb {
		if vu.respCbSet {
			s.cb = vu.respCb
			s.noCb = vu.respCb == nil
		} else {
			s.cb = defaultCallback
		}
	}
	return s
}

func parseTimeout(v goja.Value) (time.Duration, error) {
	switch x := v.Export().(type) {
	case int64:
		return time.Duration(x) * time.Millisecond, nil
	case float64:
		return time.Duration(x * float64(time.Millisecond)), nil
	}
	d, err := time.ParseDuration(v.String())
	if err != nil {
		return 0, fmt.Errorf("timeout %q tidak valid (contoh: '30s' atau 30000)", v.String())
	}
	return d, nil
}

func (vu *VU) encodeBody(v goja.Value) ([]byte, string) {
	switch x := v.Export().(type) {
	case string:
		return []byte(x), ""
	case goja.ArrayBuffer:
		return x.Bytes(), ""
	case []byte:
		return x, ""
	}
	obj, ok := v.(*goja.Object)
	if !ok || obj.ClassName() == "Array" {
		return []byte(v.String()), ""
	}
	keys := obj.Keys()
	hasFile := false
	for _, k := range keys {
		if _, ok := obj.Get(k).Export().(*fileData); ok {
			hasFile = true
			break
		}
	}
	if hasFile {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, k := range keys {
			fv := obj.Get(k)
			if f, ok := fv.Export().(*fileData); ok {
				name := f.Filename
				if name == "" {
					name = k
				}
				ct := f.ContentType
				if ct == "" {
					ct = "application/octet-stream"
				}
				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscape(k), quoteEscape(name)))
				h.Set("Content-Type", ct)
				part, _ := w.CreatePart(h)
				_, _ = part.Write(f.Data)
				continue
			}
			_ = w.WriteField(k, fv.String())
		}
		_ = w.Close()
		return buf.Bytes(), w.FormDataContentType()
	}
	form := url.Values{}
	for _, k := range keys {
		fv := obj.Get(k)
		if arr, ok := fv.(*goja.Object); ok && arr.ClassName() == "Array" {
			for _, ik := range arr.Keys() {
				form.Add(k, arr.Get(ik).String())
			}
			continue
		}
		form.Add(k, fv.String())
	}
	return []byte(form.Encode()), "application/x-www-form-urlencoded"
}

func quoteEscape(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }

func (vu *VU) httpFile(call goja.FunctionCall) goja.Value {
	d := call.Argument(0)
	var data []byte
	switch x := d.Export().(type) {
	case string:
		data = []byte(x)
	case goja.ArrayBuffer:
		data = x.Bytes()
	case []byte:
		data = x
	default:
		data = []byte(d.String())
	}
	f := &fileData{Data: data}
	if a := call.Argument(1); !isNil(a) {
		f.Filename = a.String()
	}
	if a := call.Argument(2); !isNil(a) {
		f.ContentType = a.String()
	}
	return vu.rt.ToValue(f)
}

func (vu *VU) expectedStatuses(call goja.FunctionCall) goja.Value {
	cb := &statusCallback{}
	for _, a := range call.Arguments {
		if obj, ok := a.(*goja.Object); ok {
			lo, hi := int(obj.Get("min").ToInteger()), int(obj.Get("max").ToInteger())
			if lo > hi {
				vu.throw("expectedStatuses: min (%d) lebih besar dari max (%d)", lo, hi)
			}
			cb.ranges = append(cb.ranges, statusRange{lo, hi})
			continue
		}
		n := int(a.ToInteger())
		cb.ranges = append(cb.ranges, statusRange{n, n})
	}
	return vu.rt.ToValue(cb)
}

func (vu *VU) cookieJarObj() goja.Value {
	vu.httpClient()
	o := vu.rt.NewObject()
	parse := func(v goja.Value) *url.URL {
		u, err := url.Parse(v.String())
		if err != nil || u.Host == "" {
			vu.throw("URL cookie tidak valid: %q", v.String())
		}
		return u
	}
	_ = o.Set("cookiesForURL", func(c goja.FunctionCall) goja.Value {
		u := parse(c.Argument(0))
		out := vu.rt.NewObject()
		group := map[string][]any{}
		var order []string
		for _, ck := range vu.jar.Cookies(u) {
			if _, ok := group[ck.Name]; !ok {
				order = append(order, ck.Name)
			}
			group[ck.Name] = append(group[ck.Name], ck.Value)
		}
		for _, n := range order {
			_ = out.Set(n, vu.rt.NewArray(group[n]...))
		}
		return out
	})
	_ = o.Set("set", func(c goja.FunctionCall) goja.Value {
		u := parse(c.Argument(0))
		ck := &http.Cookie{Name: c.Argument(1).String(), Value: c.Argument(2).String()}
		if opts := c.Argument(3); !isNil(opts) {
			oo := opts.ToObject(vu.rt)
			if v := oo.Get("domain"); !isNil(v) {
				ck.Domain = v.String()
			}
			if v := oo.Get("path"); !isNil(v) {
				ck.Path = v.String()
			}
			if v := oo.Get("max_age"); !isNil(v) {
				ck.MaxAge = int(v.ToInteger())
			}
			if v := oo.Get("secure"); !isNil(v) {
				ck.Secure = v.ToBoolean()
			}
			if v := oo.Get("http_only"); !isNil(v) {
				ck.HttpOnly = v.ToBoolean()
			}
		}
		vu.jar.SetCookies(u, []*http.Cookie{ck})
		return goja.Undefined()
	})
	_ = o.Set("delete", func(c goja.FunctionCall) goja.Value {
		u := parse(c.Argument(0))
		vu.jar.SetCookies(u, []*http.Cookie{{Name: c.Argument(1).String(), Path: "/", MaxAge: -1}})
		return goja.Undefined()
	})
	_ = o.Set("clear", func(goja.FunctionCall) goja.Value {
		vu.jar, _ = cookiejar.New(nil)
		vu.client.Jar = vu.jar
		return goja.Undefined()
	})
	return o
}

// countConn menghitung byte yang lewat (untuk data_sent / data_received).
type countConn struct {
	net.Conn
	vu *VU
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.vu.recv.Add(int64(n))
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.vu.sent.Add(int64(n))
	return n, err
}

// httpClient dibuat sekali per VU: koneksi dipakai ulang antar iterasi seperti browser.
func (vu *VU) httpClient() *http.Client {
	if vu.client != nil {
		return vu.client
	}
	opts := vu.sh.Opts
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	hosts := opts.Hosts
	vu.transport = &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			// host yang dipetakan di options.hosts selalu dihubungi langsung
			if len(hosts) > 0 && (hosts[r.URL.Host] != "" || hosts[r.URL.Hostname()] != "") {
				return nil, nil
			}
			return http.ProxyFromEnvironment(r)
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if len(hosts) > 0 {
				addr = remapHost(hosts, addr)
			}
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &countConn{Conn: c, vu: vu}, nil
		},
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: opts.InsecureSkipTLSVerify}, //nolint:gosec
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableKeepAlives:     opts.NoConnectionReuse,
	}
	vu.jar, _ = cookiejar.New(nil)
	vu.client = &http.Client{
		Transport:     vu.transport,
		Jar:           vu.jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return vu.client
}

func remapHost(hosts map[string]string, addr string) string {
	if to, ok := hosts[addr]; ok {
		return to
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	to, ok := hosts[host]
	if !ok {
		return addr
	}
	if _, _, err := net.SplitHostPort(to); err == nil {
		return to
	}
	return net.JoinHostPort(to, port)
}

type tracer struct {
	mu                                                              sync.Mutex
	start, getConn, connStart, connDone, tlsStart, tlsDone, gotConn time.Time
	wrote, firstByte                                                time.Time
	reused                                                          bool
	remote                                                          net.Addr
}

func (t *tracer) mark(p *time.Time) {
	t.mu.Lock()
	*p = time.Now()
	t.mu.Unlock()
}

func (t *tracer) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { t.mark(&t.getConn) },
		ConnectStart: func(_, _ string) {
			t.mu.Lock()
			if t.connStart.IsZero() {
				t.connStart = time.Now()
			}
			t.mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				t.mark(&t.connDone)
			}
		},
		TLSHandshakeStart: func() { t.mark(&t.tlsStart) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { t.mark(&t.tlsDone) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			t.gotConn = time.Now()
			t.reused = info.Reused
			if info.Conn != nil {
				t.remote = info.Conn.RemoteAddr()
			}
			t.mu.Unlock()
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.mark(&t.wrote) },
		GotFirstResponseByte: func() { t.mark(&t.firstByte) },
	}
}

func span(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return ms(b.Sub(a))
}

// timings memecah waktu request seperti k6: blocked (termasuk DNS), connecting, tls,
// sending, waiting (TTFB), receiving. duration = sending + waiting + receiving.
func (t *tracer) timings(end time.Time) timings {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := t.getConn
	if start.IsZero() {
		start = t.start
	}
	var r timings
	if !t.gotConn.IsZero() {
		if t.reused || t.connStart.IsZero() {
			r.blocked = span(start, t.gotConn)
		} else {
			r.blocked = span(start, t.connStart)
			r.connecting = span(t.connStart, t.connDone)
			r.tls = span(t.tlsStart, t.tlsDone)
		}
		ref := t.gotConn
		if !t.wrote.IsZero() {
			r.sending = span(t.gotConn, t.wrote)
			ref = t.wrote
		}
		if !t.firstByte.IsZero() {
			r.waiting = span(ref, t.firstByte)
			r.receiving = span(t.firstByte, end)
		} else {
			r.waiting = span(ref, end)
		}
	} else if !t.connStart.IsZero() {
		r.blocked = span(start, t.connStart)
		cEnd := t.connDone
		if cEnd.IsZero() {
			cEnd = end
		}
		r.connecting = span(t.connStart, cEnd)
		if !t.tlsStart.IsZero() {
			tEnd := t.tlsDone
			if tEnd.IsZero() {
				tEnd = end
			}
			r.tls = span(t.tlsStart, tEnd)
		}
	} else {
		r.blocked = span(start, end)
	}
	r.duration = r.sending + r.waiting + r.receiving
	return r
}

func (t *tracer) remoteAddr() (string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if a, ok := t.remote.(*net.TCPAddr); ok {
		return a.IP.String(), a.Port
	}
	return "", 0
}

func isRedirect(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// doRequest menjalankan request (termasuk redirect) tanpa menyentuh runtime JS,
// sehingga aman dipanggil paralel oleh http.batch().
func (vu *VU) doRequest(s *reqSpec) *httpResult {
	client := vu.httpClient()
	iterCtx := vu.ctx
	ctx, cancel := context.WithTimeout(iterCtx, s.timeout)
	defer cancel()

	res := &httpResult{spec: s}
	method, target, body, hasBody := s.method, s.url, s.body, s.hasBody
	headers := s.headers.Clone()
	for hop := 0; ; hop++ {
		var rdr io.Reader
		if hasBody {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rdr)
		if err != nil {
			res.url, res.errCode, res.errMsg = target, 1020, "URL tidak valid: "+err.Error()
			return res
		}
		req.Header = headers.Clone()
		if ua := vu.sh.Opts.UserAgent; ua != "" && req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", ua)
		}
		if s.host != "" {
			req.Host = s.host
		}
		for _, c := range s.cookies {
			req.AddCookie(c)
		}
		tr := &tracer{start: time.Now()}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), tr.trace()))
		res.reqMethod, res.reqURL, res.reqHeaders, res.reqBody = method, target, req.Header, body

		resp, err := client.Do(req)
		if err != nil {
			if iterCtx.Err() != nil {
				res.url, res.errCode, res.errMsg = target, 1000, "request dibatalkan karena tes berakhir"
				return res
			}
			res.t = tr.timings(time.Now())
			res.url, res.status, res.statusText, res.proto = target, 0, "", ""
			res.errCode, res.errMsg = classifyError(err, s.timeout)
			if p, _ := vu.transport.Proxy(req); p != nil {
				res.errMsg += fmt.Sprintf(" (lewat proxy %s dari HTTPS_PROXY/HTTP_PROXY; tambahkan host ke NO_PROXY bila tidak perlu proxy)", p.Host)
			}
			res.remoteIP, res.remotePort = tr.remoteAddr()
			vu.emitHTTP(res, method, target)
			vu.debugHTTP(req, nil, nil, res)
			return res
		}
		var b []byte
		var rerr error
		if s.respType == "none" {
			_, rerr = io.Copy(io.Discard, resp.Body)
		} else {
			b, rerr = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		res.t = tr.timings(time.Now())
		res.url, res.status, res.statusText, res.proto = target, resp.StatusCode, resp.Status, resp.Proto
		res.headers, res.body, res.cookies = resp.Header, b, resp.Cookies()
		res.remoteIP, res.remotePort = tr.remoteAddr()
		res.errCode, res.errMsg = 0, ""
		if rerr != nil {
			if iterCtx.Err() != nil {
				res.errCode, res.errMsg = 1000, "request dibatalkan karena tes berakhir"
				return res
			}
			res.errCode, res.errMsg = classifyError(rerr, s.timeout)
		} else if resp.StatusCode >= 400 && resp.StatusCode < 600 {
			res.errCode = 1000 + resp.StatusCode
		}
		vu.emitHTTP(res, method, target)
		vu.debugHTTP(req, resp, b, res)

		if rerr == nil && isRedirect(resp.StatusCode) && hop < s.redirects {
			if loc := resp.Header.Get("Location"); loc != "" {
				if next, perr := req.URL.Parse(loc); perr == nil {
					code := resp.StatusCode
					if code == 303 || ((code == 301 || code == 302) && method != "GET" && method != "HEAD") {
						if method != "HEAD" {
							method = "GET"
						}
						body, hasBody = nil, false
						headers.Del("Content-Type")
						headers.Del("Content-Length")
					}
					if next.Host != req.URL.Host {
						headers.Del("Authorization")
						headers.Del("Cookie")
					}
					target = next.String()
					continue
				}
			}
		}
		return res
	}
}

func (vu *VU) emitHTTP(res *httpResult, method, hopURL string) {
	s := res.spec
	b := vu.sh.B
	tags := make(metrics.Tags, len(s.tags)+8)
	for k, v := range s.tags {
		tags[k] = v
	}
	tags["method"] = method
	tags["url"] = hopURL
	if s.userName {
		tags["name"] = s.name
	} else {
		tags["name"] = hopURL
	}
	tags["status"] = strconv.Itoa(res.status)
	if res.proto != "" {
		tags["proto"] = res.proto
	}
	if res.errCode != 0 {
		tags["error_code"] = strconv.Itoa(res.errCode)
	}
	expected := false
	if s.cb != nil {
		expected = res.status != 0 && res.errMsg == "" && s.cb.expected(res.status)
		tags["expected_response"] = strconv.FormatBool(expected)
	}
	now := time.Now()
	t := res.t
	samples := make([]metrics.Sample, 0, 11)
	add := func(m *metrics.Metric, v float64, tg metrics.Tags) {
		samples = append(samples, metrics.Sample{Metric: m, Value: v, Tags: tg, Time: now})
	}
	add(b.HTTPReqs, 1, tags)
	add(b.HTTPReqDuration, t.duration, tags)
	add(b.HTTPReqBlocked, t.blocked, tags)
	add(b.HTTPReqConnecting, t.connecting, tags)
	add(b.HTTPReqTLS, t.tls, tags)
	add(b.HTTPReqSending, t.sending, tags)
	add(b.HTTPReqWaiting, t.waiting, tags)
	add(b.HTTPReqReceiving, t.receiving, tags)
	if s.cb != nil {
		failed := 1.0
		if expected {
			failed = 0
		}
		add(b.HTTPReqFailed, failed, tags)
	}
	if sent := vu.sent.Swap(0); sent > 0 {
		add(b.DataSent, float64(sent), s.tags)
	}
	if recv := vu.recv.Swap(0); recv > 0 {
		add(b.DataReceived, float64(recv), s.tags)
	}
	vu.emit(samples...)
	if res.errMsg != "" {
		vu.sh.HTTPErrs.Add(strconv.Itoa(res.errCode) + "|" + res.errMsg)
	}
}

func (vu *VU) debugHTTP(req *http.Request, resp *http.Response, body []byte, res *httpResult) {
	mode := vu.sh.Opts.HTTPDebug
	if mode == "" {
		return
	}
	st := vu.sh.Console.Err
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\n", st.Cyan(">"), req.Method, req.URL.String())
	for k, vs := range req.Header {
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.Join(vs, ", "))
	}
	if mode == "full" && len(res.reqBody) > 0 {
		fmt.Fprintf(&b, "  %s\n", snippet(res.reqBody, 2000))
	}
	if resp == nil {
		fmt.Fprintf(&b, "%s ERROR %d: %s (%s)\n", st.Red("<"), res.errCode, res.errMsg, fmtMs(res.t.duration+res.t.blocked+res.t.connecting+res.t.tls))
	} else {
		fmt.Fprintf(&b, "%s %s %s (%s)\n", st.Magenta("<"), resp.Proto, resp.Status, fmtMs(res.t.duration))
		for k, vs := range resp.Header {
			fmt.Fprintf(&b, "  %s: %s\n", k, strings.Join(vs, ", "))
		}
		if mode == "full" && len(body) > 0 {
			fmt.Fprintf(&b, "  %s\n", snippet(body, 2000))
		}
	}
	vu.sh.Console.Log(strings.TrimRight(b.String(), "\n"))
}

func fmtMs(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) + "ms" }

func snippet(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + fmt.Sprintf("… (+%d byte)", len(s)-n)
	}
	return s
}

// classifyError memetakan error jaringan ke kode error k6 + penjelasan.
func classifyError(err error, timeout time.Duration) (int, string) {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var op *net.OpError
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		if errors.As(err, &op) && op.Op == "dial" {
			return 1211, "timeout saat membuka koneksi"
		}
		return 1050, fmt.Sprintf("request timeout (melebihi %s)", timeout)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return 1101, "host tidak ditemukan (DNS): " + dnsErr.Name
		}
		return 1100, "error DNS: " + dnsErr.Err
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return 1212, "koneksi ditolak (server mati, port salah, atau kewalahan)"
	case errors.Is(err, syscall.ECONNRESET):
		return 1220, "koneksi direset oleh server"
	case errors.Is(err, syscall.EPIPE):
		return 1201, "koneksi diputus server (broken pipe)"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return 1000, "server menutup koneksi sebelum respons selesai (EOF)"
	}
	var uaErr x509.UnknownAuthorityError
	if errors.As(err, &uaErr) {
		return 1310, "sertifikat TLS tidak dikenal (untuk server uji pakai --insecure-skip-tls-verify)"
	}
	var hnErr x509.HostnameError
	if errors.As(err, &hnErr) {
		return 1311, "sertifikat TLS tidak cocok dengan nama host"
	}
	var ciErr x509.CertificateInvalidError
	if errors.As(err, &ciErr) {
		return 1300, "sertifikat TLS tidak valid: " + ciErr.Error()
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return 1300, "error TLS (apakah server benar memakai HTTPS?)"
	}
	if errors.As(err, &op) {
		if op.Op == "dial" {
			return 1210, "gagal membuka koneksi: " + op.Err.Error()
		}
		return 1200, "error koneksi TCP: " + op.Err.Error()
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "tls:"):
		return 1300, "error TLS: " + msg
	case strings.Contains(msg, "http2"):
		return 1600, "error HTTP/2: " + msg
	}
	return 1000, msg
}

func (vu *VU) responseObject(res *httpResult) *goja.Object {
	rt := vu.rt
	o := rt.NewObject()
	_ = o.Set("status", res.status)
	_ = o.Set("status_text", res.statusText)
	_ = o.Set("proto", res.proto)
	_ = o.Set("url", res.url)
	_ = o.Set("remote_ip", res.remoteIP)
	_ = o.Set("remote_port", res.remotePort)
	_ = o.Set("error", res.errMsg)
	_ = o.Set("error_code", res.errCode)

	headers := rt.NewObject()
	for k, vs := range res.headers {
		_ = headers.Set(k, strings.Join(vs, ", "))
	}
	_ = o.Set("headers", headers)

	cookies := rt.NewObject()
	byName := map[string][]any{}
	var order []string
	for _, c := range res.cookies {
		co := rt.NewObject()
		_ = co.Set("name", c.Name)
		_ = co.Set("value", c.Value)
		_ = co.Set("domain", c.Domain)
		_ = co.Set("path", c.Path)
		_ = co.Set("httpOnly", c.HttpOnly)
		_ = co.Set("secure", c.Secure)
		_ = co.Set("maxAge", c.MaxAge)
		if !c.Expires.IsZero() {
			_ = co.Set("expires", c.Expires.UnixMilli())
		} else {
			_ = co.Set("expires", 0)
		}
		if _, ok := byName[c.Name]; !ok {
			order = append(order, c.Name)
		}
		byName[c.Name] = append(byName[c.Name], co)
	}
	for _, n := range order {
		_ = cookies.Set(n, rt.NewArray(byName[n]...))
	}
	_ = o.Set("cookies", cookies)

	switch res.spec.respType {
	case "none":
		_ = o.Set("body", goja.Null())
	case "binary":
		_ = o.Set("body", rt.NewArrayBuffer(res.body))
	default:
		_ = o.Set("body", string(res.body))
	}

	t := rt.NewObject()
	_ = t.Set("duration", res.t.duration)
	_ = t.Set("blocked", res.t.blocked)
	_ = t.Set("connecting", res.t.connecting)
	_ = t.Set("tls_handshaking", res.t.tls)
	_ = t.Set("sending", res.t.sending)
	_ = t.Set("waiting", res.t.waiting)
	_ = t.Set("receiving", res.t.receiving)
	_ = o.Set("timings", t)

	rq := rt.NewObject()
	_ = rq.Set("method", res.reqMethod)
	_ = rq.Set("url", res.reqURL)
	rh := rt.NewObject()
	for k, vs := range res.reqHeaders {
		arr := make([]any, len(vs))
		for i, v := range vs {
			arr[i] = v
		}
		_ = rh.Set(k, rt.NewArray(arr...))
	}
	_ = rq.Set("headers", rh)
	_ = rq.Set("body", string(res.reqBody))
	_ = o.Set("request", rq)

	var parsed goja.Value
	_ = o.Set("json", func(call goja.FunctionCall) goja.Value {
		if parsed == nil {
			if res.spec.respType == "none" {
				vu.throw("res.json(): body tidak tersedia (responseType 'none' atau discardResponseBodies aktif)")
			}
			v, err := vu.parseJSON(string(res.body))
			if err != nil {
				vu.throw("res.json(): body bukan JSON valid (status %d, url %s): %s", res.status, res.url, snippet(res.body, 120))
			}
			parsed = v
		}
		sel := call.Argument(0)
		if isNil(sel) || sel.String() == "" {
			return parsed
		}
		return vu.jsonSelect(parsed, sel.String())
	})
	_ = o.Set("html", func(goja.FunctionCall) goja.Value {
		vu.throw("res.html() belum didukung serbu. Ambil nilai dari HTML dengan findBetween() dari 'serbu/utils' atau dengan regex")
		return nil
	})
	return o
}

// jsonSelect mendukung path sederhana: "data.items.0.id" dan "items.#" (panjang array).
func (vu *VU) jsonSelect(v goja.Value, path string) goja.Value {
	cur := v
	for _, part := range strings.Split(path, ".") {
		if isNil(cur) {
			return goja.Undefined()
		}
		obj, ok := cur.(*goja.Object)
		if !ok {
			return goja.Undefined()
		}
		if part == "#" && obj.ClassName() == "Array" {
			return obj.Get("length")
		}
		cur = obj.Get(part)
	}
	if cur == nil {
		return goja.Undefined()
	}
	return cur
}

func (vu *VU) httpBatch(call goja.FunctionCall) goja.Value {
	if vu.inInit {
		vu.throw("http.batch() tidak bisa dipakai di init context")
	}
	arg := call.Argument(0)
	if isNil(arg) {
		vu.throw("http.batch() butuh array atau objek berisi request")
	}
	obj := arg.ToObject(vu.rt)
	isArr := obj.ClassName() == "Array"
	keys := obj.Keys()
	specs := make([]*reqSpec, len(keys))
	for i, k := range keys {
		specs[i] = vu.parseBatchItem(obj.Get(k))
	}
	vu.httpClient()
	limit, perHost := vu.sh.Opts.Batch, vu.sh.Opts.BatchPerHost
	if limit <= 0 {
		limit = 20
	}
	if perHost <= 0 {
		perHost = 6
	}
	sem := make(chan struct{}, limit)
	hostSem := map[string]chan struct{}{}
	hostOf := func(raw string) string {
		if u, err := url.Parse(raw); err == nil {
			return u.Host
		}
		return raw
	}
	for _, s := range specs {
		if h := hostOf(s.url); hostSem[h] == nil {
			hostSem[h] = make(chan struct{}, perHost)
		}
	}
	results := make([]*httpResult, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func(i int, s *reqSpec) {
			defer wg.Done()
			hs := hostSem[hostOf(s.url)]
			sem <- struct{}{}
			hs <- struct{}{}
			results[i] = vu.doRequest(s)
			<-hs
			<-sem
		}(i, s)
	}
	wg.Wait()
	if isArr {
		arr := make([]any, len(results))
		for i, r := range results {
			arr[i] = vu.responseObject(r)
		}
		return vu.rt.NewArray(arr...)
	}
	out := vu.rt.NewObject()
	for i, k := range keys {
		_ = out.Set(k, vu.responseObject(results[i]))
	}
	return out
}

func (vu *VU) parseBatchItem(v goja.Value) *reqSpec {
	if s, ok := v.Export().(string); ok {
		return vu.parseRequest("GET", vu.rt.ToValue(s), nil, nil)
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		vu.throw("item http.batch() harus string URL, array [method, url, body, params], atau objek {method, url, body, params}")
	}
	if obj.ClassName() == "Array" {
		get := func(i int) goja.Value { return obj.Get(strconv.Itoa(i)) }
		return vu.parseRequest(strings.ToUpper(get(0).String()), get(1), get(2), get(3))
	}
	method := "GET"
	if m := obj.Get("method"); !isNil(m) {
		method = strings.ToUpper(m.String())
	}
	return vu.parseRequest(method, obj.Get("url"), obj.Get("body"), obj.Get("params"))
}
