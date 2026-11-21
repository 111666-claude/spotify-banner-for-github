package main

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Cover images are downloaded server-side, validated, and embedded into the
// SVG as data URIs so visitors never have to request third-party URLs.
const (
	// maxCoverBytes bounds a single cover download; anything larger is
	// rejected as a malicious (or simply wasteful) image.
	maxCoverBytes = 512 * 1024
	// maxCoverCacheEntries bounds the in-memory cover cache.
	maxCoverCacheEntries = 512
)

// coverCache maps a cover URL to its data URI. An empty string is a negative
// entry: the cover is known to be missing/invalid and a placeholder is drawn
// instead of refetching.
var (
	coverCache = map[string]string{}
	coverMu    sync.Mutex
)

// coverClient has a hard timeout so a hung cover host cannot stall rendering.
var coverClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 10,
	},
}

// allowedCoverTypes maps the sniffed content types we accept as covers.
var allowedCoverTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// fetchCoverDataURI returns the cached or freshly downloaded data URI for a
// cover image. ok is false when the cover is missing, unreachable, too large,
// or not actually an image — callers should draw a placeholder in that case.
func fetchCoverDataURI(rawurl string) (uri string, ok bool) {
	coverMu.Lock()
	if cached, hit := coverCache[rawurl]; hit {
		coverMu.Unlock()
		return cached, cached != ""
	}
	coverMu.Unlock()

	uri = downloadCover(rawurl)

	coverMu.Lock()
	if len(coverCache) < maxCoverCacheEntries {
		coverCache[rawurl] = uri
	}
	coverMu.Unlock()
	return uri, uri != ""
}

// downloadCover fetches one cover and returns its data URI, or "" on any
// validation failure. Never trusts the server-declared Content-Type: the
// actual bytes are sniffed instead.
func downloadCover(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "" // dead or non-HTTP cover URL
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return ""
	}
	resp, err := coverClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	if resp.ContentLength > maxCoverBytes {
		return "" // announced too large: don't even read the body
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxCoverBytes {
		return "" // unreadable, empty, or lied about its size
	}
	ct := http.DetectContentType(data)
	if !allowedCoverTypes[ct] {
		return "" // not actually an image
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
}
