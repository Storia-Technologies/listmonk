package main

// Claude connector (MCP server).
//
// This file is a Storia addition to listmonk. Like sequences.go it is
// self-contained so upstream releases merge cleanly: its tables are created
// here, its SQL is inline, and upstream files only carry one-line hooks.
//
// It makes this listmonk instance a remote MCP server at <root_url>/mcp, so a
// person can add it to Claude as a connector and have Claude work in their
// listmonk for them. Three parts:
//
//   - A small OAuth 2.1 authorization server (this file). Claude registers
//     itself, the person signs in with their normal listmonk login and
//     approves, and Claude gets a token that acts as that listmonk user.
//   - The MCP endpoint (this file): JSON-RPC over HTTP POST, tools only.
//   - The tools (mcp_tools.go). Each one calls an existing listmonk HTTP
//     handler in-process as the signed-in user, so listmonk's own validation
//     and permission checks apply unchanged.
//
// The connector can never send to a list unless "let Claude schedule and
// send" is switched on in Settings > Claude connector. It is off by default.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	null "gopkg.in/volatiletech/null.v6"
)

const (
	mcpPath = "/mcp"

	mcpAccessTTL  = time.Hour
	mcpRefreshTTL = 60 * 24 * time.Hour
	mcpCodeTTL    = 5 * time.Minute
	mcpRequestTTL = 15 * time.Minute

	// The most request body the MCP and OAuth endpoints will read.
	mcpMaxBody = 2 << 20

	mcpServerName = "listmonk"
)

// The protocol versions this server speaks, newest first.
var mcpProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpSchema = `
CREATE TABLE IF NOT EXISTS mcp_config (
    id              INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    allow_sending   BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
INSERT INTO mcp_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS mcp_clients (
    client_id       TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    redirect_uris   TEXT[] NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- An authorization request waiting on the person's yes or no, and then the
-- one-time code that is handed back to the client.
CREATE TABLE IF NOT EXISTS mcp_auth_requests (
    id              TEXT PRIMARY KEY,
    client_id       TEXT NOT NULL REFERENCES mcp_clients(client_id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri    TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT '',
    code_challenge  TEXT NOT NULL,
    code_hash       TEXT NULL UNIQUE,
    expires_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- An authorization request parked while the person signs in. listmonk's
-- login keeps only the path of where to go next, so the query is kept here.
CREATE TABLE IF NOT EXISTS mcp_login_resumes (
    id              TEXT PRIMARY KEY,
    query           TEXT NOT NULL,
    expires_at      TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS mcp_tokens (
    id                  SERIAL PRIMARY KEY,
    client_id           TEXT NOT NULL REFERENCES mcp_clients(client_id) ON DELETE CASCADE,
    user_id             INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access_hash         TEXT NOT NULL UNIQUE,
    refresh_hash        TEXT NOT NULL UNIQUE,
    access_expires_at   TIMESTAMP WITH TIME ZONE NOT NULL,
    refresh_expires_at  TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_used_at        TIMESTAMP WITH TIME ZONE NULL
);
`

// mcpEcho is the app's router, kept so tools can build request contexts for
// the handlers they call.
var mcpEcho *echo.Echo

// mcpEnabled reports whether the connector is switched on for this instance.
// It is on unless LISTMONK_mcp__enabled is set to false.
func mcpEnabled() bool {
	if !ko.Exists("mcp.enabled") {
		return true
	}
	return ko.Bool("mcp.enabled")
}

// initMCP creates the connector's tables and starts its housekeeping. It is
// called once on boot.
func (a *App) initMCP() {
	if !mcpEnabled() {
		a.log.Println("the Claude connector (MCP) is disabled on this instance")
		return
	}

	if _, err := a.db.Exec(mcpSchema); err != nil {
		a.log.Printf("error creating connector tables, the Claude connector will not work: %v", err)
		return
	}

	// Clear out expired requests and tokens, and clients that registered
	// but never completed a sign-in.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			for _, q := range []string{
				`DELETE FROM mcp_auth_requests WHERE expires_at < NOW()`,
				`DELETE FROM mcp_login_resumes WHERE expires_at < NOW()`,
				`DELETE FROM mcp_tokens WHERE refresh_expires_at < NOW()`,
				`DELETE FROM mcp_clients c WHERE c.created_at < NOW() - INTERVAL '1 day'
					AND NOT EXISTS (SELECT 1 FROM mcp_tokens t WHERE t.client_id = c.client_id)
					AND NOT EXISTS (SELECT 1 FROM mcp_auth_requests r WHERE r.client_id = c.client_id)`,
			} {
				if _, err := a.db.Exec(q); err != nil {
					a.log.Printf("mcp: housekeeping error: %v", err)
				}
			}
		}
	}()
}

