package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- hashing ----------

func sha256Bytes(b []byte) string {
	h := sha256.Sum256(b)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// combineFileHash mirrors FileProfile.CombineHash: SHA256("{fileName}|{CONTENT_HASH_UPPER}")
func combineFileHash(fileName, contentSHA string) string {
	return sha256Bytes([]byte(fileName + "|" + strings.ToUpper(contentSHA)))
}

func isValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// normalizeSHA256 returns the uppercase form or an official-style error.
func normalizeSHA256(s string) (string, error) {
	if !isValidSHA256(s) {
		return "", errors.New("Hash must be a 64-character SHA-256 hex string.")
	}
	return strings.ToUpper(s), nil
}

// ---------- time ----------

const dbTimeLayout = "2006-01-02 15:04:05.0000000"

var parseLayouts = []string{
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

func parseTimeFlexible(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range parseLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func formatDBTime(t time.Time) string { return t.UTC().Format(dbTimeLayout) }

// formatAPITime renders like .NET DateTimeOffset round-trip: up to 7 fraction
// digits with trailing zeros trimmed, always +00:00 offset (UTC).
func formatAPITime(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05.0000000")
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		frac := strings.TrimRight(s[i+1:], "0")
		if frac == "" {
			s = s[:i]
		} else {
			s = s[:i+1] + frac
		}
	}
	return s + "+00:00"
}

// ---------- files ----------

// safeFileName rejects path separators (mirrors InvalidFileName in the official controller).
func safeFileName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`)
}

// writeFileAtomic writes to a temp file then renames over the destination.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp-xc"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// copyToFile streams r into path while computing the SHA-256 of the content.
func copyToFile(path string, r io.Reader) (string, int64, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	cerr := f.Close()
	if err != nil {
		os.Remove(path)
		return "", 0, err
	}
	if cerr != nil {
		os.Remove(path)
		return "", 0, cerr
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), n, nil
}

// contentTypeFor mirrors a FileExtensionContentTypeProvider-ish mapping.
func contentTypeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".html", ".htm":
		return "text/html"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".zip":
		return "application/x-zip-compressed"
	case ".md":
		return "text/markdown"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func isImageFileName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".gif", ".bmp", ".png":
		return true
	}
	return false
}

func humanErr(f string, a ...any) error { return fmt.Errorf(f, a...) }
