package studio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Svc: &Service{BaseDir: t.TempDir()}, Store: st, Runtime: &Runtime{}, Token: "secret-token", Loopback: true}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, target, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Host = "127.0.0.1:8090"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(r *http.Request) { r.Header.Set("Authorization", "Bearer secret-token") }

func TestAuth(t *testing.T) {
	h := testServer(t)

	if w := do(h, "GET", "/healthz", "", nil); w.Code != 200 {
		t.Fatalf("healthz needs no token: %d", w.Code)
	}
	w := do(h, "GET", "/api/studio/designs", "", nil)
	if w.Code != 401 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("API without token: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := do(h, "GET", "/", "", nil); w.Code != 401 || !strings.Contains(w.Body.String(), "token") {
		t.Fatalf("page without token: %d", w.Code)
	}
	if w := do(h, "GET", "/?token=wrong", "", nil); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := do(h, "GET", "/pipeline/api/status", "", nil); w.Code != 401 {
		t.Fatalf("pipeline API without token: %d", w.Code)
	}

	// The one-time link sets a cookie and redirects to a URL without the token.
	w = do(h, "GET", "/?token=secret-token&tab=design", "", nil)
	if w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), "token") || !strings.Contains(w.Header().Get("Location"), "tab=design") {
		t.Fatalf("token link: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", cookies)
	}
	withCookie := func(r *http.Request) { r.AddCookie(cookies[0]) }
	if w := do(h, "GET", "/api/studio/designs", "", withCookie); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("cookie auth: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/", "", withCookie); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("UI with cookie: %d", w.Code)
	}
	if w := do(h, "GET", "/design/stone", "", withCookie); w.Code != 200 || !strings.Contains(w.Body.String(), "<") {
		t.Fatalf("client routes fall back to index.html: %d", w.Code)
	}
	if w := do(h, "GET", "/api/studio/runtime", "", bearer); w.Code != 200 || !strings.Contains(w.Body.String(), `"stopped"`) {
		t.Fatalf("bearer auth: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/pipeline/", "", bearer); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pipeline without a deployment: %d", w.Code)
	}
}

func TestSignInForm(t *testing.T) {
	h := testServer(t)
	post := func(body string, mod func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/signin", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Host = "127.0.0.1:8090"
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// A browser without the cookie gets a page with a form to paste the token.
	w := do(h, "GET", "/", "", nil)
	csp := w.Header().Get("Content-Security-Policy")
	nonce := csp[strings.Index(csp, "'nonce-")+7:]
	nonce = nonce[:strings.Index(nonce, "'")]
	if w.Code != 401 || !strings.Contains(w.Body.String(), `action="/signin"`) || !strings.Contains(w.Body.String(), `nonce="`+nonce+`"`) {
		t.Fatalf("sign-in page: %d nonce %q\n%s", w.Code, nonce, w.Body.String())
	}
	if w := do(h, "GET", "/?token=stale", "", nil); w.Code != 401 || !strings.Contains(w.Body.String(), "not valid") {
		t.Fatalf("a stale link explains itself: %d", w.Code)
	}

	if w := post("token=wrong&next=%23%2Fdesign%2Fstone", nil); w.Code != 401 || !strings.Contains(w.Body.String(), "not right") || len(w.Result().Cookies()) != 0 ||
		!strings.Contains(w.Body.String(), `value="#/design/stone"`) {
		t.Fatalf("wrong token: %d %v (the page keeps where to return)\n%s", w.Code, w.Result().Cookies(), w.Body.String())
	}
	w = post("token=+secret-token%0A&next=%23%2Fdesign%2Fstone", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/#/design/stone" {
		t.Fatalf("pasted token (with stray whitespace) signs in and returns to the page: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", c)
	}
	if w := do(h, "GET", "/api/studio/designs", "", func(r *http.Request) { r.AddCookie(c[0]) }); w.Code != 200 {
		t.Fatalf("the cookie works: %d", w.Code)
	}
	for _, next := range []string{"https%3A%2F%2Fevil.example", "%2F%2Fevil.example", "%23%2Fx%0D%0ASet-Cookie%3A+a%3Db"} {
		if w := post("token=secret-token&next="+next, nil); w.Header().Get("Location") != "/" {
			t.Fatalf("next=%s must not redirect elsewhere: %q", next, w.Header().Get("Location"))
		}
	}
	if w := post("token=secret-token", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site sign-in posts are refused: %d", w.Code)
	}
}

func TestBrowserProtections(t *testing.T) {
	h := testServer(t)
	// DNS rebinding: a page on evil.example resolving to 127.0.0.1 sends its own Host.
	if w := do(h, "GET", "/api/studio/designs", "", func(r *http.Request) { bearer(r); r.Host = "evil.example:8090" }); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host header: %d", w.Code)
	}
	// Cross-site writes are refused even with the cookie (browsers send Sec-Fetch-Site).
	body := `{"version":1,"name":"x","root":{"table":"dbo.t","key":"id","fields":[]},"related":[],"output":{"include_json":false}}`
	w := do(h, "PUT", "/api/studio/designs/x", body, func(r *http.Request) { bearer(r); r.Header.Set("Sec-Fetch-Site", "cross-site") })
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site write: %d", w.Code)
	}
	if w := do(h, "PUT", "/api/studio/designs/x", body, func(r *http.Request) { bearer(r); r.Header.Set("Sec-Fetch-Site", "same-origin") }); w.Code != 200 {
		t.Fatalf("same-origin write: %d %s", w.Code, w.Body.String())
	}
}

func TestDesignAPI(t *testing.T) {
	h := testServer(t)
	body, _ := json.Marshal(exampleDesign())
	if w := do(h, "PUT", "/api/studio/designs/other", string(body), bearer); w.Code != 400 {
		t.Fatalf("name must match the URL: %d", w.Code)
	}
	if w := do(h, "PUT", "/api/studio/designs/stone", string(body), bearer); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	w := do(h, "GET", "/api/studio/designs/stone", "", bearer)
	var got Design
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Related) != 4 {
		t.Fatalf("load: %d %v %+v", w.Code, err, got)
	}
	if w := do(h, "GET", "/api/studio/designs/missing", "", bearer); w.Code != 404 {
		t.Fatalf("missing design: %d", w.Code)
	}
	if w := do(h, "PUT", "/api/studio/designs/stone", "{not json", bearer); w.Code != 400 {
		t.Fatalf("bad JSON: %d", w.Code)
	}
	if w := do(h, "DELETE", "/api/studio/designs/stone", "", bearer); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := do(h, "GET", "/api/studio/designs", "", bearer); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("list after delete: %s", w.Body.String())
	}
}
