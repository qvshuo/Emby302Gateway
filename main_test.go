package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateway(t *testing.T) {
	var queries, resolves atomic.Int32
	var openlistURL string
	openlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolves.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "token" {
			t.Error("OpenList request")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["path"] != "/115/a b.mkv" {
			t.Error("OpenList path")
		}
		if r.UserAgent() == "fail" {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]string{"raw_url": "https://cdn.example/movie?ua=" + url.QueryEscape(r.UserAgent())}})
	}))
	defer openlist.Close()
	openlistURL = openlist.URL
	playback := func() string {
		return fmt.Sprintf(`{ "PlaySessionId":"session", "Extra":9007199254740993, "MediaSources":[{"Id":"remote","Path":%q,"IsRemote":true,"TranscodingUrl":"old","Unknown":9007199254740993},{"Id":"local","Path":"/mnt/a.mkv","IsRemote":false,"SupportsTranscoding":true},{"Id":"other","Path":"https://other.example/d/a.mkv","IsRemote":true,"SupportsTranscoding":true},{"Id":"live","Path":%q,"IsRemote":true,"IsInfiniteStream":true,"SupportsTranscoding":true}]}`, openlistURL+"/d/115/a%20b.mkv", openlistURL+"/d/115/a%20b.mkv")
	}
	untouched := `{ "MediaSources" : [{"Id":"other","Path":"https://other.example/d/a.mkv","IsRemote":true,"SupportsTranscoding":true}], "Extra":9007199254740993 }`
	var receivedQuery, receivedBody, receivedEncoding string
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Set-Cookie", "session=kept")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/user":
			if r.Header.Get("X-Emby-Token") == "blocked" {
				io.WriteString(w, `{"Id":"user","Policy":{"EnableMediaPlayback":false}}`)
				return
			}
			io.WriteString(w, `{"Id":"user","Policy":{"EnableMediaPlayback":true}}`)
		case "/Sessions":
			io.WriteString(w, `[{"DeviceId":"device","UserId":"user"}]`)
		case "/Items":
			queries.Add(1)
			if r.URL.Query().Get("api_key") != "client" && r.Header.Get("X-Emby-Token") != "client" && r.Header.Get("X-Emby-Token") != "member" && r.Header.Get("X-Emby-Token") != "blocked" && !strings.Contains(r.Header.Get("Authorization"), `Token="client"`) {
				w.WriteHeader(401)
				return
			}
			item := r.URL.Query().Get("Ids")
			if item == "query-error" {
				w.WriteHeader(500)
				return
			}
			if item == "malformed" {
				io.WriteString(w, "invalid")
				return
			}
			isRemote, infinite, path := true, false, openlistURL+"/d/115/a%20b.mkv"
			switch item {
			case "local":
				isRemote = false
				path = "/mnt/a.mkv"
			case "other":
				path = "https://other.example/d/a.mkv"
			case "live":
				infinite = true
			case "relative":
				path = "/d/115/a.mkv"
			}
			json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Path": "/library/" + item + ".strm", "MediaSources": []any{map[string]any{"Id": "remote", "Path": path, "IsRemote": isRemote, "IsInfiniteStream": infinite}}}}})
		case "/Items/42/PlaybackInfo", "/emby/Items/42/PlaybackInfo":
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			receivedQuery = r.URL.RawQuery
			receivedEncoding = r.Header.Get("Accept-Encoding")
			w.Header().Set("ETag", "original")
			io.WriteString(w, playback())
		case "/Items/other/PlaybackInfo":
			w.Header().Set("ETag", "unchanged")
			io.WriteString(w, untouched)
		case "/Items/bad/PlaybackInfo":
			w.Header().Set("ETag", "unchanged")
			io.WriteString(w, "{invalid")
		case "/System/Info":
			io.WriteString(w, `{ "HttpServerPortNumber":8096,"LocalAddresses":["http://upstream:8096"] }`)
		case "/web/basehtmlplayer.js":
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, `mediaSource.IsRemote&&"DirectPlay"===playMethod?null:"anonymous"`)
		case "/redirect":
			w.Header().Set("Location", "/original-location")
			w.WriteHeader(302)
		case "/echo":
			receivedQuery = r.URL.RawQuery
			receivedEncoding = r.Header.Get("Accept-Encoding")
			b, _ := io.ReadAll(r.Body)
			w.WriteHeader(201)
			w.Write(b)
		default:
			if r.Header.Get("X-Emby-Token") == "blocked" {
				w.WriteHeader(500)
				return
			}
			if r.Header.Get("X-Emby-Token") != "client" && r.URL.Query().Get("api_key") != "client" && !strings.Contains(r.Header.Get("Authorization"), `Token="client"`) {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "video/test")
			w.Header().Set("Content-Range", "bytes 0-7/100")
			w.WriteHeader(206)
			if r.Method != "HEAD" {
				io.WriteString(w, "upstream")
			}
		}
	}))
	defer emby.Close()
	g := newGateway(config{emby.URL, "client", openlist.URL, "token", 18096, time.Minute})
	call := func(method, path, ua, body string, headers http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header = headers.Clone()
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		r.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	t.Run("selective playback", func(t *testing.T) {
		for _, prefix := range []string{"", "/emby"} {
			w := call("POST", prefix+"/Items/42/PlaybackInfo?api_key=client&custom=a%20b", "ua", "request", nil)
			if w.Code != 200 || receivedBody != "request" || receivedQuery != "api_key=client&custom=a%20b" || receivedEncoding != "identity" || w.Header().Get("X-Upstream") != "yes" || w.Header().Get("Set-Cookie") == "" || w.Header().Get("ETag") != "" {
				t.Fatal("request or response headers changed incorrectly")
			}
			var b map[string]json.RawMessage
			if json.Unmarshal(w.Body.Bytes(), &b) != nil || string(b["Extra"]) != "9007199254740993" {
				t.Fatal("number precision")
			}
			var ss []map[string]json.RawMessage
			json.Unmarshal(b["MediaSources"], &ss)
			if string(ss[0]["SupportsTranscoding"]) != "false" || ss[0]["TranscodingUrl"] != nil || string(ss[0]["Unknown"]) != "9007199254740993" {
				t.Fatal(ss[0])
			}
			var stream string
			json.Unmarshal(ss[0]["DirectStreamUrl"], &stream)
			u, _ := url.Parse(stream)
			if u.Path != "/videos/42/stream" || u.Query().Get("PlaySessionId") != "session" || u.Query().Get("api_key") != "client" || u.Query().Get("Static") != "true" {
				t.Fatal(stream)
			}
			// Emby Web's getUrl adds /emby to its server address before the source URL.
			if response := call("GET", "/emby"+stream, "webclient", "", nil); response.Code != http.StatusFound {
				t.Fatalf("client-resolved stream must redirect: status=%d", response.Code)
			}
			for _, s := range ss[1:] {
				if string(s["SupportsTranscoding"]) != "true" || s["DirectStreamUrl"] != nil {
					t.Fatal("non-target mutated")
				}
			}
		}
		w := call("GET", "/Items/42/PlaybackInfo", "ua", "", http.Header{"X-Emby-Token": {"client"}})
		if !strings.Contains(w.Body.String(), "api_key=client") {
			t.Fatal("caller header token lost")
		}
	})
	t.Run("unchanged responses", func(t *testing.T) {
		for path, want := range map[string]string{"/Items/other/PlaybackInfo": untouched, "/Items/bad/PlaybackInfo": "{invalid", "/System/Info": `{ "HttpServerPortNumber":8096,"LocalAddresses":["http://upstream:8096"] }`} {
			w := call("GET", path, "ua", "", nil)
			if w.Body.String() != want || w.Header().Get("X-Upstream") != "yes" {
				t.Fatal(path, w.Body.String())
			}
			if strings.Contains(path, "PlaybackInfo") && w.Header().Get("ETag") != "unchanged" {
				t.Fatal("validator lost")
			}
		}
	})
	t.Run("other methods preserve encoding", func(t *testing.T) {
		call("HEAD", "/Items/42/PlaybackInfo", "ua", "", http.Header{"Accept-Encoding": {"gzip"}})
		if receivedEncoding != "gzip" {
			t.Fatal("HEAD request encoding changed")
		}
	})
	t.Run("scoped player patch", func(t *testing.T) {
		w := call("GET", "/web/basehtmlplayer.js", "ua", "", nil)
		if !strings.Contains(w.Body.String(), "XOpenListDirectStream") || !strings.Contains(w.Body.String(), `"DirectPlay"===playMethod`) || !strings.Contains(w.Body.String(), `?null:"anonymous"`) {
			t.Fatal(w.Body.String())
		}
	})
	t.Run("redirect cache auth UA expiry", func(t *testing.T) {
		before := resolves.Load()
		for _, ua := range []string{"one", "one", "two"} {
			w := call("GET", "/videos/42/stream.mkv?Static=true&MediaSourceId=remote&api_key=client", ua, "", nil)
			if w.Code != 302 || w.Header().Get("Location") != "https://cdn.example/movie?ua="+ua || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w)
			}
		}
		if resolves.Load() != before+2 {
			t.Fatal("cache or UA")
		}
		for _, headers := range []http.Header{{"X-Emby-Token": {"client"}}, {"Authorization": {`Emby Client="Test", Token="client"`}}} {
			if w := call("GET", "/videos/42/stream?Static=true", "one", "", headers); w.Code != 302 {
				t.Fatal("header auth")
			}
		}
		for _, p := range []string{"/videos/42/stream?Static=true", "/videos/42/stream?Static=true&api_key=invalid"} {
			if w := call("GET", p, "one", "", nil); w.Code != 401 || w.Header().Get("Location") != "" {
				t.Fatal("unauthorized cache access")
			}
		}
		g.mu.Lock()
		g.cache[cacheKey{"/115/a b.mkv", "one"}] = entry{"https://expired", time.Now().Add(-time.Second)}
		g.cache[cacheKey{"unused", "ua"}] = entry{"https://expired", time.Now().Add(-time.Second)}
		g.mu.Unlock()
		call("GET", "/videos/42/stream?Static=true&api_key=client", "one", "", nil)
		g.mu.Lock()
		_, ok := g.cache[cacheKey{"unused", "ua"}]
		g.mu.Unlock()
		if ok || resolves.Load() != before+3 {
			t.Fatal("expiry cleanup")
		}
	})
	t.Run("passthrough and failure fallback", func(t *testing.T) {
		before := resolves.Load()
		for _, item := range []string{"local", "other", "live", "relative", "query-error", "malformed"} {
			w := call("GET", "/videos/"+item+"/original?api_key=client", "ua", "", nil)
			if w.Code != 206 || w.Body.String() != "upstream" {
				t.Fatal(item, w.Code)
			}
		}
		for _, path := range []string{"/videos/42/stream?Static=false", "/videos/42/stream?VideoCodec=h264", "/videos/42/streaming", "/videos/42/stream/Subtitles", "/videos/42/master.m3u8", "/Items/42/Download", "/Audio/42/stream"} {
			w := call("GET", path+func() string {
				if strings.Contains(path, "?") {
					return "&"
				}
				return "?"
			}()+"api_key=client", "ua", "", nil)
			if w.Code != 206 {
				t.Fatal(path, w.Code)
			}
		}
		w := call("HEAD", "/videos/42/stream?api_key=client", "ua", "", nil)
		if w.Code != 206 || resolves.Load() != before {
			t.Fatal("HEAD or fallback resolved OpenList")
		}
		w = call("GET", "/videos/42/stream?Static=true&MediaSourceId=missing&api_key=client", "ua", "", nil)
		if w.Code != 206 {
			t.Fatal("source fallback")
		}
		w = call("GET", "/videos/42/stream?Static=true&api_key=client", "fail", "", nil)
		if w.Code != 206 || w.Body.String() != "upstream" {
			t.Fatal("OpenList failure fallback")
		}
		beforeQueries := queries.Load()
		call("POST", "/videos/42/stream?api_key=client", "ua", "request", nil)
		if queries.Load() != beforeQueries {
			t.Fatal("POST intercepted")
		}
	})
	t.Run("HTTP preservation", func(t *testing.T) {
		w := call("GET", "/redirect", "ua", "", nil)
		if w.Code != 302 || w.Header().Get("Location") != "/original-location" {
			t.Fatal("redirect followed")
		}
		w = call("POST", "/echo?x=a%20b&x=c%2Fd", "ua", "body", http.Header{"Accept-Encoding": {"gzip"}})
		if w.Code != 201 || w.Body.String() != "body" || receivedQuery != "x=a%20b&x=c%2Fd" || receivedEncoding != "gzip" {
			t.Fatal("HTTP preservation")
		}
		w = call("GET", "/videos/local/original?api_key=client", "ua", "", http.Header{"Range": {"bytes=0-7"}})
		if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 0-7/100" {
			t.Fatal("Range")
		}
	})
	t.Run("ordinary user and body identity", func(t *testing.T) {
		w := call("POST", "/Items/42/PlaybackInfo", "ua", `{"UserId":"user"}`, http.Header{"X-Emby-Token": {"member"}})
		var body struct {
			MediaSources []struct{ DirectStreamUrl string }
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		stream := body.MediaSources[0].DirectStreamUrl
		u, _ := url.Parse(stream)
		if u.Query().Get("UserId") != "user" || u.Query().Get("api_key") != "member" {
			t.Fatal("body identity or credentials lost")
		}
		if w = call("GET", stream, "one", "", http.Header{"X-Emby-Token": {"member"}}); w.Code != 302 {
			t.Fatal("ordinary user", w.Code)
		}
		if w = call("GET", "/videos/42/stream?Static=true&DeviceId=device", "one", "", http.Header{"X-Emby-Token": {"member"}}); w.Code != 302 {
			t.Fatal("device identity", w.Code)
		}
	})
	t.Run("playback policy", func(t *testing.T) {
		before := resolves.Load()
		w := call("GET", "/videos/42/stream?Static=true&UserId=user", "one", "", http.Header{"X-Emby-Token": {"blocked"}})
		if w.Code == 302 || resolves.Load() != before {
			t.Fatal("disabled playback bypassed")
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := call("GET", "/videos/42/stream?Static=true&api_key=client", "one", "", nil)
				if w.Code != 302 {
					t.Error(w.Code)
				}
			}()
		}
		wg.Wait()
	})
}

func TestWebSocketAndStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "upgrade required", 400)
			return
		}
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		rw.Flush()
		frame := make([]byte, 4)
		if _, e = io.ReadFull(rw, frame); e == nil {
			rw.Write(frame)
			rw.Flush()
		}
	}))
	defer backend.Close()
	g := newGateway(config{backend.URL, "client", backend.URL, "token", 18096, time.Minute})
	front := httptest.NewServer(g)
	defer front.Close()
	u, _ := url.Parse(front.URL)
	conn, e := net.DialTimeout("tcp", u.Host, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "GET /embywebsocket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", u.Host)
	reader := bufio.NewReader(conn)
	req, _ := http.NewRequest("GET", front.URL, nil)
	res, e := http.ReadResponse(reader, req)
	if e != nil || res.StatusCode != 101 {
		t.Fatal("websocket handshake", e)
	}
	frame := []byte{0x81, 2, 'o', 'k'}
	conn.Write(frame)
	got := make([]byte, 4)
	if _, e = io.ReadFull(reader, got); e != nil || !bytes.Equal(got, frame) {
		t.Fatal("bidirectional upgrade", e)
	}
	conn.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	res, e = client.Get(front.URL + "/events")
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	line, e := bufio.NewReader(res.Body).ReadString('\n')
	if e != nil || line != "data: first\n" {
		t.Fatal("stream flush", e)
	}
}

