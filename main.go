package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	emby, key, openlist, token string
	port                       int
	ttl                        time.Duration
}
type cacheKey struct{ path, ua string }
type entry struct {
	url     string
	expires time.Time
}
type gateway struct {
	cfg            config
	emby, openlist *url.URL
	proxy          *httputil.ReverseProxy
	api            *http.Client
	mu             sync.Mutex
	cache          map[cacheKey]entry
}

type playbackUser struct{}
type originalRequest struct{}

var authParameter = regexp.MustCompile(`(?i)(?:^|[\s,])(UserId|DeviceId|Token)=("[^"]*"|[^,\s]+)`)

var playbackRoute = regexp.MustCompile(`(?i)^(?:/emby)?/items/([^/]+)/playbackinfo$`)
var videoRoute = regexp.MustCompile(`(?i)^(?:/emby)?/videos/([^/]+)/(?:stream(?:\.[^/]+)?|original(?:\.[^/]+)?)$`)
var playerExpression = regexp.MustCompile(`mediaSource\.IsRemote\s*&&\s*"DirectPlay"\s*===\s*playMethod\s*\?\s*null\s*:\s*"anonymous"`)

func loadConfig() (config, error) {
	data, err := os.ReadFile(".env")
	if err != nil {
		return config{}, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if ok {
			env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), "\"'")
		}
	}
	for _, k := range []string{"EMBY_HOST", "EMBY_API_KEY", "OPENLIST_ADDR", "OPENLIST_TOKEN"} {
		if env[k] == "" {
			return config{}, fmt.Errorf("missing env: %s", k)
		}
	}
	positive := func(k string, fallback int) (int, error) {
		v, ok := env[k]
		if !ok {
			return fallback, nil
		}
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 {
			return 0, fmt.Errorf("invalid env: %s must be a positive integer", k)
		}
		return n, nil
	}
	port, err := positive("PORT", 18096)
	if err != nil {
		return config{}, err
	}
	ttl, err := positive("CACHE_TTL", 180)
	if err != nil {
		return config{}, err
	}
	if port > 65535 || int64(ttl) > int64((1<<63-1)/time.Second) {
		return config{}, fmt.Errorf("PORT or CACHE_TTL out of range")
	}
	for _, key := range []string{"EMBY_HOST", "OPENLIST_ADDR"} {
		u, err := url.Parse(env[key])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return config{}, fmt.Errorf("invalid env: %s must be an HTTP(S) base URL", key)
		}
		env[key] = strings.TrimRight(env[key], "/")
	}
	return config{env["EMBY_HOST"], env["EMBY_API_KEY"], env["OPENLIST_ADDR"], env["OPENLIST_TOKEN"], port, time.Duration(ttl) * time.Second}, nil
}
func newGateway(c config) *gateway {
	emby, _ := url.Parse(c.emby)
	openlist, _ := url.Parse(c.openlist)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	g := &gateway{cfg: c, emby: emby, openlist: openlist,
		api: &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cache: make(map[cacheKey]entry)}
	g.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(emby)
			p.Out = p.Out.WithContext(context.WithValue(p.Out.Context(), originalRequest{}, p.In))
			p.Out.Host = p.In.Host
			p.Out.URL.RawQuery = p.In.URL.RawQuery
			p.SetXForwarded()
			if (playbackRoute.MatchString(p.In.URL.Path) && (p.In.Method == "GET" || p.In.Method == "POST")) || (p.In.Method == "GET" && strings.HasSuffix(p.In.URL.Path, "/basehtmlplayer.js")) {
				p.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: g.modifyResponse,
	}
	return g
}

