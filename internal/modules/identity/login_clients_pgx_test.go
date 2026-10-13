//go:build pgx

// Seeded login clients isolate runtime fixture credentials from HTTP call sites.
// Dependencies: existing request/challenge fixtures, stdlib and Argon2; callers:
// identity PG tests. One per-run secret feeds both seed hashes and request bodies;
// prepared clients acquire CSRF before concurrency barriers, never alter assertions.
package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"golang.org/x/crypto/argon2"
)

const (
	challengeLoginIP    = "192.0.2.1"
	sessionLoginIP      = "192.0.2.20"
	knownSessionLoginIP = "192.0.2.21"
	repairInitialIP     = "198.51.100.10"
	repairOtherIP       = "198.51.100.11"
	repairStaleIP       = "198.51.100.12"
	repairFreshIP       = "198.51.100.13"
	deviceInitialIP     = "192.0.2.50"
	deviceRecoveryIP    = "192.0.2.56"
	deviceOtherIP       = "192.0.2.57"
	rotationInitialIP   = "203.0.113.40"
	concurrentFailureIP = "203.0.113.30"
	rotationFirstIP     = "203.0.113.41"
	rotationSecondIP    = "203.0.113.42"
)

var failedLoginIPs = [...]string{"192.0.2.10", "192.0.2.11"}
var expiryLoginIPs = [...]string{repairInitialIP, repairOtherIP, repairStaleIP}
var deviceFailureIPs = [...]string{"192.0.2.51", "192.0.2.52", "192.0.2.53", "192.0.2.54", "192.0.2.55"}
var reconnectIPs = [...]string{"198.51.100.20", "198.51.100.21"}
var ipv6LoginIPs = [...]string{"2001:db8:55:66::1", "2001:db8:55:66::abcd"}

var seededLoginCredential = func() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "t-" + hex.EncodeToString(b)
}()

func seededLoginPHC() string {
	salt := []byte("0123456789abcdef")
	hash := argon2.IDKey([]byte(seededLoginCredential), salt, 2, 19456, 1, 32)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash)
}

type seededLoginClient struct {
	router         *platform.Router
	body, csrf, ip string
	cookies        []*http.Cookie
}

func prepareSeededLogin(t *testing.T, r *platform.Router, email, ip string, invalid bool, extra ...*http.Cookie) seededLoginClient {
	t.Helper()
	c, csrf := challengePG(t, r)
	credential := seededLoginCredential
	if invalid {
		credential += "-invalid"
	}
	dto := struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}{email, credential}
	body, err := json.Marshal(dto)
	if err != nil {
		t.Fatal("fixture login encoding failed")
	}
	return seededLoginClient{r, string(body), csrf, ip, append(extra, c)}
}
func (c seededLoginClient) send() *httptest.ResponseRecorder {
	return requestPG(c.router, "POST", "/api/v1/auth/login", c.body, c.csrf, c.ip, c.cookies...)
}
func loginAsSeeded(t *testing.T, r *platform.Router, email, ip string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return prepareSeededLogin(t, r, email, ip, false, extra...).send()
}
func loginWithInvalidSeededCredential(t *testing.T, r *platform.Router, email, ip string, extra ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return prepareSeededLogin(t, r, email, ip, true, extra...).send()
}

// Test bodies may select the seeded or invalid client, but never construct a
// credential/IP authentication tuple. Parse source so formatting cannot evade it.
func seededLoginCallsiteViolations(tree *ast.File) []string {
	var problems []string
	ast.Inspect(tree, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "testPassword" || id.Name == "loginPG" || id.Name == "password") {
			problems = append(problems, "credential escaped seeded client: "+id.Name)
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "requestPG" || len(call.Args) < 3 {
			return true
		}
		lit, ok := call.Args[2].(*ast.BasicLit)
		if !ok {
			return true
		}
		route, err := strconv.Unquote(lit.Value)
		if err == nil && route == "/api/v1/auth/login" {
			verb, ok := call.Args[1].(*ast.BasicLit)
			if ok {
				method, _ := strconv.Unquote(verb.Value)
				if method == "POST" {
					problems = append(problems, "credential request bypasses seeded client")
				}
			}
		}
		return true
	})
	return problems
}
func TestSeededLoginCallsitesKeepCredentialsInClient(t *testing.T) {
	for _, path := range []string{"login_pgx_test.go", "login_repair_pgx_test.go"} {
		tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range seededLoginCallsiteViolations(tree) {
			t.Errorf("%s: %s", path, problem)
		}
	}
	// Negative controls model legacy identifiers without storing a credential tuple.
	for _, name := range []string{"testPassword", "loginPG", "password"} {
		tree, err := parser.ParseFile(token.NewFileSet(), "negative", "package fixture; func probe(){"+name+"()}", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(seededLoginCallsiteViolations(tree)) == 0 {
			t.Errorf("legacy credential shape accepted: %s", name)
		}
	}
}

// The fake psql executes this test binary and checks only booleans: credentials
// remain in child environments, with no diagnostics or value capture on disk.
func TestFixturePSQLChildEnvironment(t *testing.T) {
	expected := os.Getenv("OSC_FIXTURE_PSQL_EXPECTED")
	if expected == "" {
		return
	}
	var want map[string]string
	if json.Unmarshal([]byte(expected), &want) != nil {
		t.Fatal("invalid fixture expectations")
	}
	for key, value := range want {
		if os.Getenv(key) != value {
			t.Errorf("native field mismatch: %s", key)
		}
	}
	for _, key := range []string{"PGSERVICE", "PGPASSFILE", "PGHOSTADDR", "PGOPTIONS"} {
		if _, exists := os.LookupEnv(key); exists {
			t.Errorf("ambient field survived: %s", key)
		}
	}
}
func TestFixturePSQLNativeEnvironment(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("fixture executable unavailable")
	}
	shim := "#!/bin/sh\nexec \"$OSC_FIXTURE_TEST_BINARY\" -test.run '^TestFixturePSQLChildEnvironment$'\n"
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OSC_FIXTURE_TEST_BINARY", binary)
	for _, key := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE", "PGSERVICE", "PGPASSFILE", "PGHOSTADDR", "PGOPTIONS"} {
		t.Setenv(key, "ambient-stale")
	}
	role := "fixture_role"
	secret := seededLoginCredential
	u := url.URL{Scheme: "postgresql", Host: "[2001:db8::123]:6543", Path: "/fixture db", RawQuery: "sslmode=disable&connect_timeout=7"}
	u.User = url.UserPassword(role, secret)
	fields := map[string]string{
		"DATABASE_URL":      u.String(),
		"PGHOST":            u.Hostname(),
		"PGPORT":            u.Port(),
		"PGUSER":            role,
		"PGPASSWORD":        secret,
		"PGDATABASE":        "fixture db",
		"PGSSLMODE":         "disable",
		"PGCONNECT_TIMEOUT": "7",
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal("fixture expectations unavailable")
	}
	t.Setenv("OSC_FIXTURE_PSQL_EXPECTED", string(encoded))
	if fixturePSQL(context.Background(), u.String(), "SELECT 1;") != nil {
		t.Fatal("native psql environment contract failed")
	}
}
