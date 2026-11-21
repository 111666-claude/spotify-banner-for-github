package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// resetState clears all package-level caches between tests.
func resetState(t *testing.T) {
	t.Helper()
	tokenMu.Lock()
	cachedToken = ""
	tokenMu.Unlock()
	musicMu.Lock()
	musicCache = nil
	musicMu.Unlock()
	coverMu.Lock()
	coverCache = map[string]string{}
	coverMu.Unlock()
}

// useBackends points the Spotify base URLs at a fake server for the test.
func useBackends(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldAPI, oldAccounts := spotifyAPIBase, spotifyAccountsBase
	spotifyAPIBase, spotifyAccountsBase = srv.URL, srv.URL
	t.Cleanup(func() {
		spotifyAPIBase, spotifyAccountsBase = oldAPI, oldAccounts
	})
}

// tinyPNG returns a valid small PNG encoded in memory.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{uint8(60 * x), uint8(60 * y), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding png: %v", err)
	}
	return buf.Bytes()
}

// trackEntry describes one fake recently-played track. cover may be a path on
// the fake cover server, an absolute URL, or "" for "no album images".
type trackEntry struct {
	name   string
	artist string
	cover  string
}

// tracksJSON builds a Spotify recently-played response body.
func tracksJSON(coverBase string, entries ...trackEntry) string {
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i, e := range entries {
		if i > 0 {
			sb.WriteString(",")
		}
		album := `"album":{"images":[]}`
		if e.cover != "" {
			u := e.cover
			if !strings.HasPrefix(u, "http") {
				u = coverBase + u
			}
			album = fmt.Sprintf(`"album":{"images":[{"url":"%s","width":640,"height":640},{"url":"%s","width":64,"height":64}]}`, u+"?big", u)
		}
		fmt.Fprintf(&sb, `{"track":{"name":%q,"artists":[{"name":%q}],%s}}`, e.name, e.artist, album)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// fakeSpotify is a configurable fake for the Spotify API + accounts servers.
type fakeSpotify struct {
	srv *httptest.Server

	recentlyPlayed func(w http.ResponseWriter, r *http.Request)

	apiCalls   int32
	authCalls  int32
	tokenCalls int32
}

func newFakeSpotify(t *testing.T) *fakeSpotify {
	t.Helper()
	f := &fakeSpotify{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/me/player/recently-played", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.apiCalls, 1)
		if f.recentlyPlayed != nil {
			f.recentlyPlayed(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"items":[]}`)
	})
	mux.HandleFunc("/oauth2/v2/auth", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.authCalls, 1)
		io.WriteString(w, `{"code": "fake-auth-code"}`)
	})
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.tokenCalls, 1)
		io.WriteString(w, `{"access_token":"tok-fresh","token_type":"Bearer"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// fakeCovers is a fake cover-art host with good, oversized, spoofed, and
// missing image endpoints, plus a hit counter.
type fakeCovers struct {
	srv  *httptest.Server
	hits int32
}

func newFakeCovers(t *testing.T) *fakeCovers {
	t.Helper()
	f := &fakeCovers{}
	pngBytes := tinyPNG(t)
	mux := http.NewServeMux()
	count := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.hits, 1)
	}
	mux.HandleFunc("/ok/", func(w http.ResponseWriter, r *http.Request) {
		count(w, r)
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		count(w, r)
		// Valid PNG magic followed by far more than maxCoverBytes of data:
		// sniffs as an image but must be rejected on size.
		body := make([]byte, maxCoverBytes+1024)
		copy(body, pngBytes)
		w.Header().Set("Content-Type", "image/png")
		w.Write(body)
	})
	mux.HandleFunc("/spoof", func(w http.ResponseWriter, r *http.Request) {
		count(w, r)
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "<html><body>not an image at all</body></html>")
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) {
		count(w, r)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCovers) url(path string) string { return f.srv.URL + path }

func doRequest(target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	myHandler(rec, req)
	return rec
}

// --- Tests ---

func TestListModeKeepsOriginalLayout(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tracksJSON(covers.srv.URL,
			trackEntry{"Song A", "Artist A", "/ok/a.png"},
			trackEntry{"Song B", "Artist B", "/ok/b.png"},
		))
	}
	useBackends(t, sp.srv)

	rec := doRequest("/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`width="700" height="810"`,
		`>1. Song A</text>`,
		`>2. Song B</text>`,
		`x="67" y="114" width="589" height="24"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list SVG missing %q", want)
		}
	}
	// List mode must not fetch or embed covers.
	if got := atomic.LoadInt32(&covers.hits); got != 0 {
		t.Errorf("cover server hit %d times in list mode, want 0", got)
	}
	if strings.Contains(body, "data:image") && strings.Count(body, "data:image") > 1 {
		t.Errorf("list SVG embeds cover data URIs")
	}
}

func TestGridPositionsAndEmbedding(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tracksJSON(covers.srv.URL,
			trackEntry{"Song A", "Artist A", "/ok/a.png"},
			trackEntry{"Song B", "Artist B", "/ok/b.png"},
			trackEntry{"Song A", "Artist A", "/ok/a.png"}, // duplicate play
			trackEntry{"Song C", "Artist C", "/ok/c.png"},
			trackEntry{"Song D", "Artist D", "/ok/d.png"},
			trackEntry{"Song E", "Artist E", "/ok/e.png"},
		))
	}
	useBackends(t, sp.srv)

	rec := doRequest("/?grid=1&width=700")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// 700px canvas → 4 columns; 5 unique tracks → 2 rows.
	// Row pitch = 128 (cell) + 36 (labels) + 16 (gap) = 180.
	wantPos := []struct{ x, y int }{
		{24, 110},  // Song A
		{168, 110}, // Song B
		{312, 110}, // Song C
		{456, 110}, // Song D
		{24, 290},  // Song E, second row
	}
	for _, p := range wantPos {
		frag := fmt.Sprintf(`<image x="%d" y="%d" width="128" height="128"`, p.x, p.y)
		if !strings.Contains(body, frag) {
			t.Errorf("grid missing cover at (%d,%d): %q not found", p.x, p.y, frag)
		}
	}

	// Covers are embedded as data URIs; nothing points at a third-party URL.
	if !strings.Contains(body, `xlink:href="data:image/png;base64,`) {
		t.Errorf("grid SVG does not embed cover data URIs")
	}
	if strings.Contains(body, `xlink:href="http`) {
		t.Errorf("grid SVG references an external image URL")
	}

	// Duplicate track rendered once; names and artists shown.
	if n := strings.Count(body, "Song A"); n != 1 {
		t.Errorf("duplicate track rendered %d times, want 1", n)
	}
	for _, want := range []string{"Song B", "Artist C", "Song E", "Artist E"} {
		if !strings.Contains(body, want) {
			t.Errorf("grid SVG missing label %q", want)
		}
	}
}

func TestGridCoverCacheHit(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tracksJSON(covers.srv.URL,
			trackEntry{"Song A", "Artist A", "/ok/a.png"},
			trackEntry{"Song B", "Artist B", "/ok/b.png"},
		))
	}
	useBackends(t, sp.srv)

	if rec := doRequest("/?grid=1"); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(&covers.hits); got != 2 {
		t.Fatalf("after first render: cover hits = %d, want 2", got)
	}
	rec := doRequest("/?grid=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("second request: status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(&covers.hits); got != 2 {
		t.Errorf("after second render: cover hits = %d, want 2 (cache miss)", got)
	}
	if !strings.Contains(rec.Body.String(), `xlink:href="data:image/png;base64,`) {
		t.Errorf("cached render lost embedded covers")
	}
}

func TestGridInvalidCoversGetPlaceholder(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tracksJSON(covers.srv.URL,
			trackEntry{"Good", "Artist G", "/ok/g.png"},
			trackEntry{"Too Big", "Artist B", "/big"},
			trackEntry{"Spoofed", "Artist S", "/spoof"},
			trackEntry{"Gone", "Artist X", "/gone"},
			trackEntry{"No Images", "Artist N", ""},
			trackEntry{"Bad Scheme", "Artist Y", "ftp://example.com/c.png"},
		))
	}
	useBackends(t, sp.srv)

	rec := doRequest("/?grid=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Exactly one real cover embedded (the good one). The 128px size
	// distinguishes grid covers from the 100px logo, also a data URI.
	if n := strings.Count(body, `height="128" xlink:href="data:image`); n != 1 {
		t.Errorf("embedded covers = %d, want 1 (only the valid one)", n)
	}
	// Five placeholder tiles with the music-note glyph.
	if n := strings.Count(body, ">♪</text>"); n != 5 {
		t.Errorf("placeholder tiles = %d, want 5", n)
	}
	// The oversized image must not bloat the SVG.
	if rec.Body.Len() > 100*1024 {
		t.Errorf("SVG size = %d bytes, oversized image leaked into output", rec.Body.Len())
	}
	// All tracks still listed by name.
	for _, name := range []string{"Too Big", "Spoofed", "Gone", "No Images", "Bad Scheme"} {
		if !strings.Contains(body, name) {
			t.Errorf("track %q missing from grid", name)
		}
	}

	// Failures are negatively cached: a second render refetches nothing.
	hits := atomic.LoadInt32(&covers.hits)
	doRequest("/?grid=1")
	if got := atomic.LoadInt32(&covers.hits); got != hits {
		t.Errorf("failed covers refetched: hits %d → %d", hits, got)
	}
}

func TestGridNarrowCanvasRejected(t *testing.T) {
	resetState(t)
	sp := newFakeSpotify(t)
	useBackends(t, sp.srv)

	for _, target := range []string{"/?grid=1&width=100", "/?grid=1&width=-5", "/?grid=1&width=abc"} {
		rec := doRequest(target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, rec.Code)
		}
	}
	rec := doRequest("/?grid=1&width=100")
	if !strings.Contains(rec.Body.String(), "too narrow") {
		t.Errorf("narrow canvas error message missing 'too narrow': %q", rec.Body.String())
	}
	// Exactly at the minimum works.
	if rec := doRequest("/?grid=1&width=176"); rec.Code != http.StatusOK {
		t.Errorf("minimum width 176: status = %d, want 200", rec.Code)
	}
}

