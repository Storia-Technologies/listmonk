package main

// The Claude connector's tools. See mcp.go for the overview.
//
// Every tool runs an existing listmonk HTTP handler in-process, as the
// signed-in user, through mcpInvoke. Nothing here talks to the database for
// listmonk's own data, so validation, permissions and side effects are
// exactly what the admin UI and the REST API get.
//
// Rules the tool set keeps to:
//   - Nothing is deleted. There are no delete tools.
//   - Campaigns are only ever created as drafts.
//   - Scheduling, sending and switching a sequence on are refused unless
//     "let Claude schedule and send" is on (mcp_config.allow_sending).
//   - Stopping things (unschedule, pause, switch a sequence off) is always allowed.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/knadh/listmonk/internal/auth"
	"github.com/labstack/echo/v4"
)

const (
	// The longest campaign body handed back to the client in one result.
	mcpMaxBodyOut = 100000

	mcpSendingOffMsg = "Scheduling and sending from Claude is switched off for this listmonk, so nothing was sent or scheduled. " +
		"The user can do it themselves in listmonk, or switch on \"Let Claude schedule and send\" under Settings > Claude connector."
)

type mcpArgs map[string]any

type mcpTool struct {
	Name        string
	Title       string
	Description string
	// JSON Schema properties and the names of the required ones.
	Props    map[string]any
	Required []string
	ReadOnly bool
	// Only offered when sequences are enabled on this instance.
	Sequences bool
	Fn        func(a *App, u auth.User, args mcpArgs) (any, error)
}

// --- Argument helpers --------------------------------------------------------

func (m mcpArgs) str(k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func (m mcpArgs) has(k string) bool {
	v, ok := m[k]
	return ok && v != nil
}

func (m mcpArgs) num(k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		n := 0
		fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		return n
	}
	return 0
}

func (m mcpArgs) boolean(k string) bool {
	b, _ := m[k].(bool)
	return b
}

func (m mcpArgs) nums(k string) []int {
	out := []int{}
	arr, _ := m[k].([]any)
	for _, v := range arr {
		if f, ok := v.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func (m mcpArgs) strs(k string) []string {
	out := []string{}
	arr, _ := m[k].([]any)
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// --- Schema helpers ----------------------------------------------------------

func pStr(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func pInt(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
func pBool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func pInts(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": desc}
}
func pStrs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
func pEnum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}

// --- Running handlers --------------------------------------------------------

// mcpInvoke runs a listmonk HTTP handler in-process as the given user and
// returns the "data" part of its JSON response. target is the path and query
// the handler would normally be reached at; params are its path parameters.
func (a *App) mcpInvoke(u auth.User, method, target string, params map[string]string, body any, h echo.HandlerFunc) (json.RawMessage, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}

	req := httptest.NewRequest(method, target, &buf)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()

	c := mcpEcho.NewContext(req, rec)
	c.Set(auth.UserHTTPCtxKey, u)
	if len(params) > 0 {
		names, vals := make([]string, 0, len(params)), make([]string, 0, len(params))
		for k, v := range params {
			names = append(names, k)
			vals = append(vals, v)
		}
		c.SetParamNames(names...)
		c.SetParamValues(vals...)
	}

	if err := h(c); err != nil {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return nil, fmt.Errorf("%v", he.Message)
		}
		return nil, err
	}
	if rec.Code >= http.StatusBadRequest {
		return nil, fmt.Errorf("listmonk answered %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	out := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("unexpected response from listmonk: %v", err)
	}
	return out.Data, nil
}

// asMap and asList decode a handler's data for trimming.
func asMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// pick keeps only the named keys of an object.
func pick(v any, keys ...string) map[string]any {
	m, _ := v.(map[string]any)
	out := map[string]any{}
	for _, k := range keys {
		if val, ok := m[k]; ok {
			out[k] = val
		}
	}
	return out
}

// pickAll applies pick to every object in a list.
func pickAll(list []any, keys ...string) []any {
	out := make([]any, 0, len(list))
	for _, v := range list {
		out = append(out, pick(v, keys...))
	}
	return out
}

// ids pulls the "id" of every object in a list, as campaigns report their
// lists and media.
func ids(v any) []int {
	out := []int{}
	for _, it := range asList(v) {
		if m, ok := it.(map[string]any); ok {
			if f, ok := m["id"].(float64); ok {
				out = append(out, int(f))
			}
		}
	}
	return out
}

func idParam(id int) map[string]string {
	return map[string]string{"id": mcpAtoi(id)}
}

var (
	mcpCampaignSummary = []string{"id", "name", "subject", "status", "type", "tags", "send_at", "started_at",
		"created_at", "updated_at", "lists", "to_send", "sent", "views", "clicks", "bounces", "content_type", "template_id"}
	mcpSubscriberFields = []string{"id", "email", "name", "status", "attribs", "lists", "created_at"}
)

// --- Tool list and dispatch --------------------------------------------------

// mcpToolList is the tools/list result.
func (a *App) mcpToolList() []map[string]any {
	out := []map[string]any{}
	for _, t := range mcpTools {
		if t.Sequences && !sequencesEnabled() {
			continue
		}

		props := t.Props
		if props == nil {
			props = map[string]any{}
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(t.Required) > 0 {
			schema["required"] = t.Required
		}

		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": schema,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": false,
				"openWorldHint":   false,
			},
		})
	}
	return out
}

func (a *App) mcpFindTool(name string) (mcpTool, bool) {
	for _, t := range mcpTools {
		if t.Name == name && (!t.Sequences || sequencesEnabled()) {
			return t, true
		}
	}
	return mcpTool{}, false
}

// mcpRunTool runs a tool and wraps its outcome as a tools/call result. A
// tool failing is a normal result with isError set, so the model can read
// what went wrong and adjust.
func (a *App) mcpRunTool(t mcpTool, u auth.User, args mcpArgs) (res map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("mcp: tool %s panicked: %v", t.Name, r)
			res = mcpToolText("Something went wrong inside listmonk running this tool.", true)
		}
	}()

	for _, k := range t.Required {
		if !args.has(k) {
			return mcpToolText(fmt.Sprintf("Missing required argument: %s", k), true)
		}
	}

	out, err := t.Fn(a, u, args)
	if err != nil {
		return mcpToolText(err.Error(), true)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return mcpToolText("Could not encode the result.", true)
	}
	return mcpToolText(string(b), false)
}

