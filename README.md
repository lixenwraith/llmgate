# llmgate

The one way from web pages to a local language model. A reverse proxy sends
`/PREFIX/NAME` to llmgate's app `NAME`; llmgate cleans and rate-limits the
request, queues it in the one line every app shares to the model, and
streams its place, its run and its outcome back as NDJSON. The model is
Ollama, on loopback, which nothing but llmgate reaches. The first app is
[lwdescribe](apps/lwdescribe): [LogWisp](https://github.com/lixenwraith/logwisp)
flags drafted from a description, for a page that runs LogWisp's engine.

```
browser ── proxy /PREFIX/NAME ──> llmgate JAIL:8888 ──> Ollama 127.0.0.1:11434
                                   │ apps/NAME: Take, Run
                                   └ /var/log/llmgate/{llmgate,requests}.log
```

## Layout

- `main.go`: `llmgate.toml`, the logs, the listeners
- `gate/`: the HTTP contract, admission (`Clean`, limits), the line, the review
  record
- `model/`: the model server, Ollama's chat API: greedy, the model kept loaded
- `apps/lwdescribe/`: the first app
- `jail/`: a Bastille template laid out as the jail: `Bastillefile`, rc.d,
  ipfw, newsyslog, the configuration and the app's prompt, examples and guide
- `deploy/`: `build.sh` pulls and builds as the checkouts' owner,
  `update.sh` brings a running jail up to the build as root

## The HTTP contract

- `GET /NAME`: `200 {"model", "waiting", "limits": {"client": {"burst",
  "every_s", "daily"}, "app": {...}}, ...the app's own}` while the model
  server is up and warmed, else `503 {"error":{"code":"unavailable"}}`. A page
  shows an app's box, and states a client's limits, on a 200 alone.
- `GET /NAME/guide`, for an app that has one, whether or not the model is up:
  the app's task as plain text for a visitor's own model. lwdescribe's is its
  prompt, the catalog and the examples, its answers asked as lw's flags a line
  each.
- `POST /NAME`, `application/json`, at most `max_body` bytes: the app's body
  (lwdescribe: `{"text": "..."}`). A refusal is JSON with its status:
  `400 invalid_request|text_invalid`, `404 not_found`, `405`, `413 too_large`,
  `415`, `429 rate_limited|daily_limit` (the client's own limits),
  `503 app_rate_limited|app_daily_limit` (the app's, across clients),
  `503 unavailable|busy|already_queued`, with `Retry-After` on 429 and 503.
- Admitted, `200 application/x-ndjson`, one event a line:
  `{"event":"queued","position":N,"eta_s":S}` (again whenever the place
  moves), `waiting` and `running` heartbeats, `{"event":"running","model":M}`,
  then `{"event":"done","id":ID,"ms":T,...the app's fields}` or
  `{"event":"error","code":"timeout|model_error|...","id":ID}`.
- Two sets of limits, each a burst, the time one more comes back, and
  requests a UTC day: a client's own (an address; IPv6 by its /64), and the
  app's across its clients, so that a crowd of first requests cannot hold the
  model. lwdescribe: a visitor 1, 10 s, 20; the app 6, 20 s, 500. A request
  the app's limits or the line turn away costs the client nothing.
- A client holds one place in line at most, the line holds `waiting` places
  across apps, and one request runs at a time. A request's `deadline` covers
  its wait in line.

## Build

The jail runs two static binaries built for freebsd/amd64, on any machine
with Go 1.27.4 and GNU make 4 or BSD make (FreeBSD: `pkg install go127`),
the jail's host among them; nothing is built in the jail:

- `llmgate`, from this repository.
- `lw`, from LogWisp. lwdescribe checks every draft with it, so it is built
  in the LogWisp checkout of the engine the drafts are for: `LOGWISP`, by
  default `../logwisp`, a checkout beside this one. Given `SCHEMA`, that
  engine's `schema.json`, `lw --schema` must match it byte for byte.

```sh
make jail [LOGWISP=../logwisp] [SCHEMA=path/to/schema.json]
make check      # gofmt, vet, tests; the examples through lw once make lw ran
make testdata   # the tests' schema, from that lw, when LogWisp's schema changes
```

The binaries land in `jail/usr/local/bin/`, beside the template's files.

## The jail

One VNET jail with the two services as nologin users: `llm` runs Ollama,
`llmgate` the gate, each under daemon(8) as root, which holds the pidfile and
Ollama's log. The template, for Bastille 1.4 or later, is `jail/` with the two
binaries in it; copy it into Bastille's templates directory
(`bastille_templatesdir`, by default `/usr/local/bastille/templates`) as
`llmgate/jail`, with rsync from packages:

```sh
mkdir -p /usr/local/bastille/templates/llmgate
rsync -a --delete jail/ /usr/local/bastille/templates/llmgate/jail/   # or root@HOST: from elsewhere
```

On the server, create the jail as the other VNET jails on the jails' bridge
are, with their gateway and resolver, and apply the template:

```sh
bastille create -B -g GATEWAY -n RESOLVER llm RELEASE JAIL_ADDR/MASK BRIDGE
bastille template llm llmgate/jail --arg NGINX_ADDR=NGINX_ADDR   # optional: --arg MODEL=NAME
bastille limits llm add memoryuse 8G                              # with racct on, below
```

Nothing of a deployment is in this repository; it reaches the jail when the
template is applied, and through rc.conf after:

- `NGINX_ADDR`, the address the proxy connects from as the jail sees it: the
  one source ipfw admits to 8888 and the one proxy whose `X-Real-IP` llmgate
  believes.
- The jail's own address, which Bastille knows: llmgate's `listen`.
- `MODEL`, pulled once (default `qwen3:1.7b`): `model.name`.
- Anything else: `LLMGATE_` variables over `llmgate.toml`, by path, in the
  jail's rc.conf: `bastille sysrc llm llmgate_env="LLMGATE_MODEL_THREADS=6"`.

The template loads the jail's ipfw rules first (a new VNET may drop
everything until they load), installs `ollama`, creates the users, copies the
files and renders `llmgate.toml`, enables both services and pulls `MODEL`,
outbound through the host's NAT. It can be applied again: it stops llmgate,
overwrites the files it copies and starts both services again.

- Memory: rctl caps the jail, the model's memory with it, once the host
  runs with racct: `sysctl kern.racct.enable` says 1. Else
  `kern.racct.enable=1` in the host's `/boot/loader.conf` and a reboot, or no
  cap; Bastille's `limits` refuses while racct is off.
- CPU: no cpuset. Ollama's threads (`model.threads`, 4 by default) run on
  whichever cores are free; work that should yield to them runs at idle
  priority on the host (`idprio 31 COMMAND`).

Check it:

```sh
bastille cmd llm service llmgate check          # configuration ok, examples checked
bastille cmd llm fetch -qo - http://127.0.0.1:8889/  # ready, model, waiting, outcomes
fetch -qo - http://JAIL_ADDR:8888/lwdescribe    # from NGINX_ADDR, the one source ipfw admits
```

## The proxy

A route that strips its prefix, buffers nothing (the answer streams) and
counts POSTs alone, so a page's GET probe is free. With nginx, in `http`:

```nginx
map $request_method $llm_key {
    GET     "";
    default $remote_addr;    # $proxy_protocol_addr behind PROXY protocol
}
limit_req_zone $llm_key zone=llm:1m rate=6r/m;
upstream llm_gate {
    server JAIL_ADDR:8888;
    keepalive 4;
}
```

and in the `server` of the pages:

```nginx
location /PREFIX/ {
    proxy_pass http://llm_gate/;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header X-Real-IP $remote_addr;
    proxy_buffering off;
    proxy_read_timeout 60s;
    client_max_body_size 8k;
    limit_req zone=llm burst=3 nodelay;
    limit_req_status 429;
}
```

## Logs

- `/var/log/llmgate/llmgate.log`: operations (start, the model up or down,
  warm-ups); `[log]` keeps 10 MB files, 500 MB, 14 days
- `/var/log/llmgate/requests.log`: one JSON record a request; as above
- `/var/log/llm.log`: Ollama's own output; newsyslog keeps 10 MB, 7 files
- `/var/log/messages`: llmgate refusing to start or crashing, the jail's syslog

A request record: `id` (the one the page shows), `app`, `client` (an HMAC of
the address under a key that lives in memory, new each start, so a client is
one name within a UTC day and a restart), `outcome`
(`done|error|timeout|left|refused`), `code`, `error` (an error outcome's
cause), `queue_ms`, `run_ms`, and the
app's own: lwdescribe adds `calls`, `prompt_tokens`, `answer_tokens`,
`declined`, `refused`, `settled` (draft lines its code corrected) and, with `record_text`, `text`, `flags`, `problems`
and `refused_drafts` (each draft a repair replaced, with lw's words).
Reviewing, with jq on the host, under the jail's root:

```sh
jq -c 'select(.fields.outcome != "done") | .fields' requests.log
jq -r 'select(.fields.refused) | .fields | .text + "\n" + .flags + "\n" + .problems + "\n"' requests.log
```

## Updating

As the checkouts' owner, then as root on the jail's host; each script works
from its own checkout, wherever that is:

```sh
deploy/build.sh             # LOGWISP=DIR for a LogWisp not beside this one
deploy/update.sh            # -j JAIL for a jail not named llm
```

build.sh fast-forwards this checkout and LogWisp's to their latest and runs
`make jail`, `lw` stamped with its commit's time, so an unchanged LogWisp
builds the same `lw`. update.sh renders `llmgate.toml` and `ipfw.rules` from this checkout with the
installed `llmgate.toml`'s `listen`, `proxies` and `model.name`, and runs the
new llmgate's `-check` on them, the new `lw` and assets and the jail's
`llmgate_env` before it installs anything. Then it installs what differs,
restarts ipfw or llm when their files changed and llmgate after any change,
and waits for the gate to open.

## Operating

- A new model: `bastille cmd llm ollama pull NAME`, then
  `LLMGATE_MODEL_NAME=NAME` in `llmgate_env` and llmgate restarted; it warms
  the model before answering.
- Threads: `LLMGATE_MODEL_THREADS=N` in `llmgate_env`; a change reloads the
  model. Generation stops gaining at about the physical cores free; `run_ms`
  in `requests.log` compares.
- Faster: a smaller model (`qwen3:0.6b`, about three times as fast as
  `qwen3:1.7b`, with more mistakes) or fewer examples (a shorter prefix to
  warm, the same speed after).
- `LLMGATE_APPS_LWDESCRIBE_RECORD_TEXT=false` in `llmgate_env` keeps the
  numbers alone.
- Constrained output fails on the model server (the warm-up logs it):
  `LLMGATE_APPS_LWDESCRIBE_CONSTRAIN=array` in `llmgate_env`; the language and
  `lw --dump` still check every draft.

## Adding an app

An app is a package under `apps/` with a type that meets `gate.App`:

```go
Name() string                        // served at /NAME
Admission() gate.Admission           // limits: each client's own, and the app's across them
Info() map[string]any                // GET's fields beside model, waiting and limits
Take(body []byte) (gate.Task, error) // the body checked; *gate.Refusal tells the visitor
```

`Task.Run(ctx, model.Model)` returns the fields of the `done` event, and
`Task.Record()` what the review log keeps. A `Warm(ctx, model.Model)` method
fills the prompt cache with the app's fixed prefix before the gate opens, and
a `Guide() []byte` method is served at `/NAME/guide`. `main.go` gets a section
under `Config.Apps` and the app's `New`; every app shares the one model and
the one line.

## License

BSD 3-Clause, in [LICENSE](LICENSE).
