package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const hardcodedUserID = "default_user"

const recCols = "ID, UserId, Type, Text, Size, TransferDataFile, TransferDataSha256, TransferDataMd5, FilePaths, Hash, CreateTime, LastAccessed, LastModified, Stared, Pinned, \"From\", Tags, ExtraData, Version, IsDeleted"

type Store struct {
	db           *sql.DB
	root         string // data root (contains data/history.db, file/, history/)
	mu           sync.Mutex
	hasTDHashCol bool // TransferDataHash column present (added by official >= 3.3)
}

func openStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "file"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "history"), 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(root, "data", "history.db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(8000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	st := &Store{db: db, root: root}
	st.detectColumns()
	return st, nil
}

// detectColumns checks for optional columns added by newer official versions.
func (s *Store) detectColumns() {
	rows, err := s.db.Query("PRAGMA table_info(HistoryRecords)")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return
		}
		if name == "TransferDataHash" {
			s.hasTDHashCol = true
		}
	}
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- path helpers ----------

func (s *Store) stagingPath(name string) string { return filepath.Join(s.root, "file", name) }
func (s *Store) fileDir() string                { return filepath.Join(s.root, "file") }
func (s *Store) historyDir() string             { return filepath.Join(s.root, "history") }

func workingDirName(t ProfileType, hash string) string {
	return fmt.Sprintf("%s_%s", t.String(), strings.ToUpper(hash))
}

func (s *Store) workingDir(t ProfileType, hash string) string {
	return filepath.Join(s.historyDir(), workingDirName(t, hash))
}

func (s *Store) deleteWorkingDir(t ProfileType, hash string) {
	_ = os.RemoveAll(s.workingDir(t, hash))
}

// moveToPersistent moves the staging file into history/{Type}_{HASH}/{filename}.
func (s *Store) moveToPersistent(staging, filename string, t ProfileType, hash string) (string, error) {
	dir := s.workingDir(t, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filename)
	if err := os.Rename(staging, dst); err != nil {
		// cross-device fallback: copy + delete
		if b, rerr := os.ReadFile(staging); rerr == nil {
			if werr := os.WriteFile(dst, b, 0o644); werr != nil {
				return "", werr
			}
			_ = os.Remove(staging)
		} else {
			return "", err
		}
	}
	// official semantics: TransferDataFile is stored relative to the record's
	// working dir, i.e. just the file name for single-file records.
	return filename, nil
}

// ---------- row scanning ----------

type scanner interface{ Scan(dest ...any) error }

func scanRecord(row scanner) (*Record, error) {
	var r Record
	var t, stared, pinned, isDel int64
	var fp, tags string
	var extra sql.NullString
	var ct, la, lm string
	err := row.Scan(&r.ID, &r.UserID, &t, &r.Text, &r.Size, &r.TransferDataFile,
		&r.TransferDataSha256, &r.TransferDataMd5, &fp, &r.Hash,
		&ct, &la, &lm, &stared, &pinned, &r.From, &tags, &extra, &r.Version, &isDel)
	if err != nil {
		return nil, err
	}
	r.Type = ProfileType(t)
	r.Stared = stared != 0
	r.Pinned = pinned != 0
	r.IsDeleted = isDel != 0
	if ct != "" {
		r.CreateTime, _ = parseTimeFlexible(ct)
	}
	if la != "" {
		r.LastAccessed, _ = parseTimeFlexible(la)
	}
	if lm != "" {
		r.LastModified, _ = parseTimeFlexible(lm)
	}
	_ = json.Unmarshal([]byte(fp), &r.FilePaths)
	if r.FilePaths == nil {
		r.FilePaths = []string{}
	}
	_ = json.Unmarshal([]byte(tags), &r.Tags)
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if extra.Valid {
		v := extra.String
		r.ExtraData = &v
	}
	return &r, nil
}

