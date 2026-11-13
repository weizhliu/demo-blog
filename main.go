// demo-blog: one public article that signed-in people can edit.
//
// A small app for trying out Lift (https://lift.kabob.io):
//   - anyone can read the article, no account needed
//   - "Sign in with Kabob" goes through Lift (OpenID Connect); who may sign in
//     is set on the app's page in Lift
//   - anyone who is signed in can edit the article
//
// Lift gives the container PORT, APP_URL, LIFT_ISSUER and LIFT_CLIENT_ID.
// You add LIFT_CLIENT_SECRET (generated on the app's page in Lift).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "demo_blog_session"
	loginCookie   = "demo_blog_login"
	sessionTTL    = 7 * 24 * time.Hour
	loginTTL      = 10 * time.Minute
	maxBody       = 64 << 10
)

type config struct {
	port, appURL, issuer, clientID, clientSecret, dataDir string
	key                                                    []byte
}

type article struct {
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// What we remember about a signed-in person. Lives in a signed cookie, so
// nothing is lost when Lift puts the app to sleep and wakes it again.
type session struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
}

// Kept between /auth/login and /auth/callback.
type loginState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Exp      int64  `json:"exp"`
}

type app struct {
	cfg config
	mu  sync.Mutex // guards the article file and the lazily built OIDC client

	provider *oidc.Provider
}

func main() {
	cfg := config{
		port:         env("PORT", "8080"),
		appURL:       strings.TrimRight(env("APP_URL", "http://localhost:8080"), "/"),
		issuer:       strings.TrimRight(os.Getenv("LIFT_ISSUER"), "/"),
		clientID:     os.Getenv("LIFT_CLIENT_ID"),
		clientSecret: os.Getenv("LIFT_CLIENT_SECRET"),
		dataDir:      env("DATA_DIR", "/data"),
	}

	// Cookies are signed with SESSION_SECRET, or a key derived from the client secret.
	if secret := env("SESSION_SECRET", cfg.clientSecret); secret != "" {
		sum := sha256.Sum256([]byte("demo-blog cookies:" + secret))
		cfg.key = sum[:]
	}

	a := &app{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.show)
	mux.HandleFunc("GET /edit", a.editForm)
	mux.HandleFunc("POST /edit", a.save)
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /auth/logout", a.logout)

	if !a.loginConfigured() {
		log.Print("sign-in is off: set LIFT_CLIENT_SECRET (and deploy on Lift for LIFT_ISSUER / LIFT_CLIENT_ID)")
	}

	log.Printf("demo-blog listening on :%s (%s)", cfg.port, cfg.appURL)
	log.Fatal(http.ListenAndServe(":"+cfg.port, mux))
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func (a *app) loginConfigured() bool {
	return a.cfg.issuer != "" && a.cfg.clientID != "" && a.cfg.clientSecret != "" && a.cfg.key != nil
}

// ---- Article -----------------------------------------------------------------

func (a *app) articlePath() string { return filepath.Join(a.cfg.dataDir, "article.json") }

func (a *app) load() article {
	a.mu.Lock()
	defer a.mu.Unlock()

	var art article
	if raw, err := os.ReadFile(a.articlePath()); err == nil && json.Unmarshal(raw, &art) == nil {
		return art
	}

	return article{
		Title: "Hello from Lift",
		Body: "This article is public: anyone can read it without an account.\n\n" +
			"Sign in with Kabob to edit it. Who may sign in is decided on this app's page in Lift.",
	}
}

func (a *app) store(art article) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	raw, err := json.MarshalIndent(art, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.cfg.dataDir, 0o755); err != nil {
		return err
	}
	// Write then rename, so a crash never leaves half an article.
	tmp := a.articlePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.articlePath())
}

// ---- Pages -------------------------------------------------------------------

type page struct {
	Article    article
	Paragraphs []string
	User       *session
	LoginOn    bool
	Notice     string
	Error      string
}

func (a *app) show(w http.ResponseWriter, r *http.Request) {
	art := a.load()
	p := page{Article: art, Paragraphs: paragraphs(art.Body), User: a.currentUser(r), LoginOn: a.loginConfigured()}

	switch r.URL.Query().Get("notice") {
	case "saved":
		p.Notice = "Saved."
	case "denied":
		p.Error = "Your Kabob account isn't on this app's access list, so you can read but not edit."
	case "expired":
		p.Error = "That sign-in attempt expired. Please try again."
	}
	render(w, http.StatusOK, showTmpl, p)
}

