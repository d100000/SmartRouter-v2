"""End-to-end routing and billing checks against a running SmartRouter and mock upstreams."""
import os, sys, json, time, threading, collections
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lib import *
from concurrent.futures import ThreadPoolExecutor

PRICE = 50  # (10 prompt + 20 completion x 2) x model ratio 1 x group ratio 1
results = []

def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("PASS " if ok else "FAIL ") + name + (" | " + detail if detail else ""), flush=True)

def bootstrap():
    """First run on an empty database: create the root user and an API key."""
    if not json.loads(http("GET", BASE + "/api/setup")[1])["data"]["status"]:
        st, raw = http("POST", BASE + "/api/setup", {"username": "root", "password": "E2e-Passw0rd!", "confirmPassword": "E2e-Passw0rd!", "SelfUseModeEnabled": False, "DemoSiteEnabled": False})
        assert json.loads(raw)["success"], raw
    if not db("select key from tokens where name='e2e'"):
        st, r = api("POST", "/api/token/", {"name": "e2e", "remain_quota": 500000000, "unlimited_quota": False, "expired_time": -1})
        assert r["success"], r

bootstrap()
KEY = "sk-" + db("select key from tokens where name='e2e'")[0][0]
MODELS = ["m1", "m2", "m3", "m4", "m5", "m6", "m7"]

def setup():
    ratio = {m: 1 for m in MODELS + ["e2e-model"]}
    completion = {m: 2 for m in MODELS + ["e2e-model"]}
    assert api("PUT", "/api/option/", {"key": "ModelRatio", "value": json.dumps(ratio)})[1]["success"]
    assert api("PUT", "/api/option/", {"key": "CompletionRatio", "value": json.dumps(completion)})[1]["success"]
    options = {"RetryTimes": "3"}
    if not BASELINE:
        options["channel_affinity_setting.return_to_priority_idle_seconds"] = "3"
    else:
        # The original defaults have no prefer-mode rule for prompt_cache_key.
        rule = [{"name": "prompt cache key", "model_regex": ["^.*$"], "path_regex": [], "key_sources": [{"type": "gjson", "path": "prompt_cache_key"}], "value_regex": "", "ttl_seconds": 0, "skip_retry_on_failure": False, "session_mode": "prefer", "include_using_group": True, "include_model_name": True, "include_rule_name": True}]
        options["channel_affinity_setting.rules"] = json.dumps(rule)
    st, r = api("PATCH", "/api/option/request_policy", {"options": options})
    assert st == 200 and r["success"], r
    have = {row[0]: row[1] for row in db("select name,id from channels")}
    plan = [  # name, port, model, priority, weight
        ("m1-a", 4101, "m1", 0, 1), ("m1-b", 4102, "m1", 0, 1),
        ("m2-a", 4101, "m2", 0, 1), ("m2-b", 4102, "m2", 0, 1),
        ("m3-a", 4103, "m3", 0, 1),
        ("m4-primary", 4101, "m4", 10, 1), ("m4-backup", 4102, "m4", 0, 1),
        ("m5-heavy", 4103, "m5", 0, 9), ("m5-light", 4104, "m5", 0, 1),
        ("m6-a", 4103, "m6", 0, 1), ("m6-b", 4104, "m6", 0, 1),
        ("m7-primary", 4101, "m7", 10, 1), ("m7-backup", 4102, "m7", 0, 1),
    ]
    ids = {}
    for name, port, model, priority, weight in plan:
        if name not in have:
            st, r = api("POST", "/api/channel/", {"mode": "single", "channel": {"name": name, "type": 1, "key": f"sk-mock-{name}", "base_url": f"http://127.0.0.1:{port}", "models": model, "group": "default", "priority": priority, "weight": weight, "status": 1}})
            assert st == 200 and r["success"], r
            have[name] = r["data"]["id"]
        ids[name] = have[name]
    time.sleep(7)  # channel cache sync interval is 5 seconds
    return ids

def ledger():
    user = db("select quota, used_quota, request_count from users where id=1")[0]
    token = db("select remain_quota, used_quota from tokens where name='e2e'")[0]
    channels = dict(db("select id, used_quota from channels"))
    logs = db("select count(*), coalesce(sum(quota),0) from logs where type=2")[0]
    return {"user_quota": user[0], "user_used": user[1], "user_requests": user[2], "token_remain": token[0], "token_used": token[1], "channels": channels, "log_count": logs[0], "log_quota": logs[1]}