// initMCPHandlers registers the connector's routes.
func initMCPHandlers(e *echo.Echo, a *App) {
	if !mcpEnabled() {
		return
	}
	mcpEcho = e

	// Discovery and OAuth endpoints that the client calls directly.
	e.GET("/.well-known/oauth-protected-resource", a.MCPResourceMetadata)
	e.GET("/.well-known/oauth-protected-resource"+mcpPath, a.MCPResourceMetadata)
	e.GET("/.well-known/oauth-authorization-server", a.MCPServerMetadata)
	e.GET("/.well-known/oauth-authorization-server"+mcpPath, a.MCPServerMetadata)
	e.POST(mcpPath+"/oauth/register", a.MCPRegisterClient)
	e.POST(mcpPath+"/oauth/token", a.MCPToken)

	// The MCP endpoint itself. Only POST does anything.
	e.POST(mcpPath, a.MCPEndpoint)
	e.GET(mcpPath, func(c echo.Context) error {
		c.Response().Header().Set("Allow", "POST")
		return c.NoContent(http.StatusMethodNotAllowed)
	})
	e.DELETE(mcpPath, func(c echo.Context) error {
		c.Response().Header().Set("Allow", "POST")
		return c.NoContent(http.StatusMethodNotAllowed)
	})

	// The approval page, shown in the person's browser. It needs a normal
	// listmonk login, and sends them to the login page first if they have none.
	page := e.Group("", a.auth.Middleware, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if _, ok := c.Get(auth.UserHTTPCtxKey).(*echo.HTTPError); ok {
				return a.mcpToLogin(c)
			}
			return next(c)
		}
	})
	page.GET(mcpPath+"/oauth/authorize", a.MCPAuthorizePage)
	page.GET(mcpPath+"/oauth/resume/:rid", a.MCPAuthorizeResume)
	page.POST(mcpPath+"/oauth/authorize", a.MCPAuthorizeDecision)

	// The admin screen's API (Settings > Claude connector).
	api := e.Group("", a.auth.Middleware, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err, ok := c.Get(auth.UserHTTPCtxKey).(*echo.HTTPError); ok {
				return err
			}
			return next(c)
		}
	})
	pm := a.auth.Perm
	api.GET("/api/mcp", pm(a.GetMCPInfo, "settings:get"))
	api.PUT("/api/mcp/settings", pm(a.UpdateMCPSettings, "settings:manage"))
	api.DELETE("/api/mcp/connections/:id", pm(hasID(a.DeleteMCPConnection), "settings:manage"))
}

// mcpRoot is this instance's public address without a trailing slash.
func (a *App) mcpRoot() string {
	return strings.TrimRight(a.urlCfg.RootURL, "/")
}

// mcpResource is the canonical address of the MCP endpoint, which is also
// the OAuth resource identifier tokens are issued for.
func (a *App) mcpResource() string {
	return a.mcpRoot() + mcpPath
}

// ---------------------------------------------------------------------------
// Discovery.

// MCPResourceMetadata serves OAuth Protected Resource Metadata (RFC 9728).
func (a *App) MCPResourceMetadata(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"resource":                 a.mcpResource(),
		"authorization_servers":    []string{a.mcpRoot()},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "listmonk",
	})
}

