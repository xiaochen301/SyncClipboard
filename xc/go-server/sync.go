package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handlerError carries an HTTP status + official-style plain text message.
type handlerError struct {
	Code    int
	Message string
}

func (e *handlerError) Error() string { return e.Message }

func httpErr(code int, msg string) *handlerError { return &handlerError{Code: code, Message: msg} }

// writeErr mirrors ASP.NET BadRequest(msg): exact body, no trailing newline.
func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

func writePlain(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := marshalJSON(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ---------- small helpers ----------

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (s *Server) profilePath() string { return filepath.Join(s.store.root, "SyncClipboard.json") }

// ---------- root / propfind / file folder ----------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writePlain(w, "Server is running.")
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFileFolderEnsure(w http.ResponseWriter, r *http.Request) {
	_ = os.MkdirAll(s.store.fileDir(), 0o755)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleFileFolderDelete(w http.ResponseWriter, r *http.Request) {
	_ = os.RemoveAll(s.store.fileDir())
	w.WriteHeader(http.StatusOK)
}

// ---------- current profile ----------

func (s *Server) loadProfileFile() (ProfileDto, string) {
	b, err := os.ReadFile(s.profilePath())
	if err != nil {
		d := emptyTextProfile()
		return d, etagFor(nil)
	}
	var dto ProfileDto
	if err := json.Unmarshal(b, &dto); err != nil {
		d := emptyTextProfile()
		return d, etagFor(nil)
	}
	return dto, etagFor(b)
}

func etagFor(b []byte) string {
	h := sha256Bytes(b)
	return `"` + strings.ToLower(h[:32]) + `"`
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	dto, _ := s.loadProfileFile()
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	// official [FromBody]: non-JSON content types -> 415 ProblemDetails
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeUnsupportedMediaPD(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
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
	var dto ProfileDto
	if err := json.Unmarshal(body, &dto); err != nil {
		writeValidationProblem(w, [][2]string{
			{"$", jsonBindErrorText(err, body)},
			{"dto", "The dto field is required."},
		})
		return
	}
	if herr := s.putProfile(dto); herr != nil {
		writeErr(w, herr.Code, herr.Message)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// putProfile implements the official PUT /SyncClipboard.json semantics.
func (s *Server) putProfile(dto ProfileDto) *handlerError {
	// 1. normalize TransferDataHash
	if dto.TransferDataHash != nil {
		n, err := normalizeSHA256(*dto.TransferDataHash)
		if err != nil {
			return httpErr(http.StatusBadRequest, err.Error())
		}
		dto.TransferDataHash = &n
	}
	if !dto.HasData && dto.TransferDataHash != nil {
		return httpErr(http.StatusBadRequest, "TransferDataHash cannot be set when HasData is false")
	}

	// 2. dedup: reuse an existing (non-deleted) history entry with the same Type+Hash
	if strings.TrimSpace(dto.Hash) != "" {
		rec, err := s.store.GetByTypeHash(dto.Type, dto.Hash)
		if err != nil {
			return httpErr(http.StatusInternalServerError, err.Error())
		}
		if rec != nil && !rec.IsDeleted {
			now := time.Now().UTC()
			rec.LastModified = now
			rec.LastAccessed = now
			rec.Version++
			if err := s.store.Update(rec); err != nil {
				return httpErr(http.StatusInternalServerError, err.Error())
			}
			s.notifyHistory(rec)
			p := rec.toProfileDto()
			if err := s.saveProfileAndNotify(&p); err != nil {
				return httpErr(http.StatusInternalServerError, err.Error())
			}
			return nil
		}
	}

	// 3. create a new profile
	typ := dto.Type
	finalHash := strings.ToUpper(strings.TrimSpace(dto.Hash))
	var contentHash, fileName, stagingPath string

	if !dto.HasData {
		if typ != TypeText {
			return httpErr(http.StatusBadRequest, "Inline data does not match the profile hash.")
		}
		h := sha256Bytes([]byte(dto.Text))
		if finalHash != "" && !strings.EqualFold(h, finalHash) {
			return httpErr(http.StatusBadRequest, "Inline data does not match the profile hash.")
		}
		finalHash = h
	} else {
		if dto.DataName == nil || strings.TrimSpace(*dto.DataName) == "" {
			return httpErr(http.StatusBadRequest, "DataName cannot be null or empty when HasData is true")
		}
		fileName = filepath.Base(strings.TrimSpace(*dto.DataName))
		stagingPath = filepath.Join(s.store.fileDir(), fileName)
		if !fileExists(stagingPath) {
			return httpErr(http.StatusNotFound, "Transfer data file not found")
		}
		actual, err := sha256File(stagingPath)
		if err != nil {
			return httpErr(http.StatusInternalServerError, err.Error())
		}
		if dto.TransferDataHash != nil && !strings.EqualFold(actual, *dto.TransferDataHash) {
			return httpErr(http.StatusBadRequest, "Hash is not match data.")
		}
		contentHash = actual
		if typ == TypeFile && isImageFileName(fileName) {
			typ = TypeImage
		}
		if typ == TypeText {
			if finalHash != "" && !strings.EqualFold(actual, finalHash) {
				return httpErr(http.StatusBadRequest, "Hash is not match data.")
			}
			finalHash = actual
		} else {
			ph := combineFileHash(fileName, actual)
			if finalHash != "" && !strings.EqualFold(ph, finalHash) {
				return httpErr(http.StatusBadRequest, "Hash is not match data.")
			}
			finalHash = ph
		}
	}
	if finalHash == "" {
		return httpErr(http.StatusBadRequest, "Hash is required")
	}

	now := time.Now().UTC()
	rec := &Record{
		UserID:       hardcodedUserID,
		Type:         typ,
		Text:         dto.Text,
		Hash:         finalHash,
		CreateTime:   now,
		LastAccessed: now,
		LastModified: now,
		Version:      0, // official: PUT-created records start at version 0
		From:         "",
		FilePaths:    []string{},
		Tags:         []string{},
	}
	if dto.Size != nil {
		rec.Size = *dto.Size
	}
	if !dto.HasData && typ == TypeText {
		// inline text size is computed like C# text.Length (UTF-16 units)
		rec.Size = int64(utf16Len(dto.Text))
	}
	if dto.HasData {
		rel, err := s.store.moveToPersistent(stagingPath, fileName, typ, finalHash)
		if err != nil {
			return httpErr(http.StatusInternalServerError, err.Error())
		}
		rec.TransferDataFile = rel
		rec.TransferDataHash = &contentHash
		rec.TransferDataSha256 = ""
		rec.FilePaths = []string{rel}
		if typ != TypeText {
			rec.Text = fileName
		}
		if rec.Size == 0 {
			if st, err := os.Stat(filepath.Join(s.store.historyDir(), filepath.FromSlash(rel))); err == nil {
				rec.Size = st.Size()
			}
		}
	}

	// 4. upsert: official AddProfile updates an existing (possibly soft-deleted) entry
	existing, err := s.store.GetByTypeHash(typ, finalHash)
	if err != nil {
		return httpErr(http.StatusInternalServerError, err.Error())
	}
	if existing != nil {
		existing.LastAccessed = now
		existing.IsDeleted = false
		existing.LastModified = now
		if rec.TransferDataFile != "" {
			existing.TransferDataFile = rec.TransferDataFile
			existing.TransferDataHash = rec.TransferDataHash
			existing.FilePaths = rec.FilePaths
		}
		existing.Version++
		if err := s.store.Update(existing); err != nil {
			return httpErr(http.StatusInternalServerError, err.Error())
		}
		s.notifyHistory(existing)
		p := existing.toProfileDto()
		if err := s.saveProfileAndNotify(&p); err != nil {
			return httpErr(http.StatusInternalServerError, err.Error())
		}
		return nil
	}
	if err := s.store.Insert(rec); err != nil {
		return httpErr(http.StatusInternalServerError, err.Error())
	}
	s.notifyHistory(rec)
	p := rec.toProfileDto()
	if err := s.saveProfileAndNotify(&p); err != nil {
		return httpErr(http.StatusInternalServerError, err.Error())
	}
	return nil
}

func (s *Server) saveProfileAndNotify(p *ProfileDto) error {
	// official v3.1.7/v3.3.1 does not emit syncedAt; keep parity.
	b, err := marshalJSON(toFileDTO(*p))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.profilePath(), b); err != nil {
		return err
	}
	s.broadcastProfile(*p)
	return nil
}

// ---------- file transfer endpoints ----------

func (s *Server) fileNameFromPath(r *http.Request) (string, bool) {
	esc := r.URL.EscapedPath()
	const prefix = "/file/"
	if !strings.HasPrefix(esc, prefix) {
		return "", false
	}
	raw := esc[len(prefix):]
	// ASP.NET keeps the encoded slash form in route values ("a%2Fb.txt"),
	// so preserve %2F/%2f literally through decoding.
	raw = strings.ReplaceAll(raw, "%2F", "%252F")
	raw = strings.ReplaceAll(raw, "%2f", "%252f")
	name, err := url.PathUnescape(raw)
	if err != nil || !safeFileName(name) {
		return "", false
	}
	return name, true
}

func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	name, ok := s.fileNameFromPath(r)
	if !ok {
		writeBadRequestPD(w)
		return
	}
	path := s.findRecentTransferFile(name)
	if path == "" {
		writeNotFoundPD(w)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeNotFoundPD(w)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	_, _ = w.Write(b)
}

// findRecentTransferFile mirrors HistoryService.GetRecentTransferFile: scan all
// records by LastAccessed DESC, match the TransferDataFile basename, resolve it
// under the record's working dir and return the first existing file.
func (s *Server) findRecentTransferFile(name string) string {
	if name == "" {
		return ""
	}
	rows, err := s.store.db.Query(
		`SELECT `+s.store.recCols+` FROM HistoryRecords WHERE UserId=? ORDER BY LastAccessed DESC`,
		hardcodedUserID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := s.store.scanRecord(rows)
		if err != nil {
			return ""
		}
		base := rec.TransferDataFile
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if base != name {
			continue
		}
		if full := s.resolveRecordDataPath(rec); full != "" {
			return full
		}
	}
	return ""
}

// resolveRecordDataPath finds the on-disk file backing a record's transfer
// data. Official semantics: TransferDataFile/FilePaths are relative to the
// record's working dir (history/{Type}_{Hash}/); absolute values pass through.
func (s *Server) resolveRecordDataPath(rec *Record) string {
	wd := s.store.workingDir(rec.Type, rec.Hash)
	cands := make([]string, 0, 4)
	add := func(p string) {
		if p == "" {
			return
		}
		if filepath.IsAbs(p) {
			cands = append(cands, p)
			return
		}
		rel := filepath.FromSlash(p)
		cands = append(cands, filepath.Join(wd, rel))
		cands = append(cands, filepath.Join(s.store.historyDir(), rel))
	}
	add(rec.TransferDataFile)
	for _, fp := range rec.FilePaths {
		add(fp)
	}
	for _, c := range cands {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

func (s *Server) handleFilePut(w http.ResponseWriter, r *http.Request) {
	name, ok := s.fileNameFromPath(r)
	if !ok {
		writeBadRequestPD(w)
		return
	}
	dst := filepath.Join(s.store.fileDir(), name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	f, err := os.Create(dst)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := f.Close(); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------- hub notify helpers ----------

func (s *Server) notifyHistory(rec *Record) {
	dto := rec.toDTO()
	s.hub.Broadcast("RemoteHistoryChanged", dto)
}

func (s *Server) broadcastProfile(p ProfileDto) {
	s.hub.Broadcast("RemoteProfileChanged", p)
}

// deleteRecordDataIfDeleted mirrors DeleteProfileDataIfNeed: remove the on-disk
// working directory when the record is (soft) deleted.
func (s *Server) deleteRecordDataIfDeleted(rec *Record) {
	if rec.IsDeleted {
		s.store.deleteWorkingDir(rec.Type, rec.Hash)
	}
}
