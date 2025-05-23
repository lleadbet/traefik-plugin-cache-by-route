// Package traefik_plugin_cache_by_route is a plugin to cache responses to disk.
package traefik_plugin_cache_by_route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/pquerna/cachecontrol"
)

// Config configures the middleware.
type Config struct {
	Path                   string   `json:"path" yaml:"path" toml:"path"`
	MaxExpiry              int      `json:"maxExpiry" yaml:"maxExpiry" toml:"maxExpiry"`
	Cleanup                int      `json:"cleanup" yaml:"cleanup" toml:"cleanup"`
	AddStatusHeader        bool     `json:"addStatusHeader" yaml:"addStatusHeader" toml:"addStatusHeader"`
	AllowedHTTPMethods     []string `json:"allowedHTTPMethods" yaml:"allowedHTTPMethods" toml:"allowedHTTPMethods"`
	SkipCacheControlHeader bool     `json:"skipCacheControlHeader" yaml:"skipCacheControlHeader" toml:"skipCacheControlHeader"`
	DefaultTTL             int      `json:"defaultTTL" yaml:"defaultTTL" toml:"defaultTTL"`
	URIs                   []Uri    `json:"uris" yaml:"uris" toml:"uris"`
}

type Uri struct {
	Pattern string `json:"pattern" yaml:"pattern" toml:"pattern"`
	TTL     int    `json:"ttl" yaml:"ttl" toml:"ttl"`
}

// CreateConfig returns a config instance.
func CreateConfig() *Config {
	return &Config{
		MaxExpiry:              int((5 * time.Minute).Seconds()),
		Cleanup:                int((5 * time.Minute).Seconds()),
		AllowedHTTPMethods:     []string{"GET", "HEAD"},
		DefaultTTL:             0,
		SkipCacheControlHeader: false,
		AddStatusHeader:        true,
	}
}

const (
	cacheHeader      = "Cache-Status"
	cacheHitStatus   = "hit"
	cacheMissStatus  = "miss"
	cacheErrorStatus = "error"
)

type cache struct {
	name   string
	cache  *fileCache
	cfg    *Config
	uriMap map[*regexp.Regexp]int
	next   http.Handler
}

// New returns a plugin instance.
func New(_ context.Context, next http.Handler, cfg *Config, name string) (http.Handler, error) {
	if cfg.MaxExpiry <= 1 {
		return nil, errors.New("maxExpiry must be greater or equal to 1")
	}

	if cfg.Cleanup <= 1 {
		return nil, errors.New("cleanup must be greater or equal to 1")
	}

	fc, err := newFileCache(cfg.Path, time.Duration(cfg.Cleanup)*time.Second)
	if err != nil {
		return nil, err
	}

	uriMap := make(map[*regexp.Regexp]int)
	for _, uri := range cfg.URIs {
		re, err := regexp.Compile(uri.Pattern)
		if err != nil {
			continue // skip invalid regex patterns to avoid crashing the plugin
		}
		uriMap[re] = uri.TTL
	}

	m := &cache{
		name:   name,
		cache:  fc,
		cfg:    cfg,
		uriMap: uriMap,
		next:   next,
	}

	return m, nil
}

type cacheData struct {
	ExpiresAt time.Time
	Status    int
	Headers   map[string][]string
	Body      []byte
}

// ServeHTTP serves an HTTP request.
func (m *cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cs := cacheMissStatus

	key := cacheKey(r)

	b, err := m.cache.Get(key)
	if err == nil {
		var data cacheData

		err := json.Unmarshal(b, &data)
		if err != nil {
			cs = cacheErrorStatus
		} else {
			for key, vals := range data.Headers {
				for _, val := range vals {
					w.Header().Add(key, val)
				}
			}
			if m.cfg.AddStatusHeader {
				maxAge := data.ExpiresAt.Sub(time.Now()).Seconds()
				w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", int(maxAge)))
				w.Header().Set(cacheHeader, cacheHitStatus)
			}
			w.WriteHeader(data.Status)
			_, _ = w.Write(data.Body)
			return
		}
	}

	if m.cfg.AddStatusHeader {
		w.Header().Set(cacheHeader, cs)
	}

	rw := &responseWriter{ResponseWriter: w}
	m.next.ServeHTTP(rw, r)

	expiry, ok := m.cacheable(r, w, rw.status)
	if !ok {
		return
	}

	data := cacheData{
		ExpiresAt: time.Now().Add(expiry),
		Status:    rw.status,
		Headers:   w.Header(),
		Body:      rw.body,
	}

	b, err = json.Marshal(data)
	if err != nil {
		log.Printf("Error serializing cache item: %v", err)
	}

	if err = m.cache.Set(key, b, expiry); err != nil {
		log.Printf("Error setting cache item: %v", err)
	}
}

func (m *cache) cacheable(r *http.Request, w http.ResponseWriter, status int) (time.Duration, bool) {
	if !m.cfg.SkipCacheControlHeader {
		reasons, expireBy, err := cachecontrol.CachableResponseWriter(r, status, w, cachecontrol.Options{})
		if err != nil || len(reasons) > 0 {
			return 0, false
		}

		if m.cfg.SkipCacheControlHeader {
			expireBy = time.Now().Add(time.Duration(m.cfg.DefaultTTL) * time.Second)
		}

		expiry := time.Until(expireBy)
		maxExpiry := time.Duration(m.cfg.MaxExpiry) * time.Second

		if maxExpiry < expiry {
			expiry = maxExpiry
		}

		return expiry, true
	}

	requestUrl := r.URL.String()
	for re, ttl := range m.uriMap {
		if re.MatchString(requestUrl) {
			expiry := time.Duration(ttl) * time.Second
			maxExpiry := time.Duration(m.cfg.MaxExpiry) * time.Second

			if maxExpiry < expiry {
				expiry = maxExpiry
			}

			return expiry, true
		}
	}
	if m.cfg.DefaultTTL > 0 {
		expiry := time.Duration(m.cfg.DefaultTTL) * time.Second
		maxExpiry := time.Duration(m.cfg.MaxExpiry) * time.Second

		if maxExpiry < expiry {
			expiry = maxExpiry
		}

		return expiry, true
	}
	return 0, false
}

func cacheKey(r *http.Request) string {
	return r.Method + r.Host + r.URL.Path
}

type responseWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (rw *responseWriter) Header() http.Header {
	return rw.ResponseWriter.Header()
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	rw.body = append(rw.body, p...)
	return rw.ResponseWriter.Write(p)
}

func (rw *responseWriter) WriteHeader(s int) {
	rw.status = s
	rw.ResponseWriter.WriteHeader(s)
}