func TestOpenlistPath(t *testing.T) {
	g := newGateway(config{"http://emby", "client", "https://openlist.example:5244", "token", 18096, time.Minute})
	for input, want := range map[string]string{"https://openlist.example:5244/d/115/a%20b": "/115/a b", "https://other.example:5244/d/a": "", "http://openlist.example:5244/d/a": "", "https://openlist.example:5244/d/": "", "https://openlist.example:5244/d/a%ZZ": "", "/d/115/a": "", "https://user@openlist.example:5244/d/a": ""} {
		if got := g.openlistPath(input); got != want {
			t.Errorf("%q: %q", input, got)
		}
	}
}
func TestConfig(t *testing.T) {
	old, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chdir(t.TempDir()); e != nil {
		t.Fatal(e)
	}
	defer os.Chdir(old)
	base := "EMBY_HOST='http://localhost:8096'\nEMBY_API_KEY=key\nOPENLIST_ADDR=http://localhost:5244\nOPENLIST_TOKEN=token\n"
	for _, extra := range []string{"", "PORT=0\n", "CACHE_TTL=-1\n", "PORT=abc\n", "PORT=65536\n", "OPENLIST_ADDR=ftp://host\n", "EMBY_HOST=not-a-url\n"} {
		os.WriteFile(".env", []byte(base+extra), 0600)
		c, e := loadConfig()
		if extra == "" {
			if e != nil || c.port != 18096 || c.ttl != 180*time.Second {
				t.Fatal(c, e)
			}
		} else if e == nil {
			t.Fatal("invalid config accepted", extra)
		}
	}
}

