// Package httpcache provides a http.RoundTripper implementation that works as a
// mostly RFC-compliant cache for http responses.
//
// It is only suitable for use as a 'private' cache (i.e. for a web-browser or an API-client
// and not for a shared proxy).
//
package httpcache

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chinanjjohn2012/httpcache/diskcache"
	"github.com/chinanjjohn2012/httpcache/leveldbcache"
	"github.com/chinanjjohn2012/httpcache/lrucache"
	"github.com/chinanjjohn2012/httpcache/memcache"
	"github.com/chinawebeye/glog"
)

const (
	stale = iota
	fresh
	transparent
	// XFromCache is the header added to responses that are returned from the cache
	XFromCache = "X-From-Cache"
)
const (
	logVNum    glog.Level = 2 //set glog V level
	TimeFormat            = "Mon, 02 Jan 2006 15:04:05 GMT"
)

// A Cache interface is used by the Transport to store and retrieve responses.
type Cache interface {
	// Get returns the []byte representation of a cached response and a bool
	// set to true if the value isn't empty
	Get(key string) (responseBytes []byte, ok bool)
	// Set stores the []byte representation of a response against a key
	Set(key string, responseBytes []byte)
	// Delete removes the value associated with the key
	Delete(key string)
}

// cacheKey returns the cache key for req.
func cacheKey(req *http.Request) string {
	return req.URL.String()
}

// CachedResponse returns the cached http.Response for req if present, and nil
// otherwise.
func CachedResponse(c Cache, req *http.Request) (resp *http.Response, err error) {
	cachedVal, ok := c.Get(cacheKey(req))
	if !ok {
		return
	}

	b := bytes.NewBuffer(cachedVal)
	return http.ReadResponse(bufio.NewReader(b), req)
}

// MemoryCache is an implemtation of Cache that stores responses in an in-memory map.
type MemoryCache struct {
	mu    sync.RWMutex
	items map[string][]byte
}

// Get returns the []byte representation of the response and true if present, false if not
func (c *MemoryCache) Get(key string) (resp []byte, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	resp, ok = c.items[key]
	if ok {
		glog.V(logVNum).Infof("Cache.Get key = %v", key)
	}

	return resp, ok
}

// Set saves response resp to the cache with key
func (c *MemoryCache) Set(key string, resp []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = resp
	glog.V(logVNum).Infof("Cache.Set key = %v", key)
}

// Delete removes key from the cache
func (c *MemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	glog.V(logVNum).Infof("Cache.Delete key = %v", key)
}

// NewMemoryCache returns a new Cache that will store items in an in-memory map
func NewMemoryCache() *MemoryCache {
	c := &MemoryCache{items: map[string][]byte{}}
	return c
}

// onEOFReader executes a function on reader EOF or close
type onEOFReader struct {
	rc io.ReadCloser
	fn func()
}

func (r *onEOFReader) Read(p []byte) (n int, err error) {
	n, err = r.rc.Read(p)
	if err == io.EOF {
		r.runFunc()
	}
	return
}

func (r *onEOFReader) Close() error {
	err := r.rc.Close()
	r.runFunc()
	return err
}

func (r *onEOFReader) runFunc() {
	if fn := r.fn; fn != nil {
		fn()
		r.fn = nil
	}
}

// Transport is an implementation of http.RoundTripper that will return values from a cache
// where possible (avoiding a network request) and will additionally add validators (etag/if-modified-since)
// to repeated requests allowing servers to return 304 / Not Modified
type Transport struct {
	// The RoundTripper interface actually used to make requests
	// If nil, http.DefaultTransport is used

	//Transport http.RoundTripper
	Transport *http.Transport //john change 2015.11.3

	Cache Cache
	// If true, responses returned from the cache will be given an extra header, X-From-Cache
	MarkCachedResponses bool
	// guards modReq
	mu sync.RWMutex
	// Mapping of original request => cloned
	modReq map[*http.Request]*http.Request
}

