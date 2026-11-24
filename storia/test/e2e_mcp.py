"""End-to-end check of the Claude connector (MCP) against the local test stack.

    docker compose -p seqtest -f storia/test/docker-compose.yml up -d
    python storia/test/e2e_mcp.py <api token from `docker logs seqtest_app`>

It plays the part of Claude: discovers the server, registers, signs in as the
admin user through the real login and approval pages, swaps the code for
tokens, then drives every tool. Takes a few seconds. Nothing leaves the
machine (mail goes to Mailpit).
"""
import base64
import hashlib
from http import cookiejar
import json
import re
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "http://localhost:9100"
MCP = BASE + "/mcp"
MAIL = "http://localhost:8125"
API_TOKEN = sys.argv[1]
ADMIN_USER, ADMIN_PASS = "admin", "storia-test-1"
REDIRECT = "http://localhost:8765/callback"
RUN = str(int(time.time()))[-6:]

failures = []


def check(name, cond, detail=""):
    print(("PASS  " if cond else "FAIL  ") + name + (f"  [{detail}]" if detail and not cond else ""))
    if not cond:
        failures.append(name)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def opener(jar=None):
    handlers = [NoRedirect()]
    if jar is not None:
        handlers.append(urllib.request.HTTPCookieProcessor(jar))
    return urllib.request.build_opener(*handlers)


def http(method, url, *, headers=None, body=None, form=None, jar=None):
    """One request, no redirects followed. Returns (status, headers, text)."""
    data = None
    headers = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with opener(jar).open(req, timeout=30) as r:
            return r.status, r.headers, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()


def js(text):
    try:
        return json.loads(text)
    except Exception:
        return {}


def admin_api(method, path, body=None):
    auth = "Basic " + base64.b64encode(f"api:{API_TOKEN}".encode()).decode()
    status, _, text = http(method, BASE + path, headers={"Authorization": auth}, body=body)
    assert status == 200, f"{method} {path} -> {status}: {text}"
    return js(text)["data"]


_rpc_id = 0


def rpc(token, method, params=None, *, raw=False, headers=None):
    global _rpc_id
    _rpc_id += 1
    h = {"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25"}
    if token:
        h["Authorization"] = "Bearer " + token
    h.update(headers or {})
    msg = {"jsonrpc": "2.0", "id": _rpc_id, "method": method}
    if params is not None:
        msg["params"] = params
    status, hdrs, text = http("POST", MCP, headers=h, body=msg)
    if raw:
        return status, hdrs, text
    assert status == 200, f"{method} -> {status}: {text}"
    return js(text)


def tool(token, name, args=None):
    """Call a tool. Returns (is_error, decoded result or error text)."""
    res = rpc(token, "tools/call", {"name": name, "arguments": args or {}})
    assert "result" in res, res
    text = res["result"]["content"][0]["text"]
    if res["result"]["isError"]:
        return True, text
    return False, json.loads(text)


def pkce():
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    return verifier, challenge


def authorize_url(client_id, challenge, state, **extra):
    q = {"response_type": "code", "client_id": client_id, "redirect_uri": REDIRECT, "state": state,
         "code_challenge": challenge, "code_challenge_method": "S256", "resource": MCP}
    q.update(extra)
    return BASE + "/mcp/oauth/authorize?" + urllib.parse.urlencode(q)


