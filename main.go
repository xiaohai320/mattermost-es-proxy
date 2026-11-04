// mm-es-substring-proxy: a reverse proxy between Mattermost and Elasticsearch
// that injects a substring match into posts search queries, so Mattermost
// gets arbitrary-position substring search without modifying Mattermost
// source code. Two interchangeable strategies, selected by SUBSTRING_MODE:
//
//   - wildcard (default): exact, case-insensitive substring via a
//     message.wildcard sub-field (ES >= 7.9). Small index size, no fuzz.
//   - ngram: token-window substring via a message.ngram sub-field (requires
//     the substring_analyzer defined in index settings) with
//     minimum_should_match fuzzing (MSM, default 75%). 3~5x index bloat,
//     tolerant of near-matches.
//
// Behavior:
//   - POST {index-pattern}/_search where index matches POSTS_PATTERN: parse body,
//     wrap every message-targeting leaf clause (including ones under must_not)
//     in place with {"bool":{"should":[orig, substring-match]}} — each clause
//     gets its own term, so exclusions stay substring-aware while positive
//     terms never leak into negation branches. Existing semantics preserved.
//   - Everything else: transparent pass-through (streaming).
//
// Env:
//
//	ES_UPSTREAM        upstream ES base URL (default http://1.1.1.1:9200; set it!)
//	LISTEN_ADDR        listen address (default :9210)
//	POSTS_PATTERN      comma-separated prefixes to rewrite (default posts)
//	                  (any index whose name starts with any entry gets rewritten;
//	                  use "posts_" if you also have non-daily posts* indices that
//	                  must stay out of the rewrite)
//	SUBSTRING_MODE     "wildcard" (default) or "ngram" — the substring strategy
//	MSM                minimum_should_match for the ngram match (default 75%,
//	                  ngram mode only)
//	AUTO_PATCH_NGRAM   =1 to auto-add the missing message sub-field on daily
//	                  indexes (best-effort; name kept for config compatibility,
//	                  the field patched depends on SUBSTRING_MODE)
//
// Auth: none configured here. The inbound Authorization header (sent by
// Mattermost's ElasticsearchSettings Username/Password) is forwarded to the
// upstream as-is; the auto-patch maintenance calls re-use the most recent
// inbound Authorization header (see lastAuth).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// wildcardEscaper escapes the three characters that have special meaning in a
// wildcard query (* ? \) so a user-typed term is matched literally, with the
// surrounding * we add ourselves being the only wildcards in the pattern.
// Package-level: strings.NewReplacer builds an internal table, so it must not
// be constructed per call.
var wildcardEscaper = strings.NewReplacer(
	`\`, `\\`,
	`*`, `\*`,
	`?`, `\?`,
)

func escapeWildcard(term string) string {
	return wildcardEscaper.Replace(term)
}

// strategy holds everything that varies between the substring modes; one
// instance is resolved from SUBSTRING_MODE at startup (see resolveStrategy).
type strategy struct {
	name         string // "wildcard" or "ngram" (for logs)
	subField     string // message.<subField> is the field we ensure and query
	mappingPatch map[string]interface{}
}

var (
	strat *strategy
	msm   string
)

// resolveStrategy maps SUBSTRING_MODE to a concrete strategy. Called once
// from main; mode dispatch must not happen anywhere else.
func resolveStrategy(mode string) *strategy {
	switch mode {
	case "ngram":
		return &strategy{
			name:     "ngram",
			subField: "ngram",
			mappingPatch: map[string]interface{}{
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "text",
						"fields": map[string]interface{}{
							"ngram": map[string]interface{}{
								"type":            "text",
								"analyzer":        "substring_analyzer",
								"search_analyzer": "substring_analyzer",
							},
						},
					},
				},
			},
		}
	default: // "wildcard"
		return &strategy{
			name:     "wildcard",
			subField: "wildcard",
			mappingPatch: map[string]interface{}{
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type": "text",
						"fields": map[string]interface{}{
							"wildcard": map[string]interface{}{
								"type": "wildcard",
							},
						},
					},
				},
			},
		}
	}
}

// substringMatch builds the substring branch injected alongside the original
// leaf clause.
func (s *strategy) substringMatch(term string) map[string]interface{} {
	if s.name == "ngram" {
		return map[string]interface{}{
			"match": map[string]interface{}{
				"message." + s.subField: map[string]interface{}{
					"query":                term,
					"minimum_should_match": msm,
				},
			},
		}
	}
	return map[string]interface{}{
		"wildcard": map[string]interface{}{
			"message." + s.subField: map[string]interface{}{
				"value": "*" + escapeWildcard(term) + "*",
			},
		},
	}
}

// wrapLeaf replaces one leaf clause in place with
// bool{should:[orig-leaf, substring-match]} and returns it as JSON. Because it
// only replaces the message clause itself, all sibling restrictions (channel
// filters, user filters, type filters in the enclosing bool) keep applying
// automatically — no filter copying needed, no permission leak.
// The substring branch comes from the selected strategy (see resolveStrategy).
func wrapLeaf(queryType string, origClause json.RawMessage, term string) json.RawMessage {
	inner := map[string]interface{}{
		"bool": map[string]interface{}{
			"should": []interface{}{
				map[string]interface{}{queryType: json.RawMessage(origClause)},
				strat.substringMatch(term),
			},
			"minimum_should_match": 1,
		},
	}
	b, _ := json.Marshal(inner)
	return b
}

// rewriteQuery walks the query tree exactly once, doing both jobs the old
// extract/inject pair did separately: for every leaf clause that targets a
// message field it records the user-typed term AND replaces the leaf in place
// with a bool{should:[orig, substring-match]} branch (the match shape depends
// on the selected strategy). This includes leaves under
// must_not — a negation must also exclude substring matches — but crucially
// each clause only ever gets its own term: a positive term from elsewhere in
// the tree is never OR'd into a negation branch (that would wrongly exclude
// every message containing the positive term as a substring).
//
// Returns the (possibly rewritten) query, the terms found, and an error only
// if the query JSON itself is malformed (propagated from recursion).
func rewriteQuery(query json.RawMessage) (json.RawMessage, []string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(query, &probe); err != nil {
		return query, nil, err
	}
	// leaf: simple_query_string / query_string whose fields include message
	// (no fields => all fields, includes message)
	for _, key := range []string{"simple_query_string", "query_string"} {
		if raw, ok := probe[key]; ok {
			var sqs struct {
				Fields json.RawMessage `json:"fields"`
				Query  string          `json:"query"`
			}
			if err := json.Unmarshal(raw, &sqs); err == nil && sqs.Query != "" {
				f, _ := json.Marshal(sqs.Fields)
				if string(f) == "null" || string(f) == "" || strings.Contains(string(f), "message") {
					return wrapLeaf(key, raw, sqs.Query), []string{sqs.Query}, nil
				}
			}
		}
	}
	// leaf: match/match_phrase/match_phrase_prefix on a message field
	for _, key := range []string{"match", "match_phrase", "match_phrase_prefix"} {
		if raw, ok := probe[key]; ok {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err == nil {
				for field, v := range m {
					if !strings.Contains(field, "message") {
						continue
					}
					var s string
					var obj struct {
						Query string `json:"query"`
					}
					if err := json.Unmarshal(v, &obj); err == nil && obj.Query != "" {
						s = obj.Query
					} else {
						json.Unmarshal(v, &s)
					}
					if s == "" {
						continue
					}
					return wrapLeaf(key, raw, s), []string{s}, nil
				}
			}
		}
	}
	// leaf: multi_match whose fields include message
	if raw, ok := probe["multi_match"]; ok {
		var m struct {
			Query  string      `json:"query"`
			Fields interface{} `json:"fields"`
		}
		if err := json.Unmarshal(raw, &m); err == nil && m.Query != "" {
			f, _ := json.Marshal(m.Fields)
			if string(f) == "null" || string(f) == "" || strings.Contains(string(f), "message") {
				return wrapLeaf("multi_match", raw, m.Query), []string{m.Query}, nil
			}
		}
	}
	// internal node: recurse into bool children (must_not included — its
	// leaves get their own terms injected, see the doc comment above)
	if raw, ok := probe["bool"]; ok {
		var b map[string]json.RawMessage
		if err := json.Unmarshal(raw, &b); err != nil {
			return query, nil, err
		}
		changed := false
		var terms []string
		for _, section := range []string{"must", "should", "filter", "must_not"} {
			node, ok := b[section]
			if !ok {
				continue
			}
			var arr []json.RawMessage
			if err := json.Unmarshal(node, &arr); err != nil {
				arr = []json.RawMessage{node}
			}
			out := make([]json.RawMessage, 0, len(arr))
			anyChanged := false
			for _, clause := range arr {
				newClause, t, err := rewriteQuery(clause)
				if err != nil {
					return query, nil, err
				}
				out = append(out, newClause)
				terms = append(terms, t...)
				anyChanged = anyChanged || len(t) > 0
			}
			if anyChanged {
				changed = true
				merged, err := json.Marshal(out)
				if err != nil {
					return query, nil, err
				}
				b[section] = merged
			}
		}
		if changed {
			newBool, err := json.Marshal(b)
			if err != nil {
				return query, nil, err
			}
			full, err := json.Marshal(map[string]interface{}{"bool": json.RawMessage(newBool)})
			if err != nil {
				return query, nil, err
			}
			return full, terms, nil
		}
	}
	return query, nil, nil
}

// rewriteSearch parses the search body as a plain raw-key map (unknown
// top-level keys are preserved verbatim), rewrites the query if it contains
// any message-targeting clause, and re-marshals. Returns the body unchanged
// when there is nothing to rewrite.
func rewriteSearch(body []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	q, ok := m["query"]
	if !ok || len(q) == 0 {
		return body, nil
	}
	nq, terms, err := rewriteQuery(q)
	if err != nil {
		return nil, err
	}
	if len(terms) == 0 || bytes.Equal(nq, q) {
		return body, nil
	}
	m["query"] = nq
	return json.Marshal(m)
}

type patchState struct {
	done    bool
	attempt time.Time
}

const patchRetryCooldown = 2 * time.Minute

// shouldAttempt rate-limits background maintenance attempts per key: once a
// key is marked done it never retries, and after a failure it retries at most
// once per cooldown — so a broken upstream or bad mapping never adds latency
// or log spam to every request.
func shouldAttempt(m *sync.Map, key string) bool {
	now := time.Now()
	if v, ok := m.Load(key); ok {
		st := v.(patchState)
		if st.done {
			return false
		}
		if now.Sub(st.attempt) < patchRetryCooldown {
			return false
		}
	}
	m.Store(key, patchState{attempt: now})
	return true
}

func markDone(m *sync.Map, key string) {
	m.Store(key, patchState{done: true, attempt: time.Now()})
}

var (
	upstream      *url.URL
	listenAddr    string
	postsPrefixes []string

	// substring sub-field auto-patch: mm's own index template defines "message"
	// explicitly, which drops our message.<subField> on every new daily index
	// (posts_2026_11_04 etc). The proxy remembers which concrete index names
	// it has already patched and PUTs the missing mapping once per index,
	// right before forwarding the first request that touches it.
	// Auth: the patch calls re-use the Authorization header from the most
	// recent client request (mm sends its ES credentials on every call, and
	// since auth is pass-through the proxy holds no credentials of its own).
	patchedIndexes sync.Map // concrete index name -> patchState
	patternChecked sync.Map // "pattern@YYYY-MM-DD" -> patchState
	lastAuth       struct {
		sync.Mutex
		header string
	}
	// mappingClient is used only for the background mapping GET/PUT; the
	// timeout bounds the synchronous auto-patch so a stalled upstream ES
	// cannot hang a user's search.
	mappingClient = &http.Client{Timeout: 10 * time.Second}
)

// hasSubField reports whether the strategy's sub-field already exists in a
// mapping document.
func hasSubField(mapping json.RawMessage, subField string) bool {
	var doc struct {
		Mappings struct {
			Properties struct {
				Message struct {
					Fields json.RawMessage `json:"fields"`
				} `json:"message"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if json.Unmarshal(mapping, &doc) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(doc.Mappings.Properties.Message.Fields, &fields) != nil {
		return false
	}
	_, ok := fields[subField]
	return ok
}

