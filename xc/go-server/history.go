package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const transferHashHeader = "X-SyncClipboard-Transfer-Data-Hash"

// ---------- routing ----------

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/history"), "/")
	var sub []string
	if rest != "" {
		sub = strings.Split(rest, "/")
	}
	switch {
	case len(sub) == 0 && r.Method == http.MethodPost:
		s.handleHistoryCreate(w, r)
	case len(sub) == 1 && sub[0] == "query" && r.Method == http.MethodPost:
		s.handleHistoryQuery(w, r)
	case len(sub) == 1 && sub[0] == "statistics" && r.Method == http.MethodGet:
		s.handleHistoryStatistics(w, r)
	case len(sub) == 1 && sub[0] == "clear" && r.Method == http.MethodDelete:
		s.handleHistoryClear(w, r)
	case len(sub) == 1 && r.Method == http.MethodGet:
		s.handleHistoryGet(w, r, sub[0])
	case len(sub) == 2 && sub[1] == "data" && r.Method == http.MethodGet:
		s.handleHistoryData(w, r, sub[0])
	case len(sub) == 2 && r.Method == http.MethodPatch:
		s.handleHistoryPatch(w, r, sub[0], sub[1])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// ---------- form helpers ----------

func firstForm(vals map[string][]string, key string) string {
	for k, list := range vals {
		if strings.EqualFold(k, key) {
			if len(list) > 0 {
				return list[0]
			}
			return ""
		}
	}
	return ""
}

func (s *Server) readFormValues(r *http.Request) (map[string][]string, error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, err
		}
		if r.MultipartForm == nil {
			return map[string][]string{}, nil
		}
		return r.MultipartForm.Value, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.PostForm, nil
}

// ---------- query ----------

func (s *Server) handleHistoryQuery(w http.ResponseWriter, r *http.Request) {
	vals, _ := s.readFormValues(r) // form parse errors are tolerated like the official binder
	q := historyQuery{Page: 1, Types: 0xF}
	var verrs [][2]string
	addErr := func(field, val string) {
		verrs = append(verrs, [2]string{field, "The value '" + val + "' is not valid for " + field + "."})
	}
	if v := firstForm(vals, "Page"); v != "" {
		if n, err := parseIntSafe(v); err == nil {
			q.Page = n
		} else {
			addErr("Page", v)
		}
	}
	if v := firstForm(vals, "Before"); v != "" {
		if t, ok := parseTimeFlexible(v); ok {
			q.Before = &t
		} else {
			addErr("Before", v)
		}
	}
	if v := firstForm(vals, "After"); v != "" {
		if t, ok := parseTimeFlexible(v); ok {
			q.After = &t
		} else {
			addErr("After", v)
		}
	}
	if v := firstForm(vals, "ModifiedAfter"); v != "" {
		if t, ok := parseTimeFlexible(v); ok {
			q.ModifiedAfter = &t
		} else {
			addErr("ModifiedAfter", v)
		}
	}
	if v := firstForm(vals, "Types"); v != "" {
		if m, ok := parseTypeFilter(v); ok {
			q.Types = m
		} else {
			addErr("Types", v)
		}
	}
	q.SearchText = firstForm(vals, "SearchText")
	if v := firstForm(vals, "Starred"); v != "" {
		if b, err := parseBoolSafe(v); err == nil {
			q.Starred = &b
		} else {
			addErr("Starred", v)
		}
	}
	if v := firstForm(vals, "SortByLastAccessed"); v != "" {
		if b, err := parseBoolSafe(v); err == nil {
			q.SortByLastAccessed = b
		} else {
			addErr("SortByLastAccessed", v)
		}
	}
	if len(verrs) > 0 {
		writeValidationProblem(w, verrs)
		return
	}
	if q.Before != nil && q.After != nil && !q.After.Before(*q.Before) {
		writeErr(w, http.StatusBadRequest, "after must be less than before")
		return
	}
	recs, err := s.store.ListHistory(q)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	out := make([]HistoryRecordDto, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.toDTO())
	}
	writeJSON(w, http.StatusOK, out)
}

func parseIntSafe(v string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
	return n, err
}

func parseBoolSafe(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("bad bool")
}

// ---------- get single / data ----------