def settle():
    """Wait until asynchronous accounting stops moving."""
    last = None
    for _ in range(40):
        time.sleep(0.5)
        now = ledger()
        if now == last:
            return now
        last = now
    return last

def reset_mocks():
    for port in (4101, 4102, 4103, 4104):
        mock(port, fail=0.0, status=503, delay_ms=0, reset=True)

def run(model, count, workers=1, stream_every=0, session=None, pause=0.0):
    """Send requests; returns list of (status, served_port)."""
    def one(i):
        st, raw = chat(KEY, model=model, stream=bool(stream_every and i % stream_every == 0), session=session(i) if session else None)
        if pause:
            time.sleep(pause)
        return st, served_by(raw) if st == 200 else None
    if workers == 1:
        return [one(i) for i in range(count)]
    with ThreadPoolExecutor(workers) as pool:
        return list(pool.map(one, range(count)))

def billing_delta(before, after):
    return {
        "user_quota": before["user_quota"] - after["user_quota"],
        "user_used": after["user_used"] - before["user_used"],
        "user_requests": after["user_requests"] - before["user_requests"],
        "token_remain": before["token_remain"] - after["token_remain"],
        "token_used": after["token_used"] - before["token_used"],
        "channels": sum(after["channels"].values()) - sum(before["channels"].values()),
        "log_count": after["log_count"] - before["log_count"],
        "log_quota": after["log_quota"] - before["log_quota"],
    }

def expect_billing(tag, before, successes):
    after = settle()
    d = billing_delta(before, after)
    want = successes * PRICE
    check(f"{tag}: user balance decreased by exactly {successes} x {PRICE}", d["user_quota"] == want, f"delta={d['user_quota']} want={want}")
    check(f"{tag}: user used_quota, token remain and token used agree", d["user_used"] == want and d["token_remain"] == want and d["token_used"] == want, str({k: d[k] for k in ('user_used', 'token_remain', 'token_used')}))
    check(f"{tag}: channel used_quota total agrees", d["channels"] == want, f"delta={d['channels']}")
    check(f"{tag}: one consume log per success, quota sum agrees", d["log_count"] == successes and d["log_quota"] == want, f"logs={d['log_count']} quota={d['log_quota']}")
    return after

def snapshot(model):
    st, r = api("GET", f"/api/scheduling/?group=default&model={model}&stream=false")
    return r["data"]

def shares(served):
    counter = collections.Counter(port for st, port in served if st == 200)
    return dict(counter)

def scenario_billing_success(ids):
    reset_mocks()
    before = settle()
    served = run("m1", 200, workers=8, stream_every=2)
    ok = sum(1 for st, _ in served if st == 200)
    check("S1: 200 concurrent requests (half streaming) all succeed", ok == 200, f"ok={ok}")
    stats = mock_stats()
    check("S1: upstream saw exactly 200 calls", stats["4101"]["hits"] + stats["4102"]["hits"] == 200, f"{stats['4101']['hits']}+{stats['4102']['hits']}")
    after = expect_billing("S1", before, ok)
    per_channel = {name: after["channels"][ids[name]] - before["channels"].get(ids[name], 0) for name in ("m1-a", "m1-b")}
    by_port = shares(served)
    check("S1: each channel is charged for exactly the requests it served", per_channel["m1-a"] == by_port.get(4101, 0) * PRICE and per_channel["m1-b"] == by_port.get(4102, 0) * PRICE, f"{per_channel} served={by_port}")

