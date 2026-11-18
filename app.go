// app.go — the HTTP routing and auth layer for the four surfaces (git smart HTTP,
// REST v3, GraphQL v4, MCP), written against a small request/response abstraction
// so the transport (hand-rolled raw TCP vs net/http) can vary without touching
// routing. All domain work lives in forge.go / gitproto.go / rest.go / gql.go / mcp.go.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"
const maxBody = 128 * 1024 * 1024

type httpRequest struct {
	method  string
	path    string // percent-encoded pathname, as Node's url.pathname
	query   url.Values
	headers map[string]string // lower-cased keys
	body    []byte
	host    string
}

type httpResponse struct {
	status  int
	headers map[string]string
	body    []byte
}

func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func jsonResponse(status int, v any, headers map[string]string) httpResponse {
	if status == 204 || v == nil {
		return httpResponse{status: status, headers: headers}
	}
	h := map[string]string{"content-type": jsonType}
	for k, val := range headers {
		h[k] = val
	}
	return httpResponse{status: status, headers: h, body: marshalJSON(v)}
}

func bufferResponse(status int, body []byte, contentType string) httpResponse {
	return httpResponse{status: status, headers: map[string]string{"content-type": contentType}, body: body}
}

var requestSeq int64

func nextRequestID() string {
	n := atomic.AddInt64(&requestSeq, 1)
	return fmt.Sprintf("swift-%s-%s", base36(n), base36(int64(n*7919%0xffffff)))
}

func base36(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789abcdefghijklmnopqrstuvwxyz"
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%36]}, out...)
		n /= 36
	}
	return string(out)
}

var bearerRe = regexp.MustCompile(`(?i)^Bearer (.+)$`)

// The bearer token of a request, or "" when no Authorization header rides it.
func bearerOf(req *httpRequest) string {
	header := req.headers["authorization"]
	if header == "" {
		return ""
	}
	if m := bearerRe.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return header
}

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

type app struct {
	forge         *Forge
	expectedToken string
}

func newApp(expectedToken string) *app {
	return &app{forge: NewForge(), expectedToken: expectedToken}
}