func (s *Server) handleHistoryGet(w http.ResponseWriter, r *http.Request, profileID string) {
	t, hash, ok := parseProfileId(profileID)
	if !ok || hash == "" {
		writeErr(w, http.StatusBadRequest, "Invalid profileId format. Expected format: 'Type-Hash'")
		return
	}
	rec, err := s.store.GetByTypeHash(t, hash)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		writeNotFoundPD(w)
		return
	}
	writeJSON(w, http.StatusOK, rec.toDTO())
}

func (s *Server) handleHistoryData(w http.ResponseWriter, r *http.Request, profileID string) {
	t, hash, ok := parseProfileId(profileID)
	if !ok || hash == "" {
		writeErr(w, http.StatusBadRequest, "Invalid profileId format. Expected format: 'Type-Hash'")
		return
	}
	rec, err := s.store.GetByTypeHash(t, hash)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		writeNotFoundPD(w)
		return
	}
	if rec.TransferDataFile == "" && len(rec.FilePaths) == 0 {
		writeNotFoundPD(w)
		return
	}
	full := s.resolveRecordDataPath(rec)
	if full == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "History transfer data is invalid",
			"Stored transfer data is invalid and cannot be regenerated.", "history_data_invalid")
		return
	}
	b, err := os.ReadFile(full)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "History transfer data is invalid",
			"Stored transfer data is invalid and cannot be regenerated.", "history_data_invalid")
		return
	}
	name := filepath.Base(full)
	// official serves a freshly computed SHA-256; emit the exact header casing.
	if actual, err := sha256File(full); err == nil {
		w.Header()["X-SyncClipboard-Transfer-Data-Hash"] = []string{actual}
		// official PrepareTransferData backfills TransferDataHash into the row
		if rec.TransferDataHash == nil || !strings.EqualFold(*rec.TransferDataHash, actual) {
			v := actual
			rec.TransferDataHash = &v
			_ = s.store.Update(rec)
		}
	} else if h := rec.transferHash(); h != nil {
		w.Header()["X-SyncClipboard-Transfer-Data-Hash"] = []string{*h}
	}
	setAttachmentDisposition(w, name)
	w.Header().Set("Content-Type", contentTypeFor(name))
	_, _ = w.Write(b)
}

func setAttachmentDisposition(w http.ResponseWriter, name string) {
	ascii := make([]rune, 0, len(name))
	for _, c := range name {
		if c < 128 && c != '"' && c != '\\' {
			ascii = append(ascii, c)
		} else {
			ascii = append(ascii, '_')
		}
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s; filename*=UTF-8''%s",
		string(ascii), rfc5987Escape(name)))
}

func rfc5987Escape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// writeProblem mirrors the official ProblemDetails 422 body:
// {"title":...,"status":422,"detail":...,"code":...}
func writeProblem(w http.ResponseWriter, status int, title, detail, code string) {
	body := struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}{Title: title, Status: status, Detail: detail, Code: code}
	b, _ := marshalJSON(body)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeNotFoundPD mirrors the MVC [ApiController] ProblemDetails 404 body.
func writeNotFoundPD(w http.ResponseWriter) {
	body := struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Status  int    `json:"status"`
		TraceID string `json:"traceId"`
	}{
		Type:    "https://tools.ietf.org/html/rfc9110#section-15.5.5",
		Title:   "Not Found",
		Status:  http.StatusNotFound,
		TraceID: newTraceID(),
	}
	b, _ := marshalJSON(body)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(b)
}

// newTraceID returns a W3C traceparent-style id like ASP.NET's traceId.
func newTraceID() string {
	return fmt.Sprintf("00-%016x%016x-%016x-00", rand.Uint64(), rand.Uint64(), rand.Uint64())
}

// ---------- create (multipart) ----------

