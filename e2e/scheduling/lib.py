import json, sqlite3, time, urllib.request, urllib.error

import os
BASE = os.environ.get("SR_BASE", "http://127.0.0.1:3300")
DB = os.environ.get("SR_DB", "./data/sr.db")
BASELINE = os.environ.get("SR_BASELINE") == "1"
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
_admin = {"token": None, "at": 0}

def http(method, url, body=None, headers=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with _opener.open(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read()

def admin_token():
    if _admin["token"] and time.time() - _admin["at"] < 600:
        return _admin["token"]
    status, raw = http("POST", BASE + "/api/user/login", {"username": "root", "password": "E2e-Passw0rd!"})
    _admin.update(token=json.loads(raw)["data"]["access_token"], at=time.time())
    return _admin["token"]

def api(method, path, body=None):
    status, raw = http(method, BASE + path, body, {"Authorization": "Bearer " + admin_token()})
    try:
        return status, json.loads(raw)
    except Exception:
        return status, raw

def chat(key, model="e2e-model", session=None, stream=False, extra=None, headers=None):
    body = {"model": model, "messages": [{"role": "user", "content": "ping"}], "stream": stream}
    if session:
        body["prompt_cache_key"] = session
    body.update(extra or {})
    status, raw = http("POST", BASE + "/v1/chat/completions", body, {"Authorization": "Bearer " + key, **(headers or {})})
    return status, raw

def served_by(raw):
    """Upstream port that answered, parsed from the mock's reply text."""
    text = raw.decode("utf-8", "replace")
    import re
    found = re.findall(r"from (\d{4})", text.replace('"}}],"', ''))
    if found:
        return int(found[-1])
    # streamed replies split "hello " and "from 4101" across chunks
    joined = "".join(re.findall(r'"content":\s*"([^"]*)"', text))
    found = re.findall(r"from (\d{4})", joined)
    return int(found[-1]) if found else None

def mock(port, **ctl):
    return http("POST", f"http://127.0.0.1:{port}/__ctl", ctl)

def mock_stats():
    return json.loads(http("GET", "http://127.0.0.1:4101/__stats")[1])

def db(query, args=()):
    con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=30)
    try:
        return con.execute(query, args).fetchall()
    finally:
        con.close()