// NewTransport returns a new Transport with the
// provided Cache implementation and MarkCachedResponses set to true
func NewTransport(c Cache) *Transport {
	return &Transport{Cache: c, MarkCachedResponses: true}
}

// Client returns an *http.Client that caches responses.
func (t *Transport) Client() *http.Client {
	return &http.Client{Transport: t}
}

// varyMatches will return false unless all of the cached values for the headers listed in Vary
// match the new request
func varyMatches(cachedResp *http.Response, req *http.Request) bool {
	for _, header := range headerAllCommaSepValues(cachedResp.Header, "vary") {
		header = http.CanonicalHeaderKey(header)
		if header != "" && req.Header.Get(header) != cachedResp.Header.Get("X-Varied-"+header) {
			glog.V(logVNum).Infof("varyMatches  false header = %v", header)
			return false
		}
	}
	//glog.V(logVNum).Infof("varyMatches  true")
	return true
}

// setModReq maintains a mapping between original requests and their associated cloned requests
func (t *Transport) setModReq(orig, mod *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.modReq == nil {
		t.modReq = make(map[*http.Request]*http.Request)
	}
	if mod == nil {
		delete(t.modReq, orig)
	} else {
		t.modReq[orig] = mod
	}
}

// RoundTrip takes a Request and returns a Response
//
// If there is a fresh Response already in cache, then it will be returned without connecting to
// the server.
//
// If there is a stale Response, then any validators it contains will be set on the new request
// to give the server a chance to respond with NotModified. If this happens, then the cached Response
// will be returned.
func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	cacheKey := cacheKey(req)
	cacheableMethod := req.Method == "GET" || req.Method == "HEAD"
	var cachedResp *http.Response
	if cacheableMethod {
		cachedResp, err = CachedResponse(t.Cache, req)
	} else {
		// Need to invalidate an existing value
		t.Cache.Delete(cacheKey)
	}

	transport := t.Transport
	if transport == nil {
		//john change 2015.11.3
		if true {
			var ok bool
			if transport, ok = http.DefaultTransport.(*http.Transport); !ok {
				return nil, err
			}
		} else {
			//transport = http.DefaultTransport
		}
	}

	if cachedResp != nil && err == nil && cacheableMethod && req.Header.Get("range") == "" {
		if t.MarkCachedResponses {
			cachedResp.Header.Set(XFromCache, "1")
		}

		if varyMatches(cachedResp, req) {
			// Can only use cached value if the new request doesn't Vary significantly
			freshness := getFreshness(cachedResp.Header, req.Header)
			if freshness == fresh {
				glog.V(logVNum).Infof("fresh url = %v", req.URL.String())
				return cachedResp, nil
			}

			if freshness == stale {
				var req2 *http.Request
				// Add validators if caller hasn't already done so
				//glog.V(logVNum).Infof("stale url = %v", req.URL.String())

				etag := cachedResp.Header.Get("etag")
				if etag != "" {
					reqEtag := req.Header.Get("etag")
					if etag == reqEtag {
						return RespNotModified(cachedResp, req)
					}

					reqIfNoneMatch := req.Header.Get("if-none-match")
					if etag == reqIfNoneMatch {
						return RespNotModified(cachedResp, req)
					}

					if reqEtag == "" && reqIfNoneMatch == "" {
						req2 = cloneRequest(req)
						req2.Header.Set("if-none-match", etag)
						glog.V(logVNum).Infof("Header.Set if-none-match : %v", etag)
					}
				}

				lastModified := cachedResp.Header.Get("last-modified")
				if lastModified != "" {
					reqLastModified := req.Header.Get("last-modified")
					if result, err := headerTimeCmp(lastModified, reqLastModified); err == nil {
						if result >= 0 {
							return RespNotModified(cachedResp, req)
						}
					}

					reqIfModifiedSince := req.Header.Get("if-modified-since")
					if result, err := headerTimeCmp(lastModified, reqIfModifiedSince); err == nil {
						if result >= 0 {
							return RespNotModified(cachedResp, req)
						}
					}

					if reqLastModified == "" && reqIfModifiedSince == "" {
						if req2 == nil {
							req2 = cloneRequest(req)
						}
						req2.Header.Set("if-modified-since", lastModified)
						glog.V(logVNum).Infof("Header.Set if-modified-since : %v", lastModified)
					}
				}
				if req2 != nil {
					// Associate original request with cloned request so we can refer to
					// it in CancelRequest()
					//t.setModReq(req, req2) john close 2015.11.4
					req = req2
					/* john close 2015.11.4 b
					defer func() {
						// Release req/clone mapping on error
						if err != nil {
							t.setModReq(req, nil)
						}
						if resp != nil {
							// Release req/clone mapping on body close/EOF
							resp.Body = &onEOFReader{
								rc: resp.Body,
								fn: func() { t.setModReq(req, nil) },
							}
						}
					}()
					john close 2015.11.4 e */
				}
			}
		}

		resp, err = transport.RoundTrip(req)
		glog.V(logVNum).Infof("transport.RoundTrip url = %v", req.URL.String())

		if err == nil && req.Method == "GET" && resp.StatusCode == http.StatusNotModified {
			return resp, nil

			// Replace the 304 response with the one from cache, but update with some new headers
			endToEndHeaders := getEndToEndHeaders(resp.Header)
			glog.V(logVNum).Infof("endToEndHeaders = %v", endToEndHeaders)
			for _, header := range endToEndHeaders {
				cachedResp.Header[header] = resp.Header[header]
			}
			cachedResp.Status = fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK))
			cachedResp.StatusCode = http.StatusOK
			resp = cachedResp
		} else if (err != nil || (cachedResp != nil && resp.StatusCode >= 500)) &&
			req.Method == "GET" && canStaleOnError(cachedResp.Header, req.Header) {
			// In case of transport failure and stale-if-error activated, returns cached content
			// when available
			cachedResp.Status = fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK))
			cachedResp.StatusCode = http.StatusOK
			return cachedResp, nil
		} else {
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Cache.Delete(cacheKey)
			}
			if err != nil {
				glog.Errorf("err = %v", err)
				return nil, err
			}
		}
	} else {
		reqCacheControl := parseCacheControl(req.Header)
		if _, ok := reqCacheControl["only-if-cached"]; ok {
			resp = newGatewayTimeoutResponse(req)
		} else {
			resp, err = transport.RoundTrip(req)
			if err != nil {
				glog.Errorf("transport.RoundTrip err = %v", err)
				return nil, err
			}
		}
	}

	reqCacheControl := parseCacheControl(req.Header)
	respCacheControl := parseCacheControl(resp.Header)

	glog.V(logVNum+1).Infof("url = %v, reqCacheControl = %v, respCacheControl = %v", req.URL.String(), reqCacheControl, respCacheControl)

	if canStore(reqCacheControl, respCacheControl) {
		for _, varyKey := range headerAllCommaSepValues(resp.Header, "vary") {
			varyKey = http.CanonicalHeaderKey(varyKey)
			fakeHeader := "X-Varied-" + varyKey
			reqValue := req.Header.Get(varyKey)
			if reqValue != "" {
				resp.Header.Set(fakeHeader, reqValue)
				//glog.V(logVNum).Infof("Header.Set fakeHeader = %v, reqValue = %v", fakeHeader, reqValue)
			}
		}
		respBytes, err := httputil.DumpResponse(resp, true)
		if err == nil {
			t.Cache.Set(cacheKey, respBytes)
		}
	} else {
		t.Cache.Delete(cacheKey)
	}
	return resp, nil
}