def sign_in_and_approve(client_id, decision="approve"):
    """The browser part: login page, approval page, the person's decision.
    Returns (verifier, params on the redirect back to the client)."""
    verifier, challenge = pkce()
    state = secrets.token_urlsafe(8)
    url = authorize_url(client_id, challenge, state)
    jar = cookiejar.CookieJar()

    # Not signed in yet: sent to the login page, which sends us back afterwards.
    status, hdrs, _ = http("GET", url, jar=jar)
    assert status == 307 and "/admin/login" in hdrs["Location"], (status, hdrs.get("Location"))
    next_ = urllib.parse.parse_qs(urllib.parse.urlparse(hdrs["Location"]).query)["next"][0]

    status, hdrs, text = http("POST", BASE + "/admin/login", jar=jar,
                              form={"username": ADMIN_USER, "password": ADMIN_PASS, "next": next_})
    assert status == 302, f"login -> {status}: {text[:200]}"
    # listmonk's login drops the query from the return address, so the
    # request is parked and picked up again at a resume address.
    resume = urllib.parse.urljoin(BASE, hdrs["Location"])
    assert "/mcp/oauth/resume/" in resume, f"login didn't return to the connection: {resume}"
    status, hdrs, text = http("GET", resume, jar=jar)
    assert status == 302, f"resume -> {status}: {text[:200]}"
    back = urllib.parse.urljoin(BASE, hdrs["Location"])
    assert "/mcp/oauth/authorize?" in back and "code_challenge" in back, f"login lost the way back: {back}"
    assert http("GET", resume, jar=jar)[0] == 400, "a resume address must only work once"

    status, _, page = http("GET", back, jar=jar)
    assert status == 200, f"approval page -> {status}: {page[:300]}"
    m = re.search(r'name="request_id" value="([^"]+)"', page)
    assert m, page[:500]

    status, hdrs, text = http("POST", BASE + "/mcp/oauth/authorize", jar=jar,
                              headers={"Origin": BASE}, form={"request_id": m.group(1), "decision": decision})
    assert status == 302, f"decision -> {status}: {text[:300]}"
    params = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(hdrs["Location"]).query))
    assert hdrs["Location"].startswith(REDIRECT), hdrs["Location"]
    assert params.get("state") == state, params
    return verifier, params, page, jar


def token_request(**form):
    status, _, text = http("POST", BASE + "/mcp/oauth/token", form=form)
    return status, js(text)


# --- Setup: point mail at Mailpit and set the public address -------------------
settings = admin_api("GET", "/api/settings")
settings["app.root_url"] = BASE
settings["smtp"][0].update({"enabled": True, "host": "mail", "port": 1025, "auth_protocol": "none",
                            "tls_type": "none", "username": "", "password": ""})
http("PUT", BASE + "/api/settings", body=settings,
     headers={"Authorization": "Basic " + base64.b64encode(f"api:{API_TOKEN}".encode()).decode()})
time.sleep(3)
for _ in range(60):
    try:
        if http("GET", BASE + "/admin/login")[0] == 200:
            break
    except Exception:
        pass
    time.sleep(2)
admin_api("PUT", "/api/mcp/settings", {"allow_sending": False})