func marshalStrings(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------- CRUD ----------

// GetByTypeHash mirrors the official Query (Hash LIKE hash, case-insensitive).
func (s *Store) GetByTypeHash(t ProfileType, hash string) (*Record, error) {
	row := s.db.QueryRow("SELECT "+recCols+" FROM HistoryRecords WHERE UserId=? AND Type=? AND Hash LIKE ? LIMIT 1",
		hardcodedUserID, int(t), hash)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

func (s *Store) getByID(id int64) (*Record, error) {
	row := s.db.QueryRow("SELECT "+recCols+" FROM HistoryRecords WHERE ID=?", id)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return rec, err
}

func (s *Store) Insert(rec *Record) error {
	res, err := s.db.Exec(`INSERT INTO HistoryRecords
		(UserId,Type,Text,Size,TransferDataFile,TransferDataSha256,TransferDataMd5,FilePaths,Hash,
		 CreateTime,LastAccessed,LastModified,Stared,Pinned,"From",Tags,ExtraData,Version,IsDeleted)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.UserID, int(rec.Type), rec.Text, rec.Size, rec.TransferDataFile,
		rec.TransferDataSha256, rec.TransferDataMd5, marshalStrings(rec.FilePaths), rec.Hash,
		formatDBTime(rec.CreateTime), formatDBTime(rec.LastAccessed), formatDBTime(rec.LastModified),
		b2i(rec.Stared), b2i(rec.Pinned), rec.From, marshalStrings(rec.Tags),
		rec.ExtraData, rec.Version, b2i(rec.IsDeleted))
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err == nil {
		rec.ID = id
	}
	s.syncLegacyHashColumn(rec)
	return nil
}

// syncLegacyHashColumn keeps the optional TransferDataHash column (added by
// newer official versions) in sync when present.
func (s *Store) syncLegacyHashColumn(rec *Record) {
	if !s.hasTDHashCol || rec.TransferDataSha256 == "" || rec.ID == 0 {
		return
	}
	_, _ = s.db.Exec("UPDATE HistoryRecords SET TransferDataHash=? WHERE ID=?", rec.TransferDataSha256, rec.ID)
}

func (s *Store) Update(rec *Record) error {
	_, err := s.db.Exec(`UPDATE HistoryRecords SET
		Type=?,Text=?,Size=?,TransferDataFile=?,TransferDataSha256=?,TransferDataMd5=?,FilePaths=?,Hash=?,
		CreateTime=?,LastAccessed=?,LastModified=?,Stared=?,Pinned=?,"From"=?,Tags=?,ExtraData=?,Version=?,IsDeleted=?
		WHERE ID=?`,
		int(rec.Type), rec.Text, rec.Size, rec.TransferDataFile, rec.TransferDataSha256, rec.TransferDataMd5,
		marshalStrings(rec.FilePaths), rec.Hash,
		formatDBTime(rec.CreateTime), formatDBTime(rec.LastAccessed), formatDBTime(rec.LastModified),
		b2i(rec.Stared), b2i(rec.Pinned), rec.From, marshalStrings(rec.Tags),
		rec.ExtraData, rec.Version, b2i(rec.IsDeleted), rec.ID)
	if err == nil {
		s.syncLegacyHashColumn(rec)
	}
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func typeBitList(mask uint32) []int {
	var out []int
	for t := 0; t <= int(TypeNone); t++ {
		if mask&(1<<uint(t)) != 0 {
			out = append(out, t)
		}
	}
	return out
}

// ListHistory mirrors HistoryService.GetListAsync.
func (s *Store) ListHistory(q historyQuery) ([]*Record, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	where := []string{"UserId = ?"}
	args := []any{hardcodedUserID}
	timeCol := "CreateTime"
	if q.SortByLastAccessed {
		timeCol = "LastAccessed"
	}
	if q.After != nil {
		where = append(where, timeCol+" >= ?")
		args = append(args, formatDBTime(*q.After))
	}
	if q.Before != nil {
		where = append(where, timeCol+" < ?")
		args = append(args, formatDBTime(*q.Before))
	}
	if q.Types != 0xF { // ProfileTypeFilter.All
		maskList := typeBitList(q.Types)
		if len(maskList) == 0 {
			return []*Record{}, nil
		}
		ph := make([]string, len(maskList))
		for i, v := range maskList {
			ph[i] = "?"
			args = append(args, v)
		}
		where = append(where, "Type IN ("+strings.Join(ph, ",")+")")
	}
	if q.SearchText != "" {
		where = append(where, "Text LIKE ?")
		args = append(args, "%"+q.SearchText+"%")
	}
	if q.Starred != nil {
		where = append(where, "Stared = ?")
		args = append(args, b2i(*q.Starred))
	}
	if q.ModifiedAfter != nil {
		where = append(where, "LastModified >= ?")
		args = append(args, formatDBTime(*q.ModifiedAfter))
	}
	query := "SELECT " + recCols + " FROM HistoryRecords WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + timeCol + " DESC, ID DESC LIMIT 50 OFFSET ?"
	args = append(args, (q.Page-1)*50)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Counts returns statistics counts.
func (s *Store) Counts() (total, active, starred, deleted int, err error) {
	row := s.db.QueryRow(`SELECT COUNT(*),
		SUM(CASE WHEN IsDeleted=0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN Stared=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN IsDeleted=1 THEN 1 ELSE 0 END)
		FROM HistoryRecords WHERE UserId=?`, hardcodedUserID)
	var a, st, d sql.NullInt64
	var t int
	if err := row.Scan(&t, &a, &st, &d); err != nil {
		return 0, 0, 0, 0, err
	}
	return t, int(a.Int64), int(st.Int64), int(d.Int64), nil
}

// ActiveCount returns the number of non-deleted records.
func (s *Store) ActiveCount() (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM HistoryRecords WHERE UserId=? AND IsDeleted=0`, hardcodedUserID)
	var n int
	err := row.Scan(&n)
	return n, err
}

// DeleteAll removes all rows and returns the removed records.
func (s *Store) DeleteAll() ([]*Record, error) {
	rows, err := s.db.Query("SELECT "+recCols+" FROM HistoryRecords WHERE UserId=?", hardcodedUserID)
	if err != nil {
		return nil, err
	}
	var recs []*Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = s.db.Exec("DELETE FROM HistoryRecords WHERE UserId=?", hardcodedUserID)
	return recs, err
}

// SelectForTrim collects the oldest records to remove for max-count/retention.
func (s *Store) selectOldest(extraWhere string, extraArgs []any, limit int) ([]*Record, error) {
	order := `CASE WHEN LastModified > LastAccessed THEN LastModified ELSE LastAccessed END ASC`
	args := []any{hardcodedUserID}
	args = append(args, extraArgs...)
	args = append(args, limit)
	q := "SELECT " + recCols + " FROM HistoryRecords WHERE UserId=? AND Stared=0 AND Pinned=0 AND IsDeleted=0" +
		extraWhere + " ORDER BY " + order + " LIMIT ?"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SelectDeletedBefore returns deleted tombstones older than cutoff.
func (s *Store) SelectDeletedBefore(cutoff time.Time) ([]*Record, error) {
	rows, err := s.db.Query("SELECT "+recCols+" FROM HistoryRecords WHERE UserId=? AND IsDeleted=1 AND LastModified < ?",
		hardcodedUserID, formatDBTime(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) PurgeRecord(id int64) error {
	_, err := s.db.Exec("DELETE FROM HistoryRecords WHERE ID=?", id)
	return err
}

// ListWorkingDirs returns history/{Type}_{Hash} directory names present on disk.
func (s *Store) ListWorkingDirs() ([]string, error) {
	entries, err := os.ReadDir(s.historyDir())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ---------- retention (mirrors HistoryCleaner) ----------

// TrimToMaxCount removes oldest non-starred/pinned records until count <= max.
// Returns removed records (caller broadcasts + deletes files).
func (s *Store) TrimToMaxCount(max uint32) ([]*Record, error) {
	if max == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []*Record
	for {
		count, err := s.ActiveCount()
		if err != nil {
			return removed, err
		}
		if count <= int(max) {
			return removed, nil
		}
		n := count - int(max)
		if n > 500 {
			n = 500
		}
		batch, err := s.selectOldest("", nil, n)
		if err != nil || len(batch) == 0 {
			return removed, err
		}
		now := time.Now().UTC()
		for _, rec := range batch {
			rec.IsDeleted = true
			rec.LastModified = now
			rec.Version++
			if err := s.Update(rec); err != nil {
				return removed, err
			}
			removed = append(removed, rec)
		}
	}
}

// TrimRetention removes records older than retentionMinutes (0 = disabled).
func (s *Store) TrimRetention(minutes uint32) ([]*Record, error) {
	if minutes == 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []*Record
	for {
		batch, err := s.selectOldest(" AND LastModified < ? AND LastAccessed < ?",
			[]any{formatDBTime(cutoff), formatDBTime(cutoff)}, 500)
		if err != nil || len(batch) == 0 {
			return removed, err
		}
		now := time.Now().UTC()
		for _, rec := range batch {
			rec.IsDeleted = true
			rec.LastModified = now
			rec.Version++
			if err := s.Update(rec); err != nil {
				return removed, err
			}
			removed = append(removed, rec)
		}
	}
}
