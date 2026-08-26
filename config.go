package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type Config struct {
	NavidromeURL    string
	NavidromeUser   string
	NavidromePass   string
	NavidromeLegacy bool
	MPDHost         string
	MPDPort         int
	MPDPassword     string
	PollInterval    time.Duration
	Listen          string
	StateFile       string // where the runtime mode is persisted
	APIToken        string
	Verbose         bool
}

// readDotenv loads a dotenv-style config file (KEY=VALUE lines, # comments)
// into a map. An empty path means "no config file"; a missing file is an error.
func readDotenv(path string) (map[string]string, error) {
	cfg := map[string]string{}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		// strip surrounding quotes
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		cfg[key] = val
	}
	return cfg, nil
}

func loadConfig() Config {
	var envFile string
	flag.StringVar(&envFile, "env-file", "", "path to a dotenv-style config file (KEY=VALUE)")
	flag.StringVar(&cURL, "navidrome-url", "", "Navidrome Subsonic base URL")
	flag.StringVar(&cUser, "navidrome-user", "", "Navidrome username")
	flag.StringVar(&cPass, "navidrome-password", "", "Navidrome password")
	flag.BoolVar(&cLegacy, "legacy-auth", false, "use plaintext p= auth")
	flag.StringVar(&cHost, "mpd-host", "", "MPD host")
	mpdPort := flag.Int("mpd-port", 0, "MPD port")
	flag.StringVar(&cMpdPass, "mpd-password", "", "MPD password")
	poll := flag.Float64("poll-interval", 0, "poll interval seconds")
	flag.StringVar(&cListen, "listen", "", "status API + web UI listen address")
	flag.StringVar(&cToken, "api-token", "", "optional bearer token for the API")
	flag.StringVar(&cStateFile, "state-file", "", "file where the runtime sync mode is persisted")
	flag.BoolVar(&cVerbose, "verbose", false, "verbose debug logging")
	flag.Parse()

	cfg, err := readDotenv(envFile)
	if err != nil {
		log.Fatalf("cannot read config file %q: %v", envFile, err)
	}

	// Precedence: explicit flag > environment > config file > built-in default.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	pick := func(flagName, envKey, flagVal, def string) string {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		if v := cfg[envKey]; v != "" {
			return v
		}
		return def
	}
	pickBool := func(flagName, envKey string) bool {
		if set[flagName] {
			return true
		}
		if v := os.Getenv(envKey); v != "" {
			return v == "1" || strings.EqualFold(v, "true")
		}
		if v := cfg[envKey]; v != "" {
			return v == "1" || strings.EqualFold(v, "true")
		}
		return false
	}
	pickInt := func(flagName, envKey string, flagVal int, def int) int {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return mustInt(v)
		}
		if v := cfg[envKey]; v != "" {
			return mustInt(v)
		}
		return def
	}
	pickFloat := func(flagName, envKey string, flagVal float64, def float64) float64 {
		if set[flagName] {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return mustFloat(v)
		}
		if v := cfg[envKey]; v != "" {
			return mustFloat(v)
		}
		return def
	}

	c := Config{}
	c.NavidromeURL = pick("navidrome-url", "NAVIDROME_URL", cURL, "")
	c.NavidromeUser = pick("navidrome-user", "NAVIDROME_USER", cUser, "")
	c.NavidromePass = pick("navidrome-password", "NAVIDROME_PASSWORD", cPass, "")
	c.NavidromeLegacy = pickBool("legacy-auth", "NAVIDROME_LEGACY_AUTH")
	c.MPDHost = pick("mpd-host", "MPD_HOST", cHost, "127.0.0.1")
	c.MPDPort = pickInt("mpd-port", "MPD_PORT", *mpdPort, 6600)
	c.MPDPassword = pick("mpd-password", "MPD_PASSWORD", cMpdPass, "")
	c.PollInterval = time.Duration(pickFloat("poll-interval", "POLL_INTERVAL", *poll, 5) * float64(time.Second))
	c.Listen = pick("listen", "BATON_LISTEN", cListen, "127.0.0.1:8080")
	c.APIToken = pick("api-token", "BATON_API_TOKEN", cToken, "")
	c.Verbose = pickBool("verbose", "VERBOSE")
	if c.StateFile == "" {
		c.StateFile = defaultStateFile()
	}
	return c
}

// defaultStateFile returns where baton persists the runtime sync mode, following
// the XDG state convention. Returns "" when no sensible location can be found,
// which disables persistence (mode still changes, just not across restarts).
func defaultStateFile() string {
	if v := os.Getenv("BATON_STATE_FILE"); v != "" {
		return v
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "baton", "mode")
}

var (
	cURL, cUser, cPass, cHost, cMpdPass, cListen, cToken, cStateFile string
	cLegacy, cVerbose                                                bool
)

func mustInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		log.Fatalf("invalid int: %s", s)
	}
	return i
}

// normalizeURL adds http:// when the value is a bare host:port.
func normalizeURL(u string) string {
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return "http://" + u
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Fatalf("invalid float: %s", s)
	}
	return f
}
