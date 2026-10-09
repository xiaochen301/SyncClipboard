package main

import (
	"context"
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
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.authAll(srv.router()),
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

func (s *Server) router() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 1 {
			p = strings.TrimSuffix(p, "/")
		}
		switch {
		case p == "/" || p == "":
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				s.handleRoot(w, r)
			case "PROPFIND":
				s.handlePropfind(w, r)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case p == "/SyncClipboard.json":
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				s.handleGetProfile(w, r)
			case http.MethodPut:
				s.handlePutProfile(w, r)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case p == "/file":
			switch r.Method {
			case "PROPFIND", "MKCOL", http.MethodGet, http.MethodHead:
				s.handleFileFolderEnsure(w, r)
			case http.MethodDelete:
				s.handleFileFolderDelete(w, r)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case strings.HasPrefix(p, "/file/"):
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				s.handleFileGet(w, r)
			case http.MethodPut:
				s.handleFilePut(w, r)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case p == "/api/version" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			writePlain(w, s.cfg.ServerVersion)
		case p == "/api/time" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`"` + formatAPITime(time.Now().UTC()) + `"`))
		case p == "/api/history" || strings.HasPrefix(p, "/api/history/"):
			s.handleHistory(w, r)
		case p == "/SyncClipboardHub/negotiate":
			if r.Method == http.MethodPost {
				s.hub.handleNegotiate(w, r)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case p == "/SyncClipboardHub":
			if r.Method == http.MethodGet {
				s.hub.handleWS(w, r)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
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
