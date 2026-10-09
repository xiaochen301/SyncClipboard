// synccheck is the protocol validation tool for syncclipboard-server-xc.
// Modes:
//
//	synccheck -mode http  -base URL -user U -pass P   (read-only battery)
//	synccheck -mode hub   -base URL ...               (SignalR connect test)
//	synccheck -mode write -base URL ...               (write paths; LOCAL copies only!)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	mode := flag.String("mode", "http", "http | hub | write")
	base := flag.String("base", "http://127.0.0.1:5033", "base URL")
	user := flag.String("user", "admin", "")
	pass := flag.String("pass", "admin", "")
	flag.Parse()

	baseURL := strings.TrimRight(*base, "/")
	c := &http.Client{Timeout: 30 * time.Second}

	do := func(method, path string, body []byte, hdr map[string]string, auth bool) (*http.Response, []byte) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, baseURL+path, rd)
		if err != nil {
			fmt.Printf("REQERR %s %s: %v\n", method, path, err)
			return nil, nil
		}
		if auth {
			req.SetBasicAuth(*user, *pass)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			fmt.Printf("ERR %s %s: %v\n", method, path, err)
			return nil, nil
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	show := func(label string, resp *http.Response, body []byte) {
		if resp == nil {
			fmt.Printf("[%s] NO-RESPONSE\n", label)
			return
		}
		trunc := string(body)
		if len(trunc) > 360 {
			trunc = trunc[:360] + "..."
		}
		fmt.Printf("[%s] %d ct=%q etag=%q body=%q\n", label, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), trunc)
	}

	switch *mode {
	case "http":
		resp, b := do("GET", "/SyncClipboard.json", nil, nil, false)
		show("401-check", resp, b)
		resp, b = do("GET", "/api/version", nil, nil, true)
		show("version", resp, b)
		resp, b = do("GET", "/api/time", nil, nil, true)
		show("time", resp, b)
		resp, b = do("PROPFIND", "/", nil, nil, true)
		show("propfind-root", resp, b)
		resp, b = do("PROPFIND", "/file", nil, nil, true)
		show("propfind-file", resp, b)
		resp, b = do("GET", "/SyncClipboard.json", nil, nil, true)
		show("profile", resp, b)
		resp, b = do("GET", "/api/history/statistics", nil, nil, true)
		show("stats", resp, b)

		var mb bytes.Buffer
		mw := multipart.NewWriter(&mb)
		_ = mw.WriteField("Page", "1")
		_ = mw.WriteField("Types", "All")
		_ = mw.WriteField("SortByLastAccessed", "False")
		_ = mw.Close()
		resp, b = do("POST", "/api/history/query", mb.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()}, true)
		show("query", resp, b)

	case "hub":
		resp, b := do("POST", "/SyncClipboardHub/negotiate?negotiateVersion=1", nil, nil, true)
		show("negotiate", resp, b)
		var neg struct {
			ConnectionToken string `json:"connectionToken"`
			ConnectionID    string `json:"connectionId"`
		}
		if err := json.Unmarshal(b, &neg); err != nil {
			fmt.Println("negotiate parse failed")
			os.Exit(1)
		}
		wsURL := baseURL + "/SyncClipboardHub?id=" + neg.ConnectionToken
		wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
		wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hdr := http.Header{}
		hdr.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(*user+":"+*pass)))
		ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			fmt.Printf("[hub] dial failed: %v\n", err)
			os.Exit(1)
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		wc, cancel2 := context.WithTimeout(ctx, 10*time.Second)
		err = ws.Write(wc, websocket.MessageText, []byte("{\"protocol\":\"json\",\"version\":1}\x1e"))
		cancel2()
		if err != nil {
			fmt.Printf("[hub] handshake write failed: %v\n", err)
			os.Exit(1)
		}
		rc, cancel3 := context.WithTimeout(ctx, 10*time.Second)
		_, data, err := ws.Read(rc)
		cancel3()
		if err != nil {
			fmt.Printf("[hub] handshake read failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("[hub] handshake response: %q\n", string(data))
		fmt.Println("[hub] connected; listening 25s for frames (expecting keepalive pings)...")
		end := time.Now().Add(25 * time.Second)
		for time.Now().Before(end) {
			rc, cancel4 := context.WithTimeout(ctx, time.Until(end))
			_, data, err := ws.Read(rc)
			cancel4()
			if err != nil {
				break
			}
			s := strings.TrimRight(string(data), "\x1e")
			if strings.Contains(s, `"type":6`) {
				fmt.Println("[hub] ping")
				continue
			}
			if len(s) > 300 {
				s = s[:300] + "..."
			}
			fmt.Printf("[hub] frame: %s\n", s)
		}
		fmt.Println("[hub] done")

	case "write":
		stamp := time.Now().Format("20060102-150405")
		name := "synccheck-" + stamp + ".txt"
		content := []byte("synccheck content " + stamp)
		resp, b := do("PUT", "/file/"+name, content, nil, true)
		show("put-file", resp, b)
		ch := sha256Hex(content)
		ph := combineHash(name, ch)

		prof := map[string]any{
			"type": "File", "hash": ph, "text": name, "hasData": true,
			"dataName": name, "transferDataHash": ch, "size": len(content),
		}
		pj, _ := json.Marshal(prof)
		resp, b = do("PUT", "/SyncClipboard.json", pj, map[string]string{"Content-Type": "application/json"}, true)
		show("put-profile", resp, b)
		resp, b = do("GET", "/file/"+name, nil, nil, true)
		show("get-file", resp, b)

		var mb bytes.Buffer
		mw := multipart.NewWriter(&mb)
		now := time.Now().UTC().Format("2006-01-02T15:04:05.0000000+00:00")
		_ = mw.WriteField("hash", ph)
		_ = mw.WriteField("type", "File")
		_ = mw.WriteField("createTime", now)
		_ = mw.WriteField("lastModified", now)
		_ = mw.WriteField("lastAccessed", now)
		_ = mw.WriteField("starred", "False")
		_ = mw.WriteField("pinned", "False")
		_ = mw.WriteField("version", "1")
		_ = mw.WriteField("isDeleted", "False")
		_ = mw.WriteField("text", name)
		_ = mw.WriteField("size", fmt.Sprint(len(content)))
		_ = mw.WriteField("hasData", "True")
		fw, _ := mw.CreateFormFile("data", name)
		_, _ = fw.Write(content)
		_ = mw.Close()
		hdr := map[string]string{"Content-Type": mw.FormDataContentType(), "X-SyncClipboard-Transfer-Data-Hash": ch}
		resp, b = do("POST", "/api/history", mb.Bytes(), hdr, true)
		show("post-history", resp, b)

		patch := []byte(`{"starred":true,"version":9}`)
		resp, b = do("PATCH", "/api/history/File/"+ph, patch, map[string]string{"Content-Type": "application/json"}, true)
		show("patch", resp, b)
		resp, b = do("GET", "/api/history/File-"+ph, nil, nil, true)
		show("get-record", resp, b)
		resp, b = do("GET", "/api/history/File-"+ph+"/data", nil, nil, true)
		show("get-record-data", resp, b)
		resp, b = do("GET", "/SyncClipboard.json", nil, nil, true)
		show("profile-after", resp, b)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func combineHash(fileName, contentSHA string) string {
	return sha256Hex([]byte(fileName + "|" + strings.ToUpper(contentSHA)))
}