func queryValue(q url.Values, key string) string {
	if value := q.Get(key); value != "" {
		return value
	}
	for name, values := range q {
		if strings.EqualFold(name, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func setQuery(q url.Values, key, value string) {
	for name := range q {
		if strings.EqualFold(name, key) {
			delete(q, name)
		}
	}
	q.Set(key, value)
}

func requestValue(r *http.Request, key string) string {
	q := r.URL.Query()
	if value := queryValue(q, key); value != "" {
		return value
	}
	if key == "Token" {
		for _, value := range []string{r.Header.Get("X-Emby-Token"), queryValue(q, "api_key"), queryValue(q, "X-Emby-Token")} {
			if value != "" {
				return value
			}
		}
	}
	if key == "DeviceId" {
		for _, value := range []string{queryValue(q, "X-Emby-Device-Id"), r.Header.Get("X-Emby-Device-Id")} {
			if value != "" {
				return value
			}
		}
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		for _, pair := range authParameter.FindAllStringSubmatch(r.Header.Get(header), -1) {
			if strings.EqualFold(pair[1], key) {
				return strings.Trim(pair[2], `"`)
			}
		}
	}
	return ""
}

func (g *gateway) openlistPath(path string) string {
	u, err := url.Parse(path)
	prefix := strings.TrimRight(g.openlist.Path, "/") + "/d/"
	if err != nil || u.User != nil || !strings.EqualFold(u.Scheme, g.openlist.Scheme) || !strings.EqualFold(u.Host, g.openlist.Host) || !strings.HasPrefix(u.Path, prefix) || len(u.Path) == len(prefix) {
		return ""
	}
	return "/" + strings.TrimPrefix(u.Path, prefix)
}

func (g *gateway) modifyResponse(res *http.Response) error {
	r, _ := res.Request.Context().Value(originalRequest{}).(*http.Request)
	if r == nil {
		r = res.Request
	}
	playback := playbackRoute.FindStringSubmatch(r.URL.Path)
	player := strings.HasSuffix(r.URL.Path, "/basehtmlplayer.js") && r.Method == "GET"
	if res.StatusCode != http.StatusOK || (r.Method != "GET" && r.Method != "POST") || (len(playback) == 0 && !player) || (res.Header.Get("Content-Encoding") != "" && res.Header.Get("Content-Encoding") != "identity") {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(data), res.Body), res.Body}
		return nil
	}
	res.Body.Close()
	modified := data
	if player {
		// Only sources rewritten by this gateway receive the CORS exception.
		modified = playerExpression.ReplaceAllFunc(data, func(original []byte) []byte {
			return []byte("mediaSource.XOpenListDirectStream?null:(" + string(original) + ")")
		})
	} else {
		modified = g.playbackInfo(r, data, playback[1])
	}
	res.Body = io.NopCloser(bytes.NewReader(modified))
	if !bytes.Equal(data, modified) {
		res.ContentLength = int64(len(modified))
		res.Header.Set("Content-Length", strconv.Itoa(len(modified)))
		for _, key := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest"} {
			res.Header.Del(key)
		}
	}
	return nil
}