/*
// CancelRequest calls CancelRequest on the underlaying transport if implemented or
// throw a warning otherwise.
func (t *Transport) CancelRequest(req *http.Request) {
	type canceler interface {
		CancelRequest(*http.Request)
	}
	tr, ok := t.Transport.(canceler)
	if !ok {
		log.Printf("httpcache: Client Transport of type %T doesn't support CancelRequest; Timeout not supported", t.Transport)
		return
	}

	t.mu.RLock()
	if modReq, ok := t.modReq[req]; ok {
		t.mu.RUnlock()
		t.mu.Lock()
		delete(t.modReq, req)
		t.mu.Unlock()
		tr.CancelRequest(modReq)
	} else {
		t.mu.RUnlock()
		tr.CancelRequest(req)
	}
}
*/
// ErrNoDateHeader indicates that the HTTP headers contained no Date header.
var ErrNoDateHeader = errors.New("no Date header")

// Date parses and returns the value of the Date header.
func Date(respHeaders http.Header) (date time.Time, err error) {
	dateHeader := respHeaders.Get("date")
	if dateHeader == "" {
		err = ErrNoDateHeader
		return
	}

	return time.Parse(time.RFC1123, dateHeader)
}

type realClock struct{}

func (c *realClock) since(d time.Time) time.Duration {
	return time.Since(d)
}

