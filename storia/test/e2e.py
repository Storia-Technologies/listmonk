"""End-to-end check of Sequences against the local test stack.

    docker compose -p seqtest -f storia/test/docker-compose.yml up -d
    python storia/test/e2e.py <api token from `docker logs seqtest_app`>

Takes about five minutes: the engine ticks once a minute and the steps are
set one minute apart. Every e-mail lands in Mailpit, nothing leaves the machine.
"""
import base64
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = "http://localhost:9100"
MAIL = "http://localhost:8125"
TOKEN = sys.argv[1]
AUTH = "Basic " + base64.b64encode(f"api:{TOKEN}".encode()).decode()
RUN = str(int(time.time()))[-6:]

failures = []


def call(method, path, body=None, base=BASE, auth=True, form=None):
    data, headers = None, {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    if auth:
        headers["Authorization"] = AUTH
    req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw[:1] in "{[" else raw)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        return e.code, (json.loads(raw) if raw[:1] in "{[" else raw)


def ok(method, path, body=None):
    status, out = call(method, path, body)
    assert status == 200, f"{method} {path} -> {status}: {out}"
    return out["data"]


def check(name, cond, detail=""):
    print(("PASS  " if cond else "FAIL  ") + name + (f"  [{detail}]" if detail and not cond else ""))
    if not cond:
        failures.append(name)


def wait_healthy():
    for _ in range(60):
        try:
            if call("GET", "/api/health")[0] == 200:
                return
        except Exception:
            pass
        time.sleep(2)
    raise SystemExit("listmonk never came back")


def mails():
    """{recipient: [subject, ...]} for everything Mailpit holds from this run."""
    _, out = call("GET", "/api/v1/messages?limit=500", base=MAIL, auth=False)
    got = {}
    for m in out["messages"]:
        for to in m["To"]:
            got.setdefault(to["Address"], []).append(m["Subject"])
    return got, out["messages"]


def subjects(addr):
    return sorted(mails()[0].get(addr, []))


def people(seq_id):
    rows = ok("GET", f"/api/sequences/{seq_id}/subscribers?per_page=all")["results"]
    return {r["email"]: r for r in rows}


def wait_for(desc, fn, timeout=150):
    end = time.time() + timeout
    while time.time() < end:
        if fn():
            return True
        time.sleep(5)
    print(f"      (timed out waiting for: {desc})")
    return False


# --- Setup -------------------------------------------------------------------
settings = ok("GET", "/api/settings")
settings["app.root_url"] = BASE
settings["smtp"][0].update({"enabled": True, "host": "mail", "port": 1025, "auth_protocol": "none",
                            "tls_type": "none", "username": "", "password": ""})
call("PUT", "/api/settings", settings)
time.sleep(3)
wait_healthy()

cfg = ok("GET", "/api/config")
check("server config says sequences are on", cfg.get("sequences") is True)

funnel = ok("POST", "/api/lists", {"name": f"Funnel {RUN}", "type": "private", "optin": "single"})
clients = ok("POST", "/api/lists", {"name": f"Clients {RUN}", "type": "private", "optin": "single"})
other = ok("POST", "/api/lists", {"name": f"Other {RUN}", "type": "private", "optin": "single"})


def campaign(name, lists):
    return ok("POST", "/api/campaigns", {
        "name": name, "subject": name, "lists": lists, "from_email": "Test <test@example.com>",
        "content_type": "richtext", "messenger": "email", "type": "regular", "tags": ["sequence"],
        "body": '<p>Hi {{ .Subscriber.Name }}</p><p><a href="{{ UnsubscribeURL }}">Unsubscribe</a></p>',
    })


def subscriber(tag, lists):
    email = f"{tag}-{RUN}@example.com"
    ok("POST", "/api/subscribers", {"email": email, "name": tag.title(), "status": "enabled",
                                    "lists": lists, "preconfirm_subscriptions": True})
    return email


c1, c2, c3 = (campaign(f"Seq {RUN} email {n}", [funnel["id"]]) for n in (1, 2, 3))
c_wrong = campaign(f"Seq {RUN} wrong list", [other["id"]])

seq = ok("POST", "/api/sequences", {"name": f"Welcome {RUN}", "list_id": funnel["id"],
                                    "exit_list_ids": [clients["id"]]})
sid = seq["id"]
check("new sequence starts switched off", seq["status"] == "paused")

# --- Guards ------------------------------------------------------------------
status, _ = call("PUT", f"/api/sequences/{sid}/status", {"status": "active"})
check("can't switch on with no steps", status == 400)

status, out = call("POST", f"/api/sequences/{sid}/steps", {"campaign_id": c_wrong["id"], "delay_minutes": 0})
check("rejects an e-mail not addressed to the sequence's list", status == 400, str(out))

ok("POST", f"/api/sequences/{sid}/steps", {"campaign_id": c1["id"], "delay_minutes": 0})
ok("POST", f"/api/sequences/{sid}/steps", {"campaign_id": c3["id"], "delay_minutes": 1})
seq = ok("POST", f"/api/sequences/{sid}/steps", {"campaign_id": c2["id"], "delay_minutes": 1})

status, _ = call("POST", f"/api/sequences/{sid}/steps", {"campaign_id": c1["id"], "delay_minutes": 0})
check("rejects the same e-mail twice", status == 400)

# Steps went in as 1, 3, 2. Put them in order.
by_camp = {s["campaign_id"]: s["id"] for s in seq["steps"]}
seq = ok("PUT", f"/api/sequences/{sid}/steps/order", {"ids": [by_camp[c1["id"]], by_camp[c2["id"]], by_camp[c3["id"]]]})
check("re-ordering steps works", [s["campaign_id"] for s in seq["steps"]] == [c1["id"], c2["id"], c3["id"]])