func TestPlaybackCredentials(t *testing.T) {
	g := newGateway(config{"http://emby", "configured-admin-key", "http://openlist", "token", 18096, time.Minute})
	for _, tc := range []struct{ name, query, header, value, upstream, want string }{
		{"query", "?api_key=query-token", "X-Emby-Token", "header-token", "upstream-token", "query-token"},
		{"header", "", "X-Emby-Token", "header-token", "upstream-token", "header-token"},
		{"authorization", "", "Authorization", `Emby Token="authorization-token"`, "upstream-token", "authorization-token"},
		{"emby authorization", "", "X-Emby-Authorization", `Emby Token="emby-token"`, "upstream-token", "emby-token"},
		{"upstream", "", "", "", "upstream-token", "upstream-token"},
		{"no credentials", "", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://gateway/Items/42/PlaybackInfo"+tc.query, nil)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			data, _ := json.Marshal(map[string]any{"MediaSources": []any{map[string]any{"Id": "source", "Path": "http://openlist/d/movie.mp4", "IsRemote": true, "DirectStreamUrl": "/videos/42/stream?api_key=" + tc.upstream}}})
			result := g.playbackInfo(r, data, "42")
			var body struct {
				MediaSources []struct{ DirectStreamUrl string }
			}
			if err := json.Unmarshal(result, &body); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(body.MediaSources[0].DirectStreamUrl)
			if u.Query().Get("api_key") != tc.want {
				t.Fatal("caller credential not preserved")
			}
			if strings.Contains(string(result), g.cfg.key) {
				t.Fatal("configured key leaked")
			}
		})
	}
}

