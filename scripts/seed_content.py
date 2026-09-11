"""The knowledge the seeded brains are filled with.

Kept apart from the script that loads it: one file decides HOW things are
created, this one decides WHAT. The content is deliberately written the way a
real knowledge base is written, because a brain full of "lorem ipsum" proves
nothing about search, about drill-down, or about an agent that has to answer
from it.

Product vocabulary only. No implementation names appear in anything a user or
an agent can read.
"""

BRAINS = [
    # ------------------------------------------------------------------ 1
    {
        "name": "Support Playbook",
        "description": "How we answer customers: policies, limits, and the "
                       "line between what an assistant may decide and what a "
                       "person must.",
        "locked": False,
        "categories": [
            {
                "name": "Billing",
                "description": "Invoices, plans, proration, failed payments.",
                "documents": [
                    ("Plans and what they include", """# Plans

We sell three plans. The plan sets the seat count, the monthly conversation
allowance, and whether a workspace may connect its own tool servers.

| Plan | Seats | Conversations / month | Own tool servers |
|---|---|---|---|
| Starter | 5 | 2,000 | No |
| Team | 25 | 20,000 | Yes |
| Scale | Unlimited | 200,000 | Yes |

A conversation is counted when the assistant produces its first answer, not
when a person opens a window. An abandoned draft costs nothing.

**Overage.** Going past the allowance does not stop the workspace. It bills at
the overage rate on the next invoice, and the workspace owner gets a notice at
80% and again at 100%. We do not silently degrade the service: a customer who
is over their allowance still gets answers.
"""),
                    ("Proration when a plan changes", """# Proration

**Upgrades take effect immediately.** The customer is charged the difference
for the remainder of the billing period, calculated by the day, and the new
limits apply the moment the change is saved.

**Downgrades take effect at the end of the period.** They keep what they paid
for until it runs out. We do not refund the difference on a downgrade, and we
do not remove seats mid-period, because a seat that vanishes mid-month locks a
person out of a conversation they are in the middle of.

If a downgrade would leave the workspace with fewer seats than it has active
people, the change is refused with the names of the people who would be locked
out. Ask the customer to remove the seats first. This is the one place where we
would rather annoy an administrator than surprise a user.
"""),
                    ("A payment failed. What happens", """# Failed payments

A failed payment does not close an account, and it does not take the assistant
away on the same day.

1. **Day 0.** The charge fails. We retry, and email the billing contact.
2. **Day 3 and day 7.** We retry again. The workspace still works normally.
3. **Day 14.** The workspace goes read-only: conversations can be read, no new
   ones can be started. Everything is still there.
4. **Day 45.** The workspace is suspended. Data is kept for 90 more days and
   can be restored in full by paying the outstanding invoice.

Nothing is deleted before day 135, and a customer who pays at any point before
that gets everything back exactly as it was. If a customer is on day 40 and
distressed, you may extend the read-only period by 14 days once, without asking
anyone. Say that you have done it and when it now expires.
"""),
                    ("Invoices, receipts, and VAT", """# Invoices

Invoices are issued on the first of the month for the month just ended, and are
available under Billing in the console. Any administrator can download them;
the customer does not need to ask us.

**Changing a past invoice.** We can change the billing address, the company
name and the VAT number on an issued invoice, and reissue it. We cannot change
the amount, the dates, or what was billed: an invoice is a record of what
happened, and rewriting it is not a thing we do.

**VAT.** Business customers in the EU with a valid VAT number are not charged
VAT. If a customer was charged VAT because their number was missing or invalid
at the time, add the number, and we credit the VAT on the next invoice rather
than reissuing the old one.
"""),
                ],
            },
            {
                "name": "Refunds and credits",
                "description": "What we give back, when, and who may decide it.",
                "documents": [
                    ("The refund policy", """# Refunds

**Within 30 days of the first payment**, a customer who asks for a refund gets
one. No reason is required and no approval is needed. Say yes, and say it
without making them justify it.

**After 30 days**, we do not refund a period the customer used. We credit
instead: the credit is applied to the next invoice, and it never expires.

**We always refund, at any time, when the fault is ours.** An outage that broke
a customer's working day, a bug that lost their work, a charge for something we
did not deliver: refund it. If you are hesitating over whether something counts
as our fault, it counts as our fault.

An assistant may **propose** a refund and prepare it. It may never **issue**
one: money leaving the company is a decision a person makes. This is not
because the assistant would get it wrong. It is because a customer who has been
wronged deserves a person who says so.
"""),
                    ("Service credits for an outage", """# Service credits

When the platform is unavailable, credits are automatic. The customer does not
have to notice, ask, or prove anything.

| Availability in a month | Credit |
|---|---|
| Below 99.9% | 10% of the monthly fee |
| Below 99.0% | 25% |
| Below 95.0% | 50% |

Availability is measured against the assistant answering, not against a page
loading. A console that renders while every answer times out is an outage, and
counting it as uptime would be a lie we tell ourselves.

Credits are applied within one billing period and appear as a line on the
invoice that names the incident. A customer should be able to read their
invoice and see that we admitted it.
"""),
                    ("When a customer disputes a charge", """# Chargebacks

A chargeback is a customer telling their bank they could not get an answer from
us. Treat the dispute as a support failure first and a billing event second.

1. **Contact them the same day.** Ask what the charge was for, in their words.
2. **If the charge was wrong, refund it** and withdraw the dispute together.
   Do not fight a dispute you would have refunded.
3. **If the charge was right**, send the invoice, the usage, and the date they
   agreed to the plan. Politely.

Never suspend a workspace because a dispute was opened. A customer in a dispute
is still a customer, and taking their tools away mid-argument is how a
disagreement becomes an ex-customer.
"""),
                ],
            },
            {
                "name": "Escalation",
                "description": "When to stop answering and get a person.",
                "documents": [
                    ("What must be escalated", """# Escalate these, always

Some things are not for an assistant, and not for a first-line agent either.
Hand them over immediately, and say to the customer that you are doing it.

- **Anything about a person's safety.** Stop. Get a human. Now.
- **A legal threat, or a regulator.** Do not answer the substance, do not
  apologise in a way that concedes fact, do not speculate. Acknowledge, and
  escalate.
- **A data breach, suspected or claimed.** Even if you are sure it is nothing.
  Especially then.
- **A demand for another customer's data**, however plausible the reason.
- **A refund we would not normally give**, above 500 EUR.
- **Anything that reads as a person in distress**, whatever it is nominally about.

The rule behind the list: escalate anything where being wrong is expensive and
being slow is cheap. That is the opposite of the usual instinct, which is why
it has to be written down.
"""),
                    ("How to hand over without losing the customer", """# The handover

A bad handover is worse than no handover, because the customer tells their
story twice and concludes that nobody is listening.

**Before you hand over**, write three lines: what they want, what has been
tried, and what you think is going on. The person taking it should never have
to ask the customer to start again.

**Tell the customer what happens next**, with a time: "Sara is picking this up,
and she will write to you today." Then make sure Sara actually does. A promise
of a person is a promise.

**Stay on the thread** until the other person has actually answered. A handover
is not a door you close behind you.
"""),
                    ("Priority and response times", """# Priorities

| Priority | What it means | First response |
|---|---|---|
| Urgent | Nobody in the workspace can work | 1 hour, any hour |
| High | A team is blocked, or money is wrong | 4 working hours |
| Normal | It is broken but there is a way around it | 1 working day |
| Low | A question, a request, an idea | 3 working days |

The customer's own words decide the priority, not our judgement of them. If
they say they are blocked, they are blocked. We can be wrong about how bad
something is; they cannot be wrong about how bad it is for them.

A first response is a person engaging with the problem. An automated
acknowledgement is not a first response, and counting it as one is how support
teams learn to lie about their numbers.
"""),
                ],
            },
            {
                "name": "Common issues",
                "description": "The things that actually get asked.",
                "documents": [
                    ("The assistant says it cannot see a tool", """# A tool is missing

When someone says the assistant "does not have" a tool, one of four things is
true, and they are quick to tell apart in this order:

1. **The tool is disabled** in the workspace. Check the tool list. A disabled
   tool is invisible to the assistant, deliberately.
2. **The person is not in a group the tool is granted to.** Tool grants are by
   group. A tool with no grants is available to everyone; a tool with grants is
   available only to those groups.
3. **The workflow that matched them does not allow it.** A workflow can narrow
   the tool set for a person on a channel. This is the one people forget.
4. **The tool server it came from is unreachable**, and the tool is flagged as
   missing.

The fourth case looks like the first three but is not a configuration problem.
Check the connection before you go changing permissions, or you will "fix" a
working configuration and make the real fault harder to find.
"""),
                    ("Answers are slower than they were", """# Slow answers

First, separate slow to *start* from slow to *finish*.

**Slow to start** (nothing appears for several seconds) is almost always the
model provider queueing us, or a workspace whose assistant is configured with a
reasoning model for a job that does not need one. Reasoning models think before
they speak, and that is exactly what the customer is watching.

**Slow to finish** (it starts immediately, then crawls) is usually a long
conversation. Every turn carries the ones before it. Suggest starting a new
conversation for a new subject, which is good practice anyway.

**A conversation that stalls mid-answer** and then resumes is not slow, it is
being retried. That is the platform doing its job, and it is worth telling the
customer that the answer will still arrive.
"""),
                    ("The assistant answered from the wrong knowledge", """# It used the wrong brain

An assistant answers from the knowledge it was given. If the answer came from
the wrong place, the assistant is rarely the thing that is wrong.

- **Was another brain assigned?** An assistant reads only the brains attached
  to it.
- **Did the search find the wrong document?** Look at what it cited. A document
  with a vague title gets found for the wrong questions.
- **Are there two documents that disagree?** Then the assistant picked one, and
  the real problem is that both exist. Fix the knowledge, not the prompt.

The last case is the common one, and it is a knowledge problem wearing the
costume of a bug. When two documents contradict each other, an assistant cannot
be right; it can only be lucky.
"""),
                ],
            },
        ],
        "links": {
            "The refund policy": ["Service credits for an outage",
                                  "What must be escalated",
                                  "When a customer disputes a charge"],
            "A payment failed. What happens": ["Plans and what they include",
                                               "The refund policy"],
            "What must be escalated": ["How to hand over without losing the customer",
                                       "Priority and response times"],
            "The assistant says it cannot see a tool": [
                "The assistant answered from the wrong knowledge"],
        },
    },

    # ------------------------------------------------------------------ 2
    {
        "name": "Product Manual",
        "description": "What the product is, what its pieces are called, and "
                       "how they fit together.",
        "locked": False,
        "categories": [
            {
                "name": "Getting started",
                "description": "The first hour.",
                "documents": [
                    ("What this product is", """# The assistant, and everything behind it

The product has two faces.

**The chat client** is what most people ever see: a place to ask, and an
assistant that answers, uses tools on your behalf, and asks before it does
anything it should ask about.

**The console** is where that assistant is decided. Who it is, which model
speaks for it, what it is allowed to do, what it knows, and how all of that
changes depending on who is asking and where they are asking from.

The second one is the product. The first one is the part you show people.
"""),
                    ("The first ten minutes", """# Setting up

1. **Connect a model provider.** Add a vendor, paste its key, register at least
   one model. Nothing answers until something can.
2. **Check the default assistant.** There is one, called Assistant. Give it the
   model you just registered.
3. **Say something to it.** If it answers, the spine of the system works: the
   client reached the gateway, the gateway reached the provider, and the answer
   came back through every layer in between.
4. **Then, and only then**, start configuring. Tools, knowledge, workflows.

Doing it in this order means that when something breaks, you know which change
broke it. Configuring everything and then testing is how an afternoon
disappears.
"""),
                    ("Words we use, and what they mean", """# Vocabulary

- **Workspace** — a tenant. People, knowledge, tools and assistants belong to
  exactly one, and nothing crosses between them.
- **Agent** — an assistant's identity: its instructions, its model, its tools.
- **Tool** — something the assistant can *do*, as opposed to say. Every tool
  declares how dangerous it is.
- **Brain** — curated knowledge the assistant can navigate: categories,
  documents, and the links between them.
- **Workflow** — a rule that changes the assistant for some people on some
  channels. This is how one deployment serves a support team and a sales team
  without being two deployments.
- **Channel** — where the asking happens: the chat client, an API call, or
  another agent connecting to us.
- **Run** — one turn of the assistant answering. A run outlives the window that
  started it.

If you learn one of these, learn *workflow*. It is the idea the rest hangs off.
"""),
                ],
            },
            {
                "name": "Assistants and tools",
                "description": "What the assistant is allowed to do.",
                "documents": [
                    ("How an assistant answers", """# The loop

An assistant does not simply reply. It runs a loop:

1. It reads the conversation, its instructions, and the knowledge it has.
2. It either **answers**, or it **asks for a tool**.
3. If it asks for a tool, the tool runs, and the result comes back into the
   loop as something it now knows.
4. Round it goes, until it has an answer.

Two things make this safe. The loop is **bounded**: it cannot run forever.
And every tool call is **checked at the moment it happens**, against who is
asking, not against who was asking when the conversation started.

That second point matters more than it sounds. Permission is not a ticket you
are handed at the door and can wave for the rest of the evening.
"""),
                    ("Risk, and why some tools stop and ask", """# Confirmation

Every tool declares its risk. Reading is not writing; writing to us is not
writing to a customer; and none of those are moving money.

- `read_only` — looks at something.
- `internal_write` — changes something of ours.
- `external_communication` — says something to someone outside.
- `financial_action` — money moves.
- `destructive_action` — something ceases to exist.
- `admin_action` — the rules themselves change.

Anything above the reading line stops and asks. The assistant prepares the
action, shows you exactly what it is about to do, and waits.

**An approved action is an action that works.** Everything that could fail was
checked *before* you were asked, so "yes" is never followed by "actually, no".
An approval that then fails is not an error message: it is a broken promise,
and we treat it as a bug in the design.
"""),
                    ("Where tools come from", """# Tools

Tools arrive from three places, and end up in one list.

- **Built in.** Shipped with the platform.
- **Connected servers.** A third-party tool server, connected once, its tools
  projected into the same list under a prefix that never changes.
- **Your workflows.** Something you built, exposed as a tool.

The list is the point. Whatever the origin, a tool is granted, disabled,
approved and audited the same way, and the assistant cannot tell the difference.
One registry, one permission checkpoint, many sources.

When a connected server changes, the platform does not just accept it. A new
tool arrives **inert**, and someone has to allow it. A tool that changed shape
has its approval **re-locked**. A tool that vanished is **flagged**, not
silently dropped. Somebody has to look.
"""),
                ],
            },
            {
                "name": "Brains",
                "description": "Curated knowledge, and why it is not a pile of files.",
                "documents": [
                    ("Why a brain is not a folder of documents", """# Brains

A brain is knowledge the assistant can *navigate*: a small number of categories,
each holding documents, with links between the documents that mean something.

This is deliberately not "upload your files and hope". A pile of documents
answers a question by finding a passage that looks like the question. A brain
answers it by going to the right place and then following what that place points
at. The difference shows up the moment two documents nearly match and only one
is correct.

It is also **curated**, which means somebody is responsible for it. A brain that
nobody owns rots, and a rotten brain is worse than none, because it is confidently
wrong.
"""),
                    ("Writing a document an assistant can use", """# Writing for a brain

- **Title it with the question it answers**, not the topic it covers. "The
  refund policy" is findable. "Refunds" is a category.
- **Say the rule first.** The reasoning can come after. An assistant, like a
  person in a hurry, reads the top.
- **Be specific about numbers, dates and limits.** "Quickly" is not a policy.
  "Within 30 days" is.
- **Say what is *not* allowed**, explicitly. An assistant will not infer a
  prohibition from silence, and neither will a new colleague.
- **Link to the documents a reader will need next.** The links are the part a
  pile of files does not have. Use them.

If two documents disagree, do not add a third explaining which one wins. Fix
them.
"""),
                    ("Locking a brain", """# Locked brains

A brain can be **locked**, which means the assistant may read it and may never
write to it.

Lock the ones where being wrong is expensive: policies, prices, legal wording,
anything a regulator might read. Leave unlocked the ones the assistant genuinely
improves by using: the record of what has been tried, what worked, what a
customer actually meant when they said a thing.

An unlocked brain is how the loop gets better at its job over time. A locked one
is how it stays honest while doing so.
"""),
                ],
            },
            {
                "name": "Workflows",
                "description": "The same assistant, different for different people.",
                "documents": [
                    ("Configuration is layered", """# Layers

Nothing about the assistant is configured in one place, and that is on purpose.

1. **The defaults.** What the assistant is when nobody has said otherwise.
2. **The agent.** A named assistant with its own instructions, model, tools.
3. **The workflow.** A rule that says: *for these people, on this channel,
   change these things.*

The layers are read in that order, and the most specific one that matches wins.
A workflow does not replace the assistant; it overrides the parts it names and
leaves the rest alone.

This is why a support agent and a salesperson can talk to "the same assistant"
and be right to call it different.
"""),
                    ("Matching: who, and where", """# Matching

A workflow matches on **identity** and on **channel**.

Identity is a user, a group, or a role. Channel is where they are asking from:
the chat client, a direct API call, or another agent connecting to us.

A workflow with **no** assignment matches nobody. It is not live, whatever its
status says. This catches people out, and it is the first thing to check when a
workflow "does not work".

When two workflows match the same person, **priority decides**, and exactly one
wins. Not a merge of both: one. Merging two configurations produces a third that
nobody wrote and nobody can predict.
"""),
                    ("Publishing a version", """# Versions

A workflow does not change because you edited it. It changes because you
**published a version** of it.

The version is the thing that is live. The draft is the thing you are working
on. They are different, and the difference is what lets you edit a workflow that
is currently serving people without serving them your half-finished thought.

Publishing is a separate permission from editing, for the same reason.
"""),
                ],
            },
        ],
        "links": {
            "What this product is": ["Words we use, and what they mean",
                                     "The first ten minutes"],
            "How an assistant answers": ["Risk, and why some tools stop and ask",
                                         "Where tools come from"],
            "Configuration is layered": ["Matching: who, and where",
                                         "Publishing a version",
                                         "Words we use, and what they mean"],
            "Why a brain is not a folder of documents": [
                "Writing a document an assistant can use", "Locking a brain"],
        },
    },

    # ------------------------------------------------------------------ 3
    {
        "name": "Sales Playbook",
        "description": "Pricing, positioning, objections, and the questions "
                       "that actually qualify a deal.",
        "locked": True,
        "categories": [
            {
                "name": "Pricing",
                "description": "What we charge and what we will not discount.",
                "documents": [
                    ("List prices", """# Prices

| Plan | Per seat / month | Minimum seats | Annual discount |
|---|---|---|---|
| Starter | 29 EUR | 5 | 10% |
| Team | 59 EUR | 10 | 15% |
| Scale | Talk to us | 50 | Negotiated |

These are the prices. They are on the website, and a customer who finds a
different number from you than from the site will not trust either.

**Never quote a price that is not on this page.** If a deal needs a number that
is not here, it needs Leadership, not creativity.
"""),
                    ("Discounts: what you may give", """# Discounts

You may give, without asking anyone:

- The **annual discount**, as listed.
- **Up to 10%** on a first year for a customer taking 25 seats or more.
- **A 30-day extension** of a trial, once.

You may not give, ever, without Leadership:

- More than 10% off list.
- A discount that renews. (A discount is a decision about this year, not a new
  price forever.)
- Free seats "thrown in". Seats are the unit we are paid in. Giving them away
  is a discount that hides from the forecast.

A customer who will only buy at a price we cannot give is not a customer we
lost. They are a customer we did not have.
"""),
                    ("When a customer asks for a custom contract", """# Custom terms

Anything that changes our terms goes to Leadership and Legal. That includes:
liability caps, data residency promises, custom notice periods, security
addenda, and any commitment about a feature that does not exist yet.

**Do not promise a roadmap item to close a deal.** It is the fastest way to
turn a signature into a refund, and the customer will remember who said it.

You may say what we are working on. You may not say when it will land.
"""),
                ],
            },
            {
                "name": "Positioning",
                "description": "Why someone buys this instead of the alternatives.",
                "documents": [
                    ("What we actually sell", """# The pitch

We do not sell a chatbot. Chatbots are free now, and the customer has already
tried three.

We sell **the part that makes an assistant safe to give to a company**: that it
is different for the support team than for the sales team, that it cannot do
the dangerous thing without asking, that it answers from knowledge somebody owns
rather than from whatever it half-remembers, and that every one of those rules
is enforced when the action happens rather than promised in a prompt.

The demo that lands is not "look, it answers". It is: *this same assistant, for
this person, cannot see that tool, and here is where I decided that.*
"""),
                    ("Discovery questions that actually qualify", """# Discovery

Ask these. Listen to the answer rather than waiting to talk.

1. **"Who inside your company would be allowed to give this thing a real
   permission?"** If the answer is nobody, there is no deal yet.
2. **"What would it have to be forbidden from doing?"** Their prohibitions tell
   you what they are afraid of, which is what they are buying.
3. **"Where does the answer come from today?"** If it is one person's head, you
   are selling knowledge capture. If it is a wiki nobody reads, you are selling
   curation.
4. **"What happens when it is wrong?"** This separates a toy from a project.
5. **"Who has tried this already, and what happened?"** Almost everyone has. The
   failure is your opening.

A prospect who cannot answer 1 or 4 is not qualified, however excited they are.
"""),
                    ("Competitors, honestly", """# The alternatives

**A raw model provider.** Cheaper, and genuinely fine for one team with one use.
They come to us when they have three teams, or when somebody asks who approved
what.

**A general assistant platform.** Broader, more polished, and configured for
one company at a time. We win where an assistant must be *different* for
different people in the same company, and lose where it does not need to be.

**Something they build themselves.** The most common competitor and the most
respectable. It works, until the second team wants it slightly differently.

Never rubbish an alternative. A customer who picked it once will hear you
calling them stupid.
"""),
                ],
            },
            {
                "name": "Objections",
                "description": "What they say, and what is actually being asked.",
                "documents": [
                    ('"It will say something wrong to a customer"', """# The trust objection

This is the real objection behind most of the others, and it deserves a straight
answer rather than a reassurance.

**Say:** it will be wrong sometimes, the same as a new colleague is wrong
sometimes. The question is what it is allowed to *do* while being wrong.

Then show them: an assistant that is wrong but can only read is a bad answer. An
assistant that is wrong and can email a customer is an incident. That is why
anything past reading stops and asks a person, and why the thing it asks about
is fully prepared and validated before they are asked, so approving it cannot
fail halfway.

**Do not** answer this objection with accuracy statistics. They are not asking
how often. They are asking what happens when.
"""),
                    ('"Our data cannot leave the building"', """# Data residency

Take this seriously and do not improvise.

We can run entirely inside their infrastructure, and we can run against a model
that never leaves it. What we cannot do is promise a specific regulatory
certification on a call.

**Say what is true:** the platform can be deployed on their own machines; the
knowledge stays in their database; the model can be one they host. **Then get
Leadership** for anything that has to be written into a contract.

A promise made here that Legal later withdraws costs the deal *and* the trust.
"""),
                    ('"We already pay for a model provider"', """# The overlap objection

Good. So do we, and so does everyone. That is not the thing we replace.

Ask what happens today when a person in support and a person in finance use it.
Usually the answer is: the same thing, because there is no way to make it
different. Then ask whether that is acceptable for the finance one.

We sit *above* the provider, not instead of it. They keep their key, their
model, their contract. We decide who may do what with it.
"""),
                ],
            },
        ],
        "links": {
            "List prices": ["Discounts: what you may give",
                            "When a customer asks for a custom contract"],
            "What we actually sell": ["Competitors, honestly",
                                      '"We already pay for a model provider"',
                                      "Discovery questions that actually qualify"],
            '"It will say something wrong to a customer"': [
                '"Our data cannot leave the building"'],
        },
    },

    # ------------------------------------------------------------------ 4
    {
        "name": "Engineering Runbook",
        "description": "Operating the platform: deployments, incidents, the "
                       "database, and what to look at first.",
        "locked": False,
        "categories": [
            {
                "name": "Incidents",
                "description": "When it is broken.",
                "documents": [
                    ("The first five minutes", """# First five minutes

Do these in order. The order is the whole point: it is arranged so that the
cheapest check that tells you the most comes first.

1. **Is it everyone, or one workspace?** One workspace is configuration.
   Everyone is us.
2. **Can the gateway reach its database?** Everything else is downstream of this.
3. **Is the model provider answering?** Check their status page before you read
   a single line of our logs. It is them more often than anyone likes to admit.
4. **Did anything ship in the last hour?** If so, that is your suspect until it
   is cleared.
5. **Only now**, read the logs.

**Say something before you have the answer.** A status note that says "we are
looking, we do not know yet" is worth more than a diagnosis twenty minutes
later, because the people waiting are deciding right now whether to keep waiting.
"""),
                    ("Answers are failing for one workspace only", """# One workspace

Almost always its model configuration.

- **Has the vendor's key been revoked or rotated?** The failure looks like ours
  and is not.
- **Is the model still offered by the provider?** Models are retired. A model
  that vanished fails in a way that reads like a bug.
- **Did somebody publish a workflow?** A workflow can point the assistant at a
  model that no longer exists, and it changes what happens for *some* people,
  which is why it presents as "intermittent".

The tell for the last one: it fails for one team and works for another. Nothing
in the platform itself is that selective. A workflow is.
"""),
                    ("A run is stuck", """# Stuck runs

A turn outlives the window that asked for it, so a person closing a tab does not
stop the answer, and reopening it does not start a second one. That is the
design, and it means "stuck" needs care to diagnose.

- **Still streaming, slowly?** Not stuck. The provider is slow.
- **Waiting for an approval nobody gave?** Not stuck. Parked. It will expire on
  the approval window, and the window is configurable, per assistant.
- **Actually stuck?** Cancel it explicitly. A cancelled run is a finished run,
  and the transcript records that it was cancelled rather than pretending it
  ended.

Never "fix" a parked run by approving it yourself to make the alert go away. It
is waiting for a decision, and you are not the person whose decision it is.
"""),
                ],
            },
            {
                "name": "Deployments",
                "description": "Shipping, and undoing it.",
                "documents": [
                    ("Before you ship", """# Pre-flight

- The full gate is green, locally, on the machine you are shipping from.
- The database change, if any, has been **run against a real database that you
  then looked at.** Not the test suite's scratch database: a real one, with the
  table in front of you.
- You can say, out loud, what the change destroys. If the answer is "nothing",
  you should be able to say why you are sure.
- You know how to undo it.

The gate passing is necessary and not sufficient. The suite creates and migrates
its own databases on every run, which means it goes green while the database you
are actually looking at sits on the old shape.
"""),
                    ("Rolling back", """# Rollback

Code rolls back. Data does not.

Rolling the application back to yesterday is safe and boring. Rolling a
*database change* back is neither, because the change may have already thrown
something away, and yesterday's code may not understand today's rows.

So: **a change that destroys data is a decision, not a step.** It gets said out
loud before it is run, and the thing it destroys gets named. A change that only
adds is one you can ship on a Friday. A change that removes is one you ship on a
Tuesday morning with somebody watching.
"""),
                ],
            },
            {
                "name": "The database",
                "description": "The rules, and why they are absolute.",
                "documents": [
                    ("Every query is a constant", """# Queries

A query is assembled at compile time or it is not assembled at all. A query
built from a string at run time does not fail review; it fails the build.

This is not a style preference. It is the difference between a class of bug
being *unlikely* and being *impossible*, and impossible is the only one of those
you can stop thinking about.

Values reach the database as bound parameters. There is exactly one escape
hatch, it is for migrations and tests, and if you are reaching for it anywhere
else, you have taken a wrong turn several steps back.
"""),
                    ("The shape is declared, not accumulated", """# Schema

The database's shape is **declared**, one table per file, and the file is the
definition. It is not something you reconstruct by reading twelve migrations in
order and holding the result in your head.

A migration is a *delta*: it says what changed, and it does the things a
comparison never can. Backfill a column. Delete the orphans before the
constraint can go on. Turn one column into two.

Both exist, and the suite asserts that they agree: what the migrations produce
is what we declared. When those two drift apart, the schema has quietly stopped
being something anyone understands.
"""),
                ],
            },
        ],
        "links": {
            "The first five minutes": ["Answers are failing for one workspace only",
                                       "A run is stuck"],
            "Before you ship": ["Rolling back",
                                "The shape is declared, not accumulated"],
            "Every query is a constant": ["The shape is declared, not accumulated"],
        },
    },

    # ------------------------------------------------------------------ 5
    {
        "name": "Working Here",
        "description": "Onboarding, the way we write, and the decisions we "
                       "have already made so nobody has to make them again.",
        "locked": False,
        "categories": [
            {
                "name": "Your first week",
                "description": "Where things are and who to ask.",
                "documents": [
                    ("Day one", """# Day one

- Your accounts exist before you arrive. If one does not, that is our failure,
  not an errand for you.
- You will be added to one group, and the group is what gives you access. Nobody
  gets permissions personally. If you need something you cannot reach, the
  question is which group you should be in, not which button to press for you.
- Read the Product Manual. All of it. It is short, and everything else assumes it.
- Ask the assistant something on your first day. You are about to spend a lot of
  time on it and you should know what it feels like to use it.

Nobody expects you to ship anything this week. They do expect you to ask
questions in public where the answers are useful to the next person.
"""),
                    ("How we write to each other", """# Writing

- **Say the thing first.** The context can come after. People read the first
  line and decide whether to read the second.
- **Write it where it can be found again.** A good answer in a private message
  is an answer the next person will have to ask for.
- **Disagree with the idea, in plain words.** "That will break under X" is a
  contribution. "Hmm, interesting approach" when you mean "no" is not.
- **When you are wrong, say so and move on.** Nobody is keeping score, and the
  people who look like they never make mistakes are just the ones who never say.

Length is not effort. A shorter message that respects the reader's time is more
work, not less.
"""),
                ],
            },
            {
                "name": "How we decide",
                "description": "The standards, and why they do not bend.",
                "documents": [
                    ("No shortcuts, and what that actually costs", """# The standard

We do not ship workarounds. When something needs a hack, we stop and change the
design instead.

This is expensive on the day and cheap over a year, and it is worth being honest
that the expensive day is real. You will sometimes spend an afternoon fixing a
root cause you could have papered over in ten minutes. That is the deal. The
alternative is a system where every change costs a little more than the last one
until nobody can say why it is slow.

The tell that you are taking a shortcut: you are about to add a special case
for one caller. Ask why that caller is special. Usually it is not, and the
abstraction is wrong.
"""),
                    ("Untested code is unfinished code", """# Tests

Every part of this is tested: the service, the interfaces, the background work.

A test must assert the **real behaviour** of the thing it claims to cover. A
test that passes whether or not the code works is worse than no test, because it
buys a confidence nobody has earned. Asserting only "no error came back" is the
usual way this happens.

Anything security-relevant is tested for how it **fails**, not only how it
succeeds: what happens when the thing is replayed, tampered with, expired, or
asked for by someone who may not have it. The happy path is the easy half.
"""),
                    ("Approval must equal success", """# A promise we keep

When we ask a person to confirm something, everything that could fail has already
been checked. Saying yes cannot then produce an error.

This is a design rule, not an aspiration. An approval that fails afterwards is
treated as a bug in the design of that action, and it gets fixed there, not
patched with a better error message.

The reason is not technical. When you ask someone to take responsibility for a
decision, and then the thing they authorised does not happen, you have taught
them that their approval is theatre. Do that twice and they stop reading what
they are approving.
"""),
                ],
            },
        ],
        "links": {
            "Day one": ["How we write to each other"],
            "No shortcuts, and what that actually costs": [
                "Untested code is unfinished code", "Approval must equal success"],
        },
    },
]
