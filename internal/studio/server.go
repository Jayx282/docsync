package studio

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed all:ui/dist
var uiFiles embed.FS

// Server is the studio's HTTP front end.
type Server struct {
	Svc     *Service
	Store   *Store
	Runtime *Runtime
	Token   string // required on every request (cookie or bearer)
	Log     *slog.Logger
	// Loopback restricts the Host header to localhost names (DNS-rebinding protection).
	Loopback bool
	// TokenFile, when set, is where the token is saved; the sign-in page names it.
	TokenFile string
}

const cookieName = "docsync_studio"

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	api := func(pattern string, fn func(*http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			v, err := fn(r.WithContext(ctx))
			if err != nil {
				code := http.StatusBadRequest
				var he httpError
				if errors.As(err, &he) {
					code = he.code
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, v)
		})
	}

	api("GET /api/studio/connection", func(r *http.Request) (any, error) {
		snap, err := s.Svc.Snapshot(r.Context())
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}, nil
		}
		return map[string]any{"ok": true, "server": snap.Server, "database": snap.Database, "edition": snap.Edition,
			"version": snap.Version, "snapshot_isolation": snap.Snapshot, "change_tracking": snap.CT,
			"tables": len(snap.Tables), "loaded_at": snap.LoadedAt}, nil
	})
	api("POST /api/studio/schema/refresh", func(r *http.Request) (any, error) {
		snap, err := s.Svc.Refresh(r.Context())
		if err != nil {
			return nil, err
		}
		return map[string]any{"tables": len(snap.Tables), "loaded_at": snap.LoadedAt}, nil
	})
	api("GET /api/studio/tables", func(r *http.Request) (any, error) {
		snap, err := s.Svc.Snapshot(r.Context())
		if err != nil {
			return nil, err
		}
		type row struct {
			Name    string   `json:"name"`
			Rows    int64    `json:"rows"`
			CT      bool     `json:"change_tracking"`
			Columns int      `json:"columns"`
			PK      []string `json:"primary_key"`
		}
		out := []row{}
		for _, t := range snap.SortedTables() {
			out = append(out, row{Name: t.Name, Rows: t.Rows, CT: t.CTEnabled, Columns: len(t.Columns), PK: t.PK})
		}
		return out, nil
	})
	api("GET /api/studio/tables/{name}", func(r *http.Request) (any, error) {
		snap, err := s.Svc.Snapshot(r.Context())
		if err != nil {
			return nil, err
		}
		t, ok := snap.Table(r.PathValue("name"))
		if !ok {
			return nil, httpError{http.StatusNotFound, "no such table"}
		}
		var out, in []ForeignKey
		for _, fk := range snap.FKs {
			if strings.EqualFold(fk.FromTable, t.Name) {
				out = append(out, fk)
			}
			if strings.EqualFold(fk.ToTable, t.Name) {
				in = append(in, fk)
			}
		}
		var sugg []Suggestion
		if len(t.PK) == 1 {
			sugg, _ = Suggest(snap, t.Name)
		}
		return map[string]any{"table": t, "references": nonNil(out), "referenced_by": nonNil(in), "related": nonNilS(sugg)}, nil
	})
	api("GET /api/studio/suggestions", func(r *http.Request) (any, error) {
		snap, err := s.Svc.Snapshot(r.Context())
		if err != nil {
			return nil, err
		}
		sugg, err := Suggest(snap, r.URL.Query().Get("root"))
		return nonNilS(sugg), err
	})
	api("POST /api/studio/profile", func(r *http.Request) (any, error) {
		var body struct {
			Root     string   `json:"root"`
			Relation Relation `json:"relation"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		snap, err := s.Svc.Snapshot(r.Context())
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return ProfileRelation(ctx, s.Svc.DB, snap, body.Root, body.Relation)
	})

	api("GET /api/studio/designs", func(r *http.Request) (any, error) {
		ds, err := s.Store.List()
		if ds == nil {
			ds = []*Design{}
		}
		return ds, err
	})
	api("GET /api/studio/designs/{name}", func(r *http.Request) (any, error) {
		d, err := s.Store.Get(r.PathValue("name"))
		if err != nil {
			return nil, httpError{http.StatusNotFound, "no such design"}
		}
		return d, nil
	})
	api("PUT /api/studio/designs/{name}", func(r *http.Request) (any, error) {
		var d Design
		if err := decode(r, &d); err != nil {
			return nil, err
		}
		if d.Name != r.PathValue("name") {
			return nil, errors.New("design name does not match the URL")
		}
		if d.Version == 0 {
			d.Version = 1
		}
		if !nameRe.MatchString(d.Name) {
			return nil, errors.New("design name must start with a lowercase letter and use only a-z, 0-9 and _")
		}
		return &d, s.Store.Put(&d) // drafts may be incomplete; validation happens on preview/validate/deploy
	})
	api("DELETE /api/studio/designs/{name}", func(r *http.Request) (any, error) {
		return map[string]string{"result": "deleted"}, s.Store.Delete(r.PathValue("name"))
	})

	type designBody struct {
		Design Design   `json:"design"`
		IDs    []string `json:"ids"`
		Limit  int      `json:"limit"`
	}
	api("POST /api/studio/compile", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		comp, err := s.Svc.Compile(r.Context(), &b.Design)
		if err != nil {
			return map[string]any{"errors": splitErrors(err), "yaml": "", "sql": []CompiledFragment{}}, nil
		}
		return map[string]any{"errors": []string{}, "yaml": comp.YAML, "sql": comp.SQL}, nil
	})
	api("POST /api/studio/preview", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return s.Svc.Preview(r.Context(), &b.Design, b.IDs, b.Limit)
	})
	api("POST /api/studio/validate", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return s.Svc.Validate(r.Context(), &b.Design)
	})
	api("POST /api/studio/filter/count", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return s.Svc.CountFilter(ctx, &b.Design)
	})
	api("POST /api/studio/deploy/plan", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return s.Svc.PlanDeploy(r.Context(), &b.Design, s.Runtime)
	})
	api("POST /api/studio/deploy", func(r *http.Request) (any, error) {
		var b designBody
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		if err := s.Store.Put(&b.Design); err != nil {
			return nil, err
		}
		return s.Svc.Deploy(r.Context(), &b.Design, s.Runtime)
	})
	api("GET /api/studio/runtime", func(r *http.Request) (any, error) { return s.Runtime.Status(), nil })
	api("POST /api/studio/runtime/stop", func(r *http.Request) (any, error) {
		s.Runtime.Stop()
		return s.Runtime.Status(), s.Svc.ClearActive()
	})

	mux.Handle("/pipeline/", http.StripPrefix("/pipeline", s.Runtime))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	dist, err := fs.Sub(uiFiles, "ui/dist")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", spa(dist)) // method-agnostic so it does not conflict with /pipeline/

	var h http.Handler = mux
	h = s.auth(h)
	h = http.NewCrossOriginProtection().Handler(h)
	if s.Loopback {
		h = loopbackOnly(h)
	}
	return h, nil
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	if err := dec.Decode(v); err != nil {
		return httpError{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func nonNil(fks []ForeignKey) []ForeignKey {
	if fks == nil {
		return []ForeignKey{}
	}
	return fks
}

func nonNilS(s []Suggestion) []Suggestion {
	if s == nil {
		return []Suggestion{}
	}
	return s
}

// auth requires the studio token on every request except /healthz. Opening
// /?token=... once stores it in an HttpOnly, SameSite=Strict cookie.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/signin" && r.Method == http.MethodPost {
			s.signin(w, r)
			return
		}
		problem := ""
		if tok := r.URL.Query().Get("token"); tok != "" {
			if s.valid(tok) {
				http.SetCookie(w, s.sessionCookie())
				q := r.URL.Query()
				q.Del("token")
				u := *r.URL
				u.RawQuery = q.Encode()
				http.Redirect(w, r, u.String(), http.StatusSeeOther)
				return
			}
			problem = "The token in that link is not valid. Paste the current token below."
		}
		ok := false
		if c, err := r.Cookie(cookieName); err == nil && s.valid(c.Value) {
			ok = true
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && s.valid(strings.TrimPrefix(h, "Bearer ")) {
			ok = true
		}
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/pipeline/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong studio token"})
				return
			}
			s.signinPage(w, http.StatusUnauthorized, problem)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) valid(tok string) bool {
	return s.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1
}

func (s *Server) sessionCookie() *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

// signin handles the sign-in form: a pasted token sets the session cookie, then the
// browser returns to the page it was on (a "#/..." route of the UI).
func (s *Server) signin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		s.signinPage(w, http.StatusBadRequest, "That sign-in request could not be read. Try again.")
		return
	}
	next := r.PostForm.Get("next")
	if !nextRe.MatchString(next) {
		next = ""
	}
	if !s.valid(strings.TrimSpace(r.PostForm.Get("token"))) {
		time.Sleep(300 * time.Millisecond) // slows guessing of short hand-picked tokens
		s.renderSignin(w, http.StatusUnauthorized, "That token is not right. Check that you copied all of it.", next)
		return
	}
	http.SetCookie(w, s.sessionCookie())
	http.Redirect(w, r, "/"+next, http.StatusSeeOther)
}

// nextRe accepts only UI routes ("#/design/stone"), never another site.
var nextRe = regexp.MustCompile(`^#/[A-Za-z0-9._~/%-]{0,300}$`)

// signinPage asks for the token. It is self-contained (inline style, one script with
// a per-response nonce) because the browser cannot load the UI's files yet.
func (s *Server) signinPage(w http.ResponseWriter, code int, problem string) {
	s.renderSignin(w, code, problem, "")
}

// renderSignin draws the sign-in page; next is the UI route to return to, kept
// across failed attempts (the page then sits at /signin and has no #route itself).
func (s *Server) renderSignin(w http.ResponseWriter, code int, problem, next string) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	nonce := hex.EncodeToString(raw) // hex needs no HTML escaping
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; form-action 'self'; frame-ancestors 'self'; base-uri 'none'")
	w.WriteHeader(code)
	_ = signinTmpl.Execute(w, map[string]string{"Nonce": nonce, "Problem": problem, "TokenFile": s.TokenFile, "Next": next})
}

var signinTmpl = template.Must(template.New("signin").Parse(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in · docsync studio</title>
<style>
:root{color-scheme:light dark;--bg:#f4f5f7;--panel:#fff;--border:#c9ced6;--text:#1a1d23;--muted:#5d6877;--accent:#2f6fde;--bad:#c0302f}
@media (prefers-color-scheme:dark){:root{--bg:#0e1014;--panel:#15181d;--border:#3b424e;--text:#e4e7ec;--muted:#9ba5b3;--accent:#4d8af0;--bad:#f07272}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:16px;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{width:100%;max-width:460px;background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:28px}
h1{font-size:19px;margin:0 0 8px}
p{margin:8px 0;color:var(--muted)}
code{font:13px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:var(--text);overflow-wrap:anywhere}
label{display:block;font-weight:600;margin:18px 0 6px}
input[type=password]{width:100%;height:38px;padding:0 10px;border:1px solid var(--border);border-radius:6px;background:var(--bg);color:var(--text);font:14px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
input[type=password]:focus{outline:2px solid var(--accent);outline-offset:1px}
button{margin-top:14px;height:36px;padding:0 18px;border:0;border-radius:6px;background:var(--accent);color:#fff;font:600 14px system-ui,-apple-system,"Segoe UI",sans-serif;cursor:pointer}
.problem{color:var(--bad);font-weight:500}
</style>
<main>
<h1>Sign in to docsync studio</h1>
<p>Paste the studio's access token, or open the sign-in link that <code>docsync studio</code> printed when it started.</p>
{{if .TokenFile}}<p>The token is saved in <code>{{.TokenFile}}</code>.</p>{{end}}
<form method="post" action="/signin">
<label for="token">Access token</label>
<input id="token" name="token" type="password" autocomplete="current-password" spellcheck="false" autofocus required>
<input id="next" name="next" type="hidden" value="{{.Next}}">
{{if .Problem}}<p class="problem" role="alert">{{.Problem}}</p>{{end}}
<button type="submit">Sign in</button>
</form>
</main>
<script nonce="{{.Nonce}}">
// Return to the UI route the browser asked for. Read it again on submit: changing only
// the #route of an open page does not reload it.
const next = document.getElementById("next");
const sync = () => { if (location.hash) next.value = location.hash; };
sync();
addEventListener("hashchange", sync);
document.querySelector("form").addEventListener("submit", sync);
</script>
`))

// loopbackOnly rejects requests whose Host header is not a loopback name, so a web
// page cannot reach the studio through DNS rebinding.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "studio only answers on localhost", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// spa serves the built UI, falling back to index.html for client-side routes.
func spa(dist fs.FS) http.Handler {
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such API"})
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-src 'self'; img-src 'self' data:")
		files.ServeHTTP(w, r)
	})
}