func TestEmbyBasePath(t *testing.T) {
	for _, prefix := range []string{"", "/emby"} {
		t.Run(prefix, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/base"+prefix+"/Items/42/PlaybackInfo" {
					t.Error(r.URL.Path)
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("encoding")
				}
				io.WriteString(w, `{"MediaSources":[{"Id":"source","Path":"http://openlist/d/movie.mp4","IsRemote":true}]}`)
			}))
			defer backend.Close()
			g := newGateway(config{backend.URL + "/base", "admin-key", "http://openlist", "token", 18096, time.Minute})
			r := httptest.NewRequest("POST", "http://gateway"+prefix+"/Items/42/PlaybackInfo", strings.NewReader(`{}`))
			r.Header.Set("X-Emby-Token", "caller-token")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			var body struct {
				MediaSources []struct {
					DirectStreamUrl       string
					XOpenListDirectStream bool
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			s := body.MediaSources[0]
			u, _ := url.Parse(s.DirectStreamUrl)
			if !s.XOpenListDirectStream || u.Path != "/videos/42/stream" || u.Query().Get("api_key") != "caller-token" {
				t.Fatal("upstream prefix leaked into client route or rewrite missed")
			}
		})
	}
}

func TestRunServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	defer listener.Close()
	if err := runServer(context.Background(), &http.Server{Addr: address}); err == nil {
		t.Fatal("occupied address must report a listen error")
	}
	listener.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	listening := make(chan string, 1)
	server := &http.Server{Addr: "127.0.0.1:0", BaseContext: func(l net.Listener) context.Context {
		listening <- l.Addr().String()
		return context.Background()
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, "completed")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, server) }()
	select {
	case address = <-listening:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	result := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		deadline := time.Now().Add(5 * time.Second)
		for {
			res, err := client.Get("http://" + address)
			if err != nil {
				if time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				result <- err
				return
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err == nil && string(body) != "completed" {
				err = fmt.Errorf("unexpected response: %q", body)
			}
			result <- err
			return
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("shutdown abandoned active request: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestLookupDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, tc := range []struct {
		name, body, reason string
		status             int
	}{
		{"status", "secret-response", "reason=status status=502", 502},
		{"json", "secret-invalid-json", "reason=json", 200},
		{"api", `{"code":403,"message":"secret-response"}`, "reason=api code=403", 200},
		{"url", `{"code":200,"data":{"raw_url":"https://secret:password@example.com/?token=secret"}}`, "reason=url", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			g := newGateway(config{emby: upstream.URL, openlist: upstream.URL, token: "secret", ttl: time.Minute})
			request := httptest.NewRequest("GET", "/?api_key=secret", nil)
			output.Reset()
			if got := g.resolve(request, "/secret-media"); got != "" {
				t.Fatal("unexpected resolution", got)
			}
			if !strings.Contains(output.String(), tc.reason) || strings.Contains(output.String(), "secret") {
				t.Fatalf("unsafe or missing diagnostic: %s", output.String())
			}
			if tc.name == "status" || tc.name == "json" {
				output.Reset()
				var body any
				if g.embyJSON(request, "/Items", url.Values{}, &body) {
					t.Fatal("unexpected lookup success")
				}
				if !strings.Contains(output.String(), tc.reason) || strings.Contains(output.String(), "secret") {
					t.Fatalf("unsafe or missing diagnostic: %s", output.String())
				}
			}
		})
	}
	t.Run("transport", func(t *testing.T) {
		upstream := httptest.NewServer(http.NotFoundHandler())
		upstream.Close()
		g := newGateway(config{emby: upstream.URL, openlist: upstream.URL, token: "secret"})
		request := httptest.NewRequest("GET", "/?api_key=secret", nil)
		output.Reset()
		var body any
		if g.embyJSON(request, "/Items", url.Values{}, &body) || g.resolve(request, "/secret-media") != "" {
			t.Fatal("unavailable upstream must fail")
		}
		if strings.Count(output.String(), "reason=transport") != 2 || strings.Contains(output.String(), "secret") {
			t.Fatalf("unsafe or missing diagnostics: %s", output.String())
		}
	})

}
