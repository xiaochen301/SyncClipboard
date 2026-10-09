package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------- ProfileType ----------

type ProfileType int

const (
	TypeText ProfileType = iota
	TypeFile
	TypeImage
	TypeGroup
	TypeUnknown
	TypeNone
)

func (t ProfileType) String() string {
	switch t {
	case TypeText:
		return "Text"
	case TypeFile:
		return "File"
	case TypeImage:
		return "Image"
	case TypeGroup:
		return "Group"
	case TypeUnknown:
		return "Unknown"
	case TypeNone:
		return "None"
	}
	return "None"
}

func (t ProfileType) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

func (t *ProfileType) UnmarshalJSON(b []byte) error {
	// string form (case-insensitive) or integer form
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "text":
			*t = TypeText
		case "file":
			*t = TypeFile
		case "image":
			*t = TypeImage
		case "group":
			*t = TypeGroup
		case "unknown":
			*t = TypeUnknown
		case "none", "":
			*t = TypeNone
		default:
			return fmt.Errorf("unknown ProfileType %q", s)
		}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*t = ProfileType(n)
		return nil
	}
	return fmt.Errorf("invalid ProfileType %s", string(b))
}

// parseProfileTypeString mirrors Enum.TryParse (case-SENSITIVE names, integers allowed).
func parseProfileTypeString(s string) (ProfileType, bool) {
	switch s {
	case "Text":
		return TypeText, true
	case "File":
		return TypeFile, true
	case "Image":
		return TypeImage, true
	case "Group":
		return TypeGroup, true
	case "Unknown":
		return TypeUnknown, true
	case "None":
		return TypeNone, true
	}
	if n, err := strconv.Atoi(s); err == nil {
		return ProfileType(n), true
	}
	return TypeNone, false
}

// ---------- ProfileTypeFilter (bit flags) ----------

func parseTypeFilter(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0xF, true // All (Text|File|Image|Group)
	}
	if n, err := strconv.ParseUint(s, 10, 32); err == nil {
		return uint32(n), true
	}
	switch strings.ToLower(s) {
	case "none":
		return 0, true
	case "text":
		return 1 << TypeText, true
	case "file":
		return 1 << TypeFile, true
	case "image":
		return 1 << TypeImage, true
	case "group":
		return 1 << TypeGroup, true
	case "fileandgroup":
		return (1 << TypeFile) | (1 << TypeGroup), true
	case "all":
		return 0xF, true
	}
	return 0, false
}

// ---------- APITime ----------

// APITime serializes like .NET DateTimeOffset (".fffffff trimmed, +00:00").
type APITime struct{ time.Time }

func NewAPITime(t time.Time) APITime { return APITime{t.UTC()} }

func (t APITime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + formatAPITime(t.Time) + `"`), nil
}

func (t *APITime) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	parsed, ok := parseTimeFlexible(s)
	if !ok {
		return fmt.Errorf("invalid time %q", s)
	}
	t.Time = parsed
	return nil
}

// ---------- ProfileDto ----------

type ProfileDto struct {
	Type             ProfileType `json:"type"`
	Hash             string      `json:"hash"`
	Text             string      `json:"text"`
	HasData          bool        `json:"hasData"`
	DataName         *string     `json:"dataName"`
	TransferDataHash *string     `json:"transferDataHash,omitempty"`
	Size             *int64      `json:"size,omitempty"`
	SyncedAt         *APITime    `json:"syncedAt,omitempty"`
}

// emptyTextProfile mirrors new TextProfile(string.Empty).ToProfileDto().
func emptyTextProfile() ProfileDto {
	h := sha256Bytes(nil)
	size := int64(0)
	return ProfileDto{
		Type:     TypeText,
		Hash:     h,
		Text:     "",
		HasData:  false,
		DataName: nil,
		Size:     &size,
	}
}

// profileFileDTO is the on-disk (PascalCase) representation of the current profile.
type profileFileDTO struct {
	Type             ProfileType `json:"Type"`
	Hash             string      `json:"Hash"`
	Text             string      `json:"Text"`
	HasData          bool        `json:"HasData"`
	DataName         *string     `json:"DataName"`
	TransferDataHash *string     `json:"TransferDataHash,omitempty"`
	Size             *int64      `json:"Size,omitempty"`
	SyncedAt         *APITime    `json:"SyncedAt,omitempty"`
}

func toFileDTO(p ProfileDto) profileFileDTO {
	return profileFileDTO{
		Type:             p.Type,
		Hash:             p.Hash,
		Text:             p.Text,
		HasData:          p.HasData,
		DataName:         p.DataName,
		TransferDataHash: p.TransferDataHash,
		Size:             p.Size,
		SyncedAt:         p.SyncedAt,
	}
}

// ---------- History DTOs ----------