def scenario_billing_retries(ids):
    reset_mocks()
    mock(4101, fail=0.5)
    before = settle()
    served = run("m2", 200, workers=4, stream_every=3)
    ok = sum(1 for st, _ in served if st == 200)
    stats = mock_stats()
    hits = stats["4101"]["hits"] + stats["4102"]["hits"]
    check("S2: with one channel failing half its calls, every request still succeeds through retry", ok == 200, f"ok={ok}")
    check("S2: failed attempts happened upstream", hits > 200 or stats["4101"]["hits"] > stats["4101"]["ok"], f"upstream calls={hits}, 4101 ok={stats['4101']['ok']}/{stats['4101']['hits']}")
    after = expect_billing("S2", before, ok)
    by_port = shares(served)
    per_channel = {name: after["channels"][ids[name]] - before["channels"].get(ids[name], 0) for name in ("m2-a", "m2-b")}
    check("S2: a failed attempt is never charged to its channel", per_channel["m2-a"] == by_port.get(4101, 0) * PRICE and per_channel["m2-b"] == by_port.get(4102, 0) * PRICE, f"{per_channel} served={by_port}")
    reset_mocks()

def scenario_billing_all_fail(ids):
    reset_mocks()
    mock(4103, fail=1.0, status=503)
    before = settle()
    served = run("m3", 30, workers=3, stream_every=2)
    failed = sum(1 for st, _ in served if st != 200)
    check("S3: every request fails when the only channel is down", failed == 30, f"failed={failed} statuses={collections.Counter(st for st, _ in served)}")
    after = settle()
    d = billing_delta(before, after)
    check("S3: nothing is charged for failed requests (pre-consumed quota fully returned)", d["user_quota"] == 0 and d["token_remain"] == 0 and d["token_used"] == 0 and d["channels"] == 0 and d["log_quota"] == 0, str(d))
    reset_mocks()

def scenario_priority(ids):
    reset_mocks()
    before = settle()
    warm = run("m4", 160, workers=4)
    snap = snapshot("m4")
    check("S4: dimension is active after 160 requests", snap["active"] is True, f"phase={snap['phase']}")
    steady = run("m4", 200, workers=4)
    by_port = shares(steady)
    check("S4: after activation every request goes to the higher priority channel", by_port.get(4101, 0) == 200, f"{by_port}")
    mock(4101, fail=1.0)
    outage = run("m4", 60)
    ok = sum(1 for st, _ in outage if st == 200)
    primary_hits_before = mock_stats()["4101"]["hits"]
    by_port = shares(outage)
    check("S4: primary outage, every request still succeeds on the backup", ok == 60 and by_port.get(4102, 0) == 60, f"ok={ok} {by_port}")
    wasted = primary_hits_before - 360
    check("S4: only a few requests hit the dead primary before it cools", 0 < wasted <= 6, f"calls to dead primary={wasted}")
    mock(4101, fail=0.0)
    time.sleep(17)  # first cooldown is 15 seconds
    back = run("m4", 300, workers=2)
    tail = shares(back[-100:])
    check("S4: traffic returns to the recovered primary", tail.get(4101, 0) >= 90, f"last 100: {tail}; all 300: {shares(back)}")
    total_ok = sum(1 for st, _ in warm + steady + outage + back if st == 200)
    expect_billing("S4", before, total_ok)
    reset_mocks()

def scenario_weights(ids):
    reset_mocks()
    run("m5", 150, workers=4)
    snap = snapshot("m5")
    measured = run("m5", 600, workers=4)
    by_port = shares(measured)
    heavy = by_port.get(4103, 0) / 600
    check("S5: configured 9:1 weights still shape the split after activation", snap["active"] and 0.84 <= heavy <= 0.95, f"active={snap['active']} heavy share={heavy:.3f} {by_port}")