func (a *app) editForm(w http.ResponseWriter, r *http.Request) {
	user := a.currentUser(r)
	if user == nil {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	render(w, http.StatusOK, editTmpl, page{Article: a.load(), User: user, LoginOn: true})
}

// The check that matters: writes are refused here unless the session is valid,
// whatever the page happened to show.
func (a *app) save(w http.ResponseWriter, r *http.Request) {
	user := a.currentUser(r)
	if user == nil {
		http.Error(w, "Sign in to edit.", http.StatusUnauthorized)
		return
	}
	if !a.sameOrigin(r) {
		http.Error(w, "This form must be submitted from the blog itself.", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The article is too long.", http.StatusRequestEntityTooLarge)
		return
	}

	art := article{
		Title:     strings.TrimSpace(r.PostFormValue("title")),
		Body:      strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n")),
		UpdatedBy: user.Name,
		UpdatedAt: time.Now().UTC(),
	}
	if art.Title == "" || art.Body == "" {
		render(w, http.StatusUnprocessableEntity, editTmpl,
			page{Article: art, User: user, LoginOn: true, Error: "Title and text can't be empty."})
		return
	}
	if err := a.store(art); err != nil {
		log.Printf("saving the article failed: %v", err)
		render(w, http.StatusInternalServerError, editTmpl,
			page{Article: art, User: user, LoginOn: true, Error: "Couldn't save. Does the app have a volume at /data?"})
		return
	}
	http.Redirect(w, r, "/?notice=saved", http.StatusSeeOther)
}

// Browsers send Origin on form posts; a post from another site is refused.
func (a *app) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || origin == a.cfg.appURL
}

func paragraphs(body string) []string {
	var out []string
	for _, p := range strings.Split(body, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func render(w http.ResponseWriter, status int, t *template.Template, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := t.Execute(w, p); err != nil {
		log.Printf("rendering failed: %v", err)
	}
}

// ---- Sign in with Kabob (through Lift) ---------------------------------------

// Built on first use, so the blog still starts (and serves readers) when Lift
// is briefly unreachable.
func (a *app) oidc(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	if !a.loginConfigured() {
		return nil, nil, errors.New("sign-in is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.provider == nil {
		provider, err := oidc.NewProvider(ctx, a.cfg.issuer)
		if err != nil {
			return nil, nil, err
		}
		a.provider = provider
	}

	return a.provider, &oauth2.Config{
		ClientID:     a.cfg.clientID,
		ClientSecret: a.cfg.clientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  a.cfg.appURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}, nil
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	_, oauth, err := a.oidc(r.Context())
	if err != nil {
		log.Printf("sign-in unavailable: %v", err)
		http.Error(w, "Sign-in isn't available right now.", http.StatusServiceUnavailable)
		return
	}

	ls := loginState{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Exp: time.Now().Add(loginTTL).Unix()}
	a.setCookie(w, loginCookie, ls, loginTTL)

	http.Redirect(w, r, oauth.AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusSeeOther)
}

func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	var ls loginState
	ok := a.readCookie(r, loginCookie, &ls) && ls.Exp > time.Now().Unix()
	a.clearCookie(w, loginCookie)

	q := r.URL.Query()

	// Lift refused this person: they aren't on the app's access list.
	if q.Get("error") == "access_denied" {
		http.Redirect(w, r, "/?notice=denied", http.StatusSeeOther)
		return
	}
	if !ok || q.Get("code") == "" || !hmac.Equal([]byte(q.Get("state")), []byte(ls.State)) {
		http.Redirect(w, r, "/?notice=expired", http.StatusSeeOther)
		return
	}

	provider, oauth, err := a.oidc(r.Context())
	if err != nil {
		http.Error(w, "Sign-in isn't available right now.", http.StatusServiceUnavailable)
		return
	}

	token, err := oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		log.Printf("code exchange failed: %v", err)
		http.Redirect(w, r, "/?notice=expired", http.StatusSeeOther)
		return
	}

	rawID, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: a.cfg.clientID}).Verify(r.Context(), rawID)
	if err != nil || idToken.Nonce != ls.Nonce {
		log.Printf("ID token rejected: %v", err)
		http.Redirect(w, r, "/?notice=expired", http.StatusSeeOther)
		return
	}

	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "Sign-in failed.", http.StatusBadGateway)
		return
	}

	name := claims.Name
	if name == "" {
		name = claims.Email
	}
	a.setCookie(w, sessionCookie, session{Sub: idToken.Subject, Name: name, Email: claims.Email, Exp: time.Now().Add(sessionTTL).Unix()}, sessionTTL)
	http.Redirect(w, r, "/edit", http.StatusSeeOther)
}

// End our session, then Lift's, so the next sign-in asks who you are again.
func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)

	if !a.loginConfigured() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	target := a.cfg.issuer + "/oauth/logout?" + url.Values{
		"client_id":                {a.cfg.clientID},
		"post_logout_redirect_uri": {a.cfg.appURL + "/"},
	}.Encode()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (a *app) currentUser(r *http.Request) *session {
	var s session
	if a.readCookie(r, sessionCookie, &s) && s.Exp > time.Now().Unix() && s.Sub != "" {
		return &s
	}
	return nil
}

// ---- Signed cookies ----------------------------------------------------------