// rememberAuth captures the inbound Authorization header so background
// maintenance calls (substring patch) can authenticate to upstream.
func rememberAuth(r *http.Request) {
	if v := r.Header.Get("Authorization"); v != "" {
		lastAuth.Lock()
		lastAuth.header = v
		lastAuth.Unlock()
	}
}

func patchAuthHeader() string {
	lastAuth.Lock()
	defer lastAuth.Unlock()
	return lastAuth.header
}

// ensureSubFields is called before forwarding a search on an index
// expression (possibly comma-separated and/or wildcarded). Concrete daily
// indexes are checked/patched directly; any other form (wildcard pattern,
// alias, comma list) is resolved to its concrete indices once per pattern per
// day and each daily index found there is patched as needed. Best-effort:
// failures are logged, rate-limited by shouldAttempt, and the request
// proceeds either way.
func ensureSubFields(idxExpr string) {
	if os.Getenv("AUTO_PATCH_NGRAM") != "1" {
		return
	}
	for _, part := range strings.Split(idxExpr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bare := strings.TrimSuffix(part, "*")
		if dailyIndexRe.MatchString(bare) {
			ensureSubField(bare)
		} else {
			ensureSubPattern(part)
		}
	}
}

// getMapping fetches the _mapping document for an index name or pattern.
func getMapping(indexOrPattern string) (json.RawMessage, bool) {
	req, err := http.NewRequest(http.MethodGet, upstream.String()+"/"+indexOrPattern+"/_mapping", nil)
	if err != nil {
		return nil, false
	}
	if a := patchAuthHeader(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := mappingClient.Do(req)
	if err != nil {
		log.Printf("substring patch: GET mapping %s failed: %v", indexOrPattern, err)
		return nil, false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("substring patch: GET mapping %s -> %d %s", indexOrPattern, resp.StatusCode, body)
		return nil, false
	}
	return json.RawMessage(body), true
}

// patchIndex checks one concrete index's mapping (already fetched) and PUTs
// the strategy's message sub-field if it is missing. On failure the index
// stays unmarked and is retried after the cooldown.
func patchIndex(index string, mapping json.RawMessage) {
	sub := strat.subField
	if hasSubField(mapping, sub) {
		log.Printf("substring patch: %s already has message.%s, skip", index, sub)
		markDone(&patchedIndexes, index)
		return
	}
	pb, _ := json.Marshal(strat.mappingPatch)
	req, err := http.NewRequest(http.MethodPut, upstream.String()+"/"+index+"/_mapping", bytes.NewReader(pb))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if a := patchAuthHeader(); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := mappingClient.Do(req)
	if err != nil {
		log.Printf("substring patch: PUT mapping %s failed: %v", index, err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		log.Printf("substring patch: %s message.%s sub-field ensured", index, sub)
		markDone(&patchedIndexes, index)
	} else {
		log.Printf("substring patch: PUT mapping %s -> %d %s", index, resp.StatusCode, body)
	}
}

// ensureSubField checks/patches one concrete daily index.
func ensureSubField(index string) {
	if !shouldAttempt(&patchedIndexes, index) {
		return
	}
	mapping, ok := getMapping(index)
	if !ok {
		return // left unmarked: retried after the cooldown
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(mapping, &m) != nil {
		return
	}
	if doc, ok := m[index]; ok {
		patchIndex(index, doc)
	}
}

// ensureSubPattern resolves a wildcard/alias index pattern to its concrete
// indices and patches each daily index found there. The pattern→indices
// resolution is re-run (rate-limited by shouldAttempt) until every daily
// index found is either patched or confirmed to already have the sub-field —
// so one transient failure never blocks patching for the rest of the day,
// and a newly rolled-over daily index is picked up after midnight.
func ensureSubPattern(pattern string) {
	key := pattern + "@" + time.Now().Format("2006-01-02")
	if !shouldAttempt(&patternChecked, key) {
		return
	}
	mapping, ok := getMapping(pattern)
	if !ok {
		return // left unmarked: retried after the cooldown
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(mapping, &m) != nil {
		return
	}
	allSettled := true
	for name, doc := range m {
		if !dailyIndexRe.MatchString(name) {
			continue
		}
		if v, ok := patchedIndexes.Load(name); ok && v.(patchState).done {
			continue
		}
		if hasSubField(doc, strat.subField) {
			log.Printf("substring patch: %s already has message.%s, skip", name, strat.subField)
			markDone(&patchedIndexes, name)
			continue
		}
		// not settled yet: try once now (respecting the per-index attempt
		// marker so a failing index is retried at most once per cooldown)
		if shouldAttempt(&patchedIndexes, name) {
			patchIndexDirect(name)
			if v, ok := patchedIndexes.Load(name); !ok || !v.(patchState).done {
				allSettled = false
			}
		} else {
			allSettled = false
		}
	}
	if allSettled {
		markDone(&patternChecked, key)
	}
}

// patchIndexDirect fetches an index's own mapping and patches it (used when
// a concrete index is discovered via a pattern).
func patchIndexDirect(index string) {
	mapping, ok := getMapping(index)
	if !ok {
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(mapping, &m) != nil {
		return
	}
	if doc, ok := m[index]; ok {
		patchIndex(index, doc)
	}
}

var dailyIndexRe = regexp.MustCompile(`^posts_\d{4}_\d{2}_\d{2}$`)

func shouldRewriteIndex(path string) (string, bool) {
	// path like /posts_2026_11_03/_search, /posts*/_search, /posts_*/_search
	// or /posts_2026_11_02,posts_2026_11_03/_search
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	idx := parts[0]
	last := parts[len(parts)-1]
	if last != "_search" && !strings.HasSuffix(last, "_search") {
		return "", false
	}
	// Our indices are all fixed to start with the configured prefix (default
	// "posts"), so a simple forward check suffices: the requested index
	// expression (one segment per comma-separated part, wildcard stripped)
	// starts with the prefix.
	for _, seg := range strings.Split(idx, ",") {
		seg = strings.TrimSpace(seg)
		bare := strings.TrimSuffix(seg, "*")
		if bare == "" {
			// "/*/_search" matches every index: never rewrite
			continue
		}
		for _, p := range postsPrefixes {
			if strings.HasPrefix(bare, p) {
				return idx, true
			}
		}
	}
	return "", false
}

type proxyHandler struct {
	pass *httputil.ReverseProxy
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Auth is pass-through: whatever Authorization header Mattermost sends
	// (its ElasticsearchSettings Username/Password) goes upstream unchanged.

	rememberAuth(r)
	// per-request access log: who called us, what they asked for
	log.Printf("REQ %s %s from %s ua=%q len=%d",
		r.Method, r.URL.Path,
		getRemoteIP(r), r.UserAgent(), r.ContentLength)

	if r.Method == http.MethodPost {
		if idx, ok := shouldRewriteIndex(r.URL.Path); ok {
			// before forwarding the first request on a concrete daily index,
			// make sure the mode's message sub-field exists on it
			ensureSubFields(idx)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.Body.Close()

			var rewritten []byte
			rewritten, rerr := rewriteSearch(body)
			if rerr != nil {
				log.Printf("rewrite failed, passing through original: %v", rerr)
				rewritten = body
			} else if !bytes.Equal(rewritten, body) {
				log.Printf("rewrote search on %s (len %d -> %d)", r.URL.Path, len(body), len(rewritten))
				if os.Getenv("DUMP_DSL") == "1" {
					log.Printf("DSL_IN %s", body)
					log.Printf("DSL_OUT %s", rewritten)
				}
			}

			r.Body = io.NopCloser(bytes.NewReader(rewritten))
			r.ContentLength = int64(len(rewritten))
			r.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
			h.pass.ServeHTTP(w, r)
			return
		}
	}
	// passthrough
	if r.Body != nil {
		defer r.Body.Close()
	}
	h.pass.ServeHTTP(w, r)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	upstreamStr := getenv("ES_UPSTREAM", "http://1.1.1.1:9200")
	listenAddr = getenv("LISTEN_ADDR", ":9210")
	prefixStr := getenv("POSTS_PATTERN", "posts")
	substringMode := getenv("SUBSTRING_MODE", "wildcard")
	if substringMode != "wildcard" && substringMode != "ngram" {
		log.Fatalf("bad SUBSTRING_MODE %q: must be \"wildcard\" or \"ngram\"", substringMode)
	}
	strat = resolveStrategy(substringMode)
	msm = getenv("MSM", "75%")

	var err error
	upstream, err = url.Parse(upstreamStr)
	if err != nil {
		log.Fatalf("bad ES_UPSTREAM: %v", err)
	}
	for _, p := range strings.Split(prefixStr, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			postsPrefixes = append(postsPrefixes, p)
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		req.Host = upstream.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		log.Printf("upstream error: %v", e)
		http.Error(w, "upstream error: "+e.Error(), http.StatusBadGateway)
	}

	log.Printf("mm-es-substring-proxy listening on %s -> %s (rewrite indexes prefix=%v mode=%s msm=%s)",
		listenAddr, upstreamStr, postsPrefixes, strat.name, msm)
	log.Fatal(http.ListenAndServe(listenAddr, &proxyHandler{pass: proxy}))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getRemoteIP returns the caller's IP. Prefers X-Forwarded-For /
// X-Real-IP if present (in case another proxy sits in front), else
// the TCP peer address with the port stripped.
func getRemoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// take the first hop in the chain
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
