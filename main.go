// yt-transcript : récupère les transcripts (sous-titres) de vidéos YouTube.
//
// Trois types d'entrée sont acceptés, indifféremment :
//   - une URL de vidéo  (https://youtu.be/ID, .../watch?v=ID, .../shorts/ID)
//   - une liste d'URLs  (séparées par des espaces / virgules / retours ligne)
//   - une chaîne        (.../@handle, .../channel/UC..., .../c/nom, .../user/nom)
//     ou une playlist   (.../playlist?list=...)
//
// Deux modes d'exécution :
//   - serveur HTTP (par défaut, pour CapRover)  : écoute sur $PORT (def. 8080)
//   - CLI : `yt-transcript <url|chaine> [autres urls...]`
//
// Aucune dépendance externe : uniquement la bibliothèque standard.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ----------------------------------------------------------------------------
// Modèles de données
// ----------------------------------------------------------------------------

// Segment : un fragment de transcript avec son horodatage (en secondes).
type Segment struct {
	Start float64 `json:"start"`
	Dur   float64 `json:"dur"`
	Text  string  `json:"text"`
}

// VideoTranscript : résultat pour une vidéo.
type VideoTranscript struct {
	VideoID  string    `json:"videoId"`
	URL      string    `json:"url"`
	Title    string    `json:"title,omitempty"`
	Author   string    `json:"author,omitempty"`
	Language string    `json:"language,omitempty"`
	Text     string    `json:"text,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// ----------------------------------------------------------------------------
// Client HTTP partagé
// ----------------------------------------------------------------------------

const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	// Valeurs de repli si on ne parvient pas à les extraire de la page.
	defaultClientVersion = "2.20240814.00.00"
	// Clé publique InnerTube (présente dans toute page YouTube).
	defaultAPIKey = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"
	// Le client ANDROID renvoie des URLs de sous-titres directement
	// téléchargeables (sans « Proof of Origin Token »), contrairement au
	// client WEB dont les URLs portent le marqueur exp=xpe.
	androidClientName    = "ANDROID"
	androidClientVersion = "20.10.38"
	maxChannelPages      = 200 // garde-fou anti-boucle pour la pagination
	fetchWorkers         = 6   // transcripts récupérés en parallèle
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// httpGet effectue un GET avec des en-têtes proches d'un navigateur
// (et un cookie de consentement pour éviter le mur RGPD).
func httpGet(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	return doString(req)
}

// httpPostJSON effectue un POST JSON (utilisé pour l'API interne InnerTube).
func httpPostJSON(ctx context.Context, rawURL string, payload any) (string, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	return doString(req)
}

func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cookie", "CONSENT=YES+cb.20210328-17-p0.en+FX+000")
}

func doString(req *http.Request) (string, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20)) // 25 Mo max
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return string(body), nil
}

// ----------------------------------------------------------------------------
// Analyse / classification des entrées
// ----------------------------------------------------------------------------

var videoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// extractVideoID retourne l'ID d'une vidéo à partir d'une URL ou d'un ID brut.
func extractVideoID(s string) string {
	s = strings.TrimSpace(s)
	if videoIDRe.MatchString(s) {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	switch host {
	case "youtu.be":
		id := strings.Trim(u.Path, "/")
		if videoIDRe.MatchString(id) {
			return id
		}
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		if v := u.Query().Get("v"); videoIDRe.MatchString(v) {
			return v
		}
		// /shorts/ID, /embed/ID, /live/ID, /v/ID
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 {
			switch parts[0] {
			case "shorts", "embed", "live", "v":
				if videoIDRe.MatchString(parts[1]) {
					return parts[1]
				}
			}
		}
	}
	return ""
}

type inputKind int

const (
	kindUnknown inputKind = iota
	kindVideo
	kindChannel
	kindPlaylist
)

// classify détermine la nature d'une entrée et renvoie une URL normalisée.
func classify(s string) (inputKind, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return kindUnknown, ""
	}
	if id := extractVideoID(s); id != "" {
		return kindVideo, id
	}
	// Pas un schéma ? On suppose une URL youtube.com.
	if !strings.Contains(s, "://") {
		if strings.HasPrefix(s, "@") {
			return kindChannel, "https://www.youtube.com/" + s + "/videos"
		}
		s = "https://www.youtube.com/" + strings.TrimPrefix(s, "/")
	}
	u, err := url.Parse(s)
	if err != nil {
		return kindUnknown, s
	}
	if u.Query().Get("list") != "" && u.Query().Get("v") == "" {
		return kindPlaylist, s
	}
	path := strings.Trim(u.Path, "/")
	switch {
	case strings.HasPrefix(path, "@"),
		strings.HasPrefix(path, "channel/"),
		strings.HasPrefix(path, "c/"),
		strings.HasPrefix(path, "user/"):
		// On cible l'onglet « Vidéos ».
		if !strings.HasSuffix(path, "/videos") {
			u.Path = "/" + path + "/videos"
		}
		return kindChannel, u.String()
	}
	return kindUnknown, s
}

// ----------------------------------------------------------------------------
// Extraction de JSON embarqué dans les pages YouTube
// ----------------------------------------------------------------------------

// extractEmbeddedJSON lit l'objet JSON équilibré qui suit l'un des marqueurs.
func extractEmbeddedJSON(page string, markers ...string) (string, bool) {
	for _, m := range markers {
		i := strings.Index(page, m)
		if i < 0 {
			continue
		}
		i += len(m)
		for i < len(page) && page[i] != '{' {
			i++
		}
		if i >= len(page) {
			continue
		}
		start := i
		depth, inStr, esc := 0, false, false
		for ; i < len(page); i++ {
			c := page[i]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return page[start : i+1], true
				}
			}
		}
	}
	return "", false
}

var (
	apiKeyRe        = regexp.MustCompile(`"INNERTUBE_API_KEY":"([^"]+)"`)
	clientVersionRe = regexp.MustCompile(`"INNERTUBE_CONTEXT_CLIENT_VERSION":"([^"]+)"`)
)