type timer interface {
	since(d time.Time) time.Duration
}

var clock timer = &realClock{}

// getFreshness will return one of fresh/stale/transparent based on the cache-control
// values of the request and the response
//
// fresh indicates the response can be returned
// stale indicates that the response needs validating before it is returned
// transparent indicates the response should not be used to fulfil the request
//
// Because this is only a private cache, 'public' and 'private' in cache-control aren't
// signficant. Similarly, smax-age isn't used.
func getFreshness(respHeaders, reqHeaders http.Header) (freshness int) {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)
	if _, ok := reqCacheControl["no-cache"]; ok {
		glog.V(logVNum).Infof("transparent req no-cache")
		return transparent
	}
	if _, ok := respCacheControl["no-cache"]; ok {
		glog.V(logVNum).Infof("stale resp no-cache")
		return stale
	}
	if _, ok := reqCacheControl["only-if-cached"]; ok {
		//glog.V(logVNum).Infof("fresh req only-if-cached")
		return fresh
	}

	date, err := Date(respHeaders)
	if err != nil {
		glog.V(logVNum).Infof("stale resp Date err = %v", err)
		return stale
	}
	currentAge := clock.since(date)

	var lifetime time.Duration
	var zeroDuration time.Duration

	// If a response includes both an Expires header and a max-age directive,
	// the max-age directive overrides the Expires header, even if the Expires header is more restrictive.
	if maxAge, ok := respCacheControl["max-age"]; ok {
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			glog.V(logVNum).Infof("resp max-age err = %v", err)
			lifetime = zeroDuration
		}
	} else {
		expiresHeader := respHeaders.Get("Expires")
		if expiresHeader != "" {
			expires, err := time.Parse(time.RFC1123, expiresHeader)
			if err != nil {
				glog.V(logVNum).Infof("resp Expires err = %v", err)
				lifetime = zeroDuration
			} else {
				lifetime = expires.Sub(date)
			}
		}
	}
	glog.V(logVNum+1).Infof("resp lifetime = %v, currentAge = %v", lifetime, currentAge)

	if maxAge, ok := reqCacheControl["max-age"]; ok {
		// the client is willing to accept a response whose age is no greater than the specified time in seconds
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			glog.V(logVNum).Infof("req max-age err = %v", err)
			lifetime = zeroDuration
		}
	}
	if minfresh, ok := reqCacheControl["min-fresh"]; ok {
		//  the client wants a response that will still be fresh for at least the specified number of seconds.
		minfreshDuration, err := time.ParseDuration(minfresh + "s")
		if err == nil {
			currentAge = time.Duration(currentAge + minfreshDuration)
			glog.V(logVNum).Infof("req min-fresh, currentAge = %v", currentAge)
		}
	}

	if maxstale, ok := reqCacheControl["max-stale"]; ok {
		// Indicates that the client is willing to accept a response that has exceeded its expiration time.
		// If max-stale is assigned a value, then the client is willing to accept a response that has exceeded
		// its expiration time by no more than the specified number of seconds.
		// If no value is assigned to max-stale, then the client is willing to accept a stale response of any age.
		//
		// Responses served only because of a max-stale value are supposed to have a Warning header added to them,
		// but that seems like a  hassle, and is it actually useful? If so, then there needs to be a different
		// return-value available here.
		if maxstale == "" {
			//glog.V(logVNum).Infof("fresh maxstale == \"\"")
			return fresh
		}
		maxstaleDuration, err := time.ParseDuration(maxstale + "s")
		if err == nil {
			currentAge = time.Duration(currentAge - maxstaleDuration)
			glog.V(logVNum).Infof("req max-stale, currentAge = %v", currentAge)
		}
	}

	glog.V(logVNum+1).Infof("req lifetime = %v, currentAge = %v", lifetime, currentAge)

	if lifetime > currentAge {
		//glog.V(logVNum).Infof("fresh %v > %v", lifetime, currentAge)
		return fresh
	}

	return stale
}