# --- Discovery ---------------------------------------------------------------
status, hdrs, text = rpc(None, "initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                                              "clientInfo": {"name": "e2e", "version": "0"}}, raw=True)
www = hdrs.get("WWW-Authenticate", "")
check("no token: 401 with a pointer to the sign-in details", status == 401 and "resource_metadata=" in www, f"{status} {www}")
prm_url = re.search(r'resource_metadata="([^"]+)"', www).group(1)
check("pointer is the path form of the metadata URL", prm_url == BASE + "/.well-known/oauth-protected-resource/mcp", prm_url)

_, _, text = http("GET", prm_url)
prm = js(text)
check("resource metadata names this endpoint and its authorization server",
      prm.get("resource") == MCP and prm.get("authorization_servers") == [BASE], str(prm))
check("root form of the metadata URL also works",
      js(http("GET", BASE + "/.well-known/oauth-protected-resource")[2]).get("resource") == MCP)

_, _, text = http("GET", BASE + "/.well-known/oauth-authorization-server")
asm = js(text)
check("authorization server metadata is complete",
      asm.get("issuer") == BASE and asm.get("code_challenge_methods_supported") == ["S256"]
      and all(asm.get(k, "").startswith(BASE + "/mcp/oauth/") for k in
              ("authorization_endpoint", "token_endpoint", "registration_endpoint")), str(asm))

check("GET on the endpoint is 405", http("GET", MCP)[0] == 405)

# --- Registration ------------------------------------------------------------
status, _, text = http("POST", asm["registration_endpoint"], body={"client_name": "Claude (test)", "redirect_uris": [REDIRECT]})
client = js(text)
check("client registration works", status == 201 and client.get("client_id", "").startswith("mcp_"), f"{status} {text}")
cid = client["client_id"]

status, _, _ = http("POST", asm["registration_endpoint"], body={"client_name": "x", "redirect_uris": ["http://evil.example/cb"]})
check("registration rejects a non-https redirect address", status == 400)

# --- Sign-in and approval ----------------------------------------------------
_, challenge = pkce()
status, _, page = http("GET", authorize_url("mcp_nope", challenge, "s"), jar=cookiejar.CookieJar())
check("unknown client is not shown the login-protected page as valid", status in (307, 400))

verifier, params, page, jar = sign_in_and_approve(cid)
check("approval page names the app and the signed-in user", "Claude (test)" in page and f"<strong>{ADMIN_USER}</strong>" in page)
check("approval page says it can't send (sending is off)", "can't send or schedule" in page.replace("&#39;", "'"))
check("approved: redirected back with a code", "code" in params and params.get("iss") == BASE, str(params))

# With a session: bad redirect address and missing PKCE.
status, _, text = http("GET", authorize_url(cid, challenge, "s", redirect_uri="https://evil.example/cb"), jar=jar)
check("a redirect address that wasn't registered is refused on the page, not redirected", status == 400 and "return address" in text)
status, hdrs, _ = http("GET", authorize_url(cid, "", "s"), jar=jar)
check("PKCE is mandatory", status == 302 and "error=invalid_request" in hdrs.get("Location", ""))
status, hdrs, _ = http("GET", authorize_url(cid, challenge, "s", resource="https://other.example/mcp"), jar=jar)
check("a token for some other server is refused", status == 302 and "error=invalid_target" in hdrs.get("Location", ""))

status, body = token_request(grant_type="authorization_code", code=params["code"], client_id=cid,
                             redirect_uri=REDIRECT, code_verifier="wrong-" + verifier, resource=MCP)
check("wrong PKCE verifier is refused", status == 400 and body.get("error") == "invalid_grant", str(body))
status, body = token_request(grant_type="authorization_code", code=params["code"], client_id=cid,
                             redirect_uri=REDIRECT, code_verifier=verifier, resource=MCP)
check("a code can't be tried twice (it was burned by the failed attempt)", status == 400, str(body))

verifier, params, _, _ = sign_in_and_approve(cid)
status, tokens = token_request(grant_type="authorization_code", code=params["code"], client_id=cid,
                               redirect_uri=REDIRECT, code_verifier=verifier, resource=MCP)
check("code exchanges for an access and refresh token",
      status == 200 and tokens.get("token_type") == "Bearer" and tokens.get("refresh_token") and tokens.get("expires_in") == 3600,
      str(tokens))
access = tokens["access_token"]
status, body = token_request(grant_type="authorization_code", code=params["code"], client_id=cid,
                             redirect_uri=REDIRECT, code_verifier=verifier)
check("a used code is refused", status == 400 and body.get("error") == "invalid_grant")

_, denied, _, _ = sign_in_and_approve(cid, decision="deny")
check("cancelling returns access_denied", denied.get("error") == "access_denied" and "code" not in denied, str(denied))

# --- Protocol ----------------------------------------------------------------
init = rpc(access, "initialize", {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "e2e", "version": "0"}})
r = init.get("result", {})
check("initialize agrees the version and offers tools",
      r.get("protocolVersion") == "2025-11-25" and "tools" in r.get("capabilities", {}) and r.get("serverInfo", {}).get("name") == "listmonk", str(init))
check("instructions say sending is off", "switched off" in r.get("instructions", ""))
init = rpc(access, "initialize", {"protocolVersion": "1999-01-01", "capabilities": {}, "clientInfo": {"name": "e2e", "version": "0"}},
           headers={"MCP-Protocol-Version": "2025-06-18"})
check("unknown version: offers its newest", init["result"]["protocolVersion"] == "2025-11-25")

status, _, text = http("POST", MCP, headers={"Authorization": "Bearer " + access},
                       body={"jsonrpc": "2.0", "method": "notifications/initialized"})
