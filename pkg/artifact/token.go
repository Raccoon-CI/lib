package artifact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Scope says which run and job a token was issued for. IDs are opaque to the
// toolkit, which only reads them back out of the token.
type Scope struct {
	Run, Job string
}

// Signer issues and checks the ACTIONS_RUNTIME_TOKEN a job's steps carry, and
// the signatures on blob upload URLs. The token is a JWT shaped like
// GitHub's, because @actions/artifact decodes it to learn its run and job:
// the scp claim holds "Actions.Results:<run>:<job>".
type Signer struct {
	key []byte
	now func() time.Time
}

func NewSigner(key []byte) (*Signer, error) {
	if len(key) < 16 {
		return nil, errors.New("artifact: signing key must be at least 16 bytes")
	}
	return &Signer{key: key, now: time.Now}, nil
}

var b64 = base64.RawURLEncoding

type claims struct {
	Scp string `json:"scp"`
	Iss string `json:"iss"`
	Nbf int64  `json:"nbf"`
	Exp int64  `json:"exp"`
}

const issuer = "raccoonci"

func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (s *Signer) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(strings.Join(parts, "\n")))
	return m.Sum(nil)
}

// Token returns a token for sc that works for ttl.
func (s *Signer) Token(sc Scope, ttl time.Duration) (string, error) {
	if !validID(sc.Run) || !validID(sc.Job) {
		return "", fmt.Errorf("artifact: run and job ids may only hold letters, digits, '-', '_' and '.': %q, %q", sc.Run, sc.Job)
	}
	now := s.now()
	c, _ := json.Marshal(claims{Scp: "Actions.Results:" + sc.Run + ":" + sc.Job, Iss: issuer, Nbf: now.Unix(), Exp: now.Add(ttl).Unix()})
	in := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64.EncodeToString(c)
	return in + "." + b64.EncodeToString(s.mac(in)), nil
}

var errToken = errors.New("invalid or expired token")

// Verify checks a token's signature and validity period and returns its scope.
func (s *Signer) Verify(tok string) (Scope, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Scope{}, errToken
	}
	hdr, err := b64.DecodeString(parts[0])
	if err != nil {
		return Scope{}, errToken
	}
	var h struct{ Alg string }
	if json.Unmarshal(hdr, &h) != nil || h.Alg != "HS256" {
		return Scope{}, errToken
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac(parts[0]+"."+parts[1])) {
		return Scope{}, errToken
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return Scope{}, errToken
	}
	var c claims
	if json.Unmarshal(raw, &c) != nil || c.Iss != issuer {
		return Scope{}, errToken
	}
	now := s.now().Unix()
	if now < c.Nbf || now >= c.Exp {
		return Scope{}, errToken
	}
	f := strings.Split(c.Scp, ":")
	if len(f) != 3 || f[0] != "Actions.Results" || !validID(f[1]) || !validID(f[2]) {
		return Scope{}, errToken
	}
	return Scope{Run: f[1], Job: f[2]}, nil
}

// SignUpload signs a blob upload URL for the upload id within realm. The
// "upload" label keeps these signatures apart from token signatures, and the
// realm (a run id here; "~cache" for the cache, which no run id can equal)
// keeps one service's URLs from working on another.
func (s *Signer) SignUpload(realm, id string, exp int64) string {
	return b64.EncodeToString(s.mac("upload", realm, id, fmt.Sprint(exp)))
}

// VerifyUpload checks a signature made by SignUpload and that it has not expired.
func (s *Signer) VerifyUpload(realm, id string, exp int64, sig string) bool {
	return s.now().Unix() <= exp && hmac.Equal([]byte(sig), []byte(s.SignUpload(realm, id, exp)))
}

// FromRequest verifies the bearer token of r and returns its scope.
func (s *Signer) FromRequest(r *http.Request) (Scope, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return Scope{}, errors.New("missing bearer token")
	}
	return s.Verify(tok)
}
