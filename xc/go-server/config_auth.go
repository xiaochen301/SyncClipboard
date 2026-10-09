package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// Config mirrors the official AppSettings (+ our extras).
type Config struct {
	User                    string
	Password                string
	MaxSavedHistoryCount    uint32
	HistoryRetentionMinutes uint32
	ServerVersion           string
}

func defaultConfig() Config {
	return Config{
		User:                    "admin",
		Password:                "admin",
		MaxSavedHistoryCount:    1000,
		HistoryRetentionMinutes: 0,
		ServerVersion:           "3.3.1",
	}
}

// loadConfig reads appsettings.json (AppSettings section) with env overrides,
// mirroring the official Program.cs behavior.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var root struct {
				AppSettings struct {
					UserName                *string `json:"UserName"`
					Password                *string `json:"Password"`
					MaxSavedHistoryCount    *uint32 `json:"MaxSavedHistoryCount"`
					HistoryRetentionMinutes *uint32 `json:"HistoryRetentionMinutes"`
				} `json:"AppSettings"`
				XcServerVersion *string `json:"XcServerVersion"`
			}
			if err := json.Unmarshal(b, &root); err != nil {
				return cfg, err
			}
			if root.AppSettings.UserName != nil {
				cfg.User = *root.AppSettings.UserName
			}
			if root.AppSettings.Password != nil {
				cfg.Password = *root.AppSettings.Password
			}
			if root.AppSettings.MaxSavedHistoryCount != nil {
				cfg.MaxSavedHistoryCount = *root.AppSettings.MaxSavedHistoryCount
			}
			if root.AppSettings.HistoryRetentionMinutes != nil {
				cfg.HistoryRetentionMinutes = *root.AppSettings.HistoryRetentionMinutes
			}
			if root.XcServerVersion != nil && *root.XcServerVersion != "" {
				cfg.ServerVersion = *root.XcServerVersion
			}
		} else if !os.IsNotExist(err) {
			return cfg, err
		}
	}
	// env overrides (official: static checker requires BOTH vars set)
	if u := os.Getenv("SYNCCLIPBOARD_USERNAME"); u != "" {
		if p := os.Getenv("SYNCCLIPBOARD_PASSWORD"); p != "" {
			cfg.User, cfg.Password = u, p
		}
	}
	if v := os.Getenv("SYNCCLIPBOARD_SERVER_VERSION"); v != "" {
		cfg.ServerVersion = v
	}
	return cfg, nil
}

// checkBasicAuth replicates the official BasicAuthenticationHandler semantics:
// header must start with "basic" (case-insensitive); base64 decode; split on
// ':' (first two parts); exact case-sensitive comparison.
func (s *Server) checkBasicAuth(r *http.Request) bool {
	hdr := r.Header.Get("Authorization")
	if len(hdr) < 6 || !strings.EqualFold(hdr[:5], "basic") {
		return false
	}
	token := strings.TrimSpace(hdr[6:])
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), ":", 3)
	if len(parts) < 2 {
		return false
	}
	return parts[0] == s.cfg.User && parts[1] == s.cfg.Password
}

// authAll wraps the whole router with Basic auth (official: every route is [Authorize]).
func (s *Server) authAll(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkBasicAuth(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="SyncClipboard"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