func firstSubmatch(re *regexp.Regexp, s, fallback string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return fallback
}

// ----------------------------------------------------------------------------
// Listing des vidéos d'une chaîne / playlist (scraping + pagination InnerTube)
// ----------------------------------------------------------------------------

// scrapeVideoIDs récupère tous les IDs de vidéos d'une page chaîne/playlist,
// en suivant les jetons de continuation de l'API interne. Si limit > 0, la
// pagination s'arrête dès que ce nombre d'IDs est atteint.
func scrapeVideoIDs(ctx context.Context, pageURL string, limit int) ([]string, error) {
	page, err := httpGet(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	raw, ok := extractEmbeddedJSON(page,
		`var ytInitialData = `, `window["ytInitialData"] = `, `ytInitialData = `)
	if !ok {
		return nil, errors.New("initial data not found (private, empty page, or unexpected format)")
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("unreadable initial JSON: %w", err)
	}

	apiKey := firstSubmatch(apiKeyRe, page, defaultAPIKey)
	clientVer := firstSubmatch(clientVersionRe, page, defaultClientVersion)

	seen := map[string]bool{}
	var ids []string
	addIDs := func(node any) {
		for _, id := range collectVideoIDs(node) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	addIDs(data)

	token := firstContinuation(data)
	usedTokens := map[string]bool{}
	browseURL := "https://www.youtube.com/youtubei/v1/browse?key=" + url.QueryEscape(apiKey)

	for pages := 0; token != "" && !usedTokens[token] && pages < maxChannelPages; pages++ {
		if limit > 0 && len(ids) >= limit {
			break
		}
		usedTokens[token] = true
		payload := map[string]any{
			"context": map[string]any{
				"client": map[string]any{
					"clientName":    "WEB",
					"clientVersion": clientVer,
					"hl":            "en",
					"gl":            "US",
				},
			},
			"continuation": token,
		}
		body, err := httpPostJSON(ctx, browseURL, payload)
		if err != nil {
			break // on renvoie ce qu'on a déjà collecté
		}
		var resp any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			break
		}
		before := len(ids)
		addIDs(resp)
		token = firstContinuation(resp)
		if len(ids) == before && token == "" {
			break
		}
	}

	if len(ids) == 0 {
		return nil, errors.New("no videos found")
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// collectVideoIDs parcourt récursivement le JSON et collecte les videoId.
func collectVideoIDs(node any) []string {
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if id, ok := v["videoId"].(string); ok && videoIDRe.MatchString(id) {
				out = append(out, id)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(node)
	return out
}

// firstContinuation renvoie le premier jeton continuationCommand.token trouvé.
func firstContinuation(node any) string {
	var found string
	var walk func(any) bool
	walk = func(n any) bool {
		switch v := n.(type) {
		case map[string]any:
			if cc, ok := v["continuationCommand"].(map[string]any); ok {
				if tok, ok := cc["token"].(string); ok && tok != "" {
					found = tok
					return true
				}
			}
			for _, child := range v {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range v {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	walk(node)
	return found
}

// ----------------------------------------------------------------------------
// Récupération du transcript d'une vidéo
// ----------------------------------------------------------------------------

type captionTrack struct {
	BaseURL      string `json:"baseUrl"`
	LanguageCode string `json:"languageCode"`
	Kind         string `json:"kind"` // "asr" = sous-titres auto
}

type playerResponse struct {
	Captions struct {
		PlayerCaptionsTracklistRenderer struct {
			CaptionTracks []captionTrack `json:"captionTracks"`
		} `json:"playerCaptionsTracklistRenderer"`
	} `json:"captions"`
	VideoDetails struct {
		Title  string `json:"title"`
		Author string `json:"author"`
	} `json:"videoDetails"`
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
}

// getTranscript récupère le transcript d'une vidéo, langue préférée optionnelle.
func getTranscript(ctx context.Context, videoID, lang string) VideoTranscript {
	res := VideoTranscript{
		VideoID: videoID,
		URL:     "https://www.youtube.com/watch?v=" + videoID,
	}

	pr, err := fetchPlayerResponse(ctx, videoID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Title = pr.VideoDetails.Title
	res.Author = pr.VideoDetails.Author

	if pr.PlayabilityStatus.Status != "" && pr.PlayabilityStatus.Status != "OK" {
		res.Error = "video unavailable: " + strings.TrimSpace(
			pr.PlayabilityStatus.Status+" "+pr.PlayabilityStatus.Reason)
		return res
	}

	tracks := pr.Captions.PlayerCaptionsTracklistRenderer.CaptionTracks
	if len(tracks) == 0 {
		res.Error = "no captions available for this video"
		return res
	}
	track := pickTrack(tracks, lang)
	res.Language = track.LanguageCode

	segs, err := fetchCaptionSegments(ctx, track.BaseURL)
	if err != nil {
		res.Error = "fetching captions: " + err.Error()
		return res
	}
	res.Segments = segs
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, s.Text)
	}
	res.Text = strings.Join(parts, " ")
	return res
}

// fetchPlayerResponse interroge l'API InnerTube avec le client ANDROID, dont la
// réponse fournit des URLs de sous-titres directement exploitables.
func fetchPlayerResponse(ctx context.Context, videoID string) (*playerResponse, error) {
	payload := map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    androidClientName,
				"clientVersion": androidClientVersion,
				"hl":            "en",
				"gl":            "US",
			},
		},
		"videoId": videoID,
	}
	body, err := httpPostJSON(ctx,
		"https://www.youtube.com/youtubei/v1/player?key="+defaultAPIKey, payload)
	if err != nil {
		return nil, err
	}
	var pr playerResponse
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		return nil, fmt.Errorf("unreadable player response: %w", err)
	}
	return &pr, nil
}

// pickTrack choisit la meilleure piste : langue demandée d'abord (manuelle puis
// auto puis préfixe), sinon une piste manuelle, sinon la première.
func pickTrack(tracks []captionTrack, lang string) captionTrack {
	if lang != "" {
		for _, t := range tracks {
			if t.LanguageCode == lang && t.Kind != "asr" {
				return t
			}
		}
		for _, t := range tracks {
			if t.LanguageCode == lang {
				return t
			}
		}
		for _, t := range tracks {
			if strings.HasPrefix(t.LanguageCode, lang) {
				return t
			}
		}
	}
	for _, t := range tracks {
		if t.Kind != "asr" {
			return t
		}
	}
	return tracks[0]
}

type json3Response struct {
	Events []struct {
		TStartMs    float64 `json:"tStartMs"`
		DDurationMs float64 `json:"dDurationMs"`
		Segs        []struct {
			Utf8 string `json:"utf8"`
		} `json:"segs"`
	} `json:"events"`
}

// fetchCaptionSegments télécharge et analyse les sous-titres.
//
// On privilégie le format JSON3 (horodatages précis) ; en cas d'échec on lit le
// XML brut. Le paramètre &fmt=srv3 éventuellement présent dans l'URL est retiré.
func fetchCaptionSegments(ctx context.Context, baseURL string) ([]Segment, error) {
	if strings.Contains(baseURL, "&exp=xpe") {
		return nil, errors.New("protected captions (Proof of Origin Token required)")
	}
	baseURL = strings.ReplaceAll(baseURL, "&fmt=srv3", "")

	jsonURL := baseURL
	if strings.Contains(jsonURL, "?") {
		jsonURL += "&fmt=json3"
	} else {
		jsonURL += "?fmt=json3"
	}
	if body, err := httpGet(ctx, jsonURL); err == nil {
		var j3 json3Response
		if json.Unmarshal([]byte(body), &j3) == nil && len(j3.Events) > 0 {
			var segs []Segment
			for _, ev := range j3.Events {
				var b strings.Builder
				for _, s := range ev.Segs {
					b.WriteString(s.Utf8)
				}
				text := normalizeText(b.String())
				if text == "" {
					continue
				}
				segs = append(segs, Segment{
					Start: ev.TStartMs / 1000,
					Dur:   ev.DDurationMs / 1000,
					Text:  text,
				})
			}
			if len(segs) > 0 {
				return segs, nil
			}
		}
	}
	// Repli : format XML historique.
	body, err := httpGet(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	return parseXMLCaptions(body)
}

var xmlTextRe = regexp.MustCompile(`(?s)<text([^>]*)>(.*?)</text>`)

func parseXMLCaptions(body string) ([]Segment, error) {
	matches := xmlTextRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, errors.New("empty transcript")
	}
	attrRe := regexp.MustCompile(`(\w+)="([^"]*)"`)
	var segs []Segment
	for _, m := range matches {
		var start, dur float64
		for _, a := range attrRe.FindAllStringSubmatch(m[1], -1) {
			switch a[1] {
			case "start":
				start, _ = strconv.ParseFloat(a[2], 64)
			case "dur":
				dur, _ = strconv.ParseFloat(a[2], 64)
			}
		}
		text := normalizeText(html.UnescapeString(m[2]))
		if text == "" {
			continue
		}
		segs = append(segs, Segment{Start: start, Dur: dur, Text: text})
	}
	if len(segs) == 0 {
		return nil, errors.New("empty transcript")
	}
	return segs, nil
}

var wsRe = regexp.MustCompile(`\s+`)

func normalizeText(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// ----------------------------------------------------------------------------
// Orchestration : entrées -> IDs vidéos -> transcripts
// ----------------------------------------------------------------------------

// splitInputs découpe une chaîne en entrées individuelles (espaces, virgules,
// points-virgules, retours ligne) — utile pour « une liste d'URLs ».
func splitInputs(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// resolveVideoIDs transforme une liste d'entrées en liste d'IDs vidéos
// (dédupliquée). Si limit > 0, le total est plafonné à limit.
func resolveVideoIDs(ctx context.Context, inputs []string, limit int) ([]string, []string) {
	seen := map[string]bool{}
	var ids, warnings []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, in := range inputs {
		if limit > 0 && len(ids) >= limit {
			break
		}
		kind, norm := classify(in)
		switch kind {
		case kindVideo:
			add(norm)
		case kindChannel, kindPlaylist:
			found, err := scrapeVideoIDs(ctx, norm, limit)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%q: %v", in, err))
				continue
			}
			for _, id := range found {
				add(id)
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%q: unrecognized input", in))
		}
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, warnings
}

// fetchAll récupère les transcripts en parallèle, en préservant l'ordre.
func fetchAll(ctx context.Context, ids []string, lang string) []VideoTranscript {
	results := make([]VideoTranscript, len(ids))
	sem := make(chan struct{}, fetchWorkers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = getTranscript(ctx, id, lang)
		}(i, id)
	}
	wg.Wait()
	return results
}

// ----------------------------------------------------------------------------
// Serveur HTTP
// ----------------------------------------------------------------------------

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "serve" {
		runCLI(args)
		return
	}
	runServer()
}

func runServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/api/transcripts", handleTranscripts)
	mux.HandleFunc("/ui/transcripts", handleUITranscripts)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "yt-transcript listening on :%s\n", port)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

// requestOptions regroupe les paramètres d'une requête.
type requestOptions struct {
	inputs   []string
	lang     string
	format   string
	segments bool
	limit    int
}

// gatherInputs réunit toutes les entrées d'une requête : chaîne de requête,
// formulaire urlencodé (HTMX) ou corps JSON.
func gatherInputs(r *http.Request) requestOptions {
	opt := requestOptions{}
	r.ParseForm() // peuple r.Form avec la query + le formulaire urlencodé
	q := r.Form
	for _, key := range []string{"url", "urls", "channel", "input"} {
		for _, v := range q[key] {
			opt.inputs = append(opt.inputs, splitInputs(v)...)
		}
	}
	opt.lang = q.Get("lang")
	opt.format = q.Get("format")
	opt.segments = q.Get("segments") == "1" || q.Get("segments") == "true" || q.Get("segments") == "on"
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		opt.limit = n
	}

	if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var bodyReq struct {
			URL      string   `json:"url"`
			URLs     []string `json:"urls"`
			Channel  string   `json:"channel"`
			Input    string   `json:"input"`
			Lang     string   `json:"lang"`
			Format   string   `json:"format"`
			Segments bool     `json:"segments"`
			Limit    int      `json:"limit"`
		}
		if data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(data) > 0 {
			if json.Unmarshal(data, &bodyReq) == nil {
				opt.inputs = append(opt.inputs, splitInputs(bodyReq.URL)...)
				opt.inputs = append(opt.inputs, bodyReq.URLs...)
				opt.inputs = append(opt.inputs, splitInputs(bodyReq.Channel)...)
				opt.inputs = append(opt.inputs, splitInputs(bodyReq.Input)...)
				if bodyReq.Lang != "" {
					opt.lang = bodyReq.Lang
				}
				if bodyReq.Format != "" {
					opt.format = bodyReq.Format
				}
				opt.segments = opt.segments || bodyReq.Segments
				if bodyReq.Limit != 0 {
					opt.limit = bodyReq.Limit
				}
			}
		}
	}
	return opt
}

func handleTranscripts(w http.ResponseWriter, r *http.Request) {
	opt := gatherInputs(r)
	if len(opt.inputs) == 0 {
		http.Error(w, "no input provided (url / urls / channel parameter)", http.StatusBadRequest)
		return
	}
	lang, format, withSegments := opt.lang, opt.format, opt.segments

	// Le listing d'une chaîne peut être long : on s'accorde un délai large.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	ids, warnings := resolveVideoIDs(ctx, opt.inputs, opt.limit)
	results := fetchAll(ctx, ids, lang)
	if !withSegments {
		for i := range results {
			results[i].Segments = nil
		}
	}

	if format == "text" || format == "txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for i, res := range results {
			if i > 0 {
				io.WriteString(w, "\n\n")
			}
			writeTextResult(w, res)
		}
		for _, warn := range warnings {
			fmt.Fprintf(w, "\n[warning] %s\n", warn)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(map[string]any{
		"count":    len(results),
		"results":  results,
		"warnings": warnings,
	})
}

func writeTextResult(w io.Writer, res VideoTranscript) {
	fmt.Fprintf(w, "# %s\n", firstNonEmpty(res.Title, res.VideoID))
	fmt.Fprintf(w, "# %s\n", res.URL)
	if res.Error != "" {
		fmt.Fprintf(w, "[error] %s\n", res.Error)
		return
	}
	fmt.Fprintln(w, res.Text)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, pageHTML)
}

