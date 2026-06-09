package authtok

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  s3cret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if tok != "s3cret-token" {
		t.Errorf("token = %q", tok)
	}
}

func TestLoadTokenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(path); err == nil {
		t.Error("expected error for empty token file")
	}
}

func TestLoadTokenMissing(t *testing.T) {
	if _, err := LoadToken(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestMintToken(t *testing.T) {
	a, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("token length = %d, want 64 hex chars", len(a))
	}
	b, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if a == b {
		t.Error("two minted tokens are equal")
	}
}

func TestMiddleware(t *testing.T) {
	const token = "good-token"
	handler := Middleware(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid", "Bearer good-token", http.StatusOK},
		{"missing", "", http.StatusUnauthorized},
		{"wrong token", "Bearer bad-token", http.StatusUnauthorized},
		{"wrong scheme", "Basic good-token", http.StatusUnauthorized},
		{"prefix only", "Bearer good", http.StatusUnauthorized},
		{"longer", "Bearer good-token-and-more", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("401 content type = %q", ct)
				}
				if !strings.Contains(rec.Body.String(), "UNAUTHORIZED") {
					t.Errorf("401 body = %q", rec.Body.String())
				}
			}
		})
	}
}

func verifyRequest(t *testing.T, secret, signedURL string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, signedURL, nil)
	return VerifySignedQuery(secret, req)
}

func TestSignVerifyRoundTrip(t *testing.T) {
	const secret = "hmac-secret"
	for _, path := range []string{
		"/v1/media?path=%2Fmnt%2Fmedia%2Fmovie.mkv",
		"/v1/media?path=%2Fmnt%2Fa+b%2Fsub.srt&extra=1",
		"/v1/codecs/build-x/files/libx264.so",
	} {
		signed := SignURL(secret, path, time.Now().Add(time.Minute))
		if err := verifyRequest(t, secret, signed); err != nil {
			t.Errorf("verify(%q -> %q): %v", path, signed, err)
		}
	}
}

func TestVerifyExpired(t *testing.T) {
	const secret = "hmac-secret"
	signed := SignURL(secret, "/v1/media?path=%2Fa", time.Now().Add(-time.Second))
	if err := verifyRequest(t, secret, signed); err == nil {
		t.Error("expected error for expired URL")
	}
}

func TestVerifyTampered(t *testing.T) {
	const secret = "hmac-secret"
	signed := SignURL(secret, "/v1/media?path=%2Fmnt%2Fok.mkv", time.Now().Add(time.Minute))

	// Tamper with the signed media path: must be rejected.
	tampered := strings.Replace(signed, "ok.mkv", "etc%2Fshadow", 1)
	if err := verifyRequest(t, secret, tampered); err == nil {
		t.Error("expected error for tampered path query param")
	}

	// Tamper with the expiry: must be rejected.
	tampered = strings.Replace(signed, "exp=", "exp=9", 1)
	if err := verifyRequest(t, secret, tampered); err == nil {
		t.Error("expected error for tampered expiry")
	}

	// Wrong secret: must be rejected.
	if err := verifyRequest(t, "other-secret", signed); err == nil {
		t.Error("expected error for wrong secret")
	}
}

func TestVerifyMissingParams(t *testing.T) {
	if err := verifyRequest(t, "s", "/v1/media?path=%2Fa"); err == nil {
		t.Error("expected error for missing exp/sig")
	}
	if err := verifyRequest(t, "s", "/v1/media?path=%2Fa&exp=99999999999&sig=zz"); err == nil {
		t.Error("expected error for non-hex sig")
	}
}