check("notifications get 202 and no body", status == 202 and text == "", f"{status} {text!r}")
check("ping works", rpc(access, "ping").get("result") == {})
check("unknown method is a JSON-RPC error", rpc(access, "nope/nope").get("error", {}).get("code") == -32601)
status, _, _ = rpc(access, "ping", raw=True, headers={"MCP-Protocol-Version": "1999-01-01"})
check("unsupported protocol version header is 400", status == 400)
status, _, _ = rpc(access, "ping", raw=True, headers={"Origin": "https://evil.example"})
check("a foreign web page's Origin is refused", status == 403)
status, _, _ = rpc(access, "ping", raw=True, headers={"Origin": "https://claude.ai"})
check("claude.ai's Origin is allowed", status == 200)
status, _, _ = rpc("not-a-token", "ping", raw=True)
check("a made-up token is 401", status == 401)

tools = {t["name"]: t for t in rpc(access, "tools/list")["result"]["tools"]}
expected = {"get_overview", "list_lists", "create_list", "search_subscribers", "add_subscriber", "change_subscriber_lists",
            "list_campaigns", "get_campaign", "list_templates", "create_campaign", "update_campaign", "send_test_email",
            "schedule_campaign", "stop_campaign", "list_sequences", "get_sequence", "list_sequence_people",
            "create_sequence", "add_sequence_email", "set_sequence_email_wait", "set_sequence_status"}
check("all tools are listed", set(tools) == expected, str(set(tools) ^ expected))
check("no tool can delete", not any("delete" in n or "remove" in n for n in tools))
check("every tool has a schema and a description",
      all(t["inputSchema"]["type"] == "object" and len(t["description"]) > 20 for t in tools.values()))
check("read-only tools are marked read-only",
      tools["list_lists"]["annotations"]["readOnlyHint"] is True and tools["create_list"]["annotations"]["readOnlyHint"] is False)

# --- Tools -------------------------------------------------------------------
err, ov = tool(access, "get_overview")
check("get_overview", not err and ov["signed_in_as"] == ADMIN_USER and ov["sending_allowed"] is False
      and ov["listmonk_url"] == BASE + "/admin", str(ov))

err, lst = tool(access, "create_list", {"name": f"MCP list {RUN}"})
check("create_list makes a private single opt-in list", not err and lst["type"] == "private" and lst["optin"] == "single", str(lst))
err, lists = tool(access, "list_lists", {"query": f"MCP list {RUN}"})
check("list_lists finds it with a subscriber count",
      not err and [l["id"] for l in lists["lists"]] == [lst["id"]] and lists["lists"][0]["subscriber_count"] == 0, str(lists))

me = f"me-{RUN}@example.com"
err, sub = tool(access, "add_subscriber", {"email": me, "name": "Me Test", "list_ids": [lst["id"]], "attributes": {"city": "Perth"}})
check("add_subscriber", not err and sub["email"] == me and sub["attribs"] == {"city": "Perth"}, str(sub))
err, dup = tool(access, "add_subscriber", {"email": me, "list_ids": [lst["id"]]})
check("adding the same address again is a readable error", err and isinstance(dup, str), str(dup))
err, found = tool(access, "search_subscribers", {"search": me})
check("search_subscribers finds them with their list",
      not err and found["total"] == 1 and found["subscribers"][0]["lists"][0]["id"] == lst["id"], str(found))

err, lst2 = tool(access, "create_list", {"name": f"MCP second {RUN}"})
err, res = tool(access, "change_subscriber_lists", {"subscriber_ids": [sub["id"]], "action": "add", "list_ids": [lst2["id"]]})
err2, found = tool(access, "search_subscribers", {"list_id": lst2["id"]})
check("change_subscriber_lists adds to a list", not err and not err2 and found["total"] == 1, str(found))
err, res = tool(access, "change_subscriber_lists", {"subscriber_ids": [sub["id"]], "action": "unsubscribe", "list_ids": [lst2["id"]]})
err2, found = tool(access, "search_subscribers", {"list_id": lst2["id"], "subscription_status": "unsubscribed"})
check("change_subscriber_lists unsubscribes", not err and not err2 and found["total"] == 1, str(found))

err, tpls = tool(access, "list_templates")
check("list_templates shows a default", not err and any(t["is_default"] for t in tpls["templates"]), str(tpls))

body_html = '<p>Hi {{ .Subscriber.FirstName }}</p><p><a href="{{ UnsubscribeURL }}">Unsubscribe</a></p>'
err, camp = tool(access, "create_campaign", {"name": f"MCP camp {RUN}", "subject": f"MCP subject {RUN}",
                                             "list_ids": [lst["id"]], "body": body_html})
