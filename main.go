package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	svg "github.com/ajstarks/svgo"
	"github.com/joho/godotenv"
	"github.com/lsnnt/spotify-banner-for-github/pathsconst"
)

// global token caching
var (
	cachedToken string
	tokenMu     sync.Mutex
)

const (
	clientID string = "cfe923b2d660439caf2b557b21f31221"
)

// Base URLs are package-level variables so tests can point them at fake servers.
var (
	spotifyAPIBase      = "https://api.spotify.com"
	spotifyAccountsBase = "https://accounts.spotify.com"
)

func getCachedToken() (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if cachedToken == "" {
		token, err := getToken()
		if err != nil {
			return "", fmt.Errorf("getting token: %w", err)
		}
		cachedToken = token
	}
	return cachedToken, nil
}

func invalidateToken() {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cachedToken = ""
}

// Global http client
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 10,
	},
}

// http Handler function to handle the request
func myHandler(w http.ResponseWriter, r *http.Request) {
	grid, width, err := parseViewParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tracks, err := getTracks()
	if err != nil {
		log.Println(err) // log it but don't crash
		http.Error(w, fmt.Sprintf("failed to fetch tracks: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Add("Content-Type", "image/svg+xml")
	// as per my research currrently 40 api request allowed per day so caching accordingly
	w.Header().Add("Cache-Control", "public, max-age=2160")
	if grid {
		renderGrid(w, tracks, width)
		return
	}
	renderList(w, tracks)
}

// renderList renders the original plain list of track names.
func renderList(w http.ResponseWriter, tracks []TrackInfo) {
	s := svg.New(w)
	width, height := 700, 810
	s.Start(width, height)
	s.Rect(0, 0, width, height, "fill:#79A7F2")
	s.Image(17, 2, 100, 100, pathsconst.Imagelogo())
	s.Path(pathsconst.Topheader())
	if len(tracks) == 0 {
		s.Text(width/2, height/2, "No recent tracks found", "text-anchor:middle;font-family:Arial, sans-serif;font-size:20px;")
	} else {
		maxItems := 20
		if len(tracks) < maxItems {
			maxItems = len(tracks)
		}
		for i := 0; i < maxItems; i++ {
			text := tracks[i].Name
			if len(text) > 50 {
				text = text[:47] + "..."
			}
			s.Rect(67, 114+i*35, 589, 24, "fill:#D9D9D9")
			s.Text(79, 131+i*35, fmt.Sprintf("%d. %s", i+1, text), "font-family:Arial, sans-serif;font-size:16px;")
		}
	}
	s.End()
}

// function to generate random 64 bit string Code Verifier
func getCodeVerifier() string {
	byts := make([]byte, 64)
	_, err := rand.Read(byts)
	if err != nil {
		panic(err)
	}
	token := base64.RawURLEncoding.EncodeToString(byts)
	return token
}

// Function to generate Code Challange
func getCodeChallange(codeverifier string) string {
	shaenc := sha256.Sum256([]byte(codeverifier))
	return base64.RawURLEncoding.EncodeToString(shaenc[:])
}

// Gets the bearer api token
func getApiToken(code string, codeverifier string) (string, error) {
	baseurl := spotifyAccountsBase + "/api/token"
	params := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://developer.spotify.com"},
		"code_verifier": {codeverifier},
	}
	req, err := http.NewRequest("POST", baseurl, strings.NewReader(params.Encode()))
	if err != nil {
		return "", fmt.Errorf("Error building request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error making request: %w", err)
	}
	defer resp.Body.Close()
	bodyText, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Error reading response: %w", err)
	}
	parts := strings.Split(string(bodyText), "\"access_token\":\"")
	if len(parts) < 2 {
		return "", fmt.Errorf("no access_token in token response")
	}
	token := strings.Split(parts[1], "\",")[0]
	return token, nil
}

// Gets the bearer token necessary for the api calling implementing the Oauth PKCE flow
func getToken() (string, error) {
	// Needed for PKCE auth
	codever := getCodeVerifier()
	codechal := getCodeChallange(codever)
	// read https://developer.spotify.com/documentation/web-api/tutorials/code-pkce-flow

	baseURL := spotifyAccountsBase + "/oauth2/v2/auth"
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"scope":                 {"email openid profile user-self-provisioning playlist-modify-private playlist-modify-public playlist-read-collaborative playlist-read-private ugc-image-upload user-follow-modify user-follow-read user-library-modify user-library-read user-modify-playback-state user-read-currently-playing user-read-email user-read-playback-position user-read-playback-state user-read-private user-read-recently-played user-top-read user-personalized"},
		"redirect_uri":          {"https://developer.spotify.com"},
		"code_challenge":        {codechal},
		"code_challenge_method": {"S256"},
		"state":                 {"x1X0-Qo96pi118lEr8s0MZlVQ_lgVfCW"}, //optional
		"response_mode":         {"web_message"},
		"prompt":                {"none"},
	}
	authUrl := baseURL + "?" + params.Encode()
	req, err := http.NewRequest("GET", authUrl, nil)
	if err != nil {
		return "", err
	}
	spdccok := os.Getenv("SPDC")
	req.Header.Set("Cookie", "sp_dc="+spdccok)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error doing the request %w", err)
	}
	defer resp.Body.Close()
	bodyText, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("Error reading the response %w", err)
	}
	parts := strings.Split(string(bodyText), "\"code\": \"")
	if len(parts) < 2 {
		return "", fmt.Errorf("The sp_dc cookie is expired or invalid or env variable is not set: %x", err)
	}
	code := strings.Split(parts[1], "\"")[0]
	token, err := getApiToken(code, codever)
	if err != nil {
		return "", err
	}
	return token, nil
}

func main() {
	go func() {
		if err := godotenv.Load(); err != nil {
			log.Print("failed to load .env: using envirenment SPDC variable if possible", err)
		}
		getCachedToken()
	}()
	// Required for vercel
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	s := &http.Server{
		Addr:         ":" + port,
		Handler:      http.HandlerFunc(myHandler),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Fatal(s.ListenAndServe())
}