func (g *gateway) playbackInfo(r *http.Request, data []byte, item string) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil {
		return data
	}
	var sources []json.RawMessage
	if json.Unmarshal(body["MediaSources"], &sources) != nil {
		return data
	}
	changed := false
	for i, raw := range sources {
		var source map[string]json.RawMessage
		var media struct {
			ID                         string `json:"Id"`
			Path                       string
			DirectStreamURL            string `json:"DirectStreamUrl"`
			IsRemote, IsInfiniteStream bool
			MediaStreams               []struct{ Type string }
		}
		if json.Unmarshal(raw, &media) != nil || media.ID == "" || !media.IsRemote || media.IsInfiniteStream || g.openlistPath(media.Path) == "" || json.Unmarshal(raw, &source) != nil {
			continue
		}
		if len(media.MediaStreams) > 0 {
			video := false
			for _, stream := range media.MediaStreams {
				video = video || strings.EqualFold(stream.Type, "Video")
			}
			if !video {
				continue
			}
		}
		q := r.URL.Query()
		if queryValue(q, "api_key") == "" && queryValue(q, "X-Emby-Token") == "" {
			token := requestValue(r, "Token")
			if token == "" {
				if upstream, err := url.Parse(media.DirectStreamURL); err == nil {
					token = queryValue(upstream.Query(), "api_key")
					if token == "" {
						token = queryValue(upstream.Query(), "X-Emby-Token")
					}
				}
			}
			if token != "" {
				q.Set("api_key", token)
			}
		}
		setQuery(q, "MediaSourceId", media.ID)
		setQuery(q, "Static", "true")
		user, _ := r.Context().Value(playbackUser{}).(string)
		if user == "" {
			user = requestValue(r, "UserId")
		}
		if user != "" && queryValue(q, "UserId") == "" {
			setQuery(q, "UserId", user)
		}
		if device := requestValue(r, "DeviceId"); device != "" && queryValue(q, "DeviceId") == "" && queryValue(q, "X-Emby-Device-Id") == "" {
			setQuery(q, "DeviceId", device)
		}
		var session string
		json.Unmarshal(body["PlaySessionId"], &session)
		if session != "" {
			setQuery(q, "PlaySessionId", session)
		}
		// Emby clients add their API base path when resolving DirectStreamUrl.
		stream := "/videos/" + url.PathEscape(item) + "/stream?" + q.Encode()
		source["DirectStreamUrl"], _ = json.Marshal(stream)
		source["SupportsDirectPlay"] = json.RawMessage("true")
		source["SupportsDirectStream"] = json.RawMessage("true")
		source["SupportsTranscoding"] = json.RawMessage("false")
		source["XOpenListDirectStream"] = json.RawMessage("true")
		for _, key := range []string{"TranscodingUrl", "TranscodingSubProtocol", "TranscodingContainer"} {
			delete(source, key)
		}
		sources[i], _ = json.Marshal(source)
		changed = true
	}
	if !changed {
		return data
	}
	body["MediaSources"], _ = json.Marshal(sources)
	modified, err := json.Marshal(body)
	if err != nil {
		return data
	}
	log.Printf("[playback] openlist_direct item=%s", item)
	return modified
}

func (g *gateway) embyJSON(r *http.Request, path string, q url.Values, body any) bool {
	u := *g.emby
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	for _, key := range []string{"api_key", "X-Emby-Token"} {
		if value := queryValue(r.URL.Query(), key); value != "" {
			q.Set(key, value)
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), "GET", u.String(), nil)
	if err != nil {
		log.Printf("[video] emby_lookup_failed reason=request")
		return false
	}
	req.Header = r.Header.Clone()
	req.Header.Del("Range")
	req.Header.Del("If-Range")
	req.Host = r.Host
	req.Header.Del("Forwarded")
	req.Header.Del("X-Forwarded-For")
	(&httputil.ProxyRequest{In: r, Out: req}).SetXForwarded()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	res, err := g.api.Do(req)
	if err != nil {
		log.Printf("[video] emby_lookup_failed reason=transport")
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		log.Printf("[video] emby_lookup_failed reason=status status=%d", res.StatusCode)
		return false
	}
	if json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(body) != nil {
		log.Printf("[video] emby_lookup_failed reason=json")
		return false
	}
	return true
}

func (g *gateway) mediaPath(r *http.Request, item string) string {
	q := url.Values{"Ids": {item}, "Fields": {"Path,MediaSources"}}
	if requestValue(r, "Token") != g.cfg.key {
		userID := requestValue(r, "UserId")
		if userID == "" {
			device := requestValue(r, "DeviceId")
			var sessions []struct {
				DeviceID string `json:"DeviceId"`
				UserID   string `json:"UserId"`
			}
			if device != "" && g.embyJSON(r, "/Sessions", url.Values{"DeviceId": {device}}, &sessions) {
				for _, session := range sessions {
					if session.DeviceID == device {
						userID = session.UserID
						break
					}
				}
			}
		}
		var user struct {
			ID     string `json:"Id"`
			Policy struct{ EnableMediaPlayback bool }
		}
		// Seeing an item does not grant playback; Emby must authorize the caller's user policy.
		if userID == "" || !g.embyJSON(r, "/Users/"+url.PathEscape(userID), url.Values{}, &user) || !user.Policy.EnableMediaPlayback {
			return ""
		}
		q.Set("UserId", user.ID)
	}
	var body struct {
		Items []struct {
			Path         string
			MediaType    string
			MediaSources []struct {
				ID                         string `json:"Id"`
				Path                       string
				IsRemote, IsInfiniteStream bool
			}
		}
	}
	if !g.embyJSON(r, "/Items", q, &body) || len(body.Items) != 1 {
		return ""
	}
	mediaItem := body.Items[0]
	if !strings.HasSuffix(strings.ToLower(mediaItem.Path), ".strm") || strings.EqualFold(mediaItem.MediaType, "Audio") {
		return ""
	}
	sourceID := queryValue(r.URL.Query(), "MediaSourceId")
	for _, source := range mediaItem.MediaSources {
		if sourceID != "" && source.ID != sourceID {
			continue
		}
		if source.IsRemote && !source.IsInfiniteStream {
			return g.openlistPath(source.Path)
		}
		return ""
	}
	return ""
}

