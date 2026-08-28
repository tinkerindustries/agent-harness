package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// fakeStore is the settings surface of *store.Store, backed by a map — the
// same pattern internal/settings and internal/githubauth test through.
type fakeStore struct {
	values map[string]string
}

func (f *fakeStore) Setting(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeStore) SetSetting(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeStore) DeleteSetting(ctx context.Context, key string) error {
	delete(f.values, key)
	return nil
}

// testKey is one RSA key generated per package run. Generation is the
// slowest thing in this file, and every test wants the same thing from it.
var testKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// TestParsePrivateKeyAcceptsWhatAnOperatorCanPaste is the point of the
// forgiving parser: the canonical PEM, the same PEM with its newlines
// flattened to spaces (what a single-line password field does to a paste),
// the same with no newlines at all, and a bare base64 key with no markers
// all have to reach the same key — otherwise the settings screen is a
// credential-shaped trap.
func TestParsePrivateKeyAcceptsWhatAnOperatorCanPaste(t *testing.T) {
	canonical := pkcs1PEM(testKey)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	for name, text := range map[string]string{
		"canonical PEM":       canonical,
		"spaces for newlines": strings.ReplaceAll(canonical, "\n", " "),
		"no newlines at all":  strings.ReplaceAll(canonical, "\n", ""),
		"bare base64 PKCS#1":  base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(testKey)),
		"PKCS#8 PEM":          string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParsePrivateKey(text)
			if err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
			if !got.Equal(testKey) {
				t.Error("parsed a different key than the one encoded")
			}
		})
	}
}

func TestParsePrivateKeyRejectsRubbish(t *testing.T) {
	for name, text := range map[string]string{
		"empty":      "   ",
		"not base64": "-----BEGIN RSA PRIVATE KEY-----\n!!!!\n-----END RSA PRIVATE KEY-----",
		"not a key":  base64.StdEncoding.EncodeToString([]byte("hello there")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePrivateKey(text); err == nil {
				t.Fatal("ParsePrivateKey accepted a value that is not a key")
			}
		})
	}
}

// githubStub stands in for the two App endpoints this package calls,
// counting each so a test can pin what is cached and what is re-fetched.
type githubStub struct {
	installations []Installation
	listCalls     atomic.Int64
	mintCalls     atomic.Int64
	expiresIn     time.Duration
	server        *httptest.Server
}

func newGithubStub(t *testing.T, installations ...Installation) *githubStub {
	t.Helper()
	stub := &githubStub{installations: installations, expiresIn: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		stub.listCalls.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, `{"message":"A JSON Web Token is required"}`, http.StatusUnauthorized)
			return
		}
		rows := make([]map[string]any, 0, len(stub.installations))
		for _, install := range stub.installations {
			rows = append(rows, map[string]any{"id": install.ID, "account": map[string]string{"login": install.Login}})
		}
		json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		stub.mintCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_" + r.PathValue("id"),
			"expires_at": time.Now().Add(stub.expiresIn).UTC().Format(time.RFC3339),
		})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func newProvider(t *testing.T, stub *githubStub, values map[string]string) *Provider {
	t.Helper()
	if values == nil {
		values = map[string]string{
			settings.KeyGitHubAppID:         "12345",
			settings.KeyGitHubAppPrivateKey: pkcs1PEM(testKey),
		}
	}
	p := New(settings.NewResolver(&fakeStore{values: values}))
	p.BaseURL = stub.server.URL
	return p
}

// TestTokenForOwnerRoutesByAccount is the whole reason this package exists:
// two accounts, two installations, and a token minted from the right one
// for each — which is what a fine-grained personal access token could not do.
func TestTokenForOwnerRoutesByAccount(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 11, Login: "mrgeoffrich"}, Installation{ID: 22, Login: "SomeOrg"})
	p := newProvider(t, stub, nil)
	ctx := context.Background()

	personal, err := p.TokenForOwner(ctx, "mrgeoffrich")
	if err != nil {
		t.Fatalf("TokenForOwner(personal): %v", err)
	}
	if personal != "ghs_installation_11" {
		t.Errorf("personal token = %q, want the installation-11 token", personal)
	}
	// The org's login is capitalised on GitHub and lower-cased in a URL;
	// both have to reach the same installation.
	org, err := p.TokenForOwner(ctx, "someorg")
	if err != nil {
		t.Fatalf("TokenForOwner(org): %v", err)
	}
	if org != "ghs_installation_22" {
		t.Errorf("org token = %q, want the installation-22 token", org)
	}
}