func (s *Server) handleHistoryCreate(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		// official: parsing a missing Content-Type throws -> 500
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		// [Consumes("multipart/form-data")] filter -> empty 415
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if _, params, err := mime.ParseMediaType(ct); err != nil || params["boundary"] == "" {
		writeErr(w, http.StatusBadRequest, "Invalid or missing multipart/form-data boundary")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid or missing multipart/form-data boundary")
		return
	}
	fields := map[string]string{}
	var tmpPath, origName string
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Internal server error: "+err.Error())
			return
		}
		name := p.FormName()
		if name == "data" && p.FileName() != "" {
			tmp, err := os.CreateTemp(s.store.fileDir(), ".pending-*")
			if err != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				p.Close()
				return
			}
			if _, err := io.Copy(tmp, p); err != nil {
				tmp.Close()
				_ = os.Remove(tmp.Name())
				p.Close()
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			tmp.Close()
			tmpPath = tmp.Name()
			origName = p.FileName()
			p.Close()
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(p, 16<<20))
		fields[strings.ToLower(name)] = string(b)
		p.Close()
	}

	typ, ok := parseProfileTypeString(strings.TrimSpace(fields["type"]))
	if !ok || typ == TypeNone || typ == TypeUnknown {
		writeErr(w, http.StatusBadRequest, "Type is invalid or missing")
		return
	}
	hash := strings.ToUpper(strings.TrimSpace(fields["hash"]))
	if hash == "" {
		writeErr(w, http.StatusBadRequest, "hash is required")
		return
	}
	var declared *string
	if hdrVals := r.Header.Values(transferHashHeader); len(hdrVals) > 0 {
		if len(hdrVals) != 1 {
			writeErr(w, http.StatusBadRequest, transferHashHeader+" must contain exactly one value")
			return
		}
		n, err := normalizeSHA256(strings.TrimSpace(hdrVals[0]))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		declared = &n
	}
	if tmpPath == "" && declared != nil {
		writeErr(w, http.StatusBadRequest, transferHashHeader+" cannot be set without transfer data")
		return
	}

	now := time.Now().UTC()
	parseTimeField := func(key string) time.Time {
		if v := fields[key]; v != "" {
			if t, ok := parseTimeFlexible(v); ok {
				return t
			}
		}
		return now
	}
	strToBool := func(key string) bool {
		b, err := parseBoolSafe(fields[key])
		return err == nil && b
	}
	strToInt := func(key string) int {
		if v := strings.TrimSpace(fields[key]); v != "" {
			if n, err := parseIntSafe(v); err == nil {
				return n
			}
		}
		return 0
	}
	strToInt64 := func(key string) int64 {
		if v := strings.TrimSpace(fields[key]); v != "" {
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
		return 0
	}

	incoming := &Record{
		UserID:       hardcodedUserID,
		Type:         typ,
		Hash:         hash,
		Text:         fields["text"],
		Size:         strToInt64("size"),
		CreateTime:   parseTimeField("createtime"),
		LastModified: parseTimeField("lastmodified"),
		LastAccessed: parseTimeField("lastaccessed"),
		Stared:       strToBool("starred"),
		Pinned:       strToBool("pinned"),
		Version:      strToInt("version"),
		IsDeleted:    strToBool("isdeleted"),
		FilePaths:    []string{},
		Tags:         []string{},
	}

	existing, err := s.store.GetByTypeHash(typ, hash)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var resp *Record
	var herr *handlerError
	if existing != nil {
		resp, herr = s.updateExistingRecordDto(existing, incoming, declared, tmpPath, origName)
	} else {
		resp, herr = s.addNewRecordDto(incoming, declared, tmpPath, origName)
	}
	if herr != nil {
		if herr.Code == http.StatusUnprocessableEntity {
			writeProblem(w, herr.Code, "History transfer data is invalid", herr.Message, "history_data_invalid")
			return
		}
		writeErr(w, herr.Code, herr.Message)
		return
	}
	writeJSON(w, http.StatusOK, resp.toDTO())
}

func historyShouldUpdate(oldVersion, newVersion int, oldLM, newLM time.Time) bool {
	const th = 5 * time.Minute
	gap := newLM.Sub(oldLM)
	if gap < 0 {
		gap = -gap
	}
	if gap <= th {
		return newVersion >= oldVersion
	}
	return !newLM.Before(oldLM)
}

func (s *Server) updateExistingRecordDto(existing, incoming *Record, declared *string, tmpPath, origName string) (*Record, *handlerError) {
	if existing.IsDeleted {
		if herr := s.ensureExistingRecordData(existing, declared, tmpPath, origName); herr != nil {
			return nil, herr
		}
	}
	if historyShouldUpdate(existing.Version, incoming.Version, existing.LastModified, incoming.LastModified) {
		incoming.Version = maxInt(incoming.Version, existing.Version+1)
		existing.CreateTime = incoming.CreateTime
		existing.LastAccessed = incoming.LastAccessed
		existing.LastModified = incoming.LastModified
		existing.Stared = incoming.Stared
		existing.Pinned = incoming.Pinned
		existing.Version = incoming.Version
		existing.IsDeleted = incoming.IsDeleted
		if err := s.store.Update(existing); err != nil {
			return nil, httpErr(http.StatusInternalServerError, err.Error())
		}
		s.notifyHistory(existing)
		s.deleteRecordDataIfDeleted(existing)
	}
	return existing, nil
}

