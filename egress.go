package waxseal

import (
	"container/list"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/colespringer/waxseal/internal/httpx"
)

// defaultEgressCacheSize caps cached per-egress clients. A small LRU is enough
// for typical proxy/source-address sets and bounds idle transports.
const defaultEgressCacheSize = 64

// DerivedID returns a stable key for the egress-affecting fields. Matching
// proxy/source/TLS settings share a transport, cookie jar, and warm minter;
// different settings stay isolated. An all-empty spec yields "".
func (s EgressSpec) DerivedID() string {
	if s.Proxy == "" && s.SourceAddress == "" && !s.DisableTLSVerify {
		return ""
	}
	h := sha256.Sum256([]byte(s.Proxy + "\x00" + s.SourceAddress + "\x00" + strconv.FormatBool(s.DisableTLSVerify)))
	return hex.EncodeToString(h[:])[:16]
}

// BuildEgressTransport builds an *http.Transport for the proxy, source IP, and
// TLS settings in spec. The server and CLI use it as Options.EgressTransport;
// in-process callers normally provide Options.HTTPClient instead.
func BuildEgressTransport(spec EgressSpec) (http.RoundTripper, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if spec.SourceAddress != "" {
		ip := net.ParseIP(spec.SourceAddress)
		if ip == nil {
			return nil, fmt.Errorf("waxseal: invalid source address %q", spec.SourceAddress)
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}

	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if spec.Proxy != "" {
		u, err := url.Parse(spec.Proxy)
		if err != nil {
			return nil, fmt.Errorf("waxseal: invalid proxy URL %q: %w", spec.Proxy, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if spec.DisableTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per-egress override
	}
	return tr, nil
}

// egressCache is a bounded LRU of per-egress *httpx.Clients. Each entry has its
// own transport and cookie jar because Create and GenerateIT must share cookies.
// Eviction closes idle connections.
type egressCache struct {
	mu      sync.Mutex
	max     int
	timeout time.Duration
	ll      *list.List               // front = most-recently used
	byKey   map[string]*list.Element // key -> element holding *egressEntry
}

type egressEntry struct {
	key    string
	client *httpx.Client
}

func newEgressCache(max int, timeout time.Duration) *egressCache {
	if max <= 0 {
		max = defaultEgressCacheSize
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &egressCache{max: max, timeout: timeout, ll: list.New(), byKey: make(map[string]*list.Element)}
}

// getOrBuild returns the cached client for spec, building one on a miss. The
// build runs under the lock to avoid duplicate entries for the same key.
func (c *egressCache) getOrBuild(spec EgressSpec, build func(EgressSpec) (http.RoundTripper, error)) (*httpx.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Prefer the caller's ID. If it is empty, derive one from the egress fields so
	// direct cache users do not collapse distinct proxy/source/TLS settings.
	key := spec.ID
	if key == "" {
		key = spec.DerivedID()
	}
	if el, ok := c.byKey[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*egressEntry).client, nil
	}

	rt, err := build(spec)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Transport: rt, Jar: jar, Timeout: c.timeout}
	client := httpx.New(hc)

	el := c.ll.PushFront(&egressEntry{key: key, client: client})
	c.byKey[key] = el
	c.evictLocked()
	return client, nil
}

// evictLocked drops least-recently-used entries past the cap, closing their idle
// connections.
func (c *egressCache) evictLocked() {
	for len(c.byKey) > c.max {
		el := c.ll.Back()
		if el == nil {
			return
		}
		ent := el.Value.(*egressEntry)
		c.ll.Remove(el)
		delete(c.byKey, ent.key)
		ent.client.HTTP.CloseIdleConnections()
	}
}

// closeAll closes idle connections for every cached client (Client shutdown).
func (c *egressCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, el := range c.byKey {
		el.Value.(*egressEntry).client.HTTP.CloseIdleConnections()
	}
	c.ll.Init()
	c.byKey = make(map[string]*list.Element)
}