// MCPServerMetadata serves OAuth Authorization Server Metadata (RFC 8414).
func (a *App) MCPServerMetadata(c echo.Context) error {
	root := a.mcpRoot()
	return c.JSON(http.StatusOK, map[string]any{
		"issuer":                                root,
		"authorization_endpoint":                root + mcpPath + "/oauth/authorize",
		"token_endpoint":                        root + mcpPath + "/oauth/token",
		"registration_endpoint":                 root + mcpPath + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// ---------------------------------------------------------------------------
// OAuth: client registration, approval and tokens.

type mcpClient struct {
	ClientID     string         `db:"client_id"`
	Name         string         `db:"name"`
	RedirectURIs pq.StringArray `db:"redirect_uris"`
}

// mcpOAuthErr writes an OAuth error response (RFC 6749 section 5.2).
func mcpOAuthErr(c echo.Context, status int, code, desc string) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(status, map[string]string{"error": code, "error_description": desc})
}

// mcpValidRedirect allows https anywhere and plain http only on the local
// machine, as OAuth 2.1 requires for public clients.
func mcpValidRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	return u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
}

// MCPRegisterClient handles Dynamic Client Registration (RFC 7591). Clients
// are public: there is no client secret and PKCE is mandatory.
func (a *App) MCPRegisterClient(c echo.Context) error {
	req := struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}{}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, mcpMaxBody))
	if err != nil || json.Unmarshal(body, &req) != nil {
		return mcpOAuthErr(c, http.StatusBadRequest, "invalid_client_metadata", "The request body must be JSON.")
	}

	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		return mcpOAuthErr(c, http.StatusBadRequest, "invalid_redirect_uri", "Give between 1 and 10 redirect URIs.")
	}
	for _, r := range req.RedirectURIs {
		if len(r) > 1000 || !mcpValidRedirect(r) {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_redirect_uri",
				"Redirect URIs must be https, or http on localhost.")
		}
	}

	name := strings.TrimSpace(req.ClientName)
	if len(name) > 200 {
		name = name[:200]
	}

	id, err := mcpRandom(24)
	if err != nil {
		return mcpOAuthErr(c, http.StatusInternalServerError, "server_error", "Could not create a client ID.")
	}
	id = "mcp_" + id

	if _, err := a.db.Exec(`INSERT INTO mcp_clients (client_id, name, redirect_uris) VALUES ($1, $2, $3)`,
		id, name, pq.Array(req.RedirectURIs)); err != nil {
		a.log.Printf("mcp: error registering client: %v", err)
		return mcpOAuthErr(c, http.StatusInternalServerError, "server_error", "Could not register the client.")
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// getMCPClient fetches a registered client.
func (a *App) getMCPClient(id string) (mcpClient, bool) {
	var out mcpClient
	if err := a.db.Get(&out, `SELECT client_id, name, redirect_uris FROM mcp_clients WHERE client_id = $1`, id); err != nil {
		return out, false
	}
	return out, true
}

// mcpSameResource reports whether a resource a client asked for is this
// instance's MCP endpoint. A missing value is accepted, since tokens issued
// here are only ever valid here.
func (a *App) mcpSameResource(res string) bool {
	if res == "" {
		return true
	}
	res = strings.TrimRight(res, "/")
	return strings.EqualFold(res, a.mcpResource()) || strings.EqualFold(res, a.mcpRoot())
}

var mcpPageTpl = template.Must(template.New("mcp").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{ .Title }}</title>
<style>
  body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #f6f7f9; color: #1f2933; line-height: 1.5; }
  main { max-width: 30rem; margin: 8vh auto; padding: 2rem; background: #fff; border-radius: 10px;
    box-shadow: 0 1px 3px rgba(0, 0, 0, .12); }
  h1 { font-size: 1.3rem; margin: 0 0 1rem; }
  p { margin: 0 0 1rem; }
  ul { margin: 0 0 1.25rem; padding-left: 1.2rem; }
  li { margin-bottom: .35rem; }
  .muted { color: #5f6b7a; font-size: .9rem; }
  .row { display: flex; gap: .75rem; margin-top: 1.5rem; }
  button { flex: 1; font: inherit; padding: .7rem 1rem; border-radius: 6px; border: 1px solid #c5ccd6;
    background: #fff; color: #1f2933; cursor: pointer; }
  button.primary { background: #0055d4; border-color: #0055d4; color: #fff; font-weight: 600; }
  @media (max-width: 34rem) { main { margin: 0; min-height: 100vh; border-radius: 0; box-shadow: none; } }
</style>
</head>
<body>
<main>
{{ if .Error }}
  <h1>This connection can't be completed</h1>
  <p>{{ .Error }}</p>
  <p class="muted">Close this window and try connecting again from Claude.</p>
{{ else }}
  <h1>Connect {{ .Client }} to your listmonk?</h1>
  <p>You're signed in as <strong>{{ .User }}</strong>. {{ .Client }} will be able to work in this listmonk as you:</p>
  <ul>
    <li>See your lists, subscribers, campaigns and results</li>
    <li>Add and update subscribers and lists</li>
    <li>Write and edit draft campaigns and sequences</li>
    {{ if .Sending }}<li><strong>Schedule and send campaigns, and switch sequences on</strong></li>
    {{ else }}<li>It can't send or schedule anything. You do that in listmonk.</li>{{ end }}
  </ul>
  <p class="muted">You can disconnect it at any time in listmonk under Settings, Claude connector.</p>
  <form method="post" action="{{ .Action }}">
    <input type="hidden" name="request_id" value="{{ .RequestID }}">
    <div class="row">
      <button type="submit" name="decision" value="deny">Cancel</button>
      <button type="submit" name="decision" value="approve" class="primary">Connect</button>
    </div>
  </form>
{{ end }}
</main>
</body>
</html>`))

// mcpErrPage shows a problem to the person rather than redirecting, used
// whenever the client or its redirect address can't be trusted.
func mcpErrPage(c echo.Context, msg string) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().WriteHeader(http.StatusBadRequest)
	return mcpPageTpl.Execute(c.Response(), map[string]any{"Title": "Connect to listmonk", "Error": msg})
}

// mcpRedirect sends the person back to the client with the given parameters.
func mcpRedirect(c echo.Context, redirectURI string, params url.Values) error {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return mcpErrPage(c, "The app's return address isn't valid.")
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	u.RawQuery = q.Encode()
	return c.Redirect(http.StatusFound, u.String())
}

// mcpToLogin sends someone who isn't signed in to the login page, set up to
// come back afterwards. listmonk's login only keeps the path of the return
// address and drops its query, so an authorization request's query is parked
// in the database and picked up again by MCPAuthorizeResume.
func (a *App) mcpToLogin(c echo.Context) error {
	next := c.Request().URL.Path

	if c.Request().Method == http.MethodGet && next == mcpPath+"/oauth/authorize" {
		id, err := mcpRandom(24)
		if err != nil {
			return mcpErrPage(c, "Something went wrong starting the connection.")
		}
		q := c.Request().URL.RawQuery
		if len(q) > 8000 {
			return mcpErrPage(c, "The connection request is too long.")
		}
		if _, err := a.db.Exec(`INSERT INTO mcp_login_resumes (id, query, expires_at) VALUES ($1, $2, $3)`,
			id, q, time.Now().Add(mcpRequestTTL)); err != nil {
			a.log.Printf("mcp: error parking authorization request: %v", err)
			return mcpErrPage(c, "Something went wrong starting the connection.")
		}
		next = mcpPath + "/oauth/resume/" + id
	}

	u, _ := url.Parse(a.urlCfg.LoginURL)
	q := url.Values{}
	q.Set("next", next)
	u.RawQuery = q.Encode()
	return c.Redirect(http.StatusTemporaryRedirect, u.String())
}

// MCPAuthorizeResume picks an authorization request back up after sign-in.
func (a *App) MCPAuthorizeResume(c echo.Context) error {
	var query string
	if err := a.db.Get(&query, `DELETE FROM mcp_login_resumes WHERE id = $1 AND expires_at > NOW() RETURNING query`,
		c.Param("rid")); err != nil {
		return mcpErrPage(c, "This connection request has expired.")
	}
	return c.Redirect(http.StatusFound, mcpPath+"/oauth/authorize?"+query)
}

// MCPAuthorizePage is the OAuth authorization endpoint. It checks the
// request, then shows the signed-in person what they are about to connect.
func (a *App) MCPAuthorizePage(c echo.Context) error {
	var (
		user      = auth.GetUser(c)
		q         = c.QueryParams()
		clientID  = q.Get("client_id")
		redirect  = q.Get("redirect_uri")
		state     = q.Get("state")
		challenge = q.Get("code_challenge")
	)

	// Until the client and its return address are verified, problems are
	// shown here and never redirected.
	client, ok := a.getMCPClient(clientID)
	if !ok {
		return mcpErrPage(c, "The app asking to connect isn't registered with this listmonk.")
	}
	valid := false
	for _, r := range client.RedirectURIs {
		if r == redirect {
			valid = true
			break
		}
	}
	if !valid {
		return mcpErrPage(c, "The app's return address doesn't match the one it registered.")
	}

	if q.Get("response_type") != "code" {
		return mcpRedirect(c, redirect, url.Values{"error": {"unsupported_response_type"}, "state": {state}})
	}
	if len(challenge) < 43 || len(challenge) > 128 || q.Get("code_challenge_method") != "S256" {
		return mcpRedirect(c, redirect, url.Values{"error": {"invalid_request"},
			"error_description": {"PKCE with S256 is required."}, "state": {state}})
	}
	if !a.mcpSameResource(q.Get("resource")) {
		return mcpRedirect(c, redirect, url.Values{"error": {"invalid_target"},
			"error_description": {"The requested resource is not this listmonk."}, "state": {state}})
	}
	if len(state) > 2000 {
		return mcpRedirect(c, redirect, url.Values{"error": {"invalid_request"}})
	}

	id, err := mcpRandom(24)
	if err != nil {
		return mcpErrPage(c, "Something went wrong starting the connection.")
	}
	if _, err := a.db.Exec(`INSERT INTO mcp_auth_requests (id, client_id, user_id, redirect_uri, state, code_challenge, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, client.ClientID, user.ID, redirect, state, challenge, time.Now().Add(mcpRequestTTL)); err != nil {
		a.log.Printf("mcp: error saving authorization request: %v", err)
		return mcpErrPage(c, "Something went wrong starting the connection.")
	}

	name := client.Name
	if name == "" {
		name = "This app"
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("X-Frame-Options", "DENY")
	c.Response().Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	c.Response().WriteHeader(http.StatusOK)
	return mcpPageTpl.Execute(c.Response(), map[string]any{
		"Title":     "Connect to listmonk",
		"Client":    name,
		"User":      user.Username,
		"Sending":   a.mcpAllowSending(),
		"Action":    mcpPath + "/oauth/authorize",
		"RequestID": id,
	})
}

// MCPAuthorizeDecision handles the person's yes or no from the approval page.
func (a *App) MCPAuthorizeDecision(c echo.Context) error {
	var (
		user = auth.GetUser(c)
		id   = c.FormValue("request_id")
	)

	// The page is only ever posted from this site.
	if o := c.Request().Header.Get("Origin"); o != "" && !strings.EqualFold(strings.TrimRight(o, "/"), a.mcpRoot()) {
		return mcpErrPage(c, "This approval didn't come from listmonk.")
	}

	req := struct {
		RedirectURI string `db:"redirect_uri"`
		State       string `db:"state"`
	}{}
	if err := a.db.Get(&req, `SELECT redirect_uri, state FROM mcp_auth_requests
		WHERE id = $1 AND user_id = $2 AND code_hash IS NULL AND expires_at > NOW()`, id, user.ID); err != nil {
		return mcpErrPage(c, "This approval has expired.")
	}

	if c.FormValue("decision") != "approve" {
		a.db.Exec(`DELETE FROM mcp_auth_requests WHERE id = $1`, id)
		return mcpRedirect(c, req.RedirectURI, url.Values{"error": {"access_denied"}, "state": {req.State}})
	}

	code, err := mcpRandom(32)
	if err != nil {
		return mcpErrPage(c, "Something went wrong finishing the connection.")
	}
	if _, err := a.db.Exec(`UPDATE mcp_auth_requests SET code_hash = $2, expires_at = $3 WHERE id = $1`,
		id, mcpHash(code), time.Now().Add(mcpCodeTTL)); err != nil {
		a.log.Printf("mcp: error saving authorization code: %v", err)
		return mcpErrPage(c, "Something went wrong finishing the connection.")
	}

	return mcpRedirect(c, req.RedirectURI, url.Values{"code": {code}, "state": {req.State}, "iss": {a.mcpRoot()}})
}

// MCPToken is the OAuth token endpoint: it swaps an authorization code for
// tokens, and a refresh token for a new pair.
func (a *App) MCPToken(c echo.Context) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, mcpMaxBody)

	clientID := c.FormValue("client_id")
	if _, ok := a.getMCPClient(clientID); !ok {
		return mcpOAuthErr(c, http.StatusUnauthorized, "invalid_client", "Unknown client.")
	}
	if !a.mcpSameResource(c.FormValue("resource")) {
		return mcpOAuthErr(c, http.StatusBadRequest, "invalid_target", "The requested resource is not this listmonk.")
	}

	switch c.FormValue("grant_type") {
	case "authorization_code":
		// A code works once: it is deleted as it is read.
		req := struct {
			UserID      int    `db:"user_id"`
			RedirectURI string `db:"redirect_uri"`
			Challenge   string `db:"code_challenge"`
			Expired     bool   `db:"expired"`
		}{}
		if err := a.db.Get(&req, `DELETE FROM mcp_auth_requests WHERE code_hash = $1 AND client_id = $2
			RETURNING user_id, redirect_uri, code_challenge, (expires_at < NOW()) AS expired`,
			mcpHash(c.FormValue("code")), clientID); err != nil {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_grant", "The authorization code is invalid or already used.")
		}
		if req.Expired {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_grant", "The authorization code has expired.")
		}
		if req.RedirectURI != c.FormValue("redirect_uri") {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_grant", "The redirect URI doesn't match.")
		}

		// PKCE: the verifier must hash to the challenge sent at the start.
		sum := sha256.Sum256([]byte(c.FormValue("code_verifier")))
		got := base64.RawURLEncoding.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(got), []byte(req.Challenge)) != 1 {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_grant", "The code verifier doesn't match.")
		}

		return a.mcpIssueTokens(c, clientID, req.UserID, 0)

	case "refresh_token":
		// Refresh tokens rotate: the old pair is replaced as the new one is issued.
		tk := struct {
			ID     int `db:"id"`
			UserID int `db:"user_id"`
		}{}
		if err := a.db.Get(&tk, `SELECT id, user_id FROM mcp_tokens
			WHERE refresh_hash = $1 AND client_id = $2 AND refresh_expires_at > NOW()`,
			mcpHash(c.FormValue("refresh_token")), clientID); err != nil {
			return mcpOAuthErr(c, http.StatusBadRequest, "invalid_grant", "The refresh token is invalid or has expired.")
		}

		return a.mcpIssueTokens(c, clientID, tk.UserID, tk.ID)
	}

	return mcpOAuthErr(c, http.StatusBadRequest, "unsupported_grant_type", "Use authorization_code or refresh_token.")
}

// mcpIssueTokens creates an access and refresh token pair and writes the
// token response. If replaceID is set, that existing connection is rotated
// in place rather than a new one being added.
func (a *App) mcpIssueTokens(c echo.Context, clientID string, userID, replaceID int) error {
	access, err1 := mcpRandom(32)
	refresh, err2 := mcpRandom(32)
	if err1 != nil || err2 != nil {
		return mcpOAuthErr(c, http.StatusInternalServerError, "server_error", "Could not create tokens.")
	}

	var (
		now        = time.Now()
		accessExp  = now.Add(mcpAccessTTL)
		refreshExp = now.Add(mcpRefreshTTL)
		err        error
	)
	if replaceID > 0 {
		_, err = a.db.Exec(`UPDATE mcp_tokens SET access_hash = $2, refresh_hash = $3,
			access_expires_at = $4, refresh_expires_at = $5 WHERE id = $1`,
			replaceID, mcpHash(access), mcpHash(refresh), accessExp, refreshExp)
	} else {
		_, err = a.db.Exec(`INSERT INTO mcp_tokens (client_id, user_id, access_hash, refresh_hash, access_expires_at, refresh_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			clientID, userID, mcpHash(access), mcpHash(refresh), accessExp, refreshExp)
	}
	if err != nil {
		a.log.Printf("mcp: error saving tokens: %v", err)
		return mcpOAuthErr(c, http.StatusInternalServerError, "server_error", "Could not save tokens.")
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(mcpAccessTTL.Seconds()),
		"refresh_token": refresh,
	})
}

// mcpRandom returns a URL-safe random string from n random bytes.
func mcpRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// mcpHash is how codes and tokens are stored: never in the clear.
func mcpHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// The MCP endpoint.

type mcpRPCReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCErr      `json:"error,omitempty"`
}

// mcpUnauthorized tells the client where to find out how to sign in.
func (a *App) mcpUnauthorized(c echo.Context, desc string) error {
	h := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource%s"`, a.mcpRoot(), mcpPath)
	if desc != "" {
		h += fmt.Sprintf(`, error="invalid_token", error_description="%s"`, desc)
	}
	c.Response().Header().Set("WWW-Authenticate", h)
	return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// mcpAllowedOrigin guards against a web page in someone's browser driving
// the endpoint. Server-side clients send no Origin and are unaffected.
func (a *App) mcpAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if root, err := url.Parse(a.mcpRoot()); err == nil && strings.EqualFold(root.Hostname(), h) {
		return true
	}
	return h == "localhost" || h == "127.0.0.1" ||
		h == "claude.ai" || strings.HasSuffix(h, ".claude.ai") ||
		h == "claude.com" || strings.HasSuffix(h, ".claude.com")
}

// mcpUser resolves the bearer token on a request to the listmonk user it
// was issued to.
func (a *App) mcpUser(c echo.Context) (auth.User, bool) {
	hdr := strings.TrimSpace(c.Request().Header.Get("Authorization"))
	if len(hdr) < 8 || !strings.EqualFold(hdr[:7], "Bearer ") {
		return auth.User{}, false
	}

	tk := struct {
		ID       int       `db:"id"`
		UserID   int       `db:"user_id"`
		LastUsed null.Time `db:"last_used_at"`
	}{}
	if err := a.db.Get(&tk, `SELECT id, user_id, last_used_at FROM mcp_tokens
		WHERE access_hash = $1 AND access_expires_at > NOW()`, mcpHash(strings.TrimSpace(hdr[7:]))); err != nil {
		return auth.User{}, false
	}

	user, err := a.core.GetUser(tk.UserID, "", "")
	if err != nil || user.Status != auth.UserStatusEnabled {
		return auth.User{}, false
	}

	if !tk.LastUsed.Valid || time.Since(tk.LastUsed.Time) > time.Minute {
		a.db.Exec(`UPDATE mcp_tokens SET last_used_at = NOW() WHERE id = $1`, tk.ID)
	}

	return user, true
}

// MCPEndpoint handles one JSON-RPC message per POST and answers with plain
// JSON. There are no sessions and no server-initiated messages, so there is
// no SSE stream.
func (a *App) MCPEndpoint(c echo.Context) error {
	if !a.mcpAllowedOrigin(c.Request().Header.Get("Origin")) {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "origin not allowed"})
	}

	user, ok := a.mcpUser(c)
	if !ok {
		desc := ""
		if c.Request().Header.Get("Authorization") != "" {
			desc = "The access token is invalid or has expired."
		}
		return a.mcpUnauthorized(c, desc)
	}

	if v := c.Request().Header.Get("MCP-Protocol-Version"); v != "" && !mcpSupportsVersion(v) {
		return c.JSON(http.StatusBadRequest, mcpRPCResp{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpRPCErr{Code: -32600, Message: "Unsupported MCP-Protocol-Version: " + v}})
	}

	body, err := io.ReadAll(io.LimitReader(c.Request().Body, mcpMaxBody))
	if err != nil {
		return c.JSON(http.StatusBadRequest, mcpRPCResp{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpRPCErr{Code: -32700, Message: "Could not read the request."}})
	}

	var req mcpRPCReq
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" {
		return c.JSON(http.StatusBadRequest, mcpRPCResp{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &mcpRPCErr{Code: -32700, Message: "The request must be a single JSON-RPC 2.0 message."}})
	}

	// Notifications and responses carry no ID and get no reply.
	if len(req.ID) == 0 || string(req.ID) == "null" || req.Method == "" {
		return c.NoContent(http.StatusAccepted)
	}

	resp := mcpRPCResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		p := struct {
			ProtocolVersion string `json:"protocolVersion"`
		}{}
		json.Unmarshal(req.Params, &p)

		// Agree to the client's version if this server speaks it,
		// otherwise offer the newest one it does.
		v := mcpProtocolVersions[0]
		if mcpSupportsVersion(p.ProtocolVersion) {
			v = p.ProtocolVersion
		}

		resp.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo": map[string]any{
				"name":    mcpServerName,
				"title":   "listmonk",
				"version": versionString,
			},
			"instructions": a.mcpInstructions(),
		}

	case "ping":
		resp.Result = map[string]any{}

	case "tools/list":
		resp.Result = map[string]any{"tools": a.mcpToolList()}

	case "tools/call":
		p := struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}{}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			resp.Error = &mcpRPCErr{Code: -32602, Message: "tools/call needs a tool name."}
			break
		}
		tool, ok := a.mcpFindTool(p.Name)
		if !ok {
			resp.Error = &mcpRPCErr{Code: -32602, Message: "Unknown tool: " + p.Name}
			break
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		resp.Result = a.mcpRunTool(tool, user, p.Arguments)

	default:
		resp.Error = &mcpRPCErr{Code: -32601, Message: "Method not found: " + req.Method}
	}

	return c.JSON(http.StatusOK, resp)
}

