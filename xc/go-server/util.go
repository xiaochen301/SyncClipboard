package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
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

// ---------- JSON (System.Text.Json-compatible output) ----------

// dotnetEscapeJSON rewrites a Go json product so strings escape the same
// characters System.Text.Json's default JavaScriptEncoder escapes
// (", ', +, <, >, & as uppercase \u00XX; non-BMP runes as surrogate pairs;
// other Unicode stays raw). Datetime "+00:00" suffixes keep a raw plus.
func dotnetEscapeJSON(b []byte) []byte {
	out := make([]byte, 0, len(b)+16)
	const hexDigits = "0123456789ABCDEF"
	appendEsc := func(v rune) {
		out = append(out, '\\', 'u', hexDigits[(v>>12)&0xF], hexDigits[(v>>8)&0xF], hexDigits[(v>>4)&0xF], hexDigits[v&0xF])
	}
	inStr := false
	for i := 0; i < len(b); {
		c := b[i]
		if !inStr {
			out = append(out, c)
			if c == '"' {
				inStr = true
			}
			i++
			continue
		}
		if c == '\\' && i+1 < len(b) {
			if b[i+1] == '"' {
				out = append(out, '\\', 'u', '0', '0', '2', '2')
			} else {
				out = append(out, c, b[i+1])
			}
			i += 2
			continue
		}
		if c == '"' {
			out = append(out, c)
			inStr = false
			i++
			continue
		}
		if c < 0x80 {
			switch c {
			case '\'', '+', '<', '>', '&':
				if c == '+' && isDatePlusBefore(b, i) {
					out = append(out, c)
				} else {
					appendEsc(rune(c))
				}
			default:
				out = append(out, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			appendEsc(r1)
			appendEsc(r2)
		} else {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return out
}

// isDatePlusBefore reports whether b[i] is the '+' of a "+00:00" suffix that
// directly follows an ISO datetime (kept raw like System.Text.Json's own
// DateTimeOffset writer).
func isDatePlusBefore(b []byte, i int) bool {
	if i+6 > len(b) || string(b[i:i+6]) != "+00:00" {
		return false
	}
	j := i - 1
	frac := false
	for j >= 0 && b[j] >= '0' && b[j] <= '9' {
		j--
		frac = true
	}
	if frac && j >= 0 && b[j] == '.' {
		j--
	}
	if j < 8 {
		return false
	}
	seg := b[j-7 : j+1]
	if seg[2] != ':' || seg[5] != ':' {
		return false
	}
	for k, c := range seg {
		if k == 2 || k == 5 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return b[j-8] == 'T'
}

// utf16Len counts UTF-16 code units like C# string.Length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// marshalJSON marshals like the official server: Go's HTML escaping disabled,
// then System.Text.Json-style escaping applied.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return dotnetEscapeJSON(b), nil
}

// writePDStatus mirrors the bare MVC ProblemDetails responses (type/title/status/traceId).
func writePDStatus(w http.ResponseWriter, status int, typ, title string) {
	body := fmt.Sprintf(`{"type":"%s","title":"%s","status":%d,"traceId":"%s"}`, typ, title, status, newTraceID())
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeBadRequestPD mirrors BadRequest() rendered as ProblemDetails.
func writeBadRequestPD(w http.ResponseWriter) {
	writePDStatus(w, http.StatusBadRequest, "https://tools.ietf.org/html/rfc9110#section-15.5.1", "Bad Request")
}

// writeUnsupportedMediaPD mirrors the [ApiController] [FromBody] 415 ProblemDetails.
func writeUnsupportedMediaPD(w http.ResponseWriter) {
	writePDStatus(w, http.StatusUnsupportedMediaType, "https://tools.ietf.org/html/rfc9110#section-15.5.16", "Unsupported Media Type")
}

// writeValidationProblem mirrors MVC ValidationProblemDetails (field order preserved).
func writeValidationProblem(w http.ResponseWriter, errs [][2]string) {
	var sb strings.Builder
	sb.WriteString(`{"type":"https://tools.ietf.org/html/rfc9110#section-15.5.1","title":"One or more validation errors occurred.","status":400,"errors":{`)
	for i, kv := range errs {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := marshalJSON(kv[0])
		mb, _ := marshalJSON(kv[1])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.WriteByte('[')
		sb.Write(mb)
		sb.WriteByte(']')
	}
	sb.WriteString(`},"traceId":"`)
	sb.WriteString(newTraceID())
	sb.WriteString(`"}`)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(sb.String()))
}

// isJSONContentType mirrors [FromBody]'s SystemTextJsonInputFormatter matching.
func isJSONContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if ct == "application/json" || ct == "text/json" {
		return true
	}
	return strings.HasPrefix(ct, "application/") && strings.HasSuffix(ct, "+json")
}

// jsonBindErrorText approximates System.Text.Json's binding failure message,
// translating the common Go parser errors into STJ phrasing.
func jsonBindErrorText(err error, body []byte) string {
	msg := err.Error()
	if se, ok := err.(*json.SyntaxError); ok {
		off := int(se.Offset) - 1
		if off < 0 {
			off = 0
		}
		line := 0
		col := 0
		for i := 0; i < off && i < len(body); i++ {
			if body[i] == '\n' {
				line++
				col = 0
			} else {
				col++
			}
		}
		if i := strings.Index(msg, "invalid character '"); i >= 0 {
			rest := msg[i+len("invalid character '"):]
			if len(rest) > 0 {
				ch := rest[:1]
				switch {
				case strings.Contains(msg, "object key string"):
					return fmt.Sprintf("'%s' is an invalid start of a property name. Expected a '\"'. Path: $ | LineNumber: %d | BytePositionInLine: %d.", ch, line, col)
				case strings.Contains(msg, "looking for beginning of value"):
					return fmt.Sprintf("'%s' is an invalid start of a value. Path: $ | LineNumber: %d | BytePositionInLine: %d.", ch, line, col)
				}
			}
		}
	}
	return "The JSON value could not be converted. Parser error: " + msg
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
