package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Subsonic client
// ---------------------------------------------------------------------------

type Subsonic struct {
	baseURL  string
	user     string
	password string
	legacy   bool
	client   *http.Client
}

func NewSubsonic(baseURL, user, password string, legacy bool) *Subsonic {
	return &Subsonic{
		baseURL:  strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		legacy:   legacy,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *Subsonic) params(extra url.Values) url.Values {
	p := url.Values{}
	if s.legacy {
		p.Set("u", s.user)
		p.Set("p", s.password)
	} else {
		saltBytes := make([]byte, 8)
		rand.Read(saltBytes)
		salt := hex.EncodeToString(saltBytes)
		sum := md5.Sum([]byte(s.password + salt))
		p.Set("u", s.user)
		p.Set("t", hex.EncodeToString(sum[:]))
		p.Set("s", salt)
	}
	for k, vs := range extra {
		for _, v := range vs {
			p.Add(k, v)
		}
	}
	p.Set("f", "json")
	p.Set("v", "1.16.1")
	p.Set("c", "baton")
	return p
}

func (s *Subsonic) get(method string, extra url.Values) (map[string]any, error) {
	u := fmt.Sprintf("%s/rest/%s.view", s.baseURL, method) + "?" + s.params(extra).Encode()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		SubsonicResponse struct {
			Status        string         `json:"status"`
			Error         map[string]any `json:"error"`
			PlayQueue     *PlayQueue     `json:"playQueue"`
			SearchResult3 *struct {
				Song []map[string]any `json:"song"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("%s: bad json: %w", method, err)
	}
	sr := wrapper.SubsonicResponse
	if sr.Status != "ok" {
		return nil, fmt.Errorf("%s failed: %v", method, sr.Error)
	}
	out := map[string]any{"status": "ok"}
	if sr.PlayQueue != nil {
		out["playQueue"] = sr.PlayQueue
	}
	if sr.SearchResult3 != nil {
		out["searchResult3"] = sr.SearchResult3
	}
	return out, nil
}

func (s *Subsonic) Ping() bool {
	_, err := s.get("ping", nil)
	return err == nil
}

func (s *Subsonic) SavePlayQueue(ids []string, current string, position *int) error {
	if len(ids) == 0 {
		return nil
	}
	extra := url.Values{}
	for _, id := range ids {
		extra.Add("id", id)
	}
	if current != "" {
		extra.Set("current", current)
	}
	if position != nil {
		extra.Set("position", strconv.Itoa(*position))
	}
	_, err := s.get("savePlayQueue", extra)
	return err
}

func (s *Subsonic) GetPlayQueue() (*PlayQueue, error) {
	out, err := s.get("getPlayQueue", nil)
	if err != nil {
		return nil, err
	}
	pq, ok := out["playQueue"].(*PlayQueue)
	if !ok {
		return nil, nil
	}
	return pq, nil
}

type PlayQueue struct {
	Changed        string           `json:"changed"`
	Username       string           `json:"username"`
	Current        string           `json:"current"`
	Position       int64            `json:"position"`
	PositionMillis int64            `json:"positionMilliseconds"`
	Entry          []PlayQueueEntry `json:"entry"`
}

type PlayQueueEntry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	Track      int    `json:"track"`
	DiscNumber int    `json:"discNumber"`
}

func (s *Subsonic) ResolveID(title, artist, album string, track, disc *int) (string, error) {
	if title == "" {
		return "", nil
	}
	extra := url.Values{}
	extra.Set("query", title)
	extra.Set("songCount", "50")
	extra.Set("artistCount", "0")
	extra.Set("albumCount", "0")
	out, err := s.get("search3", extra)
	if err != nil {
		return "", err
	}
	sr, ok := out["searchResult3"].(*struct {
		Song []map[string]any `json:"song"`
	})
	if !ok || sr == nil {
		return "", nil
	}
	for _, song := range sr.Song {
		if match(song, title, artist, album, track, disc) {
			if id, ok := song["id"].(string); ok {
				return id, nil
			}
		}
	}
	// fallback: album+title only (artist normalization can differ)
	for _, song := range sr.Song {
		if album != "" {
			if songTitle, _ := song["title"].(string); songTitle == title {
				if songAlbum, _ := song["album"].(string); songAlbum == album {
					if id, ok := song["id"].(string); ok {
						return id, nil
					}
				}
			}
		}
	}
	return "", nil
}

func match(song map[string]any, title, artist, album string, track, disc *int) bool {
	if t, _ := song["title"].(string); t != title {
		return false
	}
	if artist != "" {
		if a, _ := song["artist"].(string); a != artist {
			return false
		}
	}
	if album != "" {
		if a, _ := song["album"].(string); a != album {
			return false
		}
	}
	if track != nil {
		if t, _ := normInt(song["track"]); t != *track {
			return false
		}
	}
	if disc != nil {
		if d, _ := normInt(song["discNumber"]); d != *disc {
			return false
		}
	}
	return true
}

func normInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return int(f), true
	}
	return 0, false
}