// Returns true if either the request or the response includes the stale-if-error
// cache control extension: https://tools.ietf.org/html/rfc5861
func canStaleOnError(respHeaders, reqHeaders http.Header) bool {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)

	var err error
	lifetime := time.Duration(-1)

	if staleMaxAge, ok := respCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}
	if staleMaxAge, ok := reqCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}

	if lifetime >= 0 {
		date, err := Date(respHeaders)
		if err != nil {
			return false
		}
		currentAge := clock.since(date)
		if lifetime > currentAge {
			return true
		}
	}

	return false
}

func getEndToEndHeaders(respHeaders http.Header) []string {
	// These headers are always hop-by-hop
	hopByHopHeaders := map[string]struct{}{
		"Connection":          struct{}{},
		"Keep-Alive":          struct{}{},
		"Proxy-Authenticate":  struct{}{},
		"Proxy-Authorization": struct{}{},
		"Te":                struct{}{},
		"Trailers":          struct{}{},
		"Transfer-Encoding": struct{}{},
		"Upgrade":           struct{}{},
	}

	for _, extra := range strings.Split(respHeaders.Get("connection"), ",") {
		// any header listed in connection, if present, is also considered hop-by-hop
		if strings.Trim(extra, " ") != "" {
			hopByHopHeaders[http.CanonicalHeaderKey(extra)] = struct{}{}
		}
	}
	endToEndHeaders := []string{}
	for respHeader, _ := range respHeaders {
		if _, ok := hopByHopHeaders[respHeader]; !ok {
			endToEndHeaders = append(endToEndHeaders, respHeader)
		}
	}
	return endToEndHeaders
}

func canStore(reqCacheControl, respCacheControl cacheControl) (canStore bool) {
	if _, ok := respCacheControl["no-store"]; ok {
		return false
	}
	if _, ok := reqCacheControl["no-store"]; ok {
		return false
	}
	return true
}