status, _ = call("PUT", f"/api/sequences/{sid}", {"name": "x", "list_id": other["id"], "exit_list_ids": []})
check("can't change the list once there are steps", status == 400)

# Someone already on the list before the sequence is switched on.
old = subscriber("old", [funnel["id"]])
time.sleep(2)

seq = ok("PUT", f"/api/sequences/{sid}/status", {"status": "active"})
check("switches on", seq["status"] == "active" and seq["activated_at"])

status, out = call("PUT", f"/api/campaigns/{c1['id']}/status", {"status": "running"})
check("a step e-mail can't be started as a normal campaign", status == 400, str(out))

status, out = call("DELETE", f"/api/campaigns/{c1['id']}")
check("a step e-mail can't be deleted", status != 200, str(out))

# --- People join -------------------------------------------------------------
time.sleep(1)
a = subscriber("anna", [funnel["id"]])    # goes all the way through
b = subscriber("ben", [funnel["id"]])     # unsubscribed from the list via the API
c = subscriber("cara", [funnel["id"]])    # becomes a client (exit list)
d = subscriber("dan", [funnel["id"]])     # clicks the unsubscribe link in e-mail 1
e = subscriber("eve", [funnel["id"]])     # removed from the sequence by hand

s1, s2, s3 = (f"Seq {RUN} email {n}" for n in (1, 2, 3))

wait_for("e-mail 1 to all five", lambda: all(subjects(x) == [s1] for x in (a, b, c, d, e)))
for x in (a, b, c, d, e):
    check(f"{x.split('-')[0]} got e-mail 1 only", subjects(x) == [s1], str(subjects(x)))
check("the person already on the list got nothing", subjects(old) == [])
enrolled = people(sid)
check("all five who joined are listed as enrolled", all(x in enrolled for x in (a, b, c, d, e)), str(list(enrolled)))
check("the person already on the list is not enrolled", old not in enrolled)

# --- Exits before e-mail 2 ---------------------------------------------------
subs = {r["email"]: r["id"] for r in ok("GET", "/api/subscribers?per_page=all")["results"]}
ok("PUT", "/api/subscribers/lists", {"ids": [subs[b]], "action": "unsubscribe", "target_list_ids": [funnel["id"]]})
ok("PUT", "/api/subscribers/lists", {"ids": [subs[c]], "action": "add", "target_list_ids": [clients["id"]],
                                     "status": "confirmed"})
ok("DELETE", f"/api/sequences/{sid}/subscribers/{subs[e]}")

# Dan uses the real unsubscribe link from the e-mail he was sent.
_, msgs = mails()
dan_msg = next(m for m in msgs if m["To"][0]["Address"] == d)
_, full = call("GET", f"/api/v1/message/{dan_msg['ID']}", base=MAIL, auth=False)
link = re.search(r'href="(http://localhost:9100/subscription/[^"]+)"', full["HTML"])
check("e-mail 1 carries an unsubscribe link", link is not None)
if link:
    status, _ = call("POST", link.group(1).replace(BASE, ""), auth=False, form={})
    check("unsubscribe link accepted", status == 200, str(status))

wait_for("e-mail 2 to anna", lambda: subjects(a) == sorted([s1, s2]))
check("anna got e-mail 2", subjects(a) == sorted([s1, s2]), str(subjects(a)))

# Pause before e-mail 3 is due, and confirm nothing goes out while it's off.
ok("PUT", f"/api/sequences/{sid}/status", {"status": "paused"})
late = subscriber("late", [funnel["id"]])     # joins while it's off
time.sleep(130)
check("nothing sent while switched off", subjects(a) == sorted([s1, s2]), str(subjects(a)))
check("nobody enrolled while switched off", late not in people(sid))

ok("PUT", f"/api/sequences/{sid}/status", {"status": "active"})
wait_for("e-mail 3 to anna", lambda: subjects(a) == sorted([s1, s2, s3]))
check("anna got e-mail 3 after switching back on", subjects(a) == sorted([s1, s2, s3]), str(subjects(a)))
wait_for("late joiner's e-mail 1", lambda: subjects(late) == [s1])
check("someone who joined while it was off is picked up on resume", subjects(late) == [s1], str(subjects(late)))

wait_for("anna marked finished", lambda: people(sid)[a]["status"] == "completed", timeout=90)

# --- Final state -------------------------------------------------------------
p = people(sid)
check("anna is finished", p[a]["status"] == "completed", str(p[a]))
check("ben left: unsubscribed", p[b]["status"] == "exited" and p[b]["exit_reason"] == "unsubscribed", str(p[b]))
check("cara left: exit list", p[c]["status"] == "exited" and p[c]["exit_reason"] == "added to an exit list", str(p[c]))
check("dan left: unsubscribed by link", p[d]["status"] == "exited" and p[d]["exit_reason"] == "unsubscribed", str(p[d]))
check("eve left: removed by hand", p[e]["status"] == "exited" and p[e]["exit_reason"] == "removed by hand", str(p[e]))
for x in (b, c, d, e):
    check(f"{x.split('-')[0]} got nothing after leaving", subjects(x) == [s1], str(subjects(x)))

seq = ok("GET", f"/api/sequences/{sid}")
sent = [s["num_sent"] for s in seq["steps"]]
check("per-step sent counts are right (6, 1, 1)", sent == [6, 1, 1], str(sent))
check("no error recorded on the sequence", seq["last_error"] == "", seq["last_error"])
check("nobody got a duplicate", all(len(v) == len(set(v)) for v in mails()[0].values()))

print()
print("ALL PASSED" if not failures else f"{len(failures)} FAILED: {failures}")
sys.exit(1 if failures else 0)