func mcpSupportsVersion(v string) bool {
	for _, s := range mcpProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// mcpInstructions is the short brief the client is given on connecting.
func (a *App) mcpInstructions() string {
	s := "This is the user's own listmonk, their e-mail marketing tool: lists, subscribers, campaigns" +
		" (one-off e-mails to a list) and sequences (a timed series sent to each person who joins a list)." +
		" Start with get_overview. Campaigns you create are always drafts. Times are in the timezone get_overview reports."
	if a.mcpAllowSending() {
		s += " Scheduling and sending is switched on here: always show the user the subject, the lists and the" +
			" number of recipients and get a clear yes before you schedule or send anything, or switch a sequence on."
	} else {
		s += " Scheduling and sending is switched off here: you can write and edit drafts, and the user schedules or" +
			" sends them in listmonk. Tell them where to find the draft."
	}
	return s
}

// mcpAllowSending reports whether the connector may schedule and send.
func (a *App) mcpAllowSending() bool {
	var on bool
	if err := a.db.Get(&on, `SELECT allow_sending FROM mcp_config WHERE id = 1`); err != nil {
		return false
	}
	return on
}

// ---------------------------------------------------------------------------
// Admin screen API (Settings > Claude connector).

type mcpConnection struct {
	ID         int       `db:"id" json:"id"`
	ClientName string    `db:"client_name" json:"client_name"`
	Username   string    `db:"username" json:"username"`
	CreatedAt  null.Time `db:"created_at" json:"created_at"`
	LastUsedAt null.Time `db:"last_used_at" json:"last_used_at"`
}

// GetMCPInfo returns the connector's address, its setting and who is connected.
func (a *App) GetMCPInfo(c echo.Context) error {
	conns := []mcpConnection{}
	if err := a.db.Select(&conns, `SELECT t.id, COALESCE(NULLIF(cl.name, ''), 'Unnamed app') AS client_name,
		u.username, t.created_at, t.last_used_at
		FROM mcp_tokens t JOIN mcp_clients cl ON cl.client_id = t.client_id JOIN users u ON u.id = t.user_id
		WHERE t.refresh_expires_at > NOW() ORDER BY t.created_at DESC`); err != nil {
		a.log.Printf("mcp: error fetching connections: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error fetching connections.")
	}

	return c.JSON(http.StatusOK, okResp{map[string]any{
		"url":           a.mcpResource(),
		"allow_sending": a.mcpAllowSending(),
		"connections":   conns,
	}})
}

// UpdateMCPSettings switches "let Claude schedule and send" on or off.
func (a *App) UpdateMCPSettings(c echo.Context) error {
	req := struct {
		AllowSending bool `json:"allow_sending"`
	}{}
	if err := c.Bind(&req); err != nil {
		return err
	}

	if _, err := a.db.Exec(`UPDATE mcp_config SET allow_sending = $1, updated_at = NOW() WHERE id = 1`, req.AllowSending); err != nil {
		a.log.Printf("mcp: error saving settings: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error saving the setting.")
	}

	return a.GetMCPInfo(c)
}

// DeleteMCPConnection disconnects one connected app straight away.
func (a *App) DeleteMCPConnection(c echo.Context) error {
	if _, err := a.db.Exec(`DELETE FROM mcp_tokens WHERE id = $1`, getID(c)); err != nil {
		a.log.Printf("mcp: error deleting connection: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "Error disconnecting.")
	}

	return a.GetMCPInfo(c)
}

// mcpAtoi is a small helper for tools building query strings.
func mcpAtoi(n int) string {
	return strconv.Itoa(n)
}
