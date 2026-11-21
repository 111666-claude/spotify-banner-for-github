package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The json objects decoder structs
type Artist struct {
	Name string `json:"name"`
}

type SpotifyImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Album struct {
	Images []SpotifyImage `json:"images"`
}

type Track struct {
	Name    string   `json:"name"`
	Artists []Artist `json:"artists"`
	Album   Album    `json:"album"`
}

type Item struct {
	Track Track `json:"track"`
}

type RecentlyPlayedResponse struct {
	Items []Item `json:"items"`
}

// TrackInfo is the flattened, render-ready view of a recently played track.
type TrackInfo struct {
	Name     string
	Artist   string
	CoverURL string
}

var (
	musicCache []TrackInfo
	musicMu    sync.RWMutex
)

// Cached music function, used as a fallback when the Spotify API rate-limits us.
func getCachedMusic() []TrackInfo {
	musicMu.RLock()
	defer musicMu.RUnlock()

	if musicCache == nil {
		return nil
	}
	return append([]TrackInfo(nil), musicCache...)
}

// artistNames joins the track's artist names into a single display string.
func artistNames(t Track) string {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// pickCoverURL selects the smallest album image that is still big enough to
// look decent in a grid cell, falling back to any available image.
func pickCoverURL(images []SpotifyImage) string {
	best := ""
	bestWidth := int(^uint(0) >> 1)
	for _, img := range images {
		if img.URL == "" {
			continue
		}
		w := img.Width
		if w == 0 {
			w = 64 // unknown size: assume the small default
		}
		if w >= 64 && w < bestWidth {
			best, bestWidth = img.URL, w
		}
	}
	if best == "" {
		// no image met the preferred size: take any non-empty URL
		for _, img := range images {
			if img.URL != "" {
				return img.URL
			}
		}
	}
	return best
}

// dedupeTracks drops repeated plays of the same track (same name + artist,
// case-insensitive), keeping the most recent occurrence's position.
func dedupeTracks(tracks []TrackInfo) []TrackInfo {
	seen := make(map[string]struct{}, len(tracks))
	out := make([]TrackInfo, 0, len(tracks))
	for _, t := range tracks {
		key := strings.ToLower(t.Name + "\x00" + t.Artist)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}

// getTracks returns the 20 most recently played tracks. On a 401 the cached
// token is invalidated and the request retried once with a fresh token; on a
// 429 the last successfully fetched list is returned instead.
func getTracks() ([]TrackInfo, error) {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := getCachedToken()
		if err != nil {
			return nil, fmt.Errorf("getting cached token err: %w", err)
		}

		murl := spotifyAPIBase + "/v1/me/player/recently-played"
		params := url.Values{"limit": {"20"}}
		req, err := http.NewRequest("GET", murl+"?"+params.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		res, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("doing request: %w", err)
		}

		if res.StatusCode == 429 {
			res.Body.Close()
			cached := getCachedMusic()
			return cached, nil
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			invalidateToken() // invalidate token
			continue
		}
		var data RecentlyPlayedResponse
		err = json.NewDecoder(res.Body).Decode(&data)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("Json Decoding err: %w", err)
		}

		tracks := make([]TrackInfo, 0, len(data.Items))
		for _, item := range data.Items {
			tracks = append(tracks, TrackInfo{
				Name:     item.Track.Name,
				Artist:   artistNames(item.Track),
				CoverURL: pickCoverURL(item.Track.Album.Images),
			})
		}

		musicMu.Lock()
		musicCache = append([]TrackInfo(nil), tracks...) // make a copy
		musicMu.Unlock()
		return tracks, nil
	}
	return nil, fmt.Errorf("failed after token refresh, giving up")
}
