// Package authtok implements PRT's auth primitives: the shared bearer token
// (LAN-side daemon auth), per-session push tokens, and HMAC-signed media
// URLs for the masterd file server.
package authtok

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// LoadToken reads a token file and trims surrounding whitespace (trailing
// newlines are the norm for sops-managed secrets). An empty token is an
// error: it would make Middleware accept "Authorization: Bearer ".
func LoadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("authtok: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("authtok: token file %s is empty", path)
	}
	return token, nil
}

// MintToken returns a fresh random token: 32 bytes of crypto/rand, hex
// encoded (64 chars). Used by masterd for per-session push tokens.
func MintToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("authtok: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Middleware wraps next with bearer-token auth: the request must carry
// "Authorization: Bearer <token>" or it is answered with a 401 JSON
// ErrorBody. The comparison is constant-time.
func Middleware(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			// Hashing first makes the compare constant-time regardless of
			// attacker-controlled length.
			gotSum := sha256.Sum256([]byte(got))
			ok = subtle.ConstantTimeCompare(gotSum[:], want[:]) == 1
		}
		if !ok {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	// Deliberately no WWW-Authenticate header: some HTTP clients pop a
	// native Basic-auth dialog on it.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(protocol.ErrorBody{
		Error:   "UNAUTHORIZED",
		Message: "missing or invalid bearer token",
	})
}

// Signed-URL scheme (media file server)
//
// The signature is HMAC-SHA256(secret, canonical + "|" + exp) where exp is
// the unix expiry in seconds and canonical is the request path plus its
// query string EXCLUDING exp and sig, with the remaining query keys in
// url.Values.Encode order (sorted): "/p" or "/p?k=v&...". Covering the
// query binds the signature to ?path=... on /v1/media. The signature and
// expiry travel as the query params "sig" and "exp" (hex / decimal).

// SignURL signs path (which may already carry a query string, e.g.
// "/v1/media?path=%2Fmnt%2Fa.mkv") and returns it with "exp" and "sig"
// query params appended.
func SignURL(secret, path string, expiry time.Time) string {
	rawPath, rawQuery, _ := strings.Cut(path, "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Unparseable query: sign it anyway; verification will fail the same
		// way, never succeed spuriously.
		q = url.Values{}
	}
	exp := strconv.FormatInt(expiry.Unix(), 10)
	sig := computeSig(secret, canonical(rawPath, q), exp)
	q.Set("exp", exp)
	q.Set("sig", sig)
	return rawPath + "?" + q.Encode()
}

// VerifySignedQuery checks the "exp" and "sig" query params of r against
// secret: the signature must match (constant-time) and the expiry must be in
// the future. A nil return means the URL is authentic and fresh.
func VerifySignedQuery(secret string, r *http.Request) error {
	q := r.URL.Query()
	exp := q.Get("exp")
	sigHex := q.Get("sig")
	if exp == "" || sigHex == "" {
		return errors.New("authtok: missing exp or sig query param")
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return fmt.Errorf("authtok: bad exp: %w", err)
	}
	want, err := hex.DecodeString(computeSig(secret, canonical(r.URL.Path, q), exp))
	if err != nil {
		return fmt.Errorf("authtok: %w", err) // unreachable: we encoded it
	}
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return errors.New("authtok: sig is not valid hex")
	}
	if !hmac.Equal(got, want) {
		return errors.New("authtok: signature mismatch")
	}
	// Expiry is checked AFTER the signature so an attacker cannot probe
	// expiry handling with forged values.
	if time.Now().Unix() > expUnix {
		return errors.New("authtok: signed URL expired")
	}
	return nil
}

// canonical builds the signed string for a path and query, excluding the
// exp/sig params themselves. q is not mutated.
func canonical(path string, q url.Values) string {
	rest := url.Values{}
	for k, vs := range q {
		if k == "exp" || k == "sig" {
			continue
		}
		rest[k] = vs
	}
	if len(rest) == 0 {
		return path
	}
	return path + "?" + rest.Encode()
}

func computeSig(secret, canonical, exp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	mac.Write([]byte("|"))
	mac.Write([]byte(exp))
	return hex.EncodeToString(mac.Sum(nil))
}