// TestTokenForOwnerCachesUntilExpiry pins that a live token is reused —
// git asks for a credential on every network operation, and minting one per
// push would be a round trip to GitHub in front of each.
func TestTokenForOwnerCachesUntilExpiry(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 11, Login: "mrgeoffrich"})
	p := newProvider(t, stub, nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := p.TokenForOwner(ctx, "mrgeoffrich"); err != nil {
			t.Fatalf("TokenForOwner: %v", err)
		}
	}
	if got := stub.mintCalls.Load(); got != 1 {
		t.Errorf("minted %d tokens for three calls, want 1", got)
	}
	if got := stub.listCalls.Load(); got != 1 {
		t.Errorf("listed installations %d times for three calls, want 1", got)
	}
}

// TestTokenForOwnerReMintsAfterExpiry pins the other half: a token inside
// tokenSkew of its stated expiry is spent, and the next caller gets a fresh
// one rather than a credential git will be refused with.
func TestTokenForOwnerReMintsAfterExpiry(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 11, Login: "mrgeoffrich"})
	stub.expiresIn = time.Hour
	p := newProvider(t, stub, nil)
	now := time.Now()
	p.Now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := p.TokenForOwner(ctx, "mrgeoffrich"); err != nil {
		t.Fatalf("TokenForOwner: %v", err)
	}
	// Past the hour, so the cached token is both expired and outside the
	// installations TTL.
	now = now.Add(time.Hour + time.Minute)
	if _, err := p.TokenForOwner(ctx, "mrgeoffrich"); err != nil {
		t.Fatalf("TokenForOwner (after expiry): %v", err)
	}
	if got := stub.mintCalls.Load(); got != 2 {
		t.Errorf("minted %d tokens across an expiry, want 2", got)
	}
}

// TestTokenForOwnerUnknownAccountNamesWhatIsInstalled pins the error an
// operator actually meets: a clone of a repository the App was never
// installed on. The message has to distinguish that from a broken
// credential, because the fix is different — install the App there.
func TestTokenForOwnerUnknownAccountNamesWhatIsInstalled(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 11, Login: "mrgeoffrich"})
	p := newProvider(t, stub, nil)

	_, err := p.TokenForOwner(context.Background(), "someoneelse")
	if err == nil {
		t.Fatal("TokenForOwner accepted an account with no installation")
	}
	if !strings.Contains(err.Error(), "someoneelse") || !strings.Contains(err.Error(), "mrgeoffrich") {
		t.Errorf("error = %q, want it to name both the missing account and the installed one", err)
	}
	// A miss re-reads the installation list once before giving up, so an App
	// installed a moment ago works without waiting out the TTL.
	if got := stub.listCalls.Load(); got != 2 {
		t.Errorf("listed installations %d times on a miss, want 2", got)
	}
}

// TestConfiguredNeedsBothHalves pins the fallback rule: half a credential is
// not an App, so a harness mid-way through being configured keeps using
// github.token rather than failing every git operation.
func TestConfiguredNeedsBothHalves(t *testing.T) {
	stub := newGithubStub(t)
	ctx := context.Background()
	for name, values := range map[string]map[string]string{
		"neither":  {},
		"id only":  {settings.KeyGitHubAppID: "12345"},
		"key only": {settings.KeyGitHubAppPrivateKey: pkcs1PEM(testKey)},
	} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t, stub, values)
			configured, err := p.Configured(ctx)
			if err != nil {
				t.Fatalf("Configured: %v", err)
			}
			if configured {
				t.Error("Configured reported true with half a credential")
			}
		})
	}

	p := newProvider(t, stub, map[string]string{
		settings.KeyGitHubAppID:         "12345",
		settings.KeyGitHubAppPrivateKey: pkcs1PEM(testKey),
	})
	configured, err := p.Configured(ctx)
	if err != nil {
		t.Fatalf("Configured: %v", err)
	}
	if !configured {
		t.Error("Configured reported false with both halves set")
	}
}

