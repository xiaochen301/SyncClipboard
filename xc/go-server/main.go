package main

import (
	"context"
	"encoding/base64"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Server is the xc-go implementation of SyncClipboard.Server.
type Server struct {
	cfg     Config
	store   *Store
	hub     *Hub
	verbose bool
}

func main() {
	listen := flag.String("listen", "127.0.0.1:5033", "listen address")
	dataRoot := flag.String("data", "", "data root directory (contains SyncClipboard.json, file/, data/, history/)")
	confPath := flag.String("config", "", "path to appsettings.json")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()
	if *dataRoot == "" {
		log.Fatal("-data is required")
	}

	cfg, err := loadConfig(*confPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	store, err := openStore(*dataRoot)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	srv := &Server{cfg: cfg, store: store, hub: newHub(*verbose), verbose: *verbose}
	api := srv.router()
	httpSrv := &http.Server{
		Addr: *listen,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.authHeaderPrecheck(w, r)
			api.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 20 * time.Second,
	}

	go safe(srv.retentionLoop)

	log.Printf("syncclipboard-server-xc starting: listen=%s data=%s version=%s", *listen, *dataRoot, cfg.ServerVersion)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// safe runs fn, recovering from panics so a background loop can never take
// the whole process down.
func safe(fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panic recovered: %v", rec)
		}
	}()
	fn()
}

// authed returns true when the request passed Basic auth; otherwise it writes
// the official response (401 with WWW-Authenticate, or 500 for malformed Basic
// credentials that make the official handler throw) and returns false.
func (s *Server) authed(w http.ResponseWriter, r *http.Request) bool {
	hdr := r.Header.Get("Authorization")
	if len(hdr) < 5 || !strings.EqualFold(hdr[:5], "basic") {
		w.Header().Set("WWW-Authenticate", `Basic realm="SyncClipboard"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	if len(hdr) < 6 { // official: substring past end throws -> 500
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}
	token := strings.TrimSpace(hdr[6:])
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil { // official: FormatException -> 500
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 { // official: index out of range -> 500
		w.WriteHeader(http.StatusInternalServerError)
		return false
	}
	if parts[0] == s.cfg.User && parts[1] == s.cfg.Password {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="SyncClipboard"`)
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

// authHeaderPrecheck replicates the official auth handler's side effect: any
// request without valid Basic credentials carries a WWW-Authenticate header on
// whatever response (401/404/405) it ends up with.
func (s *Server) authHeaderPrecheck(w http.ResponseWriter, r *http.Request) {
	hdr := r.Header.Get("Authorization")
	if len(hdr) < 5 || !strings.EqualFold(hdr[:5], "basic") {
		w.Header().Set("WWW-Authenticate", `Basic realm="SyncClipboard"`)
		return
	}
	if len(hdr) < 6 {
		return
	}
	token := strings.TrimSpace(hdr[6:])
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return
	}
	if parts[0] == s.cfg.User && parts[1] == s.cfg.Password {
		return
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="SyncClipboard"`)
}

// do405 mirrors ASP.NET routing's 405 (empty body, Allow header).
func do405(w http.ResponseWriter, allow string) {
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) router() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 1 {
			p = strings.TrimSuffix(p, "/")
		}
		m := r.Method
		switch {
		case p == "/" || p == "":
			switch m {
			case http.MethodGet:
				if s.authed(w, r) {
					s.handleRoot(w, r)
				}
			case "PROPFIND":
				if s.authed(w, r) {
					s.handlePropfind(w, r)
				}
			default:
				do405(w, "GET, PROPFIND")
			}
		case p == "/SyncClipboard.json":
			switch m {
			case http.MethodGet:
				if s.authed(w, r) {
					s.handleGetProfile(w, r)
				}
			case http.MethodPut:
				if s.authed(w, r) {
					s.handlePutProfile(w, r)
				}
			default:
				do405(w, "GET, PUT")
			}
		case p == "/file":
			switch m {
			case "PROPFIND", "MKCOL":
				if s.authed(w, r) {
					s.handleFileFolderEnsure(w, r)
				}
			case http.MethodDelete:
				if s.authed(w, r) {
					s.handleFileFolderDelete(w, r)
				}
			default:
				do405(w, "DELETE, MKCOL, PROPFIND")
			}
		case strings.HasPrefix(p, "/file/"):
			switch m {
			case http.MethodGet, http.MethodHead:
				if s.authed(w, r) {
					s.handleFileGet(w, r)
				}
			case http.MethodPut:
				if s.authed(w, r) {
					s.handleFilePut(w, r)
				}
			default:
				do405(w, "GET, HEAD, PUT")
			}
		case p == "/api/version":
			switch m {
			case http.MethodGet:
				if s.authed(w, r) {
					writePlain(w, s.cfg.ServerVersion)
				}
			default:
				do405(w, "GET")
			}
		case p == "/api/time":
			switch m {
			case http.MethodGet:
				if s.authed(w, r) {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					_, _ = w.Write([]byte(`"` + formatAPITime(time.Now().UTC()) + `"`))
				}
			default:
				do405(w, "GET")
			}
		case p == "/api/history":
			switch m {
			case http.MethodPost:
				if s.authed(w, r) {
					s.handleHistoryCreate(w, r)
				}
			default:
				do405(w, "POST")
			}
		case p == "/api/history/query":
			switch m {
			case http.MethodPost:
				if s.authed(w, r) {
					s.handleHistoryQuery(w, r)
				}
			case http.MethodGet:
				if s.authed(w, r) {
					s.handleHistoryGet(w, r, "query")
				}
			default:
				do405(w, "GET, POST")
			}
		case p == "/api/history/statistics":
			switch m {
			case http.MethodGet:
				if s.authed(w, r) {
					s.handleHistoryStatistics(w, r)
				}
			default:
				do405(w, "GET")
			}
		case p == "/api/history/clear":
			switch m {
			case http.MethodDelete:
				if s.authed(w, r) {
					s.handleHistoryClear(w, r)
				}
			case http.MethodGet:
				if s.authed(w, r) {
					s.handleHistoryGet(w, r, "clear")
				}
			default:
				do405(w, "DELETE, GET")
			}
		case strings.HasPrefix(p, "/api/history/"):
			rest := strings.TrimPrefix(p, "/api/history/")
			seg := strings.Split(rest, "/")
			switch len(seg) {
			case 1:
				switch m {
				case http.MethodGet:
					if s.authed(w, r) {
						s.handleHistoryGet(w, r, seg[0])
					}
				default:
					do405(w, "GET")
				}
			case 2:
				if seg[1] == "data" {
					switch m {
					case http.MethodGet:
						if s.authed(w, r) {
							s.handleHistoryData(w, r, seg[0])
						}
					case http.MethodPatch:
						if s.authed(w, r) {
							s.handleHistoryPatch(w, r, seg[0], seg[1])
						}
					default:
						do405(w, "GET, PATCH")
					}
				} else {
					switch m {
					case http.MethodPatch:
						if s.authed(w, r) {
							s.handleHistoryPatch(w, r, seg[0], seg[1])
						}
					default:
						do405(w, "PATCH")
					}
				}
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case p == "/SyncClipboardHub/negotiate":
			switch m {
			case http.MethodPost:
				if s.authed(w, r) {
					s.hub.handleNegotiate(w, r)
				}
			default:
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case p == "/SyncClipboardHub":
			if m != http.MethodGet {
				do405(w, "GET")
				return
			}
			if !s.authed(w, r) {
				return
			}
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Cache-Control", "no-cache, no-store")
				w.Header().Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
				w.Header().Set("Pragma", "no-cache")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("Connection ID required"))
				return
			}
			s.hub.handleWS(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// retentionLoop mirrors HistoryCleaner: maintenance every 10 minutes, deleted
// tombstone purge + orphan folder cleanup every 12 hours.
func (s *Server) retentionLoop() {
	runMaintenance := func() {
		// sweep stale multipart temp files from the staging dir
		if entries, err := os.ReadDir(s.store.fileDir()); err == nil {
			cutoff := time.Now().Add(-1 * time.Hour)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), ".pending-") {
					continue
				}
				if info, ierr := e.Info(); ierr == nil && info.ModTime().Before(cutoff) {
					_ = os.Remove(filepath.Join(s.store.fileDir(), e.Name()))
				}
			}
		}
		removedRet, err := s.store.TrimRetention(s.cfg.HistoryRetentionMinutes)
		if err != nil {
			log.Printf("retention: %v", err)
		}
		removedMax, err := s.store.TrimToMaxCount(s.cfg.MaxSavedHistoryCount)
		if err != nil {
			log.Printf("max-count: %v", err)
		}
		for _, rec := range append(removedRet, removedMax...) {
			s.store.deleteWorkingDir(rec.Type, rec.Hash)
			s.notifyHistory(rec)
		}
		if n := len(removedRet) + len(removedMax); n > 0 {
			log.Printf("retention: soft-deleted %d records", n)
		}
	}
	runTombstones := func() {
		cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
		recs, err := s.store.SelectDeletedBefore(cutoff)
		if err != nil {
			log.Printf("tombstones: %v", err)
			return
		}
		for _, rec := range recs {
			if err := s.store.PurgeRecord(rec.ID); err != nil {
				continue
			}
			s.store.deleteWorkingDir(rec.Type, rec.Hash)
		}
		if len(recs) > 0 {
			log.Printf("tombstones: purged %d deleted records", len(recs))
		}
	}
	runOrphans := func() {
		dirs, err := s.store.ListWorkingDirs()
		if err != nil {
			return
		}
		for _, d := range dirs {
			i := strings.IndexByte(d, '_')
			if i <= 0 {
				continue
			}
			t, ok := parseProfileTypeString(d[:i])
			if !ok {
				continue
			}
			hash := d[i+1:]
			rec, err := s.store.GetByTypeHash(t, hash)
			if err != nil {
				continue
			}
			if rec == nil || rec.IsDeleted {
				_ = os.RemoveAll(filepath.Join(s.store.historyDir(), d))
			}
		}
	}

	time.Sleep(5 * time.Second)
	runMaintenance()
	runTombstones()
	runOrphans()

	mainTick := time.NewTicker(10 * time.Minute)
	tombTick := time.NewTicker(12 * time.Hour)
	defer mainTick.Stop()
	defer tombTick.Stop()
	for {
		select {
		case <-mainTick.C:
			runMaintenance()
		case <-tombTick.C:
			runTombstones()
			runOrphans()
		}
	}
}