def scenario_stickiness(ids):
    reset_mocks()
    sessions = [f"sess-{i}-{int(time.time())}" for i in range(30)]
    seen = collections.defaultdict(list)
    lock = threading.Lock()
    def one(i):
        s = sessions[i % len(sessions)]
        st, raw = chat(KEY, model="m6", session=s)
        with lock:
            seen[s].append(served_by(raw) if st == 200 else None)
    for round_ in range(8):  # one request per session per round, rounds in order
        with ThreadPoolExecutor(6) as pool:
            list(pool.map(one, range(len(sessions))))
        # The binding is written right after the response is sent; a client
        # that fires its next request in the same instant can outrun it.
        time.sleep(0.2)
    single = sum(1 for ports in seen.values() if len(set(ports)) == 1 and None not in ports)
    spread = collections.Counter(ports[0] for ports in seen.values())
    check("S6: each of 30 sessions (prompt_cache_key) stays on one channel for all 8 requests", single == 30, f"single-channel sessions={single}")
    check("S6: sessions are distributed over both channels", min(spread.get(4103, 0), spread.get(4104, 0)) >= 5, f"{dict(spread)}")
    # 2% background errors on both channels must not make sessions bounce.
    mock(4103, fail=0.02); mock(4104, fail=0.02)
    seen.clear()
    for round_ in range(20):
        with ThreadPoolExecutor(6) as pool:
            list(pool.map(one, range(len(sessions))))
        time.sleep(0.2)
    switches = 0
    failures = mock_stats()
    upstream_failures = sum(failures[p]["hits"] - failures[p]["ok"] for p in ("4103", "4104"))
    for ports in seen.values():
        ports = [p for p in ports if p]
        switches += sum(1 for a, b in zip(ports, ports[1:]) if a != b)
    check("S6: with 2% background errors sessions move at most once per real upstream failure", switches <= upstream_failures, f"channel switches={switches} over 600 requests, upstream failures={upstream_failures}")
    reset_mocks()

def scenario_return_to_priority(ids):
    reset_mocks()
    session = f"return-{int(time.time())}"
    mock(4101, fail=1.0)
    st, raw = chat(KEY, model="m7", session=session)
    check("S7: with the primary down a new session is served and bound on the backup", st == 200 and served_by(raw) == 4102, f"status={st} port={served_by(raw) if st == 200 else None}")
    for _ in range(4):  # push the primary into cooldown
        chat(KEY, model="m7")
    mock(4101, fail=0.0)
    time.sleep(17)
    # A session only returns to a tier that has rebuilt a 95% record after its outage.
    warm = run("m7", 160, workers=2)
    check("S7: recovered primary takes the non-session traffic again", shares(warm[-20:]).get(4101, 0) >= 18, f"{shares(warm)}")
    first = [served_by(chat(KEY, model="m7", session=session)[1]) for _ in range(3)]
    check("S7: an active session keeps its backup channel while in use", first == [4102, 4102, 4102], f"{first}")
    time.sleep(4.5)  # return interval was set to 3 seconds for this test
    later = [served_by(chat(KEY, model="m7", session=session)[1]) for _ in range(3)]
    check("S7: after the idle interval the session returns to the higher priority channel and stays", later == [4101, 4101, 4101], f"{later}")
    reset_mocks()

def final_invariants():
    final = settle()
    logs_quota = final["log_quota"]
    check("Final: user used_quota equals the sum of all consume logs", final["user_used"] == logs_quota, f"used={final['user_used']} logs={logs_quota}")
    check("Final: token used_quota equals the sum of all consume logs", final["token_used"] == logs_quota, f"token_used={final['token_used']}")
    check("Final: channel used_quota total equals the sum of all consume logs", sum(final["channels"].values()) == logs_quota, f"channels={sum(final['channels'].values())}")
    check("Final: balance + used is the initial balance", final["user_quota"] + final["user_used"] == 100000000, f"{final['user_quota']}+{final['user_used']}")
    time.sleep(3)
    open_attempts = db("select count(*) from channel_limit_attempts where released=0 and completed=0")[0][0]
    sent = db("select count(*) from channel_limit_attempts where sent_at_ms>0")[0][0]
    stats = mock_stats()
    upstream = sum(s["hits"] for s in stats.values())
    check("Final: no channel ledger attempt is left open", open_attempts == 0, f"open={open_attempts}")
    held = db("select coalesce(sum(held_usd_nano),0) from channel_daily_budgets")[0][0]
    check("Final: no channel budget hold is left behind", held == 0, f"held={held}")
    print(f"ledger attempts sent={sent}")

if __name__ == "__main__":
    ids = setup()
    only = sys.argv[1:]
    table = [("billing_success", scenario_billing_success), ("billing_retries", scenario_billing_retries), ("billing_all_fail", scenario_billing_all_fail),
             ("priority", scenario_priority), ("weights", scenario_weights), ("stickiness", scenario_stickiness), ("return", scenario_return_to_priority)]
    for name, fn in table:
        if not only or name in only:
            print(f"--- {name}", flush=True)
            fn(ids)
    final_invariants()
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)} passed, {len(failed)} failed")
    sys.exit(1 if failed else 0)
