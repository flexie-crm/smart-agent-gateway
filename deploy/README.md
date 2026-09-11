# Deploying Flexie SAG

One box, one stack, two front doors. This directory is the whole of it.

## Before you start

A server with **Docker**, and two names in DNS **already pointing at it**: one
for the console, one for the chat. Certificates are issued over the HTTP
challenge, so a name that resolves somewhere else does not get one and keeps not
getting one. This is the single most common reason a first deploy sits there
looking broken.

Sizing, for the product without local models: **2 cores, 4 GB, 20 GB** is
comfortable. Running models here as well is a different question, answered under
[Models on this box](#models-on-this-box).

Nothing in this stack is published except the two front doors. The database, the
queue and the machine that runs models are reachable only from inside it.

## Which edge

`sag.yml` is the product and publishes nothing. How the two surfaces are reached
is a second file, because that is the part that depends on what is already on the
box.

**A box that already runs Traefik** (Swarm): it routes, we label, and nothing of
its configuration is touched.

```sh
./secrets.sh > .env                # once. Keep this file.
$EDITOR .env                       # the two domains
./build.sh                         # Swarm starts images; it does not build them
docker stack deploy -c sag.yml -c edge.traefik.yml sag
```

**A box with nothing on 80 and 443**: we bring Caddy, which gets and renews the
certificates by itself.

```sh
./secrets.sh > .env
$EDITOR .env
docker compose -f sag.yml -f edge.caddy.yml up -d --build
```

Either way, make the first workspace and the first person who can sign in:

```sh
# Swarm
docker exec $(docker ps -qf name=sag_orchestrator) sag bootstrap \
  -workspace acme -workspace-name "Acme" \
  -email you@example.com -password 'something long'

# compose
docker compose -f sag.yml -f edge.caddy.yml run --rm orchestrator bootstrap \
  -workspace acme -workspace-name "Acme" \
  -email you@example.com -password 'something long'
```

The console is then at `https://<SAG_CONSOLE_DOMAIN>` and the chat at
`https://<SAG_CHAT_DOMAIN>`.

### Why a stack of its own

On a box that already runs something, it is tempting to add these services to the
stack that is already there. Don't. `docker stack deploy` replaces a stack's
whole definition, so every future SAG deploy would redeploy whatever else is in
it, and a mistake in this file would reach further than SAG. A separate stack on
the same proxy network gets the same routing and the same certificates with none
of that.

## The one file you must not lose

`.env` holds **`SAG_ENCRYPTION_KEYS`**, and it is the key every stored credential
is sealed with: vendor API keys, every machine's key, and the authority every
machine's certificate chains to. There is no recovery. Losing it does not lose
the database, it makes the secrets *in* the database permanently unreadable, and
you find out at the next restart.

Back it up somewhere that is not this server, and note that the stack **refuses
to start** without it rather than quietly generating a new one per boot. That
refusal is deliberate: the alternative is a deployment that works all week and
loses every credential the first time it is restarted.

## What is running

| Service | What it is |
|---|---|
| `orchestrator` | The API, and the console it serves from one origin. |
| `worker` | Background work: titles, memory, downloads. |
| `chat` | The chat, and a proxy so its `/v1` is same-origin. |
| `migrate` | Runs once at every start, before the two above. |
| `mariadb`, `nats`, `redis` | What it runs on. Never published. |
| `machine` | Optional; models on this box. See below. |
| `caddy` | Only with `edge.caddy.yml`. TLS and the two names. |

Schema changes are a **step**, not something the server does on boot: two
containers starting together would race, and a migration a request can trigger
is a migration that runs under load. `migrate` runs to completion first and the
others wait for it.

## Models on this box

Off by default. The image takes about twenty minutes to build and the models
themselves are gigabytes, so this belongs on hardware chosen for it.

If this box is that hardware:

```sh
# Swarm
docker exec $(docker ps -qf name=sag_orchestrator) sag join-token
$EDITOR .env                                   # SAG_JOIN_TOKEN, SAG_NODE_ACCEL
SAG_NODE_ACCEL=... ./build.sh machine
docker stack deploy -c sag.yml -c edge.traefik.yml sag   # now includes it

# compose
docker compose -f sag.yml -f edge.caddy.yml exec orchestrator sag join-token
$EDITOR .env
docker compose -f sag.yml -f edge.caddy.yml --profile machine up -d --build machine
```

`SAG_NODE_ACCEL` is a **build** choice, because the engine is compiled for the
hardware it runs on: `cuda` (NVIDIA, and the host needs the NVIDIA container
toolkit), `mkl` or `accelerate` (Intel), `metal` (Apple), or empty for the
processor. Empty works everywhere and is slow; a small model on a CPU answers in
seconds, a large one in minutes.

The machine registers itself and appears under **Machines** in the console. It is
never published outside the stack, and the connection to it is authenticated at
both ends (see [KB/35](../KB/35-local-inference-and-workers.md)).

Room: a model is roughly its parameter count in bytes at 8-bit, so a 7B model is
~7 GB on disk and about the same in memory while it is loaded. Plan disk for what
you intend to keep and memory for what you intend to have loaded at once.

## A machine somewhere else

The machine does not have to be in this stack. Anywhere the orchestrator can
reach it works, including a public address, because both ends authenticate with
certificates from this deployment's own authority.

The one requirement is that the machine can reach the orchestrator **once** to
register, and once a day after that to renew. Run it with:

```sh
SAG_URL=https://<SAG_CONSOLE_DOMAIN> \
  SAG_JOIN_TOKEN=<token> \
  SAG_NODE_ADVERTISE=<the address this box is reachable at>:19443 \
  sag-inference serve
```

Set `SAG_NODE_ADVERTISE` explicitly on a public box. The default is the address
we saw the registration come from, which is right behind NAT and wrong when the
machine is reached on a different address than it appears to come from.

## Day to day

```sh
# Swarm
docker service logs -f sag_orchestrator
docker stack services sag
./build.sh && docker stack deploy -c sag.yml -c edge.traefik.yml sag   # new version
docker exec $(docker ps -qf name=sag_orchestrator) sag migrate status

# compose
docker compose -f sag.yml -f edge.caddy.yml logs -f orchestrator
docker compose -f sag.yml -f edge.caddy.yml ps
docker compose -f sag.yml -f edge.caddy.yml up -d --build
```

Upgrading runs `migrate` first, then replaces the server. The server shuts down
gracefully: it stops accepting, finishes what is in flight within
`SAG_SHUTDOWN_GRACE`, and records whatever is left rather than losing it.

### Backing up

Two things, and they are different in kind:

```sh
# The data.
docker compose exec mariadb mariadb-dump -usag -p"$SAG_DB_PASSWORD" sag > sag.sql
# The bytes people uploaded.
docker run --rm -v flexie-sag_uploads:/from -v "$PWD":/to alpine \
  tar czf /to/uploads.tgz -C /from .
```

And `.env`, which is neither and matters more than both: a database restored
without its key is a database whose credentials cannot be read.

## When something is wrong

**The stack will not start and complains about a variable.** That is the guard
working. Run `./secrets.sh > .env` if you have not, or fill in the name it
mentions.

**No certificate.** Both names must resolve to THIS box and the HTTP challenge
must reach it. Check what a name actually points at (`getent hosts <name>`) and
what this box's public address is (`curl ifconfig.me`) before looking anywhere
else: a name aimed at a different server is the usual answer, and the proxy's
log will say the challenge was never answered.

**The console loads and every request fails.** `SAG_BASE_URL` must be the address
a browser actually reaches, because it is also the token issuer. It is derived
from `SAG_CONSOLE_DOMAIN`, so check that.

**A machine says it cannot serve.** It has no certificate, which means it never
registered. It will say which of `SAG_URL` and `SAG_JOIN_TOKEN` it is missing;
if it has both, either it cannot reach the orchestrator or its token is spent or
past its hour. Mint another (Machines, or `sag join-token`) and restart it: a
token is single use, so one left over from a previous attempt will not work
twice. A machine that HAS registered needs no token at all, ever again.