func random() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *app) sign(payload string) string {
	mac := hmac.New(sha256.New, a.cfg.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *app) setCookie(w http.ResponseWriter, name string, value any, ttl time.Duration) {
	raw, _ := json.Marshal(value)
	payload := base64.RawURLEncoding.EncodeToString(raw)

	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    payload + "." + a.sign(name+"|"+payload),
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   strings.HasPrefix(a.cfg.appURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *app) readCookie(r *http.Request, name string, into any) bool {
	c, err := r.Cookie(name)
	if err != nil || a.cfg.key == nil {
		return false
	}
	payload, sig, found := strings.Cut(c.Value, ".")
	if !found || !hmac.Equal([]byte(sig), []byte(a.sign(name+"|"+payload))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(raw, into) == nil
}

func (a *app) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(a.cfg.appURL, "https://"), SameSite: http.SameSiteLaxMode})
}

// ---- Templates ---------------------------------------------------------------

const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Article.Title}} · demo-blog</title>
<style>
  :root { --ink:#1c2530; --muted:#667788; --line:#dde5ec; --accent:#09a8be; --paper:#fff; --tint:#f4f8fa; --bad:#c0392b; }
  @media (prefers-color-scheme: dark) {
    :root { --ink:#e8eef3; --muted:#93a4b3; --line:#2c3844; --paper:#141b22; --tint:#1b242d; }
  }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--paper); color:var(--ink); font:17px/1.65 Georgia, "Times New Roman", serif; }
  header, main { max-width: 42rem; margin: 0 auto; padding: 0 1.25rem; }
  header { display:flex; align-items:center; gap:1rem; padding-top:1.25rem; padding-bottom:1.25rem;
           border-bottom:1px solid var(--line); font:14px/1.4 system-ui, sans-serif; }
  header .brand { font-weight:600; color:var(--ink); text-decoration:none; margin-right:auto; }
  header .who { color:var(--muted); }
  a { color:var(--accent); }
  main { padding-top:2.5rem; padding-bottom:4rem; }
  h1 { font-size:2.1rem; line-height:1.2; margin:0 0 .5rem; text-wrap:balance; }
  .meta { color:var(--muted); font:14px/1.4 system-ui, sans-serif; margin:0 0 2rem; }
  p { margin:0 0 1.2em; white-space:pre-line; overflow-wrap:anywhere; }
  .note { font:14px/1.5 system-ui, sans-serif; padding:.7rem 1rem; border-radius:6px; background:var(--tint); margin:0 0 1.5rem; }
  .note.bad { color:var(--bad); }
  form { display:grid; gap:1rem; font:15px/1.4 system-ui, sans-serif; }
  label { display:grid; gap:.35rem; font-weight:600; color:var(--muted); font-size:13px; }
  input, textarea { font:inherit; font-weight:400; color:var(--ink); background:var(--paper);
                    border:1px solid var(--line); border-radius:6px; padding:.65rem .8rem; width:100%; }
  textarea { min-height:18rem; resize:vertical; font:16px/1.6 Georgia, serif; }
  input:focus-visible, textarea:focus-visible, .btn:focus-visible { outline:2px solid var(--accent); outline-offset:2px; }
  .row { display:flex; gap:.75rem; align-items:center; justify-content:flex-end; }
  .btn { font:600 14px/1 system-ui, sans-serif; padding:.7rem 1.1rem; border-radius:6px; border:1px solid var(--accent);
         background:var(--accent); color:#fff; text-decoration:none; cursor:pointer; }
  .btn.quiet { background:none; color:var(--accent); }
</style>
</head>
<body>
<header>
  <a class="brand" href="/">demo-blog</a>
  {{if .User}}<span class="who">{{.User.Name}}</span><a href="/auth/logout">Sign out</a>
  {{else if .LoginOn}}<a href="/auth/login">Sign in with Kabob</a>{{end}}
</header>
<main>
{{if .Notice}}<p class="note" role="status">{{.Notice}}</p>{{end}}
{{if .Error}}<p class="note bad" role="alert">{{.Error}}</p>{{end}}
{{template "content" .}}
</main>
</body>
</html>`

var showTmpl = template.Must(template.Must(template.New("layout").Parse(layout)).New("content").Parse(`
<article>
  <h1>{{.Article.Title}}</h1>
  <p class="meta">
    {{if .Article.UpdatedBy}}Last edited by {{.Article.UpdatedBy}} on {{.Article.UpdatedAt.Format "2 January 2006"}}{{else}}Not edited yet{{end}}
    {{if .User}} · <a href="/edit">Edit</a>{{end}}
  </p>
  {{range .Paragraphs}}<p>{{.}}</p>{{end}}
</article>`)).Lookup("layout")

var editTmpl = template.Must(template.Must(template.New("layout").Parse(layout)).New("content").Parse(`
<form method="post" action="/edit">
  <label>Title <input name="title" value="{{.Article.Title}}" maxlength="200" required></label>
  <label>Text <textarea name="body" required>{{.Article.Body}}</textarea></label>
  <div class="row">
    <a class="btn quiet" href="/">Cancel</a>
    <button class="btn" type="submit">Save</button>
  </div>
</form>`)).Lookup("layout")