func mcpToolText(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// campaignLink is where the user finds a campaign in the admin UI.
func (a *App) campaignLink(id any) string {
	return fmt.Sprintf("%s/admin/campaigns/%v", a.mcpRoot(), id)
}

// getCampaignMap fetches a full campaign as the signed-in user.
func (a *App) getCampaignMap(u auth.User, id int) (map[string]any, error) {
	raw, err := a.mcpInvoke(u, http.MethodGet, fmt.Sprintf("/api/campaigns/%d", id), idParam(id), nil,
		a.auth.Perm(hasID(a.GetCampaign), "campaigns:get_all", "campaigns:get"))
	if err != nil {
		return nil, err
	}
	return asMap(raw), nil
}

// campaignBody builds the create/update request for a campaign from tool
// arguments, leaving out anything the caller didn't give.
func campaignBody(args mcpArgs) map[string]any {
	body := map[string]any{}
	for arg, field := range map[string]string{
		"name": "name", "subject": "subject", "body": "body", "content_type": "content_type",
		"from_email": "from_email", "alt_body": "altbody",
	} {
		if args.has(arg) {
			body[field] = args.str(arg)
		}
	}
	if args.has("template_id") {
		body["template_id"] = args.num("template_id")
	}
	if args.has("list_ids") {
		body["lists"] = args.nums("list_ids")
	}
	if args.has("tags") {
		body["tags"] = args.strs("tags")
	}
	if args.has("send_at") {
		if s := args.str("send_at"); s == "" {
			body["send_at"] = nil
		} else {
			body["send_at"] = s
		}
	}
	return body
}

const (
	mcpContentTypeDesc = "How the body is written: \"html\" (full control), \"markdown\" or \"richtext\" (simple HTML) are wrapped in the chosen template; \"plain\" is plain text."
	mcpBodyDesc        = "The e-mail body. May use listmonk template tags such as {{ .Subscriber.FirstName }}, {{ .Subscriber.Name }} and {{ UnsubscribeURL }}. The template normally adds the unsubscribe footer."
	mcpSendAtDesc      = "When it should go out, as ISO 8601 with a UTC offset, e.g. 2026-12-03T09:00:00+08:00. Storing a time on a draft does not schedule it."
)

var mcpTools = []mcpTool{
	// ----- Overview -----
	{
		Name: "get_overview", Title: "Overview", ReadOnly: true,
		Description: "Start here. Totals for subscribers, lists and campaigns, who you are signed in as, this listmonk's address, the current time and timezone, and whether you are allowed to schedule and send.",
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/dashboard/counts", nil, nil, a.GetDashboardCounts)
			if err != nil {
				return nil, err
			}
			now := time.Now()
			zone, _ := now.Zone()
			return map[string]any{
				"listmonk_url":      a.mcpRoot() + "/admin",
				"signed_in_as":      u.Username,
				"now":               now.Format(time.RFC3339),
				"timezone":          fmt.Sprintf("%s (%s)", now.Location().String(), zone),
				"sending_allowed":   a.mcpAllowSending(),
				"sequences_enabled": sequencesEnabled(),
				"default_from":      a.cfg.FromEmail,
				"counts":            asMap(raw),
			}, nil
		},
	},

	// ----- Lists -----
	{
		Name: "list_lists", Title: "List the lists", ReadOnly: true,
		Description: "Every list with its subscriber count, type (public or private) and opt-in (single or double).",
		Props:       map[string]any{"query": pStr("Optional text to search list names for.")},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			q := url.Values{"per_page": {"all"}}
			if s := args.str("query"); s != "" {
				q.Set("query", s)
			}
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/lists?"+q.Encode(), nil, nil, a.GetLists)
			if err != nil {
				return nil, err
			}
			res := asMap(raw)
			return map[string]any{
				"total": res["total"],
				"lists": pickAll(asList(res["results"]), "id", "name", "type", "optin", "status", "tags", "description", "subscriber_count", "created_at"),
			}, nil
		},
	},
	{
		Name: "create_list", Title: "Create a list",
		Description: "Create a new list. Use a private, single opt-in list unless the user wants people to be able to sign up to it publicly or to confirm by e-mail.",
		Props: map[string]any{
			"name":        pStr("The list's name."),
			"type":        pEnum("private (default) or public. Public lists appear on the public sign-up form.", "private", "public"),
			"optin":       pEnum("single (default) or double. Double sends a confirmation e-mail first.", "single", "double"),
			"description": pStr("Optional description."),
			"tags":        pStrs("Optional tags."),
		},
		Required: []string{"name"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			body := map[string]any{"name": args.str("name"), "type": "private", "optin": "single",
				"description": args.str("description"), "tags": args.strs("tags")}
			if v := args.str("type"); v != "" {
				body["type"] = v
			}
			if v := args.str("optin"); v != "" {
				body["optin"] = v
			}
			raw, err := a.mcpInvoke(u, http.MethodPost, "/api/lists", nil, body, a.auth.Perm(a.CreateList, "lists:manage_all"))
			if err != nil {
				return nil, err
			}
			return pick(asMap(raw), "id", "name", "type", "optin", "tags", "description"), nil
		},
	},

	// ----- Subscribers -----
	{
		Name: "search_subscribers", Title: "Search subscribers", ReadOnly: true,
		Description: "Find subscribers by name or e-mail, optionally within one list. Returns up to 50 per page with each person's lists. Use the total to report counts rather than paging through everyone.",
		Props: map[string]any{
			"search":              pStr("Part of a name or e-mail address. Leave out to list everyone."),
			"list_id":             pInt("Only people on this list."),
			"subscription_status": pEnum("With list_id: only people with this subscription status on that list.", "unconfirmed", "confirmed", "unsubscribed"),
			"page":                pInt("Page number, starting at 1."),
			"per_page":            pInt("Results per page, 1 to 50. Default 25."),
			"newest_first":        pBool("Order by when they were added, newest first."),
		},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			per := args.num("per_page")
			if per < 1 || per > 50 {
				per = 25
			}
			page := args.num("page")
			if page < 1 {
				page = 1
			}
			q := url.Values{"per_page": {mcpAtoi(per)}, "page": {mcpAtoi(page)}}
			if s := args.str("search"); s != "" {
				q.Set("search", s)
			}
			if id := args.num("list_id"); id > 0 {
				q.Set("list_id", mcpAtoi(id))
				if s := args.str("subscription_status"); s != "" {
					q.Set("subscription_status", s)
				}
			}
			if args.boolean("newest_first") {
				q.Set("order_by", "created_at")
				q.Set("order", "DESC")
			}
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/subscribers?"+q.Encode(), nil, nil,
				a.auth.Perm(a.QuerySubscribers, "subscribers:get_all", "subscribers:get"))
			if err != nil {
				return nil, err
			}
			res := asMap(raw)
			subs := pickAll(asList(res["results"]), mcpSubscriberFields...)
			for _, s := range subs {
				m := s.(map[string]any)
				m["lists"] = pickAll(asList(m["lists"]), "id", "name", "subscription_status")
			}
			return map[string]any{"total": res["total"], "page": res["page"], "per_page": res["per_page"], "subscribers": subs}, nil
		},
	},
	{
		Name: "add_subscriber", Title: "Add a subscriber",
		Description: "Add one person and put them on lists. Only add people who have agreed to receive e-mail. Fails if the e-mail address already exists: use change_subscriber_lists for existing people. Adding someone to a list that has a sequence switched on starts that sequence for them.",
		Props: map[string]any{
			"email":      pStr("Their e-mail address."),
			"name":       pStr("Their name."),
			"list_ids":   pInts("Lists to add them to."),
			"attributes": map[string]any{"type": "object", "description": "Optional extra fields to store on the subscriber, as an object."},
			"preconfirm": pBool("For double opt-in lists only: true marks them confirmed without sending the confirmation e-mail. Use only when they have already confirmed some other way. Default false."),
		},
		Required: []string{"email", "list_ids"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			body := map[string]any{"email": args.str("email"), "name": args.str("name"), "status": "enabled",
				"lists": args.nums("list_ids"), "preconfirm_subscriptions": args.boolean("preconfirm")}
			if attrs, ok := args["attributes"].(map[string]any); ok {
				body["attribs"] = attrs
			}
			raw, err := a.mcpInvoke(u, http.MethodPost, "/api/subscribers", nil, body,
				a.auth.Perm(a.CreateSubscriber, "subscribers:manage"))
			if err != nil {
				return nil, err
			}
			return pick(asMap(raw), mcpSubscriberFields...), nil
		},
	},
	{
		Name: "change_subscriber_lists", Title: "Change subscribers' lists",
		Description: "Add existing subscribers to lists, remove them from lists, or mark them unsubscribed from lists. \"remove\" takes them off quietly and they can be re-added later; \"unsubscribe\" records that they opted out. Adding someone to a list that has a sequence switched on starts that sequence for them.",
		Props: map[string]any{
			"subscriber_ids": pInts("The subscribers' IDs, from search_subscribers."),
			"action":         pEnum("What to do.", "add", "remove", "unsubscribe"),
			"list_ids":       pInts("The lists to act on."),
		},
		Required: []string{"subscriber_ids", "action", "list_ids"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			subs, lists := args.nums("subscriber_ids"), args.nums("list_ids")
			if len(subs) == 0 || len(lists) == 0 {
				return nil, errors.New("Give at least one subscriber ID and one list ID.")
			}
			if len(subs) > 500 {
				return nil, errors.New("At most 500 subscribers per call.")
			}
			body := map[string]any{"ids": subs, "action": args.str("action"), "target_list_ids": lists, "status": "unconfirmed"}
			if _, err := a.mcpInvoke(u, http.MethodPut, "/api/subscribers/lists", nil, body,
				a.auth.Perm(a.ManageSubscriberLists, "subscribers:manage")); err != nil {
				return nil, err
			}
			return map[string]any{"done": true, "action": args.str("action"), "subscribers": len(subs), "lists": lists}, nil
		},
	},

	// ----- Campaigns -----
	{
		Name: "list_campaigns", Title: "List campaigns", ReadOnly: true,
		Description: "Campaigns, newest first, with status, send time, lists and results (sent, views, clicks, bounces). Bodies are left out: use get_campaign for one.",
		Props: map[string]any{
			"status":   pEnum("Only campaigns with this status.", "draft", "scheduled", "running", "paused", "finished", "cancelled"),
			"query":    pStr("Text to search names and subjects for."),
			"page":     pInt("Page number, starting at 1."),
			"per_page": pInt("Results per page, 1 to 50. Default 25."),
		},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			per := args.num("per_page")
			if per < 1 || per > 50 {
				per = 25
			}
			page := args.num("page")
			if page < 1 {
				page = 1
			}
			q := url.Values{"no_body": {"true"}, "per_page": {mcpAtoi(per)}, "page": {mcpAtoi(page)},
				"order_by": {"created_at"}, "order": {"DESC"}}
			if s := args.str("status"); s != "" {
				q.Set("status", s)
			}
			if s := args.str("query"); s != "" {
				q.Set("query", s)
			}
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/campaigns?"+q.Encode(), nil, nil,
				a.auth.Perm(a.GetCampaigns, "campaigns:get_all", "campaigns:get"))
			if err != nil {
				return nil, err
			}
			res := asMap(raw)
			return map[string]any{"total": res["total"], "page": res["page"], "per_page": res["per_page"],
				"campaigns": pickAll(asList(res["results"]), mcpCampaignSummary...)}, nil
		},
	},
	{
		Name: "get_campaign", Title: "Get a campaign", ReadOnly: true,
		Description: "One campaign in full, including its body, results and a link to open it in listmonk.",
		Props:       map[string]any{"id": pInt("The campaign's ID.")},
		Required:    []string{"id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			camp, err := a.getCampaignMap(u, args.num("id"))
			if err != nil {
				return nil, err
			}
			out := pick(camp, append(mcpCampaignSummary, "from_email", "body", "altbody", "messenger")...)
			if b, ok := out["body"].(string); ok && len(b) > mcpMaxBodyOut {
				out["body"] = b[:mcpMaxBodyOut]
				out["body_truncated"] = true
			}
			out["open_in_listmonk"] = a.campaignLink(args.num("id"))
			return out, nil
		},
	},
	{
		Name: "list_templates", Title: "List templates", ReadOnly: true,
		Description: "The e-mail templates campaigns can be wrapped in, and which one is the default. Use type \"campaign\" templates for campaigns.",
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/templates?no_body=true", nil, nil,
				a.auth.Perm(a.GetTemplates, "templates:get"))
			if err != nil {
				return nil, err
			}
			var list []any
			json.Unmarshal(raw, &list)
			return map[string]any{"templates": pickAll(list, "id", "name", "type", "is_default")}, nil
		},
	},
	{
		Name: "create_campaign", Title: "Create a draft campaign",
		Description: "Create a campaign as a draft. It is never sent or scheduled by this tool. Check list_templates and an earlier campaign of the user's (get_campaign) first so the new one matches how they normally write and which template they use.",
		Props: map[string]any{
			"name":         pStr("Internal name, shown only in listmonk."),
			"subject":      pStr("The e-mail subject line."),
			"list_ids":     pInts("The lists it will go to."),
			"body":         pStr(mcpBodyDesc),
			"content_type": pEnum(mcpContentTypeDesc+" Default html.", "html", "markdown", "richtext", "plain"),
			"template_id":  pInt("The template to wrap it in. Leave out for the default template."),
			"from_email":   pStr("Sender, as \"Name <address>\". Leave out for this listmonk's default sender."),
			"alt_body":     pStr("Optional plain-text version."),
			"send_at":      pStr(mcpSendAtDesc),
			"tags":         pStrs("Optional tags."),
		},
		Required: []string{"name", "subject", "list_ids", "body"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			body := campaignBody(args)
			body["type"] = "regular"
			body["messenger"] = "email"
			if _, ok := body["content_type"]; !ok || body["content_type"] == "" {
				body["content_type"] = "html"
			}
			if _, ok := body["from_email"]; !ok || body["from_email"] == "" {
				body["from_email"] = a.cfg.FromEmail
			}
			raw, err := a.mcpInvoke(u, http.MethodPost, "/api/campaigns", nil, body,
				a.auth.Perm(a.CreateCampaign, "campaigns:manage_all", "campaigns:manage"))
			if err != nil {
				return nil, err
			}
			out := pick(asMap(raw), mcpCampaignSummary...)
			out["open_in_listmonk"] = a.campaignLink(out["id"])
			out["note"] = "Saved as a draft. Nothing has been sent or scheduled."
			return out, nil
		},
	},
	{
		Name: "update_campaign", Title: "Edit a campaign",
		Description: "Change a draft, scheduled or paused campaign. Only the fields you give are changed. This also edits the e-mail of a sequence step (use the step's campaign_id). It does not schedule or send.",
		Props: map[string]any{
			"id":           pInt("The campaign's ID."),
			"name":         pStr("Internal name."),
			"subject":      pStr("Subject line."),
			"list_ids":     pInts("Replace the lists it goes to."),
			"body":         pStr(mcpBodyDesc),
			"content_type": pEnum(mcpContentTypeDesc, "html", "markdown", "richtext", "plain"),
			"template_id":  pInt("The template to wrap it in."),
			"from_email":   pStr("Sender, as \"Name <address>\"."),
			"alt_body":     pStr("Plain-text version."),
			"send_at":      pStr(mcpSendAtDesc + " An empty string clears it."),
			"tags":         pStrs("Replace the tags."),
		},
		Required: []string{"id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			return a.mcpUpdateCampaign(u, args.num("id"), campaignBody(args))
		},
	},
	{
		Name: "send_test_email", Title: "Send a test e-mail",
		Description: "Send a campaign as a test to specific addresses so the user can see it in their inbox. Each address must already be a subscriber in this listmonk. This does not send to the campaign's lists.",
		Props: map[string]any{
			"campaign_id": pInt("The campaign to test."),
			"emails":      pStrs("Addresses to send the test to. Normally the user's own."),
		},
		Required: []string{"campaign_id", "emails"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			id, emails := args.num("campaign_id"), args.strs("emails")
			if len(emails) == 0 || len(emails) > 5 {
				return nil, errors.New("Give between 1 and 5 test addresses.")
			}
			camp, err := a.getCampaignMap(u, id)
			if err != nil {
				return nil, err
			}
			// The test handler sends whatever content is in the request, so
			// it is given the campaign exactly as saved.
			body := pick(camp, "name", "subject", "from_email", "body", "altbody", "messenger", "content_type", "headers", "template_id", "tags")
			body["type"] = "regular"
			body["lists"] = ids(camp["lists"])
			body["media"] = ids(camp["media"])
			body["subscribers"] = emails
			if _, err := a.mcpInvoke(u, http.MethodPost, fmt.Sprintf("/api/campaigns/%d/test", id), idParam(id), body,
				a.auth.Perm(hasID(a.TestCampaign), "campaigns:manage_all", "campaigns:manage")); err != nil {
				return nil, err
			}
			return map[string]any{"sent_test_to": emails}, nil
		},
	},
	{
		Name: "schedule_campaign", Title: "Schedule or send a campaign",
		Description: "Schedule a draft campaign for a time, or start sending it now. This sends real e-mail to everyone on the campaign's lists. Only works when the user has switched on \"Let Claude schedule and send\" in listmonk. Before calling it, show the user the subject, the lists, the number of recipients and the time, and get a clear yes.",
		Props: map[string]any{
			"id":   pInt("The campaign's ID."),
			"when": pStr("\"now\" to start sending immediately, or a time as ISO 8601 with a UTC offset, e.g. 2026-12-03T09:00:00+08:00."),
		},
		Required: []string{"id", "when"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			if !a.mcpAllowSending() {
				return nil, errors.New(mcpSendingOffMsg + " The draft is at " + a.campaignLink(args.num("id")))
			}
			id, when := args.num("id"), args.str("when")

			status := "running"
			if !strings.EqualFold(when, "now") {
				t, err := time.Parse(time.RFC3339, when)
				if err != nil {
					return nil, errors.New("\"when\" must be \"now\" or an ISO 8601 time with a UTC offset, e.g. 2026-12-03T09:00:00+08:00.")
				}
				if time.Until(t) < time.Minute {
					return nil, errors.New("That time has already passed. Use \"now\" to send straight away.")
				}
				if _, err := a.mcpUpdateCampaign(u, id, map[string]any{"send_at": when}); err != nil {
					return nil, err
				}
				status = "scheduled"
			}

			raw, err := a.mcpInvoke(u, http.MethodPut, fmt.Sprintf("/api/campaigns/%d/status", id), idParam(id),
				map[string]any{"status": status}, a.auth.Perm(hasID(a.UpdateCampaignStatus), "campaigns:send"))
			if err != nil {
				return nil, err
			}
			out := pick(asMap(raw), "id", "name", "subject", "status", "send_at", "lists")
			out["open_in_listmonk"] = a.campaignLink(id)
			return out, nil
		},
	},
	{
		Name: "stop_campaign", Title: "Unschedule or pause a campaign",
		Description: "Stop a campaign going out: a scheduled campaign goes back to draft, a campaign that is sending is paused. Always allowed.",
		Props:       map[string]any{"id": pInt("The campaign's ID.")},
		Required:    []string{"id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			id := args.num("id")
			camp, err := a.getCampaignMap(u, id)
			if err != nil {
				return nil, err
			}
			var to string
			switch camp["status"] {
			case "scheduled":
				to = "draft"
			case "running":
				to = "paused"
			default:
				return nil, fmt.Errorf("This campaign is %v, so there is nothing to stop.", camp["status"])
			}
			raw, err := a.mcpInvoke(u, http.MethodPut, fmt.Sprintf("/api/campaigns/%d/status", id), idParam(id),
				map[string]any{"status": to}, a.auth.Perm(hasID(a.UpdateCampaignStatus), "campaigns:send"))
			if err != nil {
				return nil, err
			}
			return pick(asMap(raw), "id", "name", "status", "send_at"), nil
		},
	},

	// ----- Sequences -----
	{
		Name: "list_sequences", Title: "List sequences", ReadOnly: true, Sequences: true,
		Description: "Sequences (drip campaigns): a timed series of e-mails sent to each person who joins a list. Shows whether each is on, its list, and how many people are in progress, finished or left early.",
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			raw, err := a.mcpInvoke(u, http.MethodGet, "/api/sequences", nil, nil,
				a.auth.Perm(a.GetSequences, "campaigns:get_all", "campaigns:get"))
			if err != nil {
				return nil, err
			}
			var list []any
			json.Unmarshal(raw, &list)
			return map[string]any{"sequences": list}, nil
		},
	},
	{
		Name: "get_sequence", Title: "Get a sequence", ReadOnly: true, Sequences: true,
		Description: "One sequence with its e-mails in order. Each step has the wait before it in minutes (after joining for the first, after the previous e-mail for the rest), its campaign_id (use get_campaign and update_campaign to read and edit the e-mail) and its sent, opens and clicks.",
		Props:       map[string]any{"id": pInt("The sequence's ID.")},
		Required:    []string{"id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			id := args.num("id")
			raw, err := a.mcpInvoke(u, http.MethodGet, fmt.Sprintf("/api/sequences/%d", id), idParam(id), nil,
				a.auth.Perm(hasID(a.GetSequence), "campaigns:get_all", "campaigns:get"))
			if err != nil {
				return nil, err
			}
			out := asMap(raw)
			out["open_in_listmonk"] = fmt.Sprintf("%s/admin/sequences/%d", a.mcpRoot(), id)
			return out, nil
		},
	},
	{
		Name: "list_sequence_people", Title: "People in a sequence", ReadOnly: true, Sequences: true,
		Description: "Who is in a sequence: how many e-mails each has had, when the next is due, and why anyone left early.",
		Props: map[string]any{
			"id":     pInt("The sequence's ID."),
			"status": pEnum("Only people with this status.", "active", "completed", "exited"),
			"page":   pInt("Page number, starting at 1. 50 per page."),
		},
		Required: []string{"id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			id, page := args.num("id"), args.num("page")
			if page < 1 {
				page = 1
			}
			q := url.Values{"per_page": {"50"}, "page": {mcpAtoi(page)}, "status": {args.str("status")}}
			raw, err := a.mcpInvoke(u, http.MethodGet, fmt.Sprintf("/api/sequences/%d/subscribers?%s", id, q.Encode()), idParam(id), nil,
				a.auth.Perm(hasID(a.GetSequenceSubscribers), "campaigns:get_all", "campaigns:get"))
			if err != nil {
				return nil, err
			}
			return asMap(raw), nil
		},
	},
	{
		Name: "create_sequence", Title: "Create a sequence", Sequences: true,
		Description: "Create a sequence that starts when someone joins a list. It is created switched off with no e-mails: add them with add_sequence_email. Only people who join the list after it is switched on get it, never the people already on the list.",
		Props: map[string]any{
			"name":          pStr("The sequence's name."),
			"list_id":       pInt("The list that starts it."),
			"exit_list_ids": pInts("Optional. People on any of these lists stop getting the sequence, e.g. a clients list."),
		},
		Required: []string{"name", "list_id"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			body := map[string]any{"name": args.str("name"), "list_id": args.num("list_id"), "exit_list_ids": args.nums("exit_list_ids")}
			raw, err := a.mcpInvoke(u, http.MethodPost, "/api/sequences", nil, body,
				a.auth.Perm(a.CreateSequence, "campaigns:manage_all", "campaigns:manage"))
			if err != nil {
				return nil, err
			}
			out := asMap(raw)
			out["open_in_listmonk"] = fmt.Sprintf("%s/admin/sequences/%v", a.mcpRoot(), out["id"])
			return out, nil
		},
	},
	{
		Name: "add_sequence_email", Title: "Add an e-mail to a sequence", Sequences: true,
		Description: "Write the next e-mail in a sequence and add it to the end. The first e-mail usually waits 0 minutes (sent within a minute of joining); later ones wait a day or more after the one before (1440 minutes = 1 day). If the sequence is already on, people part-way through will get the new e-mail.",
		Props: map[string]any{
			"sequence_id":  pInt("The sequence's ID."),
			"subject":      pStr("The e-mail subject line."),
			"body":         pStr(mcpBodyDesc),
			"wait_minutes": pInt("How long to wait before sending it: after joining for the first e-mail, after the previous e-mail for the rest. 1440 = 1 day."),
			"content_type": pEnum(mcpContentTypeDesc+" Default html.", "html", "markdown", "richtext", "plain"),
			"template_id":  pInt("The template to wrap it in. Leave out for the default template."),
			"from_email":   pStr("Sender, as \"Name <address>\". Leave out for the default sender."),
		},
		Required: []string{"sequence_id", "subject", "body", "wait_minutes"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			sid := args.num("sequence_id")
			raw, err := a.mcpInvoke(u, http.MethodGet, fmt.Sprintf("/api/sequences/%d", sid), idParam(sid), nil,
				a.auth.Perm(hasID(a.GetSequence), "campaigns:get_all", "campaigns:get"))
			if err != nil {
				return nil, err
			}
			seq := asMap(raw)
			n := len(asList(seq["steps"])) + 1

			body := campaignBody(args)
			body["name"] = fmt.Sprintf("%v: e-mail %d", seq["name"], n)
			body["lists"] = []any{seq["list_id"]}
			body["type"] = "regular"
			body["messenger"] = "email"
			body["tags"] = []string{"sequence"}
			if _, ok := body["content_type"]; !ok || body["content_type"] == "" {
				body["content_type"] = "html"
			}
			if _, ok := body["from_email"]; !ok || body["from_email"] == "" {
				body["from_email"] = a.cfg.FromEmail
			}
			raw, err = a.mcpInvoke(u, http.MethodPost, "/api/campaigns", nil, body,
				a.auth.Perm(a.CreateCampaign, "campaigns:manage_all", "campaigns:manage"))
			if err != nil {
				return nil, err
			}
			camp := asMap(raw)

			step := map[string]any{"campaign_id": camp["id"], "delay_minutes": args.num("wait_minutes")}
			raw, err = a.mcpInvoke(u, http.MethodPost, fmt.Sprintf("/api/sequences/%d/steps", sid), idParam(sid), step,
				a.auth.Perm(hasID(a.CreateSequenceStep), "campaigns:manage_all", "campaigns:manage"))
			if err != nil {
				return nil, fmt.Errorf("The e-mail was saved as draft campaign %v but could not be added to the sequence: %v", camp["id"], err)
			}
			out := asMap(raw)
			out["added_campaign_id"] = camp["id"]
			out["open_in_listmonk"] = fmt.Sprintf("%s/admin/sequences/%d", a.mcpRoot(), sid)
			return out, nil
		},
	},
	{
		Name: "set_sequence_email_wait", Title: "Change a sequence e-mail's wait", Sequences: true,
		Description: "Change how long a sequence waits before one of its e-mails. To change the e-mail's content use update_campaign with the step's campaign_id.",
		Props: map[string]any{
			"sequence_id":  pInt("The sequence's ID."),
			"step_id":      pInt("The step's ID, from get_sequence."),
			"wait_minutes": pInt("The new wait in minutes. 1440 = 1 day."),
		},
		Required: []string{"sequence_id", "step_id", "wait_minutes"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			sid, step := args.num("sequence_id"), args.num("step_id")
			raw, err := a.mcpInvoke(u, http.MethodPut, fmt.Sprintf("/api/sequences/%d/steps/%d", sid, step),
				map[string]string{"id": mcpAtoi(sid), "stepID": mcpAtoi(step)},
				map[string]any{"delay_minutes": args.num("wait_minutes")},
				a.auth.Perm(hasID(a.UpdateSequenceStep), "campaigns:manage_all", "campaigns:manage"))
			if err != nil {
				return nil, err
			}
			return asMap(raw), nil
		},
	},
	{
		Name: "set_sequence_status", Title: "Switch a sequence on or off", Sequences: true,
		Description: "Switch a sequence on or off. Switching off is always allowed and pauses it for everyone. Switching on starts sending real e-mail to everyone who joins its list from then on, so it only works when the user has switched on \"Let Claude schedule and send\" in listmonk: read the e-mails back to the user and get a clear yes first.",
		Props: map[string]any{
			"id": pInt("The sequence's ID."),
			"on": pBool("true to switch it on, false to switch it off."),
		},
		Required: []string{"id", "on"},
		Fn: func(a *App, u auth.User, args mcpArgs) (any, error) {
			id, status := args.num("id"), seqStatusPaused
			if args.boolean("on") {
				if !a.mcpAllowSending() {
					return nil, errors.New(mcpSendingOffMsg + fmt.Sprintf(" The sequence is at %s/admin/sequences/%d", a.mcpRoot(), id))
				}
				status = seqStatusActive
			}
			raw, err := a.mcpInvoke(u, http.MethodPut, fmt.Sprintf("/api/sequences/%d/status", id), idParam(id),
				map[string]any{"status": status}, a.auth.Perm(hasID(a.UpdateSequenceStatus), "campaigns:send"))
			if err != nil {
				return nil, err
			}
			return asMap(raw), nil
		},
	},
}

// mcpUpdateCampaign applies a partial change to a campaign. listmonk's
// update handler replaces a campaign's lists and attachments with whatever
// is in the request, so the current ones are sent back unless being changed.
func (a *App) mcpUpdateCampaign(u auth.User, id int, changes map[string]any) (any, error) {
	if len(changes) == 0 {
		return nil, errors.New("Nothing to change: give at least one field.")
	}

	camp, err := a.getCampaignMap(u, id)
	if err != nil {
		return nil, err
	}
	if _, ok := changes["lists"]; !ok {
		changes["lists"] = ids(camp["lists"])
	}
	changes["media"] = ids(camp["media"])

	raw, err := a.mcpInvoke(u, http.MethodPut, fmt.Sprintf("/api/campaigns/%d", id), idParam(id), changes,
		a.auth.Perm(hasID(a.UpdateCampaign), "campaigns:manage_all", "campaigns:manage"))
	if err != nil {
		return nil, err
	}
	out := pick(asMap(raw), mcpCampaignSummary...)
	out["open_in_listmonk"] = a.campaignLink(id)
	return out, nil
}