func (s *Server) ensureExistingRecordData(existing *Record, declared *string, tmpPath, origName string) *handlerError {
	if tmpPath != "" {
		return s.saveTransferDataAsync(existing, declared, tmpPath, origName)
	}
	if !existing.hasData() {
		if existing.Type == TypeText {
			return nil // inline text is valid local data
		}
		return httpErr(http.StatusBadRequest, "Needs tranfer data.")
	}
	if s.resolveRecordDataPath(existing) == "" {
		return httpErr(http.StatusBadRequest, "Needs tranfer data.")
	}
	return nil
}

// saveTransferDataAsync mirrors HistoryService.SaveTransferDataAsync: write the
// incoming stream into history/{Type}_{HASH}/, validate hashes, update fields.
func (s *Server) saveTransferDataAsync(rec *Record, declared *string, tmpPath, origName string) *handlerError {
	var fn string
	if rec.Type == TypeText {
		fn = fmt.Sprintf("Text_%s_%s.txt", time.Now().Format("2006-01-02_15-04-05"), randSuffix())
	} else {
		fn = strings.TrimSpace(rec.Text)
		if fn == "" {
			fn = origName
		}
		fn = filepath.Base(fn)
	}
	if fn == "" || fn == "." || !safeFileName(fn) {
		return httpErr(http.StatusBadRequest, "Invalid data name")
	}
	dir := s.store.workingDir(rec.Type, rec.Hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return httpErr(http.StatusInternalServerError, err.Error())
	}
	dst := filepath.Join(dir, fn)
	if err := os.Rename(tmpPath, dst); err != nil {
		// copy fallback
		in, oerr := os.Open(tmpPath)
		if oerr != nil {
			return httpErr(http.StatusInternalServerError, oerr.Error())
		}
		out, cerr := os.Create(dst)
		if cerr != nil {
			in.Close()
			return httpErr(http.StatusInternalServerError, cerr.Error())
		}
		_, cerr = io.Copy(out, in)
		in.Close()
		out.Close()
		if cerr != nil {
			_ = os.Remove(dst)
			return httpErr(http.StatusInternalServerError, cerr.Error())
		}
		_ = os.Remove(tmpPath)
	}
	actual, err := sha256File(dst)
	if err != nil {
		_ = os.Remove(dst)
		return httpErr(http.StatusInternalServerError, err.Error())
	}
	if declared != nil && !strings.EqualFold(actual, *declared) {
		_ = os.Remove(dst)
		return httpErr(http.StatusUnprocessableEntity, "History transfer data failed validation.")
	}
	switch rec.Type {
	case TypeText:
		if !strings.EqualFold(actual, rec.Hash) {
			_ = os.Remove(dst)
			return httpErr(http.StatusUnprocessableEntity, "History transfer data failed validation.")
		}
	case TypeFile, TypeImage:
		ph := combineFileHash(fn, actual)
		if !strings.EqualFold(ph, rec.Hash) {
			_ = os.Remove(dst)
			return httpErr(http.StatusUnprocessableEntity, "History transfer data failed validation.")
		}
	}
	rec.TransferDataFile = fn
	rec.TransferDataHash = &actual
	rec.TransferDataSha256 = ""
	rec.FilePaths = []string{fn}
	if rec.Type != TypeText {
		rec.Text = fn
	}
	return nil
}

func (s *Server) addNewRecordDto(incoming *Record, declared *string, tmpPath, origName string) (*Record, *handlerError) {
	rec := incoming
	if tmpPath != "" {
		if herr := s.saveTransferDataAsync(rec, declared, tmpPath, origName); herr != nil {
			return nil, herr
		}
	} else {
		rec.TransferDataHash = nil
		switch rec.Type {
		case TypeText:
			h := sha256Bytes([]byte(rec.Text))
			bad := !strings.EqualFold(h, rec.Hash)
			if rec.Size > int64(len(rec.Text)) {
				bad = true
			}
			if bad {
				return nil, httpErr(http.StatusBadRequest, "Local data is missing or does not match the profile hash.")
			}
		default:
			return nil, httpErr(http.StatusBadRequest, "Local data is missing or does not match the profile hash.")
		}
	}
	if err := s.store.Insert(rec); err != nil {
		return nil, httpErr(http.StatusInternalServerError, err.Error())
	}
	s.notifyHistory(rec)
	s.deleteRecordDataIfDeleted(rec)
	return rec, nil
}