check("create_campaign saves a draft with a link", not err and camp["status"] == "draft"
      and camp["open_in_listmonk"] == f"{BASE}/admin/campaigns/{camp['id']}", str(camp))
err, got = tool(access, "get_campaign", {"id": camp["id"]})
check("get_campaign returns the body and lists", not err and got["body"] == body_html and [l["id"] for l in got["lists"]] == [lst["id"]], str(got))

err, upd = tool(access, "update_campaign", {"id": camp["id"], "subject": f"MCP new subject {RUN}"})
err2, got = tool(access, "get_campaign", {"id": camp["id"]})
check("update_campaign changes only what it's given (lists and body kept)",
      not err and not err2 and got["subject"] == f"MCP new subject {RUN}" and got["body"] == body_html
      and [l["id"] for l in got["lists"]] == [lst["id"]], str(got))
err, camps = tool(access, "list_campaigns", {"status": "draft", "query": f"MCP camp {RUN}"})
check("list_campaigns finds it without the body", not err and camps["total"] == 1 and "body" not in camps["campaigns"][0], str(camps))

err, res = tool(access, "send_test_email", {"campaign_id": camp["id"], "emails": [me]})
time.sleep(3)
_, _, text = http("GET", MAIL + "/api/v1/messages?limit=200")
got_test = [m for m in js(text).get("messages", []) if m["To"][0]["Address"] == me]
check("send_test_email delivers to the test address only", not err and len(got_test) == 1
      and got_test[0]["Subject"] == f"MCP new subject {RUN}", f"{res} {[m['Subject'] for m in got_test]}")

when = time.strftime("%Y-%m-%dT%H:%M:%S+08:00", time.gmtime(time.time() + 8 * 3600 + 86400))
err, res = tool(access, "schedule_campaign", {"id": camp["id"], "when": when})
check("schedule_campaign is refused while sending is off", err and "switched off" in res, str(res))
err, res = tool(access, "schedule_campaign", {"id": camp["id"], "when": "now"})
check("sending now is refused while sending is off", err and "switched off" in res, str(res))
err, got = tool(access, "get_campaign", {"id": camp["id"]})
check("the campaign is still a draft", got["status"] == "draft")

err, seq = tool(access, "create_sequence", {"name": f"MCP seq {RUN}", "list_id": lst["id"], "exit_list_ids": [lst2["id"]]})
check("create_sequence makes it switched off", not err and seq["status"] == "paused" and seq["exit_list_ids"] == [lst2["id"]], str(seq))
err, s1 = tool(access, "add_sequence_email", {"sequence_id": seq["id"], "subject": "Welcome", "body": body_html, "wait_minutes": 0})
err2, s2 = tool(access, "add_sequence_email", {"sequence_id": seq["id"], "subject": "Day two", "body": body_html, "wait_minutes": 1440})
check("add_sequence_email adds steps in order", not err and not err2
      and [(s["position"], s["delay_minutes"], s["campaign_subject"]) for s in s2["steps"]] == [(1, 0, "Welcome"), (2, 1440, "Day two")], str(s2))
err, s3 = tool(access, "set_sequence_email_wait", {"sequence_id": seq["id"], "step_id": s2["steps"][1]["id"], "wait_minutes": 2880})
check("set_sequence_email_wait", not err and s3["steps"][1]["delay_minutes"] == 2880, str(s3))
err, res = tool(access, "update_campaign", {"id": s2["steps"][0]["campaign_id"], "subject": "Welcome aboard"})
err2, gs = tool(access, "get_sequence", {"id": seq["id"]})
check("a step's e-mail is edited with update_campaign and keeps its list",
      not err and not err2 and gs["steps"][0]["campaign_subject"] == "Welcome aboard" and len(gs["steps"]) == 2, str(gs))
err, res = tool(access, "set_sequence_status", {"id": seq["id"], "on": True})
check("switching a sequence on is refused while sending is off", err and "switched off" in res, str(res))
err, seqs = tool(access, "list_sequences")
check("list_sequences", not err and any(s["id"] == seq["id"] and s["status"] == "paused" for s in seqs["sequences"]), str(seqs))