type HistoryRecordDto struct {
	Hash         string      `json:"hash"`
	Text         string      `json:"text"`
	Type         ProfileType `json:"type"`
	CreateTime   APITime     `json:"createTime"`
	LastModified APITime     `json:"lastModified"`
	LastAccessed APITime     `json:"lastAccessed"`
	Starred      bool        `json:"starred"`
	Pinned       bool        `json:"pinned"`
	Size         int64       `json:"size"`
	HasData      bool        `json:"hasData"`
	Version      int         `json:"version"`
	IsDeleted    bool        `json:"isDeleted"`
}

type HistoryRecordUpdateDto struct {
	Starred      *bool    `json:"starred"`
	Pinned       *bool    `json:"pinned"`
	IsDelete     *bool    `json:"isDelete"`
	Version      *int     `json:"version"`
	LastModified *APITime `json:"lastModified"`
	LastAccessed *APITime `json:"lastAccessed"`
}

type HistoryStatisticsDto struct {
	ActiveCount     int     `json:"activeCount"`
	StarredCount    int     `json:"starredCount"`
	DeletedCount    int     `json:"deletedCount"`
	TotalCount      int     `json:"totalCount"`
	TotalFileSizeMB float64 `json:"totalFileSizeMB"`
}

// ---------- Record ----------

type Record struct {
	ID                 int64
	UserID             string
	Type               ProfileType
	Text               string
	Size               int64
	TransferDataFile   string // relative path "{Type}_{Hash}/{filename}"
	TransferDataSha256 string
	TransferDataMd5    string
	FilePaths          []string
	Hash               string
	CreateTime         time.Time
	LastAccessed       time.Time
	LastModified       time.Time
	Stared             bool
	Pinned             bool
	From               string
	Tags               []string
	ExtraData          *string
	Version            int
	IsDeleted          bool
}

// transferHash returns the content SHA-256 recorded for this record, if any.
func (r *Record) transferHash() *string {
	if s := strings.TrimSpace(r.TransferDataSha256); s != "" {
		v := strings.ToUpper(s)
		return &v
	}
	return nil
}

func (r *Record) hasData() bool {
	return len(r.FilePaths) > 0 || r.TransferDataFile != ""
}

func (r *Record) toDTO() HistoryRecordDto {
	return HistoryRecordDto{
		Hash:         r.Hash,
		Text:         r.Text,
		Type:         r.Type,
		CreateTime:   NewAPITime(r.CreateTime),
		LastModified: NewAPITime(r.LastModified),
		LastAccessed: NewAPITime(r.LastAccessed),
		Starred:      r.Stared,
		Pinned:       r.Pinned,
		Size:         r.Size,
		HasData:      r.hasData(),
		Version:      r.Version,
		IsDeleted:    r.IsDeleted,
	}
}

func (r *Record) toUpdateDTO() HistoryRecordUpdateDto {
	v := r.Version
	lm := NewAPITime(r.LastModified)
	la := NewAPITime(r.LastAccessed)
	s := r.Stared
	p := r.Pinned
	d := r.IsDeleted
	return HistoryRecordUpdateDto{
		Starred:      &s,
		Pinned:       &p,
		IsDelete:     &d,
		Version:      &v,
		LastModified: &lm,
		LastAccessed: &la,
	}
}

// toProfileDto converts a record into the profile DTO the official server would serve.
func (r *Record) toProfileDto() ProfileDto {
	dto := ProfileDto{
		Type:    r.Type,
		Hash:    r.Hash,
		Text:    r.Text,
		HasData: r.hasData(),
	}
	size := r.Size
	dto.Size = &size
	if r.hasData() {
		name := r.TransferDataFile
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if len(r.FilePaths) > 0 {
			name = r.FilePaths[0]
			if i := strings.LastIndexByte(name, '/'); i >= 0 {
				name = name[i+1:]
			}
		}
		if name != "" {
			dto.DataName = &name
		}
		if h := r.transferHash(); h != nil {
			dto.TransferDataHash = h
		}
	} else if r.Type == TypeText {
		// keep parity: text profiles expose size but no dataName
	}
	return dto
}

// parseProfileId splits "Type-Hash" like Profile.ParseProfileId.
func parseProfileId(s string) (ProfileType, string, bool) {
	i := strings.IndexByte(s, '-')
	if i < 0 {
		return TypeNone, "", false
	}
	t, ok := parseProfileTypeString(s[:i])
	if !ok {
		return TypeNone, "", false
	}
	return t, s[i+1:], true
}

// historyQuery mirrors HistoryQueryDto.
type historyQuery struct {
	Page              int
	Before            *time.Time
	After             *time.Time
	ModifiedAfter     *time.Time
	Types             uint32
	SearchText        string
	Starred           *bool
	SortByLastAccessed bool
}