func (a *app) handle(req *httpRequest) httpResponse {
	path := req.path
	method := req.method
	id := nextRequestID()

	if method == "GET" && path == "/healthz" {
		return jsonResponse(200, map[string]any{"ok": true}, nil)
	}

	token := bearerOf(req)
	authorized := token != "" && (a.expectedToken == "" || token == a.expectedToken)

	// ---- git smart HTTP -------------------------------------------------------
	if m := gitPathRe.FindStringSubmatch(path); m != nil {
		if !authorized {
			return jsonResponse(401, map[string]any{"message": "Bad credentials"}, nil)
		}
		owner, _ := url.PathUnescape(m[1])
		repoName, _ := url.PathUnescape(m[2])
		surface := m[3]
		var repo *RepoRec
		if owner == ViewerLogin {
			a.forge.mu.RLock()
			repo = a.forge.GetRepo(owner, repoName)
			a.forge.mu.RUnlock()
		}
		if repo == nil {
			return jsonResponse(404, map[string]any{"message": "Not Found"}, nil)
		}
		if surface == "info/refs" {
			service := req.query.Get("service")
			serviceName := "git-upload-pack"
			if service == "git-receive-pack" {
				serviceName = "git-receive-pack"
			}
			version2 := strings.Contains(req.headers["git-protocol"], "version=2")
			a.forge.mu.RLock()
			body := advertisement(a.forge, repo, serviceName, version2 && serviceName == "git-upload-pack")
			a.forge.mu.RUnlock()
			return bufferResponse(200, body, "application/x-"+serviceName+"-advertisement")
		}
		if surface == "git-upload-pack" {
			version2 := strings.Contains(req.headers["git-protocol"], "version=2")
			a.forge.mu.RLock()
			var out []byte
			var err error
			if version2 {
				out, err = uploadPackV2(a.forge, repo, req.body)
			} else {
				out, err = uploadPackV0(a.forge, repo, req.body)
			}
			a.forge.mu.RUnlock()
			if err != nil {
				return jsonResponse(400, map[string]any{"message": "Bad Request"}, nil)
			}
			return bufferResponse(200, out, "application/x-git-upload-pack-result")
		}
		a.forge.mu.Lock()
		out, err := receivePack(a.forge, repo, req.body)
		a.forge.mu.Unlock()
		if err != nil {
			return jsonResponse(400, map[string]any{"message": "Bad Request"}, nil)
		}
		return bufferResponse(200, out, "application/x-git-receive-pack-result")
	}

	// ---- everything below needs the trial token --------------------------------
	if !authorized {
		return jsonResponse(401, map[string]any{"message": "Bad credentials"}, map[string]string{"x-github-request-id": id})
	}

	// ---- REST v3 -----------------------------------------------------------------
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		var body map[string]any
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			if len(req.body) > 0 {
				body = parseObjectBody(req.body)
				if body == nil {
					return jsonResponse(400, map[string]any{"message": "Invalid JSON"}, map[string]string{"x-github-request-id": id})
				}
			}
		}
		rawSegments := strings.Split(strings.TrimPrefix(path, "/api/v3"), "/")
		var segments []string
		for _, segment := range rawSegments {
			if segment == "" {
				continue
			}
			decoded, err := url.PathUnescape(segment)
			if err != nil {
				decoded = segment
			}
			segments = append(segments, decoded)
		}
		if segments == nil {
			segments = []string{}
		}
		host := req.host
		if host == "" {
			host = "entry"
		}
		ctx := &restCtx{
			forge: a.forge, method: method, segments: segments, query: req.query,
			body: body, origin: "http://" + host, path: path,
		}
		if method == "GET" {
			a.forge.mu.RLock()
		} else {
			a.forge.mu.Lock()
		}
		result := routeRest(ctx)
		if method == "GET" {
			a.forge.mu.RUnlock()
		} else {
			a.forge.mu.Unlock()
		}
		headers := map[string]string{"x-github-request-id": id}
		for k, v := range result.headers {
			headers[k] = v
		}
		if result.status == 204 || result.body == nil {
			return httpResponse{status: result.status, headers: headers}
		}
		h := map[string]string{"content-type": jsonType}
		for k, v := range headers {
			h[k] = v
		}
		return httpResponse{status: result.status, headers: h, body: result.body}
	}

	// ---- GraphQL v4 ----------------------------------------------------------------
	if path == "/api/graphql" && method == "POST" {
		var parsed any
		if err := json.Unmarshal(req.body, &parsed); err != nil {
			return jsonResponse(400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON"}}}, map[string]string{"x-github-request-id": id})
		}
		a.forge.mu.Lock()
		defer a.forge.mu.Unlock()
		if batch, ok := parsed.([]any); ok {
			results := make([]any, 0, len(batch))
			for _, entry := range batch {
				results = append(results, executeGraphQL(a.forge, toGqlRequest(entry)))
			}
			return jsonResponse(200, results, map[string]string{"x-github-request-id": id})
		}
		result := executeGraphQL(a.forge, toGqlRequest(parsed))
		return jsonResponse(200, result, map[string]string{"x-github-request-id": id})
	}

	// ---- MCP ------------------------------------------------------------------------
	if path == "/mcp" {
		if method != "POST" {
			return jsonResponse(405, map[string]any{"message": "Method Not Allowed"}, nil)
		}
		var parsed any
		if err := json.Unmarshal(req.body, &parsed); err != nil {
			reply := mcpParseError()
			return jsonResponse(reply.status, reply.body, nil)
		}
		a.forge.mu.Lock()
		defer a.forge.mu.Unlock()
		if batch, ok := parsed.([]any); ok {
			var replies []any
			for _, entry := range batch {
				obj, _ := entry.(map[string]any)
				if obj == nil {
					obj = map[string]any{}
				}
				if reply := handleMcp(a.forge, obj); reply != nil {
					replies = append(replies, reply.body)
				}
			}
			if len(replies) == 0 {
				return httpResponse{status: 202}
			}
			return jsonResponse(200, replies, nil)
		}
		obj, _ := parsed.(map[string]any)
		if obj == nil {
			obj = map[string]any{}
		}
		reply := handleMcp(a.forge, obj)
		if reply == nil {
			return httpResponse{status: 202}
		}
		return jsonResponse(reply.status, reply.body, nil)
	}

	return jsonResponse(404, map[string]any{"message": "Not Found"}, map[string]string{"x-github-request-id": id})
}

func toGqlRequest(entry any) gqlRequest {
	obj, _ := entry.(map[string]any)
	if obj == nil {
		return gqlRequest{}
	}
	var req gqlRequest
	req.Query, _ = obj["query"].(string)
	req.OperationName, _ = obj["operationName"].(string)
	if vars, ok := obj["variables"].(map[string]any); ok {
		req.Variables = vars
	}
	return req
}