func TestTokenExpiryTriggersRefresh(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, tracksJSON(covers.srv.URL, trackEntry{"Fresh Song", "Fresh Artist", "/ok/f.png"}))
	}
	useBackends(t, sp.srv)

	// Seed a stale token that the API will reject.
	tokenMu.Lock()
	cachedToken = "tok-stale"
	tokenMu.Unlock()

	rec := doRequest("/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Fresh Song") {
		t.Errorf("response missing track fetched with refreshed token")
	}
	if got := atomic.LoadInt32(&sp.apiCalls); got != 2 {
		t.Errorf("API calls = %d, want 2 (401 then retry)", got)
	}
	if got := atomic.LoadInt32(&sp.tokenCalls); got != 1 {
		t.Errorf("token refreshes = %d, want 1", got)
	}
}

func TestRateLimitFallsBackToCachedTracks(t *testing.T) {
	resetState(t)
	covers := newFakeCovers(t)
	sp := newFakeSpotify(t)
	limited := false
	sp.recentlyPlayed = func(w http.ResponseWriter, r *http.Request) {
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, tracksJSON(covers.srv.URL, trackEntry{"Cached Song", "Cached Artist", "/ok/c.png"}))
	}
	useBackends(t, sp.srv)

	if rec := doRequest("/"); rec.Code != http.StatusOK {
		t.Fatalf("priming request: status = %d", rec.Code)
	}
	limited = true
	rec := doRequest("/")
	if rec.Code != http.StatusOK {
		t.Fatalf("rate-limited request: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Cached Song") {
		t.Errorf("rate-limited response missing cached track")
	}
}