func newGatewayTimeoutResponse(req *http.Request) *http.Response {
	var braw bytes.Buffer
	braw.WriteString("HTTP/1.1 504 Gateway Timeout\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(&braw), req)
	if err != nil {
		panic(err)
	}
	glog.V(logVNum).Infof("newGatewayTimeoutResponse 504")
	return resp
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
// (This function copyright goauth2 authors: https://code.google.com/p/goauth2)
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}

type cacheControl map[string]string

func parseCacheControl(headers http.Header) cacheControl {
	cc := cacheControl{}
	ccHeader := headers.Get("Cache-Control")
	for _, part := range strings.Split(ccHeader, ",") {
		part = strings.Trim(part, " ")
		if part == "" {
			continue
		}
		if strings.ContainsRune(part, '=') {
			keyval := strings.Split(part, "=")
			cc[strings.Trim(keyval[0], " ")] = strings.Trim(keyval[1], ",")
		} else {
			cc[part] = ""
		}
	}
	return cc
}

// headerAllCommaSepValues returns all comma-separated values (each
// with whitespace trimmed) for header name in headers. According to
// Section 4.2 of the HTTP/1.1 spec
// (http://www.w3.org/Protocols/rfc2616/rfc2616-sec4.html#sec4.2),
// values from multiple occurrences of a header should be concatenated, if
// the header's value is a comma-separated list.
func headerAllCommaSepValues(headers http.Header, name string) []string {
	var vals []string
	for _, val := range headers[http.CanonicalHeaderKey(name)] {
		fields := strings.Split(val, ",")
		for i, f := range fields {
			fields[i] = strings.TrimSpace(f)
		}
		vals = append(vals, fields...)
	}
	//glog.V(logVNum).Infof("headerAllCommaSepValues vals = %v", vals)
	return vals
}

// compare time old
// time1 == time2 return 0
// time1 > time2 return 1
//time1 < time2 return -1
// if  time1 == "" or time2 == "" return error
func headerTimeCmp(time1, time2 string) (int, error) {
	if time1 == "" || time2 == "" {
		return 0, fmt.Errorf("time1 || time2 empty")
	}

	t1, err := time.Parse(TimeFormat, time1)
	if err != nil {
		if time1 == time2 {
			return 0, nil
		} else {
			glog.V(logVNum).Infof("headerTimeCmp err = %v", err)
			return 0, err
		}
	}

	t2, err := time.Parse(TimeFormat, time2)
	if err != nil {
		if time1 == time2 {
			return 0, nil
		} else {
			glog.V(logVNum).Infof("headerTimeCmp err = %v", err)
			return 0, err
		}
	}

	if t1.Equal(t2) {
		return 0, nil
	} else if t1.After(t2) {
		return 1, nil
	} else {
		return -1, nil
	}
}

// NewMemoryCacheTransport returns a new Transport using the in-memory cache implementation
func NewMemoryCacheTransport() *Transport {
	c := NewMemoryCache()
	t := NewTransport(c)
	return t
}

func NewDiskCacheTransport() *Transport {
	tempDir, err := ioutil.TempDir("", "httpdiskcache")
	if err != nil {
		glog.Errorf("TempDir err = %v", err)
		return nil
	}
	glog.Infof("NewDiskCacheTransport tempDir = %v", tempDir)
	c := diskcache.New(tempDir)
	t := NewTransport(c)
	return t
}

func NewLeveldbCacheTransport() *Transport {
	tempDir, err := ioutil.TempDir("", "httpleveldbcache")
	if err != nil {
		glog.Errorf("TempDir err = %v", err)
		return nil
	}
	glog.Infof("NewLeveldbCacheTransport tempDir = %v", tempDir)
	c, err := leveldbcache.New(fmt.Sprintf("%s%c%s", tempDir, os.PathSeparator, "db"))
	if err != nil {
		glog.Errorf("New err = %v", err)
		return nil
	}
	t := NewTransport(c)
	return t
}

func NewMemCacheTransport(server []string) *Transport {
	c := memcache.New(server...)
	t := NewTransport(c)
	return t
}

func NewLruCacheTransport(capacity uint) *Transport {
	c := lrucache.New(capacity)
	t := NewTransport(c)
	return t
}

func RespNotModified(cachedResp *http.Response, req *http.Request) (resp *http.Response, err error) {
	//glog.V(logVNum).Infof("RespNotModified: resp = %v", cachedResp.Header)
	//glog.V(logVNum).Infof("RespNotModified: req= %v", req.Header)

	resp = &http.Response{
		Status:        http.StatusText(http.StatusNotModified),
		StatusCode:    http.StatusNotModified,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        cachedResp.Header,
		Request:       req,
		Close:         true,
		ContentLength: -1,
	}
	//glog.V(logVNum).Infof("RespNotModified url = %v", req.URL.String())
	return resp, err
}