// TestJWTCarriesTheAppIDAndVerifies checks the hand-rolled token against the
// public key it was signed with, and that the claims say what GitHub reads:
// the issuer is the App id, and the expiry is inside GitHub's ten-minute
// ceiling.
func TestJWTCarriesTheAppIDAndVerifies(t *testing.T) {
	now := time.Now()
	token, err := signJWT(testKey, "12345", now)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "12345" {
		t.Errorf("iss = %q, want the App id", claims.Iss)
	}
	if claims.Iat >= now.Unix() {
		t.Errorf("iat = %d, want it backdated below %d", claims.Iat, now.Unix())
	}
	if life := time.Duration(claims.Exp-claims.Iat) * time.Second; life > 10*time.Minute {
		t.Errorf("token life = %s, want at most GitHub's 10 minute ceiling", life)
	}
	if err := verifyRS256(token, &testKey.PublicKey); err != nil {
		t.Errorf("the signed token does not verify: %v", err)
	}
}

// verifyRS256 is the test's own check that the signature is what it claims,
// the half of JWT handling the harness never does.
func verifyRS256(token string, pub *rsa.PublicKey) error {
	parts := strings.Split(token, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	signing := parts[0] + "." + parts[1]
	digest := sha256Sum([]byte(signing))
	return rsa.VerifyPKCS1v15(pub, cryptoSHA256, digest, signature)
}

func TestOwnersAreLowercasedAndSorted(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 22, Login: "SomeOrg"}, Installation{ID: 11, Login: "mrgeoffrich"})
	p := newProvider(t, stub, nil)

	owners, err := p.Owners(context.Background())
	if err != nil {
		t.Fatalf("Owners: %v", err)
	}
	want := fmt.Sprint([]string{"mrgeoffrich", "someorg"})
	if fmt.Sprint(owners) != want {
		t.Errorf("Owners = %v, want %s", owners, want)
	}
}

// TestUnauthorizedNamesTheSettingsToFix pins the message a wrong App id or a
// key from a different App produces: an operator has to be told which two
// settings are implicated.
func TestUnauthorizedNamesTheSettingsToFix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()
	p := New(settings.NewResolver(&fakeStore{values: map[string]string{
		settings.KeyGitHubAppID:         "12345",
		settings.KeyGitHubAppPrivateKey: pkcs1PEM(testKey),
	}}))
	p.BaseURL = server.URL

	_, err := p.TokenForOwner(context.Background(), "mrgeoffrich")
	if err == nil {
		t.Fatal("TokenForOwner succeeded against a 401")
	}
	if !strings.Contains(err.Error(), settings.KeyGitHubAppID) || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error = %q, want GitHub's own message and the setting to fix", err)
	}
}

// TestNonNumericAppIDIsRefusedBeforeGitHubSeesIt pins the mistake worth
// catching locally: pasting the App's *client* id (Iv1....) or its slug into
// github.app_id, which GitHub would answer with an opaque 401.
func TestNonNumericAppIDIsRefusedBeforeGitHubSeesIt(t *testing.T) {
	stub := newGithubStub(t, Installation{ID: 11, Login: "mrgeoffrich"})
	p := newProvider(t, stub, map[string]string{
		settings.KeyGitHubAppID:         "Iv1.0123456789abcdef",
		settings.KeyGitHubAppPrivateKey: pkcs1PEM(testKey),
	})

	_, err := p.TokenForOwner(context.Background(), "mrgeoffrich")
	if err == nil {
		t.Fatal("TokenForOwner accepted a non-numeric App id")
	}
	if !strings.Contains(err.Error(), "numeric App id") {
		t.Errorf("error = %q, want it to name the numeric App id", err)
	}
	if stub.listCalls.Load() != 0 {
		t.Error("a non-numeric App id reached GitHub; it should be refused locally")
	}
}
