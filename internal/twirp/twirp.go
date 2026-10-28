// Package twirp holds the small pieces of the Twirp JSON protocol that the
// artifact and cache services share: errors, replies, and reading a request
// whose field names may be spelled either way.
package twirp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Error is a Twirp error. Its status follows from its code.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

var statuses = map[string]int{
	"invalid_argument": 400, "unauthenticated": 401, "permission_denied": 403,
	"not_found": 404, "bad_route": 404, "already_exists": 409,
	"failed_precondition": 412, "resource_exhausted": 429, "internal": 500,
}

// Errorf makes an error with one of Twirp's codes ("not_found", ...).
func Errorf(code, format string, a ...any) *Error {
	status, ok := statuses[code]
	if !ok {
		status = http.StatusInternalServerError
	}
	return &Error{status, code, fmt.Sprintf(format, a...)}
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// WriteError answers the way the toolkit reads errors: {"code", "msg"}.
func WriteError(w http.ResponseWriter, e *Error) {
	WriteJSON(w, e.Status, map[string]string{"code": e.Code, "msg": e.Msg})
}

// Request is a decoded request body. Clients may spell field names in
// snake_case or camelCase, wrapper types (StringValue, Int64Value) travel as
// plain values, and 64-bit integers as strings.
type Request struct{ m map[string]any }

// ReadRequest checks that the body is JSON and decodes it.
func ReadRequest(r *http.Request) (Request, *Error) {
	if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
		return Request{}, Errorf("invalid_argument", "only application/json is supported")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return Request{}, Errorf("invalid_argument", "reading the request: %v", err)
	}
	var q Request
	if err := json.Unmarshal(body, &q.m); err != nil {
		return Request{}, Errorf("invalid_argument", "malformed JSON: %v", err)
	}
	return q, nil
}

func (q Request) get(names ...string) any {
	for _, n := range names {
		if v, ok := q.m[n]; ok {
			return v
		}
	}
	return nil
}

// Str reads a string field under any of the names.
func (q Request) Str(names ...string) string {
	switch v := q.get(names...).(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// Int reads an integer field; ok is false if it is missing or not an integer.
func (q Request) Int(names ...string) (n int64, ok bool) {
	switch v := q.get(names...).(type) {
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	case float64:
		return int64(v), v == float64(int64(v))
	}
	return 0, false
}

// Strings reads a repeated string field.
func (q Request) Strings(names ...string) []string {
	list, _ := q.get(names...).([]any)
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