// uiVideo : modèle d'affichage d'un transcript pour le rendu HTML.
type uiVideo struct {
	Title    string
	URL      string
	Author   string
	Language string
	Text     string
	Error    string
	Words    int
	ReadMins int
}

var resultsTmpl = template.Must(template.New("results").Funcs(template.FuncMap{
	"fmtInt": fmtInt,
}).Parse(resultsTmplStr))

// handleUITranscripts répond à l'interface HTMX par un fragment HTML.
func handleUITranscripts(w http.ResponseWriter, r *http.Request) {
	opt := gatherInputs(r)
	if opt.limit == 0 {
		opt.limit = 10 // valeur par défaut côté UI pour garder une réponse rapide
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if len(opt.inputs) == 0 {
		resultsTmpl.Execute(w, map[string]any{"Error": "Paste at least one YouTube link."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Minute)
	defer cancel()

	ids, warnings := resolveVideoIDs(ctx, opt.inputs, opt.limit)
	results := fetchAll(ctx, ids, opt.lang)

	vids := make([]uiVideo, 0, len(results))
	for _, res := range results {
		words := 0
		if res.Text != "" {
			words = len(strings.Fields(res.Text))
		}
		vids = append(vids, uiVideo{
			Title:    firstNonEmpty(res.Title, res.VideoID),
			URL:      res.URL,
			Author:   res.Author,
			Language: res.Language,
			Text:     res.Text,
			Error:    res.Error,
			Words:    words,
			ReadMins: max(1, words/200),
		})
	}
	if len(vids) == 0 {
		resultsTmpl.Execute(w, map[string]any{
			"Error":    "No video found for this input.",
			"Warnings": warnings,
		})
		return
	}
	resultsTmpl.Execute(w, map[string]any{
		"Videos":   vids,
		"Warnings": warnings,
		"Count":    len(vids),
	})
}

// fmtInt formate un entier avec des espaces fines comme séparateurs de milliers.
func fmtInt(n int) string {
	s := strconv.Itoa(n)
	if n < 1000 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

const pageHTML = `<!doctype html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="theme-color" content="#15110d">
<title>Verbatim — YouTube transcripts</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Fraunces:ital,opsz,wght@0,9..144,500..900;1,9..144,500..800&family=Hanken+Grotesk:wght@400;500;600;700&family=Newsreader:ital,opsz,wght@0,6..72,400..560;1,6..72,400..500&display=swap" rel="stylesheet">
<script src="https://unpkg.com/htmx.org@2.0.4" integrity="sha384-HGfztofotfshcF7+8n44JQL2oJmowVChPTg48S+jvZoztPfvwD79OC/LTtG6dMp+" crossorigin="anonymous" defer></script>
<style>
  :root{
    --bg:#15110d; --bg-2:#1b1611; --card:#221b15; --card-2:#271f18;
    --line:rgba(245,237,228,.09); --line-2:rgba(245,237,228,.16);
    --ink:#f5ede4; --ink-2:#c6b7a6; --ink-3:#8b7d6e;
    --accent:#ff5a36; --accent-2:#ff7a59; --accent-ink:#1a0d07; --amber:#e9b567;
    --radius:20px; --radius-sm:13px;
    --shadow:0 24px 60px -28px rgba(0,0,0,.7);
  }
  *{box-sizing:border-box}
  html{-webkit-text-size-adjust:100%}
  body{
    margin:0; background:var(--bg); color:var(--ink);
    font-family:'Hanken Grotesk',system-ui,sans-serif; line-height:1.5;
    -webkit-font-smoothing:antialiased; text-rendering:optimizeLegibility;
    min-height:100dvh; overflow-x:hidden;
  }
  /* atmosphère : halo chaud + grain */
  body::before{content:"";position:fixed;inset:0;z-index:-2;
    background:
      radial-gradient(120% 80% at 50% -8%, rgba(255,90,54,.22), transparent 55%),
      radial-gradient(90% 60% at 90% 10%, rgba(233,181,103,.10), transparent 60%),
      var(--bg);}
  body::after{content:"";position:fixed;inset:0;z-index:-1;pointer-events:none;opacity:.05;
    background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='160' height='160'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='.85' numOctaves='2'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)'/%3E%3C/svg%3E");}

  .wrap{max-width:680px;margin:0 auto;padding:clamp(22px,6vw,46px) clamp(16px,5vw,26px) 140px;}

  .brand{display:flex;align-items:center;gap:9px;font-size:12.5px;letter-spacing:.24em;
    text-transform:uppercase;color:var(--ink-3);font-weight:600;}
  .brand .dot{width:8px;height:8px;border-radius:50%;background:var(--accent);
    box-shadow:0 0 0 4px rgba(255,90,54,.16);}

  h1{font-family:'Fraunces',serif;font-weight:600;font-optical-sizing:auto;
    font-size:clamp(40px,12.5vw,68px);line-height:.98;letter-spacing:-.02em;
    margin:.5em 0 .28em;}
  h1 em{font-style:italic;font-weight:500;color:var(--accent);}
  .tagline{color:var(--ink-2);font-size:clamp(15px,4.2vw,18px);max-width:30ch;margin:0 0 26px;}

  /* composer */
  .composer{background:linear-gradient(180deg,var(--card-2),var(--card));
    border:1px solid var(--line-2);border-radius:var(--radius);padding:14px;
    box-shadow:var(--shadow);position:relative;}
  .composer:focus-within{border-color:rgba(255,90,54,.5);
    box-shadow:var(--shadow),0 0 0 4px rgba(255,90,54,.12);}
  textarea{width:100%;border:0;outline:0;background:transparent;color:var(--ink);resize:none;
    font-family:'Hanken Grotesk',sans-serif;font-size:16px;line-height:1.55;
    padding:8px 8px 4px;min-height:84px;}
  textarea::placeholder{color:var(--ink-3);}
  .composer-row{display:flex;align-items:center;gap:10px;margin-top:8px;
    padding-top:12px;border-top:1px solid var(--line);}

  .opts{flex:1;min-width:0;}
  .opts summary{list-style:none;cursor:pointer;color:var(--ink-2);font-size:13.5px;
    font-weight:600;display:inline-flex;align-items:center;gap:7px;padding:8px 10px;
    border-radius:99px;user-select:none;-webkit-tap-highlight-color:transparent;}
  .opts summary::-webkit-details-marker{display:none}
  .opts summary:hover{color:var(--ink);background:var(--bg-2);}
  .opts summary .chev{transition:transform .2s;display:inline-block}
  .opts[open] summary .chev{transform:rotate(90deg)}
  .opt-grid{display:flex;gap:10px;margin:10px 2px 2px;flex-wrap:wrap;}
  .field{flex:1;min-width:120px;display:flex;flex-direction:column;gap:5px;}
  .field span{font-size:11.5px;letter-spacing:.12em;text-transform:uppercase;color:var(--ink-3);font-weight:600}
  .field input{background:var(--bg);border:1px solid var(--line-2);border-radius:11px;
    color:var(--ink);padding:10px 12px;font:inherit;font-size:15px;outline:0;}
  .field input:focus{border-color:rgba(255,90,54,.55)}

  .btn{font:inherit;font-weight:600;cursor:pointer;border:0;border-radius:14px;
    display:inline-flex;align-items:center;justify-content:center;gap:8px;
    -webkit-tap-highlight-color:transparent;transition:transform .12s ease, filter .2s, background .2s;}
  .btn:active{transform:scale(.97)}
  .btn-go{background:var(--accent);color:var(--accent-ink);padding:14px 22px;font-size:16px;
    box-shadow:0 10px 24px -10px rgba(255,90,54,.7);white-space:nowrap;}
  .btn-go:hover{filter:brightness(1.06)}
  .btn-go svg{width:18px;height:18px}
  .btn-go .spin{display:none}
  #go.htmx-request{pointer-events:none;filter:saturate(.6)}
  #go.htmx-request .lbl,#go.htmx-request .arrow{display:none}
  #go.htmx-request .spin{display:inline-flex;animation:spin .7s linear infinite}
  @keyframes spin{to{transform:rotate(360deg)}}

  /* loader */
  .loader{display:flex;align-items:center;gap:12px;justify-content:center;
    color:var(--ink-2);font-size:14.5px;font-weight:500;
    overflow:hidden;max-height:0;opacity:0;margin-top:0;
    transition:max-height .35s ease,opacity .35s ease,margin-top .35s ease;}
  .loader.htmx-request{max-height:70px;opacity:1;margin-top:22px}
  .loader .dots{display:inline-flex;gap:5px}
  .loader .dots i{width:7px;height:7px;border-radius:50%;background:var(--accent);
    animation:bounce 1s ease-in-out infinite}
  .loader .dots i:nth-child(2){animation-delay:.15s;background:var(--accent-2)}
  .loader .dots i:nth-child(3){animation-delay:.3s;background:var(--amber)}
  @keyframes bounce{0%,80%,100%{transform:translateY(0);opacity:.5}40%{transform:translateY(-7px);opacity:1}}

  /* résultats */
  #results{margin-top:8px}
  .results-bar{display:flex;align-items:center;justify-content:space-between;gap:12px;
    margin:30px 4px 14px;flex-wrap:wrap;}
  .results-head{font-size:12px;letter-spacing:.22em;text-transform:uppercase;
    color:var(--ink-3);font-weight:600;}
  .sort{display:inline-flex;background:var(--bg-2);border:1px solid var(--line-2);
    border-radius:99px;padding:3px;}
  .sort button{font:inherit;font-weight:600;font-size:12.5px;color:var(--ink-3);
    background:transparent;border:0;padding:7px 14px;border-radius:99px;cursor:pointer;
    transition:background .15s,color .15s;-webkit-tap-highlight-color:transparent;}
  .sort button.active{background:var(--ink);color:#17120d;}
  .sort button:not(.active):hover{color:var(--ink);}
  .card{background:linear-gradient(180deg,var(--card-2),var(--card));
    border:1px solid var(--line);border-radius:var(--radius);padding:20px 18px;
    margin-bottom:16px;box-shadow:var(--shadow);
    animation:rise .5s cubic-bezier(.2,.7,.2,1) both;}
  .card:nth-child(2){animation-delay:.04s}.card:nth-child(3){animation-delay:.08s}
  .card:nth-child(4){animation-delay:.12s}.card:nth-child(5){animation-delay:.16s}
  @keyframes rise{from{opacity:0;transform:translateY(14px)}to{opacity:1;transform:none}}
  .card-top{display:flex;align-items:center;gap:10px;margin-bottom:11px}
  .badge{font-size:11px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;
    color:var(--amber);background:rgba(233,181,103,.12);border:1px solid rgba(233,181,103,.28);
    padding:3px 9px;border-radius:99px;}
  .meta{font-size:13px;color:var(--ink-3);font-variant-numeric:tabular-nums;margin-left:auto}
  .card-title{font-family:'Fraunces',serif;font-weight:600;font-size:21px;line-height:1.18;
    letter-spacing:-.01em;margin:0 0 3px}
  .card-title a{color:var(--ink);text-decoration:none}
  .card-title a:hover{color:var(--accent)}
  .card-author{color:var(--ink-3);font-size:13.5px;margin-bottom:14px}

  .transcript{font-family:'Newsreader',Georgia,serif;font-size:17.5px;line-height:1.62;
    color:var(--ink-2);white-space:pre-wrap;word-break:break-word;}
  .transcript.clamp{max-height:14.5em;overflow:hidden;
    -webkit-mask-image:linear-gradient(180deg,#000 64%,transparent);
    mask-image:linear-gradient(180deg,#000 64%,transparent);}

  .card-actions{display:flex;gap:10px;align-items:center;margin-top:16px}
  .btn-ghost{background:var(--bg-2);color:var(--ink-2);border:1px solid var(--line-2);
    padding:12px 16px;font-size:14px;white-space:nowrap}
  .btn-ghost:hover{color:var(--ink)}
  .btn-copy{flex:1;background:var(--ink);color:#17120d;padding:14px 18px;font-size:15.5px;}
  .btn-copy svg{width:17px;height:17px}
  .btn-copy:hover{filter:brightness(1.04)}
  .btn.ok{background:var(--accent);color:var(--accent-ink)}
  .btn.ok svg{display:none}

  .card-err{border-color:rgba(255,90,54,.3)}
  .card-error{color:var(--accent-2);font-size:14.5px;margin:4px 0 0;
    background:rgba(255,90,54,.08);border:1px solid rgba(255,90,54,.2);
    padding:11px 13px;border-radius:12px}

  .flash-msg{color:var(--ink-2);text-align:center;padding:26px;border:1px dashed var(--line-2);
    border-radius:var(--radius);margin-top:24px}
  .warns{margin-top:6px}
  .warn{color:var(--ink-3);font-size:13px;padding:8px 12px;border-left:2px solid rgba(233,181,103,.4);margin:6px 0}

  /* barre flottante "tout copier" */
  .bulkbar{position:fixed;left:50%;transform:translateX(-50%);
    bottom:calc(18px + env(safe-area-inset-bottom));z-index:50;
    display:flex;align-items:center;gap:14px;
    background:rgba(34,27,21,.86);backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);
    border:1px solid var(--line-2);border-radius:99px;padding:8px 8px 8px 18px;
    max-width:calc(100vw - 26px);
    box-shadow:0 18px 40px -16px rgba(0,0,0,.8);animation:rise .4s both .1s}
  .bulkbar-label{font-size:13.5px;color:var(--ink-2);font-weight:600;white-space:nowrap}
  .btn-bulk{background:var(--accent);color:var(--accent-ink);padding:11px 18px;font-size:14.5px}
  .btn-bulk svg{width:16px;height:16px}

  footer{color:var(--ink-3);font-size:13px;margin-top:40px;text-align:center;line-height:1.7}
  footer code{background:var(--bg-2);border:1px solid var(--line);border-radius:6px;
    padding:2px 7px;font-size:12px;color:var(--ink-2)}
  footer a{color:var(--ink-2)}

  @media (prefers-reduced-motion:reduce){*{animation:none!important;transition:none!important}}
  @media (min-width:560px){
    .opt-grid{flex-wrap:nowrap}
  }
</style>
</head>
<body>
<div class="wrap">
  <div class="brand"><span class="dot"></span> Verbatim · YouTube transcripts</div>

  <h1>Everything that's said,<br><em>as text.</em></h1>
  <p class="tagline">Paste a video, a whole channel or a list of links. Get the text — and copy it in a single tap.</p>

  <form class="composer" hx-post="/ui/transcripts" hx-target="#results" hx-swap="innerHTML"
        hx-indicator="#loader" hx-disabled-elt="#go"
        hx-on::after-request="if(event.detail.successful){var r=document.getElementById('results');r.dataset.order='newest';r.scrollIntoView({behavior:'smooth',block:'start'})}">
    <textarea name="urls" rows="3" autocapitalize="off" autocomplete="off" spellcheck="false"
      placeholder="Paste here…&#10;https://youtu.be/dQw4w9WgXcQ&#10;https://www.youtube.com/@achannel&#10;several links — one per line"></textarea>
    <div class="composer-row">
      <details class="opts">
        <summary><span class="chev">›</span> Options</summary>
        <div class="opt-grid">
          <label class="field"><span>Language</span>
            <input name="lang" placeholder="auto (e.g. en, fr)" autocapitalize="off"></label>
          <label class="field"><span>Max videos</span>
            <input name="limit" type="number" inputmode="numeric" value="10" min="1" max="500"></label>
        </div>
      </details>
      <button id="go" class="btn btn-go" type="submit">
        <span class="lbl">Extract</span>
        <svg class="arrow" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M13 6l6 6-6 6"/></svg>
        <svg class="spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M12 3a9 9 0 1 0 9 9" opacity=".95"/></svg>
      </button>
    </div>
  </form>

  <div id="loader" class="loader htmx-indicator">
    <span class="dots"><i></i><i></i><i></i></span>
    <span>Fetching transcripts…</span>
  </div>

  <div id="results" data-order="newest"></div>

  <footer>
    Also available as an API — <code>GET /api/transcripts?url=…&amp;format=text</code><br>
    no key, no sign-up.
  </footer>
</div>

<script>
  function flash(btn, label){
    var lbl=btn.querySelector('.lbl'); var old=lbl?lbl.textContent:'';
    btn.classList.add('ok'); if(lbl) lbl.textContent=label||'Copied ✓';
    setTimeout(function(){ btn.classList.remove('ok'); if(lbl) lbl.textContent=old; }, 1700);
  }
  function writeClip(text, btn, label){
    if(navigator.clipboard && navigator.clipboard.writeText){
      navigator.clipboard.writeText(text).then(function(){flash(btn,label)});
    } else {
      var ta=document.createElement('textarea'); ta.value=text; document.body.appendChild(ta);
      ta.select(); try{document.execCommand('copy')}catch(e){} document.body.removeChild(ta);
      flash(btn,label);
    }
  }
  function copyCard(btn){
    var card=btn.closest('.card');
    var body=card.querySelector('.transcript');
    writeClip(body?body.innerText:'', btn, 'Copied ✓');
  }
  function copyAll(btn){
    var parts=[];
    document.querySelectorAll('#results .card').forEach(function(c){
      var b=c.querySelector('.transcript'); if(!b) return;
      var t=c.querySelector('.card-title'); var title=t?t.innerText.trim():'';
      parts.push((title?title+'\n\n':'')+b.innerText.trim());
    });
    writeClip(parts.join('\n\n———\n\n'), btn, 'All copied ✓');
  }
  function toggleClamp(btn){
    var card=btn.closest('.card');
    var body=card.querySelector('.transcript');
    var open=body.classList.toggle('clamp')===false;
    btn.textContent=open?'Collapse':'Show all';
  }
  function sortCards(btn, order){
    var results=document.getElementById('results');
    if((results.dataset.order||'newest')!==order){
      results.dataset.order=order;
      var cards=Array.prototype.slice.call(results.querySelectorAll('.card'));
      cards.reverse().forEach(function(c){ results.appendChild(c); });
    }
    var group=btn.parentNode;
    group.querySelectorAll('button').forEach(function(b){ b.classList.remove('active'); });
    btn.classList.add('active');
  }
</script>
</body>
</html>`

const resultsTmplStr = `{{if .Error}}
  <div class="flash-msg">{{.Error}}</div>
  {{range .Warnings}}<div class="warn">⚠ {{.}}</div>{{end}}
{{else}}
  <div class="results-bar">
    <span class="results-head">{{.Count}} result{{if gt .Count 1}}s{{end}}</span>
    {{if gt .Count 1}}
    <div class="sort" role="group" aria-label="Sort order">
      <button type="button" class="active" onclick="sortCards(this,'newest')">Newest</button>
      <button type="button" onclick="sortCards(this,'oldest')">Oldest</button>
    </div>
    {{end}}
  </div>
  {{if gt .Count 1}}
  <div class="bulkbar">
    <span class="bulkbar-label">{{.Count}} videos</span>
    <button class="btn btn-bulk" type="button" onclick="copyAll(this)">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2.5"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
      <span class="lbl">Copy all transcripts</span>
    </button>
  </div>
  {{end}}
  {{range .Videos}}
  <article class="card{{if .Error}} card-err{{end}}">
    <div class="card-top">
      {{if .Language}}<span class="badge">{{.Language}}</span>{{end}}
      {{if not .Error}}<span class="meta">{{fmtInt .Words}} words · ~{{.ReadMins}} min</span>{{end}}
    </div>
    <h2 class="card-title"><a href="{{.URL}}" target="_blank" rel="noopener">{{.Title}}</a></h2>
    {{if .Author}}<div class="card-author">{{.Author}}</div>{{end}}
    {{if .Error}}
      <p class="card-error">{{.Error}}</p>
    {{else}}
      <div class="transcript clamp">{{.Text}}</div>
      <div class="card-actions">
        <button class="btn btn-ghost" type="button" onclick="toggleClamp(this)">Show all</button>
        <button class="btn btn-copy" type="button" onclick="copyCard(this)">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2.5"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>
          <span class="lbl">Copy</span>
        </button>
      </div>
    {{end}}
  </article>
  {{end}}
  {{if .Warnings}}<div class="warns">{{range .Warnings}}<div class="warn">⚠ {{.}}</div>{{end}}</div>{{end}}
{{end}}`

// ----------------------------------------------------------------------------
// Mode CLI
// ----------------------------------------------------------------------------

func runCLI(args []string) {
	var inputs []string
	lang, format := "", "text"
	limit := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--lang" && i+1 < len(args):
			i++
			lang = args[i]
		case strings.HasPrefix(a, "--lang="):
			lang = strings.TrimPrefix(a, "--lang=")
		case a == "--limit" && i+1 < len(args):
			i++
			limit, _ = strconv.Atoi(args[i])
		case strings.HasPrefix(a, "--limit="):
			limit, _ = strconv.Atoi(strings.TrimPrefix(a, "--limit="))
		case a == "--json":
			format = "json"
		case a == "--text":
			format = "text"
		default:
			inputs = append(inputs, splitInputs(a)...)
		}
	}
	if len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: yt-transcript [--lang en] [--limit N] [--json] <url|channel> [url...]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ids, warnings := resolveVideoIDs(ctx, inputs, limit)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "[warning]", w)
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "no videos to process")
		os.Exit(1)
	}
	results := fetchAll(ctx, ids, lang)

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
		return
	}
	for i, res := range results {
		if i > 0 {
			fmt.Println()
		}
		writeTextResult(os.Stdout, res)
	}
}