# --- With "let Claude schedule and send" on ---------------------------------
info = admin_api("PUT", "/api/mcp/settings", {"allow_sending": True})
check("the admin setting switches on, and lists the connection",
      info["allow_sending"] is True and info["url"] == MCP and len(info["connections"]) == 1
      and info["connections"][0]["client_name"] == "Claude (test)" and info["connections"][0]["last_used_at"], str(info))
check("instructions now say to confirm before sending",
      "get a clear yes" in rpc(access, "initialize", {"protocolVersion": "2025-11-25", "capabilities": {},
                                                    "clientInfo": {"name": "e2e", "version": "0"}})["result"]["instructions"])

err, res = tool(access, "schedule_campaign", {"id": camp["id"], "when": "2020-01-01T09:00:00+08:00"})
check("a time in the past is refused", err, str(res))
err, res = tool(access, "schedule_campaign", {"id": camp["id"], "when": when})
check("schedule_campaign schedules for the given time", not err and res["status"] == "scheduled" and res["send_at"], str(res))
err, got = tool(access, "get_campaign", {"id": camp["id"]})
check("scheduling kept the lists", [l["id"] for l in got["lists"]] == [lst["id"]], str(got.get("lists")))
err, res = tool(access, "stop_campaign", {"id": camp["id"]})
check("stop_campaign puts it back to draft", not err and res["status"] == "draft", str(res))
err, res = tool(access, "stop_campaign", {"id": camp["id"]})
check("stopping a draft is a readable error", err and "nothing to stop" in res, str(res))

err, res = tool(access, "set_sequence_status", {"id": seq["id"], "on": True})
check("a sequence can be switched on", not err and res["status"] == "active", str(res))
err, res = tool(access, "set_sequence_status", {"id": seq["id"], "on": False})
check("and off", not err and res["status"] == "paused", str(res))
err, ppl = tool(access, "list_sequence_people", {"id": seq["id"]})
check("list_sequence_people", not err and ppl["total"] == 0, str(ppl))

info = admin_api("PUT", "/api/mcp/settings", {"allow_sending": False})
err, res = tool(access, "set_sequence_status", {"id": seq["id"], "on": False})
check("switching off is still allowed with sending off", not err, str(res))

# --- Bad input ---------------------------------------------------------------
err, res = tool(access, "get_campaign", {})
check("missing argument is a readable error", err and "Missing required argument" in res, str(res))
err, res = tool(access, "get_campaign", {"id": 999999})
check("unknown id is a readable error", err, str(res))
check("unknown tool is a JSON-RPC error", rpc(access, "tools/call", {"name": "delete_everything"}).get("error", {}).get("code") == -32602)

# --- Token lifecycle ---------------------------------------------------------
status, fresh = token_request(grant_type="refresh_token", refresh_token=tokens["refresh_token"], client_id=cid)
check("refresh gives a new pair", status == 200 and fresh.get("access_token") not in (None, access)
      and fresh.get("refresh_token") != tokens["refresh_token"], str(fresh))
check("the old access token stops working", rpc(access, "ping", raw=True)[0] == 401)
status, body = token_request(grant_type="refresh_token", refresh_token=tokens["refresh_token"], client_id=cid)
check("the old refresh token can't be reused", status == 400 and body.get("error") == "invalid_grant")
check("the new access token works", rpc(fresh["access_token"], "ping").get("result") == {})
info = admin_api("GET", "/api/mcp")
check("refreshing didn't add a second connection", len(info["connections"]) == 1, str(info["connections"]))

info = admin_api("DELETE", f"/api/mcp/connections/{info['connections'][0]['id']}")
check("disconnecting removes the connection", info["connections"] == [])
check("and the token stops working straight away", rpc(fresh["access_token"], "ping", raw=True)[0] == 401)
status, body = token_request(grant_type="refresh_token", refresh_token=fresh["refresh_token"], client_id=cid)
check("and it can't be refreshed", status == 400)

print()
print("ALL PASSED" if not failures else f"{len(failures)} FAILED: {failures}")
sys.exit(1 if failures else 0)
