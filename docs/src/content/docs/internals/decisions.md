---
title: Design decisions
description: >-
  Why Panorama works the way it does where that is surprising: routes that stop
  instead of swapping models, no fallback chains, cost centers that cannot
  block, a 30-day token, and secrets that never reach the database.
sidebar:
  order: 5
---

The other pages say what Panorama does. This one says why, for the decisions
that surprise people. Where an obvious alternative is a trap, it is named here
with the reason it fails, because that reason is usually the best argument for
the current design.

## Routing

### A route stops at a soft limit. It does not swap to a cheaper model.

Swapping in a cheaper model past a soft limit is right about the problem and
wrong about who should act.

A coding tool is told a route's context window and output limit once, when it
is configured, and nothing renegotiates them mid-session. Moving somebody onto
a smaller model would silently change what they are working with, and would
need a rule refusing any swap to a model with less context than the original,
which exists only to contain the damage.

Telling the engineer avoids all of that. Every route they may use is already
in their tool with its own numbers, so switching is one command and nothing is
hidden.

### A route becomes exactly one model

A route carries no chain of fallback models to try while the provider
throttles. A chain is a second model per route to discover, price, check the
context window of, and explain, and it fires under load, which is exactly when
long sessions make a smaller window hurt most.

A throttle reaches the client as a 429 in its own error format, which every
coding tool already retries. A route that should survive a throttle is a second
route the engineer can switch to.

### The route list is the only allowlist

There is no second table of routing rules beside it. Two tables would mean two
answers to "may this request proceed", and the day they disagreed the
permissive one would win without anybody noticing.

So there is one list. A name absent from it is refused. There is no wildcard
route, and no file, flag or environment variable that could create one.

### Conditions narrow. They never widen.

Every condition on a route must match. That reading makes a route narrower each
time an administrator adds to it, and it fails closed when they meant
something else.

Conditions are stored as lists on the route, not as rows that reference people.
Rows would cascade on delete, and here a cascade *widens*: delete the one
person a route names, the condition vanishes, and the route admits everybody. A
list of ids that no longer resolve simply never matches.

### Whether there is somewhere to switch to is a warning, not an error

