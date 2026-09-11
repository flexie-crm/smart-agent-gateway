# The inference node

One machine that holds models and answers questions with them. To the rest of
the product it is a vendor: it serves the same surface every hosted vendor
serves, so nothing above the adapter changes.

Design, the decisions behind it and what building it changed:
[KB/35](../KB/35-local-inference-and-workers.md).

## Building it for the machine it will run on

The accelerator is a build feature. Pick the one this box has; none means the
processor, which is right for a laptop and wrong for a server.

```sh
make node-build                 # processor only
make node-build ACCEL=cuda      # NVIDIA
make node-build ACCEL=metal     # Apple silicon
make node-build ACCEL=mkl       # Intel processors
make node-build ACCEL=accelerate
```

The first build compiles a large dependency tree and takes minutes. Later ones
do not.

## Running it

A machine adds itself. Tell it where the gateway is and give it the join token
(the Machines screen shows the line to paste, or `sag join-token` prints it):

```sh
SAG_URL=https://sag.internal \
  SAG_JOIN_TOKEN=<token> \
  ./target/release/sag-inference serve
```

It mints its own id and key on first boot, keeps them, registers itself, and
appears in the list. Nothing is typed anywhere else, and a machine that comes
back on a different address updates its own row rather than adding a second one.

Registering is also how it is given the certificate it serves with, so a machine
that has never registered cannot be reached at all: there is no unencrypted way
in. Both ends of every later connection present a certificate from the
deployment's own authority, which is what makes putting a machine on a public
address a reasonable thing to do. The join token names that authority
(`<fingerprint>_<secret>`) and the machine checks it before trusting
anything the gateway sends back, so an impostor answering the registration
cannot enrol it. The secret itself is never sent: the machine signs its request
with it instead.

The certificate is renewed by checking back in, once a day, and replaced under
the running listener. Nothing has to be restarted and nothing expires on a
machine that is up.

`sag-inference check` prints what this machine is and stops, which is worth
doing before anything depends on the node. It says whether the machine has been
enrolled, which is the difference between "not started yet" and "started and
unreachable".

| Setting | Default | What it is |
|---|---|---|
| `SAG_URL` | none | Where the gateway is. Without it the machine does not register itself. |
| `SAG_JOIN_TOKEN` | none | The shared secret that proves it may register. Needed with `SAG_URL`, and useless without it. |
| `SAG_NODE_ADVERTISE` | the address the gateway saw | Where the gateway should reach this machine. The default is right far more often than it is wrong: a machine behind NAT does not know the address that reaches it from outside. |
| `SAG_NODE_KEY` | minted and kept | The key every request must carry, inside the authenticated channel. Set it only when you have a reason to choose one; otherwise the machine mints a strong one for itself and there is no way to end up without one. |
| `SAG_NODE_ADDR` | `0.0.0.0:19443` | What to listen on. |
| `SAG_NODE_DATA` | `./data` | The directory this node owns. Nothing is written outside it. Its key, its certificate and the authority it trusts live here, so it is the one directory worth keeping. |
| `SAG_NODE_NAME` | the hostname | What this node calls itself, in logs and on screen. |
| `SAG_NODE_HUB_URL` | the public model library | Where models are searched for and fetched from. |
| `SAG_NODE_HUB_TOKEN` | none | Credentials for the library. Only needed for models published under terms that have to be accepted. |
| `SAG_NODE_LOG` | `sag_inference=info,warn` | How much to log. |

## Working on it

```sh
make node-check   # the fast loop: the control plane, seconds, no engine build
make node-ci      # the gate: format, lint and the full suite, engine included
```

`node-check` builds against a scripted engine, which is why it is quick. Both
have to pass before a change here is done.