func (g *gateway) resolve(r *http.Request, path string) string {
	key := cacheKey{path, r.UserAgent()}
	now := time.Now()
	g.mu.Lock()
	for k, v := range g.cache {
		if !now.Before(v.expires) {
			delete(g.cache, k)
		}
	}
	cached, ok := g.cache[key]
	g.mu.Unlock()
	if ok {
		return cached.url
	}
	data, _ := json.Marshal(map[string]string{"path": path})
	req, err := http.NewRequestWithContext(r.Context(), "POST", g.cfg.openlist+"/api/fs/get", bytes.NewReader(data))
	if err != nil {
		log.Printf("[openlist] resolve_failed reason=request")
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", g.cfg.token)
	req.Header.Set("User-Agent", r.UserAgent())
	res, err := g.api.Do(req)
	if err != nil {
		log.Printf("[openlist] resolve_failed reason=transport")
		return ""
	}
	defer res.Body.Close()
	var body struct {
		Code int
		Data struct {
			RawURL string `json:"raw_url"`
		}
	}
	if res.StatusCode != http.StatusOK {
		log.Printf("[openlist] resolve_failed reason=status status=%d", res.StatusCode)
		return ""
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body) != nil {
		log.Printf("[openlist] resolve_failed reason=json")
		return ""
	}
	if body.Code != 200 {
		log.Printf("[openlist] resolve_failed reason=api code=%d", body.Code)
		return ""
	}
	u, err := url.Parse(body.Data.RawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		log.Printf("[openlist] resolve_failed reason=url")
		return ""
	}
	g.mu.Lock()
	// Bound retained URLs even when a client requests many distinct sources or user agents.
	if len(g.cache) < 1024 {
		g.cache[key] = entry{u.String(), time.Now().Add(g.cfg.ttl)}
	}
	g.mu.Unlock()
	return u.String()
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && playbackRoute.MatchString(r.URL.Path) && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
		var input struct {
			UserID string `json:"UserId"`
		}
		if err == nil && len(body) <= 8<<20 && json.Unmarshal(body, &input) == nil && input.UserID != "" {
			r = r.WithContext(context.WithValue(r.Context(), playbackUser{}, input.UserID))
		}
	}
	match := videoRoute.FindStringSubmatch(r.URL.Path)
	direct := strings.EqualFold(queryValue(r.URL.Query(), "Static"), "true") || strings.HasPrefix(strings.ToLower(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]), "original")
	if r.Method == "GET" && len(match) > 0 && direct && r.Header.Get("Upgrade") == "" {
		// Authorize and classify through Emby before looking up a cached CDN URL.
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		probe := r.Clone(ctx)
		path := g.mediaPath(probe, match[1])
		raw := ""
		if path != "" {
			raw = g.resolve(probe, path)
		}
		cancel()
		if raw != "" {
			w.Header().Set("Location", raw)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusFound)
			log.Printf("[video] redirect item=%s", match[1])
			return
		}
	}
	g.proxy.ServeHTTP(w, r)
}

func runServer(ctx context.Context, s *http.Server) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("[server] shutdown_failed reason=timeout")
			s.Close()
		}
	}()
	err := s.ListenAndServe()
	cancel()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	c, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("[server] start port=%d", c.port)
	s := &http.Server{Addr: fmt.Sprintf(":%d", c.port), Handler: newGateway(c), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, s); err != nil {
		log.Fatal(err)
	}
}