func TestGridEmptyTracks(t *testing.T) {
	resetState(t)
	sp := newFakeSpotify(t) // default handler returns zero items
	useBackends(t, sp.srv)

	rec := doRequest("/?grid=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No recent tracks found") {
		t.Errorf("empty grid missing fallback message")
	}
}

func TestGridLayoutMath(t *testing.T) {
	lay := computeGridLayout(700, 5)
	if lay.cols != 4 {
		t.Errorf("cols = %d, want 4", lay.cols)
	}
	if lay.rows != 2 {
		t.Errorf("rows = %d, want 2", lay.rows)
	}
	if x, y := lay.cellOrigin(4); x != 24 || y != 290 {
		t.Errorf("cellOrigin(4) = (%d,%d), want (24,290)", x, y)
	}
	wantH := 110 + 2*(128+36) + 16 + 24
	if lay.height != wantH {
		t.Errorf("height = %d, want %d", lay.height, wantH)
	}
	// Minimum-width canvas: exactly one column.
	lay = computeGridLayout(minGridWidth, 3)
	if lay.cols != 1 || lay.rows != 3 {
		t.Errorf("min-width layout = %dx%d, want 1 col x 3 rows", lay.cols, lay.rows)
	}
}

func TestDedupeTracks(t *testing.T) {
	in := []TrackInfo{
		{Name: "A", Artist: "X"},
		{Name: "a", Artist: "x"}, // case-insensitive duplicate
		{Name: "A", Artist: "Y"}, // same name, different artist: kept
		{Name: "B", Artist: "X"},
	}
	out := dedupeTracks(in)
	if len(out) != 3 {
		t.Fatalf("dedupe kept %d tracks, want 3", len(out))
	}
	if out[1].Artist != "Y" || out[2].Name != "B" {
		t.Errorf("unexpected dedupe order: %+v", out)
	}
}
