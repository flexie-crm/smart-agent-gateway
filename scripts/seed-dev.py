#!/usr/bin/env python3
"""Fill a development database with realistic data.

This drives the REAL REST API with a real token, so everything it creates went
through the same handlers, validation and permission checks a person clicking
the console would hit. Nothing is written behind the application's back, which
is the point: seed data that bypassed the API would be data the API can not
actually produce, and it would hide exactly the bugs a seed is meant to expose.

It is idempotent. Run it twice and the second run adopts what the first made
rather than failing on a conflict or duplicating it.

Usage:

    export SAG_SEED_ANTHROPIC_KEY=...   # optional, omit to create the vendor
    export SAG_SEED_OPENAI_KEY=...      # without a credential
    export SAG_SEED_DEEPSEEK_KEY=...
    python3 scripts/seed-dev.py --url http://localhost:8080 \
        --email admin@acme.test --password admin1234

Keys are read from the environment on purpose. A key in a file is a key in the
history of a repository.
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.request

from seed_content import BRAINS


class Api:
    """A thin REST client that fails loudly and says why."""

    def __init__(self, base_url: str) -> None:
        self.base_url = base_url.rstrip("/")
        self.token: str | None = None

    def _call(self, method: str, path: str, body=None):
        url = f"{self.base_url}{path}"
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"

        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req) as resp:
                raw = resp.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as err:
            detail = err.read().decode(errors="replace")
            # A conflict is the caller's business: it means "already there".
            if err.code == 409:
                raise Conflict(detail) from err
            raise SystemExit(
                f"\n{method} {path} failed with {err.code}\n"
                f"  sent: {json.dumps(body)[:400] if body else '(no body)'}\n"
                f"  got:  {detail[:400]}\n"
            ) from err
        except urllib.error.URLError as err:
            raise SystemExit(f"cannot reach {url}: {err.reason}") from err

    def get(self, path):
        return self._call("GET", path)

    def post(self, path, body):
        return self._call("POST", path, body)

    def put(self, path, body=None):
        return self._call("PUT", path, body)

    def login(self, email: str, password: str) -> None:
        result = self._call("POST", "/v1/auth/login", {"email": email, "password": password})
        self.token = result["access_token"]
        print(f"signed in as {result['user']['email']} in workspace "
              f"{result['workspace']['slug']}")


class Conflict(Exception):
    """The thing already exists."""


def ensure(api: Api, kind: str, path: str, body: dict, key: str, value: str):
    """Create a thing, or adopt the one already there.

    `key`/`value` identify it in the collection at `path`, so a second run finds
    what the first one made instead of colliding with it.
    """
    existing = api.get(path) or []
    for item in existing:
        if item.get(key) == value:
            print(f"  = {kind}: {value} (id {item['id']})")
            return item
    try:
        made = api.post(path, body)
    except Conflict:
        # Something else made it between the list and the post, or it collides
        # on a field we did not look at. Re-read and find it.
        for item in api.get(path) or []:
            if item.get(key) == value:
                return item
        raise
    print(f"  + {kind}: {value} (id {made['id']})")
    return made


# --- the data ---------------------------------------------------------------

GROUPS = ["Support", "Engineering", "Sales", "Revenue Operations", "Leadership"]

# Roles are the permission sets a real deployment would actually want, not one
# god-role: the point of seeding them is to have something to test scoping with.
ROLES = [
    ("Knowledge Curator", [
        "brains:view", "brains:create", "brains:edit", "brains:delete",
        "models:view", "agents:view",
    ]),
    ("Support Agent", [
        "brains:view", "agents:view", "tools:view", "models:view",
        "workflows:view",
    ]),
    ("Engineer", [
        "brains:view", "brains:edit", "agents:view", "agents:edit",
        "tools:view", "tools:edit", "models:view", "vendors:view",
        "workflows:view", "workflows:edit", "workflows:publish",
        "mcp-servers:view", "mcp-servers:create", "mcp-servers:edit",
    ]),
    ("Sales Representative", [
        "brains:view", "agents:view", "workflows:view",
    ]),
    ("Auditor", [
        "users:view", "groups:view", "roles:view", "workspaces:view",
        "brains:view", "agents:view", "tools:view", "models:view",
        "vendors:view", "workflows:view", "oauth-clients:view",
    ]),
]

# People. A seed with one user tests nothing about scoping or membership.
USERS = [
    ("elira.hoxha@acme.test", "Elira Hoxha", ["Support"]),
    ("marco.bianchi@acme.test", "Marco Bianchi", ["Support"]),
    ("sara.duval@acme.test", "Sara Duval", ["Support", "Revenue Operations"]),
    ("tomas.novak@acme.test", "Tomas Novak", ["Engineering"]),
    ("aisha.rahman@acme.test", "Aisha Rahman", ["Engineering"]),
    ("liam.oconnor@acme.test", "Liam O'Connor", ["Engineering", "Leadership"]),
    ("nina.petrova@acme.test", "Nina Petrova", ["Sales"]),
    ("diego.ramos@acme.test", "Diego Ramos", ["Sales", "Revenue Operations"]),
    ("yuki.tanaka@acme.test", "Yuki Tanaka", ["Revenue Operations"]),
    ("claire.dubois@acme.test", "Claire Dubois", ["Leadership"]),
]

SEED_PASSWORD = "seed-Passw0rd!"  # 14 chars: clears the 10-character floor.

# Vendors, and the models a real deployment would register against them. Prices
# are per million tokens.
#
# EVERY model id here must be one the vendor actually offers. The gateway checks
# it against the vendor's own catalog and refuses one that is not, which is the
# whole point: a model id is typed by a person, and a person mistypes. This seed
# did exactly that (deepseek-chat, claude-haiku-4-5), and its own validation
# caught it. When a vendor retires a model, this list is what needs correcting.
VENDORS = [
    {
        "vendor_key": "anthropic",
        "name": "Anthropic",
        "env": "SAG_SEED_ANTHROPIC_KEY",
        "models": [
            ("claude-opus-4-8", "chat", 200000, True, True, True, 5.0, 25.0),
            ("claude-sonnet-5", "chat", 200000, True, True, True, 3.0, 15.0),
            ("claude-haiku-4-5-20251001", "chat", 200000, True, True, False, 1.0, 5.0),
        ],
    },
    {
        "vendor_key": "openai",
        "name": "OpenAI",
        "env": "SAG_SEED_OPENAI_KEY",
        "models": [
            ("gpt-4.1", "chat", 1047576, True, True, False, 2.0, 8.0),
            ("gpt-4.1-mini", "chat", 1047576, True, True, False, 0.4, 1.6),
            ("o3", "chat", 200000, True, True, True, 2.0, 8.0),
            ("text-embedding-3-large", "embedding", 8191, False, False, False, 0.13, 0.0),
        ],
    },
    {
        "vendor_key": "deepseek",
        "name": "DeepSeek",
        "env": "SAG_SEED_DEEPSEEK_KEY",
        "models": [
            ("deepseek-v4-flash", "chat", 65536, True, True, False, 0.27, 1.1),
            ("deepseek-v4-pro", "chat", 65536, True, True, True, 0.55, 2.19),
        ],
    },
    {
        # A local runtime, which needs no credential at all. Worth seeding
        # because "the vendor with no secret" is its own case in the UI.
        "vendor_key": "openai-compatible",
        "name": "Local Runtime",
        "base_url": "http://localhost:11434/v1",
        "env": None,
        "models": [
            ("llama3.1:8b", "chat", 131072, True, True, False, 0.0, 0.0),
        ],
    },
]

AGENTS = [
    {
        "key": "default",
        "name": "Assistant",
        "instructions": (
            "You are the Acme assistant. Answer from the knowledge you are given "
            "before you answer from memory, and say plainly when you do not know. "
            "Be brief: a person reading you is usually in the middle of something else."
        ),
        "model": "claude-sonnet-5",
        "reasoning": False,
        "tools": None,  # no opinion: inherits whatever the layer below allows
    },
    {
        "key": "support",
        "name": "Support Assistant",
        "instructions": (
            "You help the support team answer customer questions. Ground every answer "
            "in the support playbook and quote the policy you are relying on. If a "
            "question needs a refund, a credit, or anything that touches a customer's "
            "money, say what you would do and hand it to a person: you propose, they decide."
        ),
        "model": "claude-sonnet-5",
        "reasoning": False,
        "tools": None,
        "approval_ttl_seconds": 3600,
    },
    {
        "key": "sales",
        "name": "Sales Assistant",
        "instructions": (
            "You help the sales team prepare for calls. Use the sales playbook for "
            "pricing, positioning and objection handling. Never invent a discount or a "
            "commitment we have not made, and never quote a price that is not in the playbook."
        ),
        "model": "gpt-4.1",
        "reasoning": False,
        "tools": None,
    },
    {
        "key": "engineering",
        "name": "Engineering Copilot",
        "instructions": (
            "You help engineers operate the platform. Prefer the runbook over your own "
            "recollection. When an action would change a running system, describe the "
            "change and its blast radius before you propose it."
        ),
        "model": "claude-opus-4-8",
        "reasoning": True,
        "tools": None,
        "approval_ttl_seconds": 900,
    },
    {
        "key": "curator",
        "name": "Knowledge Curator",
        "instructions": (
            "You keep the knowledge base honest. When you find an answer that the "
            "knowledge base contradicts, say so and quote both. When you find a gap, "
            "say what is missing rather than filling it with a guess."
        ),
        "model": "deepseek-v4-pro",
        "reasoning": True,
        "tools": None,
    },
]

# Workflows: the same assistant, made different for a person on a channel.
WORKFLOWS = [
    {
        "name": "Support desk",
        "agent_prompt": (
            "You are answering inside the support desk. Be exact, cite the policy, and "
            "never promise a remedy the playbook does not allow."
        ),
        "model": "claude-sonnet-5",
        "reasoning": False,
        "approval_ttl_seconds": 3600,
        "assign_group": "Support",
    },
    {
        "name": "Sales floor",
        "agent_prompt": (
            "You are helping on the sales floor. Lead with the customer's problem, not "
            "our feature list. Pricing comes from the playbook, never from memory."
        ),
        "model": "gpt-4.1",
        "reasoning": False,
        "approval_ttl_seconds": None,
        "assign_group": "Sales",
    },
    {
        "name": "Engineering on-call",
        "agent_prompt": (
            "You are assisting an on-call engineer. Assume something is broken and time "
            "matters: lead with the check that discriminates fastest between causes."
        ),
        "model": "claude-opus-4-8",
        "reasoning": True,
        "approval_ttl_seconds": 900,
        "assign_group": "Engineering",
    },
]


def seed_identity(api: Api):
    print("\ngroups")
    groups = {}
    for name in GROUPS:
        groups[name] = ensure(api, "group", "/v1/groups", {"name": name}, "name", name)

    print("\nroles")
    roles = {}
    for name, permissions in ROLES:
        roles[name] = ensure(api, "role", "/v1/roles",
                             {"name": name, "permissions": permissions}, "name", name)

    # A group without a role grants nothing, so bind them.
    print("\ngroup -> role")
    binding = {
        "Support": "Support Agent",
        "Engineering": "Engineer",
        "Sales": "Sales Representative",
        "Revenue Operations": "Auditor",
        "Leadership": "Auditor",
    }
    for group_name, role_name in binding.items():
        api.put(f"/v1/groups/{groups[group_name]['id']}/roles/{roles[role_name]['id']}")
        print(f"  = {group_name} -> {role_name}")

    print("\nusers")
    for email, name, member_of in USERS:
        user = ensure(api, "user", "/v1/users",
                      {"email": email, "name": name, "password": SEED_PASSWORD},
                      "email", email)
        ids = [groups[g]["id"] for g in member_of]
        api.put(f"/v1/users/{user['id']}/groups", {"groups": ids})
        print(f"      groups: {', '.join(member_of)}")

    return groups


def seed_vendors(api: Api):
    print("\nvendors and models")
    models = {}
    for spec in VENDORS:
        body = {"vendor_key": spec["vendor_key"], "name": spec["name"]}
        if spec.get("base_url"):
            body["base_url"] = spec["base_url"]

        key = os.environ.get(spec["env"]) if spec["env"] else None
        if key:
            body["credentials"] = key

        vendor = ensure(api, "vendor", "/v1/vendors", body, "name", spec["name"])
        if key and not vendor.get("has_credentials"):
            # Adopted an existing vendor that has no secret: give it one.
            api.put(f"/v1/vendors/{vendor['id']}", {
                "vendor_key": vendor["vendor_key"], "name": vendor["name"],
                "base_url": vendor.get("base_url", ""), "credentials": key,
                "status": "active",
            })
            print(f"      credentials sealed")
        elif not key and spec["env"]:
            print(f"      no {spec['env']} in the environment: no credential stored")

        for (model_key, kind, window, tools, streaming, reasoning,
             price_in, price_out) in spec["models"]:
            made = ensure(api, "model", "/v1/models", {
                "vendor_id": vendor["id"],
                "model_key": model_key,
                "type": kind,
                "context_window": window,
                "supports_tools": tools,
                "supports_streaming": streaming,
                "supports_reasoning": reasoning,
                "privacy_level": "local" if spec["vendor_key"] == "openai-compatible"
                                 else "external_vendor",
                "input_price_per_1m": price_in,
                "output_price_per_1m": price_out,
            }, "model_key", model_key)
            models[model_key] = made
    return models


def seed_agents(api: Api, models: dict):
    print("\nagents")
    for spec in AGENTS:
        model = models.get(spec["model"])
        body = {
            "key": spec["key"],
            "name": spec["name"],
            "instructions": spec["instructions"],
            "model_id": model["id"] if model else None,
            "reasoning": spec["reasoning"],
            "status": "active",
            "tools": spec["tools"],
        }
        if spec.get("approval_ttl_seconds"):
            body["approval_ttl_seconds"] = spec["approval_ttl_seconds"]
        ensure(api, "agent", "/v1/agents", body, "key", spec["key"])


def seed_workflows(api: Api, models: dict, groups: dict):
    print("\nworkflows")
    for spec in WORKFLOWS:
        flow = ensure(api, "workflow", "/v1/workflows", {"name": spec["name"]},
                      "name", spec["name"])

        profile = {
            "system_prompt": spec["agent_prompt"],
            "reasoning": spec["reasoning"],
        }
        model = models.get(spec["model"])
        if model:
            profile["model_id"] = model["id"]
        if spec["approval_ttl_seconds"]:
            profile["approval_ttl_seconds"] = spec["approval_ttl_seconds"]

        versions = api.get(f"/v1/workflows/{flow['id']}/versions") or []
        if versions:
            version = versions[-1]
            print(f"      version {version['version']} already there")
        else:
            version = api.post(f"/v1/workflows/{flow['id']}/versions", {
                "definition": {"kind": "profile", "profile": profile},
            })
            print(f"      + version {version['version']}")

        # A workflow with no assignment matches nobody, so it is not really live.
        api.put(f"/v1/workflows/{flow['id']}/assignments", [
            {"match_type": "group",
             "match_value": str(groups[spec["assign_group"]]["id"]),
             "priority": 100},
            {"match_type": "channel", "match_value": "chat", "priority": 10},
        ])
        api.post(f"/v1/workflows/{flow['id']}/publish", {"version_id": version["id"]})
        print(f"      published, assigned to {spec['assign_group']} on chat")


def seed_tools(api: Api, groups: dict):
    """Tools are born from the code registry; what an admin owns is the policy."""
    print("\ntools")
    tools = api.get("/v1/tools") or []
    if not tools:
        print("  (none registered)")
        return
    for tool in tools:
        # Leave the code's own judgement alone where it locked approval on.
        print(f"  = {tool['name']}  risk={tool.get('risk')} "
              f"approval={tool.get('requires_approval')}"
              f"{' (locked)' if tool.get('approval_locked') else ''}")


def seed_brains(api: Api):
    print("\nbrains")
    for brain_spec in BRAINS:
        brain = ensure(api, "brain", "/v1/brains", {
            "name": brain_spec["name"],
            "description": brain_spec["description"],
            "locked": brain_spec.get("locked", False),
        }, "name", brain_spec["name"])

        # Remember every document by title so the cross-links can be resolved
        # after all of them exist: a link can only point at a document that does.
        made: dict[str, int] = {}
        existing_categories = api.get(f"/v1/brains/{brain['id']}/categories") or []
        by_name = {c["name"]: c for c in existing_categories}

        for weight, cat_spec in enumerate(brain_spec["categories"]):
            if cat_spec["name"] in by_name:
                category = by_name[cat_spec["name"]]
                print(f"  = category: {cat_spec['name']}")
            else:
                category = api.post(f"/v1/brains/{brain['id']}/categories", {
                    "name": cat_spec["name"],
                    "description": cat_spec["description"],
                    "weight": weight * 10,
                })
                print(f"  + category: {cat_spec['name']}")

            already = {d["title"] for d in (category.get("documents") or [])}
            for doc_weight, (title, content) in enumerate(cat_spec["documents"]):
                if title in already:
                    print(f"      = {title}")
                    continue
                doc = api.post(f"/v1/brain-categories/{category['id']}/documents", {
                    "title": title,
                    "content": content,
                    "weight": doc_weight * 10,
                    "related": [],
                })
                made[title] = doc["id"]
                print(f"      + {title}")

        # Now the graph. Links are made symmetric by the store, so stating each
        # edge once from one side is enough.
        for title, related_titles in brain_spec.get("links", {}).items():
            if title not in made:
                continue
            targets = [made[t] for t in related_titles if t in made]
            if not targets:
                continue
            document = api.get(f"/v1/brain-documents/{made[title]}")
            api.put(f"/v1/brain-documents/{made[title]}", {
                "title": document["title"],
                "content": document["content"],
                "weight": document.get("weight", 0),
                "category_id": document["category_id"],
                "related": targets,
            })
            print(f"      ~ {title} -> {len(targets)} link(s)")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://localhost:8080")
    parser.add_argument("--email", default="admin@acme.test")
    parser.add_argument("--password", default="admin1234")
    args = parser.parse_args()

    api = Api(args.url)
    api.login(args.email, args.password)

    groups = seed_identity(api)
    models = seed_vendors(api)
    seed_agents(api, models)
    seed_workflows(api, models, groups)
    seed_tools(api, groups)
    seed_brains(api)

    print("\ndone.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