// ---------- PATCH ----------

func (s *Server) handleHistoryPatch(w http.ResponseWriter, r *http.Request, typeStr, hash string) {
	typ, ok := parseProfileTypeString(typeStr)
	if !ok {
		typ, ok = parseProfileTypeLower(typeStr) // route model binding is case-insensitive
		if !ok {
			writeValidationProblem(w, [][2]string{{"type", "The value '" + typeStr + "' is not valid."}})
			return
		}
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeUnsupportedMediaPD(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeBadRequestPD(w)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		writeValidationProblem(w, [][2]string{
			{"", "A non-empty request body is required."},
			{"dto", "The dto field is required."},
		})
		return
	}
	var dto HistoryRecordUpdateDto
	if err := json.Unmarshal(body, &dto); err != nil {
		writeValidationProblem(w, [][2]string{
			{"$", jsonBindErrorText(err, body)},
			{"dto", "The dto field is required."},
		})
		return
	}
	rec, err := s.store.GetByTypeHash(typ, hash)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if rec == nil {
		writeNotFoundPD(w)
		return
	}
	if dto.Version == nil {
		v := rec.Version + 1
		dto.Version = &v
	}
	if dto.LastModified == nil {
		now := NewAPITime(time.Now().UTC())
		dto.LastModified = &now
	}
	if !historyShouldUpdate(rec.Version, *dto.Version, rec.LastModified, dto.LastModified.Time) {
		writeJSON(w, http.StatusConflict, rec.toUpdateDTO())
		return
	}
	if dto.IsDelete != nil && !*dto.IsDelete && rec.IsDeleted && rec.TransferDataFile != "" {
		writeNotFoundPD(w)
		return
	}
	if dto.Starred != nil {
		rec.Stared = *dto.Starred
	}
	if dto.Pinned != nil {
		rec.Pinned = *dto.Pinned
	}
	if dto.IsDelete != nil {
		rec.IsDeleted = *dto.IsDelete
	}
	rec.LastModified = dto.LastModified.Time
	if dto.LastAccessed != nil {
		rec.LastAccessed = dto.LastAccessed.Time
	}
	rec.Version = *dto.Version
	if err := s.store.Update(rec); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.notifyHistory(rec)
	s.deleteRecordDataIfDeleted(rec)
	w.WriteHeader(http.StatusOK)
}

func parseProfileTypeLower(s string) (ProfileType, bool) {
	switch strings.ToLower(s) {
	case "text":
		return TypeText, true
	case "file":
		return TypeFile, true
	case "image":
		return TypeImage, true
	case "group":
		return TypeGroup, true
	case "unknown":
		return TypeUnknown, true
	case "none":
		return TypeNone, true
	}
	return TypeNone, false
}

// ---------- statistics / clear ----------

func (s *Server) handleHistoryStatistics(w http.ResponseWriter, r *http.Request) {
	total, active, starred, deleted, err := s.store.Counts()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	var sizeBytes int64
	_ = filepath.WalkDir(s.store.historyDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			sizeBytes += info.Size()
		}
		return nil
	})
	mb := math.Round(float64(sizeBytes)/(1024*1024)*100) / 100
	if sizeBytes > 0 && mb == 0 {
		mb = 0.01
	}
	writeJSON(w, http.StatusOK, HistoryStatisticsDto{
		ActiveCount:     active,
		StarredCount:    starred,
		DeletedCount:    deleted,
		TotalCount:      total,
		TotalFileSizeMB: mb,
	})
}

func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	recs, err := s.store.DeleteAll()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	seen := map[string]struct{}{}
	for _, rec := range recs {
		key := workingDirName(rec.Type, rec.Hash)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		s.store.deleteWorkingDir(rec.Type, rec.Hash)
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": len(recs)})
}

// ---------- misc ----------

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// randSuffix mimics Path.GetRandomFileName(): 8 chars + '.' + 3 chars.
func randSuffix() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b[:8]) + "." + string(b[8:])
}