A stopping route with no alternative is a legitimate configuration ("past the
warning line, stop") and also what forgetting the second route looks like. So
the save succeeds and says so. The question is asked per person, because "there
is another route" and "there is another route for you" differ as soon as
conditions exist.

### A route for Claude Code carries its model's name

Claude Code prepares each request for the model it recognises by name, and
treats any other name as its newest model. Behind an invented name, an older
model refuses the request: Claude Haiku 4.5 behind `fast` refused every subagent
request, and behind `claude-haiku-4-5-20251001` served all of them. So the route
editor asks for the model's own name when a route is for Claude Code, and My
setup sends Claude Code no context window: it knows the window of a model it
recognises.

Not repairing the requests in the gateway, because every repair exposes the
next refusal, and every entry in a table of what each model refuses would fix
a request the client built for the wrong model. Naming removes the cause.

It is a warning rather than a refusal, and a route for Claude Code may still run
another vendor's model. Whether to restrict Claude Code to Anthropic's models is
not decided yet.

## Budgets

### Enforcement never touches the database

Routes, budgets, team membership and spend live in one in-memory snapshot,
reloaded on an interval and incremented as requests complete. A database query
between a keystroke and the first token is what this gateway promises not to
do.

The cost is stated: a request is priced when it finishes, so a limit is checked
against spend as of the last completed request, and can be overshot a little.

### A blocked request never counts against the budget that blocked it

What counts is whether a provider was actually invoked. That rule is applied
identically in the running ledger and in the query that reconciles it, because
two derivations of one rule drift apart.

### A route reads budgets and cannot set one

Otherwise "what may I spend" would have two answers, and they would disagree
the first time somebody edited only one.

## Cost centers

### Spend is allocated by what it bought, not by who spent it

Allocating by person is the obvious design and it is wrong twice. A person is
on several teams, so "who pays" has two answers and nothing to break the tie.
And charging AI spend to headcount is something finance already does through
payroll. Spend against a product is the number they cannot get elsewhere.

### A cost center cannot block

A budget can block because the gateway knows who is asking before it reads a
byte. A cost center is about the work, and the work is not known at that
moment: a session's first request has no repository yet, and its attribution
can still improve. A blocking cost center would admit every new session, then
stop somebody mid-conversation over a repository they may not own, with nothing
to switch to.

### There is no fallback to the spender's team

Falling back would produce a report that is fully allocated, in which an unknown
share of every line is an assumption. It would also remove the only thing that
ever gets a missing rule written: a repository with real spend and no rule
showing up as a task.

### An ordered rule list is fine here, and not for routing

Ordered routing rules would decide an authorization question that the route
list already decides. Allocation rules decide a question nothing else decides,
resolve to at most one line, and a mistake misfiles money instead of opening a
door. The cost
of ordering is predictability, which **Check a rule** pays back.

### Changing a rule restates closed periods

Allocation is derived from the rules, never entered by hand, and recomputed in
the same transaction as any rule change. A repository filed wrongly in October
was wrong in October, and a report that cannot be corrected is one nobody
trusts.

## Credentials and identity

### A pasted token lives 30 days and that is not configurable

A pasted token is the credential Panorama can protect least. It does not
rotate, it lives in a dotfile, and nothing about a laptop leaving the building
tells the deployment to revoke it. An operator can shorten the lifetime and
cannot lengthen it. Command-line sign-in, which rotates hourly, is the intended
answer and is not released yet.

### The identity connection is configuration. The group mapping is data.

The issuer and client secret live with every other secret, in the environment.
A secret in a database is a secret in every backup. The group mapping is
organizational data that an administrator edits on a screen, so it lives in the
database.

Integration credentials *are* stored in the database, because connecting
GitHub is an operational decision made months later by somebody with no access
to the deployment's manifest. They are encrypted first, and the key stays in
the configuration, so a dump carries nothing usable.

### A role somebody assigned by hand is not recomputed

Every sign-in recomputes a role from groups, which is how removing somebody
from a group removes their access. Applied to every account, it would silently
undo any role an administrator assigned directly, including the account that
owns a new deployment. So an account records who decided its role, and only
administrator can be assigned by hand, so that the list of exceptions stays
short and sits beside the rule it is an exception to.

### Panorama owns no identities

There is no local user, no password and no invite. The first account is created
by the first sign-in. Bootstrapping uses a one-time setup code that opens a
session and not an account, because a bootstrap administrator would be the one
row contradicting everything else.

## Providers

### The IAM role grants every region

A region is chosen in the console, possibly months after deployment, by
somebody with no IAM access. Scoping the policy to a region would turn that
into a ticket to another team.

### An unknown context window is recorded as unknown

Bedrock publishes no context window through any API. Panorama keeps a table,
and a model it has not heard of is recorded as unknown and never given a
default. A wrong window is worse than a missing one: a coding tool manages a
whole session against that number.

### Checking a provider means invoking a model

Bedrock model access is on by default, and no API reports the thing that
actually blocks invocation, a service control policy. A missing permission, a
missing marketplace subscription and a wrong model id all come back as the same
`AccessDenied`. One real request for one token is the only check that tells
them apart.

The request is streamed, as a coding tool's is, and is made against a route's
model first, so a check cannot pass on a model nobody is served while the
route's model is refused. What tells the causes apart is the sentence Bedrock
wrote rather than the error's class: an unsubmitted Anthropic use case form and
a retired model are both "not found", and a model AWS does not offer the
account is "access denied" while every availability status Bedrock publishes
for it reads available. So those statuses are shown beside a refusal as
evidence and never decide anything, and the check tries another model only
after a refusal that belongs to one model.

### A refusal is explained to the administrator, not the engineer

When Bedrock refuses a model, the engineer is told that it did and where an
administrator can see why. Bedrock's reason names the AWS account and its
roles, so it is kept for **Settings → Model providers** rather than sent to a
coding tool. Only refusals are kept, and their text is Bedrock's: nothing
derived from a request body is stored.

### The API surfaces are relayed, never translated

Anthropic Messages, OpenAI Chat Completions and OpenAI Responses are each
served natively to models that speak them. Rendering one dialect into another
is two sets of edge cases nobody tests.

Within one API, a field the model spells differently is renamed. OpenAI's GPT-5
and later models refuse `max_tokens` on Chat Completions and read the same
limit as `max_completion_tokens`, and a generic Chat Completions client such as
opencode sends only the first: it is talking to a route name it has never heard
of, so it cannot know which model is behind it. The two fields carry one number
with one meaning, so renaming one changes nothing the model does. That is the
test for a translation Panorama makes on its own, and it is the same one the
beta flags pass.

The same models refuse function tools on Chat Completions unless reasoning is
switched off. Switching it off would pass that test's opposite: the request
would succeed, from a weaker model than the engineer chose, and nothing would
say so. So nothing is rewritten. Instead the opencode configuration My setup
generates sends every route whose model speaks Responses to Responses, where
tools and reasoning work together, and keeps Chat Completions for models that
speak nothing else.

## Web search

### Web search is a tool Panorama serves

A coding tool's own search is usually a tool on its vendor's API that the
vendor runs mid-request: Claude Code asks Anthropic's API for
`web_search_20250305`, and Codex asks OpenAI's Responses API for `web_search`.
Bedrock refuses the first, and offers the second only for some models on one
endpoint. Reproducing either inside the gateway would mean holding a stream
open while the gateway ran a search and called the model again, and
translating one vendor's tool into another's. That is a tool loop on the path
that promises to add no latency, and the dialect translation the API surfaces
refuse.

So search is a tool the coding tool calls itself, over MCP, at Panorama's
`/mcp` endpoint. To the model it is one more client tool, so it works on every
route. Every supported coding tool speaks MCP. And the gateway still
authenticates the call, applies the caller's budgets and records it, which a
coding tool pointed straight at a search service would skip.

### A search is recorded as a request

A search is a row of the audit trail, with its own count and cost, rather than
a table of its own. Every total, budget, export and spend view then includes it
through the queries that already exist. A second table would be a second place
for each of them to forget. The query and the results are not stored, for the
same reason prompt bodies are not.

### Amazon's index, reached with the deployment's role

The search runs on the AgentCore Web Search Tool, which searches an index
Amazon operates. A query is not sent to a third-party search engine, and the
gateway is called with the deployment's role, so there is no key to hold.
Which gateway is used is chosen in the console, and the role may call any
gateway, for the reason it may use any region.

### A gateway the stack created is shown, not edited

When the CloudFormation stack creates the gateway, choosing that parameter is
the decision to offer search, so search is on from the first start and nobody
types the same URL into the console a second time. The console shows the
gateway and the published price without offering to change them, because the
stack deletes its gateway with the stack and a URL pasted over it would point
somewhere nothing manages. An administrator can still turn search off.

This differs from the Bedrock region, which the configuration seeds once and
the console owns from then on. A region has no resource behind it that the
stack will delete; a gateway does.

## Pricing

### A long-context price replaces the whole price

GPT-5.x on Bedrock costs more above 272K input tokens, and the higher rate
applies to every token of that request. So a tier is a second price for the
whole request, not a band for the tokens past the threshold, and the threshold
counts the whole prompt, cache hits included. A table that held one rate per
class would bill every long session at a fraction of its cost, which is the one
direction Panorama promises never to be wrong in.

### A fetched rate that disagrees leaves long requests unpriced

The Price List API has no tiers, and a fetched rate outranks a compiled one.
When the two agree on the base price the compiled tier is kept. When they
disagree, the tier is a price that is no longer the price, and billing a long
request at the short-context rate would under-report it. Unpriced is visible in
every total; a long session billed at half its cost is not.

### A language model reads the price, and code decides whether to believe it

GPT-5.x's price is published on a web page, not through an API. Reading a table
off a page is what a language model is good at, and it can also be wrong while
sounding right. So it only proposes: a price is kept when every figure it
reported is found on the page, in the row it quoted, in the column whose header
names that token class, under the heading it named. A misread row fails the
whole card, and the model goes on the unpriced list rather than into a release.

### The found table only grows, and lives outside the repository

A price in a release should be one somebody could have looked at, so a model
the finder has priced is never read again and never rewritten. The table lives
in S3 rather than in a commit, so a release cut from a pull request's branch has
the latest table, and nothing expires. Every release records the S3 version it
embedded and attaches the table itself, so what a binary priced from can always
be read back.

## The audit trail

### A row is a sitting, and every request stays one click away

A coding agent makes hundreds of requests an hour, so a trail with one row per
request is one nobody reads. A row is the requests one conversation made with
one outcome, with no more than thirty idle minutes between them. The
conversation already fixes the person, the repository, the branch and the work
item. The outcome is part of it, so that a run of refusals never hides inside a
served total.

The route is not. Claude Code sends titles, summaries and command checks to its
fast model on every turn, so splitting by route turned one sitting into two
rows, one of them small calls nobody chose. The row names the route the
conversation opened on and counts the others, so a switch is still visible.

A web search carries no conversation of its own, so it joins the one the same
person's same tool was last in, up to thirty minutes before it. That is an
inference, and two conversations at once in one tool can put a search in the
wrong one. The alternative is every search in a row of its own with no work
attached.

A request joins its row when it is recorded, so a sitting still in progress is
a row that is still growing. Every total is over the requests inside the filter
and the window, so a row and the requests it opens onto are always the same
set. The CSV export stays one line per request, because an auditor reconciles
requests, and a sum cannot be reconciled.
