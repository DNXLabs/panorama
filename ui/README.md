# Panorama console

The React application, embedded into the binary with `go:embed` and served from
the same port as the gateway.

```
ui/
  app/                React + TypeScript, built by Vite
    src/screens/      one file per screen
    src/ui.tsx        the pieces every screen is built from
  design-tokens.css   the Panorama design system, vendored - the source of truth
  dist/               build output. Not committed.
```

## Building

`make ui` builds `app/` into `dist/` in a Node container. **Every Go target
depends on it** — `make build`, `make test`, `make lint`, `make check` — because
`go:embed` refuses to compile against an empty directory.

The consequence is deliberate and worth stating: **`go build ./...` on its own
does not work from a fresh clone.** The bundle is not committed, because a
minified build in the tree would put generated code in every diff that touches
a screen. The build is one container away and the Makefile runs it for you.

For iteration, `make ui-dev` runs Vite's dev server on `:5173` with hot reload,
proxying `/api`, `/auth`, and `/v1` to a locally running `make run`. It is real
data through the real API — there is no mock layer to drift out of sync.

`make dev` is the same dev server with the gateway and Postgres brought up
beside it, and the gateway rebuilding on a Go change too — one command instead
of two terminals. The only difference the console sees is where the proxy
points: `PANORAMA_PROXY_TARGET` names the gateway, because in that stack
`localhost` is the console's own container. Unset, it is `localhost:8088`, which
is what `make ui-dev` wants.

Neither serves `ui/dist`. The page comes from Vite in both, and the embedded
build is only what a production binary would ship.

## What the console may assume

**Every route is authenticated, and the server decides what a role may see.**
The sidebar hides screens a role has no use for, and that is a convenience
rather than a control: `requireRole` on the server is what actually refuses. A
console that hid a button and called it access control would be one CSP bypass
from a leaderboard.

**Nothing in the static shell is organizational data.** The markup, the script,
the stylesheet and the two fonts are identical for every deployment and every
visitor, which is why they are the only unauthenticated routes besides the
probes and the sign-in surface. `TestConsolePageCarriesNoOrganizationData`
enforces it.

**Money is rendered from integer nanodollars, never from a float.** A cost that
changes when it is parsed is not a cost. `format.ts` is the only place rounding
happens.

## Source of truth

The **Panorama design system**, exported from Claude Design, is the source of
truth. Its token set is vendored into `design-tokens.css` — one file, a section
per file the system ships, so a re-export can be diffed against it. Nothing in
`app/` invents a colour or a size; if a value is needed and no token covers it,
the token file is where it gets added.

The system's own reference screen is **People & teams**, and `screens/People.tsx`
is built to it: the flag strip, the tab pair, the filter row, the borderless
table, the record drawer. Every other screen borrows from that assembly.

The visual direction is DNX's, on a ground of this console's own choosing — flat
by mandate, geometric type, one warm colour spent only on the thing a person is
meant to press, over neutral grey rather than the system's navy and ice blue.
Three rules carry most of it:

- **Structure speaks in caps; content speaks in sentence case.** Anything that
  labels, classifies or measures is `--font-mono` — Questrial, the brand face —
  uppercase and tracked: nav items, column heads, kickers, buttons, status tags.
  Anything a person reads as language is Lexend in sentence case. Anything that
  is a numeral is `--font-figure`, Tomorrow. The page title is the one
  exception — `--font-mono` in sentence case — because case carries the voice as
  much as family does, and a screen announcing itself in the brand face is the
  product speaking its own name rather than another line of interface copy.
- **Structure over containment.** A rule above a block, a hairline between rows,
  a divider between cells. Only a modal carries a shadow, because only a modal
  floats. This is the system's own "flat by mandate: no gradients, no drop
  shadows, no decorative effects" read one level up.
- **The system is flat but not square.** `--radius` is 10px for a panel or a
  control; `--radius-pill` is 100px for anything pressed or toggled, because the
  guidelines say "buttons/chips/switches are pill. Never square buttons". Status
  dots and avatars are circles. The only sharp edge left is a full-bleed band,
  which has no corner to round.

### Where this console diverges from the exported system

Six, all recorded at the top of `design-tokens.css` with the measurements behind
them.

**The fonts are self-hosted.** The system imports Questrial, Lexend and Mulish
from Google Fonts and records it as a caveat to be fixed with `@font-face`
rules. `fonts.css` is those rules. Mulish is not among them: nothing in the
console spends `--font-secondary`, and a family nobody sets is a download nobody
needs. Tomorrow is the other way round — a family the system does not name,
carried because the console has a job for it neither brand face does. See
below.

**The ground is neutral grey, and that is this console's own departure.** The
design system's dark scope is Secure (`#252540`, a navy) and its light scope is
Adapt (an ice blue). Both are right for the surfaces the system was drawn for —
a marketing hero is a field of brand colour. This console is a wall of data with
a blue accent doing actual work in it: the active route, the attributed series,
the figure a screen is about, every link. On a blue ground that accent stops
being a signal, because the ground is already most of the way to it. So the
surfaces are neutral grey (`#1a1b1e` up to `#383a40`; `#f4f4f6` down to
`#e1e2e6` on paper) and the blue is spent only where it means something — the
same argument the system itself makes about Empower, one level up. The greys
carry a slight cool cast rather than being mathematically neutral, which keeps
them from reading green beside the accent. What stays the system's: Future and
Evolve as the accent, Empower as the CTA, and the logo, whose blue is the
company's and does not follow the theme.

Dark being the unqualified `:root` is the smaller half of the same divergence,
and the ordinary one: an operations tool people sit in front of all day opens
dark, a marketing page opens light.

**Each ink ramp's floor is measured against the control fill, not the panel** —
because `--text-muted` sets a table cell and a select box alike, and the control
is the lightest ground in the dark theme and the darkest in the light one. The
system's dark `--text-muted` is `rgba(255,255,255,0.6)`, which lands at 4.38:1
on a control fill. It runs at 0.55 here — 5.21:1 on the grey control, 5.52:1 on
the inset. On paper the system's `grey-500` (#6F6F86) is 4.13:1 on a control and
runs at #5E6064, which is 5.33:1. Every ratio quoted here is read off the
shipped stylesheet in a browser rather than computed by hand. Raising both
floors collapses them onto the step
above, which is the finding: this range holds about four distinct legible steps
per theme, not six. So `--text-faint` aliases `--text-muted` rather than
pretending to be a step below it, and only `--text-ghost` sits under the floor —
raised in turn to clear the 3:1 WCAG asks of a non-text mark.

**The accent and the CTA are two tokens**, which is the system's own rule made
mechanical. It says it twice — "Evolve + Future for accents and interaction",
and "Empower is for CTAs only" — and the console's accent is spent 28 times as
`color`, 25 as `background` and 13 as `border-color`, so a single token carrying
Empower would have set text in `#FFAD6D`, which is 1.7:1 on paper. `--accent` is
Future on grey (6.1:1 against the control fill) and Evolve on paper (5.3:1);
`--cta` is Empower in both themes under the page grey at 9.4:1, and `.btn.primary`
is its only consumer anywhere in `styles.css`. Empower never sets a word and is
never a page background.

**There is a fourth family, and it is the one nobody looks at.** `--font-code`
keeps JetBrains Mono for the places the console renders something a person
will paste into a terminal: `.code` (which is also the block `CopyBlock`
copies), the `.input` family and `textarea.input`. It exists because none of the other three is a
monospace, and a rendered opencode config set in a geometric sans stops being
code. It is not a voice in the system and never labels anything.

**The size scale is the console's, not the system's.** The system's is drawn for
marketing and for `dx-box` product screens: body 16px at 1.7 leading against a
560px measure, labels 11–13. This console puts a five-column table with 410px of
fixed columns on a screen with a 52px figure above it, and none of those screens
fit at that scale. Families, weights, tracking and case are the system's; the
sizes are the ones the Analytics and My usage screens actually set. The one
figure carried over verbatim is `--track-wordmark`, the system's 0.18em
"EXPERIENCE" tag tracking, which is what `Panorama·DNX·AI` is set in.

**A card carries a hairline.** The system draws its panels as fills alone. This
console outlines them, because its panel fill is one step off the page ground
and a screen of eight of them needs to say where each one ends.

`.design-docs/panorama-screens-envisioning-rev2.md` (kept locally, not in the
repository) covers the screens the mock does not.

## Screens

| Screen | Path | What it answers |
| ------ | ---- | --------------- |
| Analytics | `/` | Are we on budget, is the spend attached to real work, and where did it go |
| Audit trail | `/requests` | Every request: who, model, repo, branch, ticket, cost, decision |
| People & teams | `/people` | Who is governed, which team they are on, what role they hold, and what stops them |
| Routing | `/routing` | Which routes exist, who they exist for, and which model each becomes |
| Settings | `/settings` | Deployment configuration, single sign-on and group mapping, model providers, cost centers, integrations |
| My usage | `/me` | An engineer's own spend, sessions, and standing against a limit |
| My setup | `/setup` | What my account is set to, what to run, and whether it worked |
| Set up Panorama | `/welcome` | The admin first-run wizard |

Navigation is grouped **Organization** and **Personal**, which is the trust
model made visible: the personal screens are where an individual sees their own
numbers, and they are the only screens a developer can reach that carry any.

### Analytics is one screen, behind tabs

"Is everything fine", "what are we spending" and "what did it buy" are asked of
the same four endpoints, over the same window, and share the daily chart and the
attribution coverage. So they are one screen, at `/`, with nine tabs: **Spend ·
Attribution · Teams · People · Work · Cost centers · Sessions · Models & tools ·
Attention**, and one date range rather than three to keep in step.

**Tabs, not anchors, and only the active tab is mounted.** As one scroll the
screen would be four thousand pixels with a jump list at the top, which is a
table of contents admitting the page is too long rather than a page short
enough not to need one. The tab and the window both live in the URL, so a view
is a link.

The two questions the screen has to answer within five seconds are on the first
tab, side by side at 52px: **are we on budget**, and **is spend attributed to
real work**. Everything else is a drill-down.

Teams and People are offered only to a reader who may see them. The server
refuses the data either way — `filterFrom` and `spendVisibility` are what
enforce it — so this decides what is worth showing, exactly as the sidebar
does.

## Where the console diverges from the screens document

**The people list will carry spend, for the people entitled to see it, and
does not yet.** The screens document asks for a user list with current-period
spend and quota usage in it, and the README's principles permit it: a budget holder who
cannot see where the budget went cannot hold it.

Neither the column nor the query behind it is built. `/api/v1/users` carries no
money, and the screen says so under the table rather than leaving a gap where a
number will go. What the table does carry is the *limit* on each person, which
is what an administrator opens it to change — the budget panel shows a personal
cap and never a personal spend, which is the same line the policy endpoint
draws.

When the column is built, what decides whether it is served is the server, in
one place: an administrator sees everyone, a manager sees members of teams they
lead, and a developer sees nobody — including themselves, since their own
figures are on `/me`. The sidebar hiding the screen is not the control;
`requireRole` on the endpoint is, and the entitlement to the *column* is a
second check beyond the entitlement to the *page*, because "may you open People"
and "may you see what Dana costs" are different questions.

**There is no budgets screen, and there is no configuration file holding the
model list**, although the envisioning document has both. The allowlist is discovered
from the provider and curated from the console, because the two facts that
decide the choice - which regions a request may be processed in, and what that
costs - are published by the providers and cannot be typed correctly from memory. And
a budget is set where the thing it governs is listed: on a person or a team, on
People & teams.

A route reacts to a budget's state, and it is tempting to put the two on one
screen. Not on Routing, because that puts a limit on a named person under a
heading about models and leaves the roster screen unable to answer the question
an administrator opens it with: what stops this account. So the limit lives on
the person, and each screen says in one line where the other half lives.

Rules these two screens keep:

- **A panel is where something is edited; a modal is where something is picked
  for it.** The edit is comparative — "is this the expensive one", "does the
  platform team already have one", "is this cap tighter than the team's" — so it
  is a panel, and it leaves that list visible behind the scrim. The choice being
  made *for* the panel is a modal, centred over the page, with the panel still
  open underneath: replacing the panel to choose what goes in it is how a
  half-made edit gets lost, for instance an "Add to team" that closes the person
  it was for.
- **Settings live in a panel; only a picker gets a modal.** The modal is for
  choosing items out of a set — people, teams, models — and there are three of
  them in the whole console (`PeoplePicker`, `TeamPicker`, `ModelCatalogue`).
  Anything being configured rather than chosen is a `Drawer`, because a centred
  dialog covers the roster, the catalogue or the route list that the setting is
  being decided against.
- **A picker writes on Apply.** There is no second confirmation: the panel it was
  opened from is still there, and asking somebody to commit a roster twice is how
  a roster ends up unsaved.
- **A role is read on People and decided in Settings.** The person drawer shows
  the role, whether it is pinned, and one line saying where it comes from — the
  group mapping, or *Settings › Single sign-on*, which it links to. The control lives
  beside the mapping because an exception has to be visible from the rule it is
  an exception to: an administrator reading a group mapping could not otherwise
  tell that four accounts bypass all of it without opening four drawers on
  another screen. Only administrator can be pinned, so a demotion is a
  hand-back rather than an assignment, and the mapping decides at the next
  sign-in.
- **Nothing on People & teams saves on its own.** A drawer holds every edit it
  contains — the role, three windows of limits, the roster — until **Save
  changes** commits them together, and Cancel discards them. Not autosave,
  although a lone limit would suit it: a role, a roster and three windows of
  limits saving themselves at four different moments would leave the drawer
  with no state an administrator could reason about and no way to back out of a
  half-made decision. One button, one commit, one toast.
- **Deactivating an account and deleting a team are not part of the form.**
  They act immediately and report what they did, because staging them behind the
  same button that saves a limit is how somebody deletes a team while raising
  its budget.
- **Anything that can hold an organisation is virtualised.** The roster and both
  pickers keep about a screenful of rows in the document however many accounts
  the identity provider has. That is why a row is one line and a sub-line, and
  why the roster's team cell shows two chips and a count instead of wrapping: a
  row that could be two lines tall makes the scrollbar lie.
- **A condition's empty state is stated in words.** "None means all" is the
  whole semantic of a route condition and it is the part a list of checkboxes
  cannot say on its own, so the picker says *Everyone* rather than leaving a
  reader to infer it from an absence.
- **A route's scope badge counts in the terms that narrowed it.** A route
  restricted only by tool matches every person, so counting people would report
  "8 people" for a route nobody can reach from a browser — true of the person
  condition and misleading about the route. It reads `claude-code only` instead.
- **A tool condition is picked from a list, never typed.** The server refuses a
  tool name it has never derived from a user agent, so an "add as custom" field
  would offer a condition that silently matches nothing.
- **A limit panel is its period tabs' content.** Daily, weekly and monthly are
  three separate budgets, and the switch and the figure below the tabs always
  describe the one named above them — which is why the tabs are attached to the
  panel rather than floating over it. A switch and a figure that silently mean
  "monthly" and then silently mean "daily" are how somebody caps a team at $600
  a month. Every tab whose window carries a limit shows an accent square, so
  which windows are governed is legible without clicking through all three.
- **The cap grid is the answer, and it never says "unlimited".** Every budget
  panel is followed by *every cap that reaches them* — scope down, window
  across: personal, then each team, then the organization. The **binding**
  cell — the smallest hard limit set in that window — is the only accent on the
  grid and its row's label goes with it, so the answer reads off one line rather
  than being worked out from twelve figures, and one plain sentence underneath
  says it in words. A limit nobody set reads `—`, because it is not permission
  to spend without one: it means the rung above is what stops you. An account
  with nothing above it in that window at all is amber.

  Not a single column of "what actually stops them", which could only ever
  answer for one window and so quietly implies there is one. There are three,
  they are set independently, and an administrator raising a monthly cap needs
  to see the daily circuit breaker that will then stop the person first.
- **The grid reflects the edit in progress, not what is stored.** Switching a
  limit off moves the binding cell in the same frame, because the question the
  grid answers is "what will stop them if I save this", and answering it about
  the previous state is worse than not answering it.
- **Every limit turned off means remove the budget.** The server refuses a budget
  with no limits, correctly, because it would enforce nothing — so saving a
  window with both switches off deletes that window's budget rather than trying
  to store something that cannot be stored.
- **Each flag is the filter it describes.** The strip above the tabs is one
  hairline row — an account nobody has capped, an account that has never sent a
  request, a person on no team — and clicking one narrows the roster to exactly
  what it counted, with the flag itself showing as pressed. A card per flag
  would say three sentences above a table that can already show the accounts
  they are about. "No cap above them" is the reason it has to work this way: it
  is the one condition the three select boxes cannot express.

- **A route card is two cells, and the whole card is the click target.** What
  the route is called and who it exists for on the left, what it becomes and
  what happens to it at a soft limit on the right, with one rule between them.
  There is nothing on a route that is not editable, so an "Edit route" link
  would have been a second thing to aim at inside a thing already aimed at —
  and the one place a click could land and do nothing.
- **A condition reads as the words it narrows to, not as chips.** *everyone*,
  *every team*, *every tool* — or the names, joined. An empty condition is not
  an absence to be inferred: it is everyone, and the card says so on the line
  where the names would be.
- **The route name is the one mono name on the screen, and the model beside it
  is not.** A route name is a literal an engineer types into their tool's model setting;
  a model name is read as language. That is the whole of why one is set in
  Questrial at 19px and the other in Lexend beside it.
- **The two facts under the model are divided by a gap, not by a border.** The
  pair is a two-column grid with a 1px gap over a `--rule-inner` fill, so the
  hairline is the fill showing through. Two cells drawing their own border step
  wherever their heights differ, and these two never match.
- **There is a filter bar because the list grows.** Three routes need none; a
  deployment that has been running a year has fifteen. Search narrows by name,
  the scope group separates the routes open to everyone from the ones narrowed
  to somebody, and the model box lists only the models these routes actually
  point at — the catalogue has thirty and a deployment uses three. The scope
  group is offered only to a reader who is served conditions, because a
  developer is not and it would filter on data they do not have.
- **The catalogue and the model picker are one table.** Browsing is reading it;
  choosing a route's model is the same rows with an action column on the end.
  Which models are eligible at all is the thing that must not differ between the
  two - a model that cannot stream, that its provider lists as unavailable, or
  that speaks no API this gateway serves is absent when picking and present,
  labelled, when reading - and one table is how that stays true. `routable` in
  `routes.ts` is the one test.
- **Where a model runs is a pill, and on a provider without profiles it is the
  region.** `us`, `global`, `no profile` on Amazon Bedrock; `in-region ·
  us-east-2` on Bedrock Mantle, where the provider's region is the whole
  residency answer. The line under "Processed in" names the provider and the
  APIs, because both decide which tools can use a route on it.
- **The model this slot already holds is marked with an inset rule, not a
  border.** `inset 2px 0 0 var(--accent)` over the accent tint: a border would
  grow the row by two pixels the moment it became the chosen one.
- **A context window is a bordered tag, not a pill.** It is a fact about the
  model rather than a state of it, and a third fill beside the profile and
  legacy pills would read as a third status. The same slot says *no streaming*
  where that is the thing worth knowing.
- **Past a soft limit a route stops; it never changes model.** The stance is a
  two-way segmented control and the sentence beside it swaps with it, because
  the two outcomes are opposite and neither is guessable from a switch. A
  harness is told a route's context window once, when it is configured, so
  Panorama moving somebody's model mid-session would be changing what they are
  working with without telling them. It tells them instead, and they switch.
- **A route is one model, and the card's second fact is where it answers.** A
  throttle is a 429 the client retries, not a second model, so the second cell
  names the provider and the APIs the route is served on. A route whose
  provider is off or failing says "provider not serving" beside its model,
  because that request is refused as the deployment's fault and this card is
  where it is fixed.
- **A geography is a row, not a filter.** `au.` and `global.` for one model are
  two entries, priced separately and about 10% apart, because permitting one and
  refusing the other is the whole point of showing them.
- **This screen owns the decision, and the first-run wizard does not.** A route
  editor in setup would ask somebody holding a one-time code, before there is an
  administrator and before anyone has a reason to prefer one geography, to
  decide what a company pays for. Setup establishes that a model provider can be
  reached; this screen decides what may be asked of it. Reading it is open to
  anyone signed in (an engineer whose request was refused should be able to see
  what they may ask for instead), but a developer is served only the routes
  their own requests can use, and never the conditions, because those name
  colleagues. Editing is an administrator's, and `can_edit`
  says which the caller is rather than the console re-deriving the rule.

- **A zero rate renders as a dash, never as `$0.00`.** AWS publishes no token
  class that costs nothing, so zero means the model has no rate for that class —
  and `$0.00` in a cache-read column says caching is free, which is the "unpriced
  is null, not zero" mistake made in the display layer.

## Where the code for these two screens lives

Both are larger than one file's worth, and the split is by what a piece answers
rather than by what it renders.

| File | What is in it |
| ---- | ------------- |
| `people.ts` | The derivations: a person's status, which cap binds, what a team's roster is. Read by the table, the drawers and the strip, so there is one answer. |
| `routes.ts` | The same for routing: the editing draft's shape, which people a condition admits, a model's price line. |
| `screens/People.tsx` | The screen: flag strip, tabs, filters, and which panel is open. |
| `screens/PeopleTable.tsx` | The roster row, the flag, and the team cards. |
| `screens/PeopleDrawers.tsx` | The four drawers: a person, a team, the organization, and a team that does not exist yet. |
| `screens/PeopleBudget.tsx` | The limit editor and the cap grid, for all three scopes. |
| `screens/Pickers.tsx` | Choosing people and choosing teams — one of each, used by every surface that needs them. |
| `screens/Routing.tsx` | The screen: the filter bar, the route cards, and which overlay is open. |
| `screens/RouteEditor.tsx` | A route, edited whole: name, model, stance, conditions. |
| `screens/ModelCatalogue.tsx` | One table, browsed as the catalogue or picked from as the model picker. |
| `screens/ModelProviders.tsx` | A provider's region, switch, model tables and live check, as Settings bands or inside the setup step. |

## Design notes worth keeping

**Which face a word is set in carries most of the meaning.** Lexend for prose,
`--font-mono` for identifiers and every structural label — model IDs, branches,
tickets, slugs, column heads — and `--font-figure` for numerals: money, token
counts, percentages, timestamps' figures.

**`--font-mono` is Questrial, and it is not a monospace.** It is the design
system's `--font-brand` and `--font-heading`, and the system picked it itself as
the web substitute for **Stolzl Book**, the canonical DNX typeface. The token
keeps the name `--font-mono` because renaming it would touch every rule in the
system to say nothing new.

**Stolzl is named nowhere in the stack, deliberately.** It is a commercial face
from The Northern Block, all rights reserved. Carrying the files would
redistribute them to everyone who clones an AGPL repository, and `go:embed`
would put them in every container image as well. Leaving the *name* in as a
fallback is the subtler mistake and is also avoided: a stack naming it renders
one console for the handful of people with Stolzl installed locally and another
for everybody else, and a product UI that differs between two readers is a bug.
The marketing stack is not this file and can make the other trade.

**It ships one weight and nothing asks for another.** There is no heavier cut to
ask for, so a rule setting 600 on structural text gets a synthesised smear — at
10px uppercase, a smudge rather than emphasis. The size, the case and the fill
do the emphasis instead. Two rules depend on it: `.btn.primary` carries no
font-weight (the Empower fill is already the loudest thing on the screen), and
the binding cell in the cap grid is marked by colour alone. The system's
`--weight-semibold: 600` for uppercase labels applies to its Lexend labels, not
to this face.

**Lexend runs at 350, not 400.** It is the only variable face in the set and
therefore the only one with a weight to choose. At this console's sizes — 14px
body, 13px sub-lines, a 12px note — Lexend's tall x-height and open counters
already give it more presence than its weight suggests, and regular reads
heavier beside Questrial's single cut than the hierarchy wants.
`--weight-ui-strong` (550) is the one step above it, for the three places that
set a heading in the UI face, and it is a token rather than a literal so the
step stays the same size if the body weight moves.

Two things deliberately do not follow it down. Every figure pins
`--weight-figure`, and everything on `--font-code` pins 400: `.code` and the
`.input` family both hold something copied out of, or
about to be pasted into, a terminal, and a thin monospace is where that stops
being legible. Questrial needs no pin: it ships 400 alone, so a request for 350
resolves to the only cut there is rather than being synthesised thinner.

**`--font-figure` is Tomorrow, and every numeral is set in it.** The one family
the design system does not name, carried because neither brand face does the
job: a square techno grotesque whose digits read as a readout — a meter, a
scoreboard, an instrument — which is what a gateway's figures are. Questrial
sets the words around a number and Tomorrow sets the number.

A second numeric face is safe only when nothing decides it at the call site. A
display face rationed to four places a screen, with which figures earn it
decided case by case, is a rule nobody applies twice the same way. This one is
decided by the size token: `--size-mono-fig*`, `--size-figure*`, `--size-stat` and `--size-hero` are
the figure sizes and carry the face; `--size-mono-data` is the identifier size
and stays `--font-mono`. That is why `.cell-fig` takes it and
`.cell-named .name.mono`, which renders a work-item key, does not.

Both numeric faces have **proportional digits and neither ships `tnum`** — a
Tomorrow `1` is 424 units against an `8` at 677. Columns of figures do not align
digit-to-digit and a number changes width as its value changes. Every numeric
column is right-aligned, which is what keeps a table readable anyway; the
visible remainder is a chart tooltip that shifts as it is swept.
`font-variant-numeric: tabular-nums` would be a no-op and is set nowhere.
Tomorrow is declared at weight 300 alone for its own reason: its heavier cuts
close up the counters on the digits, and a 0 that reads as an 8 at 52px defeats
the only thing a figure that size is for — so the choice runs one way, and light
opens them further. The small end was checked rather than assumed: an 11px cell
in a light cut holds, because every figure is `--text-primary` and the colour is
doing the contrast work.

**A real monospace survives as `--font-code`**, for the places that render
something a person will paste into a terminal. See the divergences above.

**The scale is whole pixels.** Half pixels (12.5, 13.5) round differently at
every zoom level, and the handoff quotes none of them. Body is 14, sub and prose
13, notes 12, mono labels 10–11, mono figures 13/15/17, a page title 26, a hero
figure 52.

**One accent and one CTA, and they are not the same colour.** `--accent` —
Future `#64B5FE` on grey, Evolve `#0057D7` on paper — covers positive states,
active navigation and the attributed series in every chart. `--cta` is Empower
`#FFAD6D` under the page grey, and it marks the thing to press and nothing else.
Warn is Empower on the dark ground and a darkened `#8A5A17` on paper, reserved for soft
limits and anything asking for attention without being an error; danger is
destructive-only. Everything else is a step on the neutral ramp.

The split is the system's own rule ("Evolve + Future for accents and
interaction"; "Empower is for CTAs only") and is not a stylistic preference:
Empower is 1.7:1 on white, so a single token doing both jobs would have set
unreadable text. It also buys a distinction the console needs — a screen where
*what is current* and *what to press* are the same colour can only say one of
them at a time.

**A chart series is separated by value, not hue.** The system publishes a
six-hue chart palette and the console does not use it, because its series are
not six categories: they are spend against unattributed spend, this period
against last, the four token classes. So the ramp is the accent, the accent at
reduced opacity, then neutral fills. The unattributed share is deliberately the
neutral one: it is an absence, and should not read as a category. Spending
Empower on a series would also break the rule that it marks only the thing to
press.

**Unattributed spend gets a visual slot, not a footnote.** In the coverage card
and in every breakdown, the unattributed remainder is a real series in neutral
grey, sitting beside the attributed figures.

**Trust claims are placed where the doubt is, and are made once.** The work
screen says individuals are never ranked against individuals; the personal
screen says you are never placed on a leaderboard. The audit trail's "metadata
only" claim is the footnote under its table, beside the one about a blocked
request never counting against the budget that blocked it, rather than a banner
above the table somebody came to look something up in. A commitment reads
better after the evidence than in front of it.

**Integrations is a band per source, and the rail is the narrow half.** 260px
of rail — the name, the state, what connecting it buys, and what is lost without
it — against the rest of the width for the only thing there is to do about it.
The administrator came here to paste a credential, so the form gets the room and
the case for it sits beside the work rather than above it. The rail sticks,
which is the point of putting it there: "Panorama never enumerates a repository"
is still on screen while somebody decides what scope to grant.

**The consequence of not connecting is shown only while it is true.** The
degraded line — attribution keeps working, detections stay unconfirmed — is a
sentence about an absence. Left up once a source answers, it reads as a warning
about a working deployment.

**A connected source is the facts the form left behind.** The same two-column
grid, the same register labels, values where the boxes were, because it is the
same information from the other side: what was typed in, and what came back. The
credential is not among them.

**Rates are not on this screen.** A rate belongs beside the model it prices, in
the catalogue on `/routing`. Two figures are not shown anywhere yet: the
cache-write rate, and where a rate came from.

**Settings is bands with no card on it, and the first one is split.** The
organization's own name and retention on the left, the deployment's configured
facts on the right, and the rule between them saying which half can be changed.
Everything on the screen is read as a document rather than operated as a
dashboard, so a container around four facts would be a box around a sentence.

**A fact is a row on Settings and a stacked pair on Integrations.** Here the
labels are questions whose shape a reader already knows - endpoint, model
providers, issuer, groups claim - and only the right column is scanned; there the label
names a field somebody filled in, and the pair is read together.

**The group mapping is a table that is stated, and edited in a panel.** A text
box and a select on every row would make the list a form and ask somebody to
spell a group name their provider may emit as a GUID. It is the console's
ordinary head-and-rows table: the row is the click target and the `×`
sits outside it — one aim for "change this", one for "delete this", and never a
button inside a button. The head is drawn whether or not anything is mapped, the
empty state closes it with the same hairline a last row would, and *Add a
mapping* rides on the table's label line so it reads as the table's control in
both states.

**`.band-sub` is not scoped to `.band-head`.** The line explaining what a
control reads from is the same kind of sentence as the line explaining what a
band answers, and scoping it would render the second one at the browser's body
size.

**`.choice-option.active` asks for no weight.** The structural face ships 400
alone, so a bolder weight would be synthesised and the one option somebody had
chosen would be the one set in a smeared face. The accent fill is the whole of
the selected state.

**Every filter belongs to the server, the outcome one included.** The audit
trail loads fifty rows at a time out of a log that holds weeks, so a control
that searched the rows already in the browser would say "nothing matches" about
fifty requests while appearing to say it about the window — the one kind of
wrong answer an audit trail cannot afford. User, repository, branch, model and
outcome are query parameters; they narrow the table, the "load more" that
follows it and the CSV export from the same string. Typing waits 300ms before it
asks, for the reason a budget does: four requests race on the way to "panorama"
and the wrong one can win. Clearing applies at once, because a click is
deliberate.

The outcome matters most of all. Filtering it in the browser would make
"Blocked & refused" mean "of the fifty rows nearest the top" while the four
boxes beside it mean "of the whole window", and two filters on one screen
answering about two different populations is worse than either being wrong on
its own.

**Every filter is in the URL, so a view of the trail is a link.** Which is what
makes "open the requests behind this row" work from Analytics, and what lets
somebody paste the exact question they are asking into a ticket.

**The outcome control carries counts, and the counts come from a query rather
than from the page.** `/api/v1/requests` returns `outcomes` — routed, blocked,
error, and their sum — computed over the window with every other filter applied
and the outcome itself cleared, so each option shows the size it would select
and the three add up to All. It travels with the first page only: the counts
describe the window, not the page, so they must not change as more rows load,
and a `GROUP BY` over weeks of the largest table in the schema is not worth
re-running for every fifty rows.

Four decisions fold into three outcomes (`allowed` is served, under the
outcome called `routed`; `blocked` and `refused` are both "no provider was
invoked"; `error` is its own), and the
fold lives in one table in the store, read in both directions, because a
decision the filter admits but the count folds elsewhere makes an option say 4
and return 3 rows, and neither side looks wrong on its own.

**The column states what the filter narrows.** The Outcome column shows the word
the gateway actually recorded, so "soft limit" and "refused" both sit under
"Blocked & refused" while meaning opposite things: past a hard limit nothing runs
until somebody raises it, past a soft one the engineer names another route and
carries on.

**Cost has two absences and they are not the same.** An em dash is a request
that never reached a provider — a refusal has no cost, and `$0.00` would read as
a free call. "Unpriced" is a request that did reach one and whose model has no
rate: recorded rather than costed at zero, which is the commitment the product
makes about unattributed spend, applied to pricing.

**Four of the columns are set in the UI face.** User, repository, branch, route
and model are things somebody named; only the measures and identifiers (time,
tokens, cost, the work-item key) are mono. Every column in mono would read as a
log file rather than as a table.

**An opened row shows a panel, not loose text.** Every expandable table in the
console — teams, people, work items, the audit trail — opens the same thing: a
contained block indented under the row, with an accent hairline along its top
saying which row opened it, an inset fill separating it from the rows either
side, and a 1px grid gap over that fill drawing the rules between its cells, so
it is always clear where the block stops. The gap being the rule is what lets a four-cell
panel and a two-cell one be ruled identically without either knowing how many
cells it has.

**Every column on the audit trail comes from the row.** The repository, the
branch and the work item are on the request row, from the same query that
filtered it, rather than matched up in the browser from a second, separately
paginated call to `/sessions`. So a repository filter and the repository shown
against a row cannot be two different answers.

**The Route column is the name the client asked for.** Every name a client may
send is a route — an engineer sets one in `ANTHROPIC_MODEL` and never
names a model — so `requested_model` is where the route is recorded, and the
column says so in the word the product uses. A refused name is the exception and
is marked rather than printed as a route: it never routed anything.

**Empty and degraded states are half the product.** A fresh deployment is the
state that will actually be looked at first, and every screen has one that
explains itself and shows the path forward. Locked capabilities are shown rather
than hidden, so the value is discoverable. `Empty` takes an `action` slot for
the one next step, so that button looks the same on every screen instead of
being improvised with an inline style per call site.

**One lifecycle for every mutation.** `ActionButton`, `SubmitButton` and
`ActionMark` are the only ways the console acknowledges a write, and they share
`useAction`, so every write reports success and failure in the same words and
the same timings.

**The button keeps its verb and its width; the outcome is said beside it.** A
button that reads Save, then Saving…, then Saved changes width three times per
press — it moves under the pointer and shoves whatever sits next to it. So the
button only ever says "Save", carries the sweep while it is working, and
`ActionSlot` reports the result alongside: ✓ Saved, or ! failed. The slot
reserves its width from an invisible copy of the longest string it can show,
because a confirmation appearing out of nothing would push a right-aligned
button sideways — the same layout shift moved one element along.

The pending state is held for a minimum of 380ms: against a local SQLite
deployment a save resolves faster than a person can perceive, so a button that
flips straight to settled reads as one that did nothing. That is not a delay
added to look busy; it is what makes the transition legible. Past it, the button
tracks the real request exactly.

Failure is a shape, not just a colour — a check for settled, an exclamation for
failed — because lime and amber are the same to a reader who cannot separate
them.

`useApi`'s `loading` means "there is nothing to show yet", not "a request is in
flight". Every screen renders `loading ? <Loading/> : <TheCard/>`, so a
`loading` that went true on every refetch would make `onSaved → reload()`
unmount the card mid-confirmation. A reload keeps what is on screen, and only a
change of *path* blanks to skeletons.

**Motion is on mount, and it is small.** Cards settle the last six pixels into
place in sequence; bars, chart columns and coverage tracks grow from zero; KPI
figures count up once. The count-up interpolates the *integer* the server sent
and formats each frame through `format.ts`, never the other way round — parsing
"$1,284.02" back into a float to animate it is exactly the arithmetic that file
exists to prevent — and it assigns the final value rather than computing it, so
a total never settles a nanodollar short.

The bar and chart transitions depend on `useMounted`: a CSS transition fires on
a *change*, and an element that mounts at its final width has never changed.

All of it is inert under `prefers-reduced-motion`, and nothing is left invisible
when it is — the entrance uses `backwards` fill and the check mark an animated
`stroke-dashoffset`, and both would otherwise hold their starting state forever.

**The console is close to iconless.** Structure is carried by rules, squares and
mono labels, so the navigation is a column of tracked caps with nothing drawn
beside it. What remains in `icons.tsx` is the handful of places a picture says
something a label cannot — the magnifier in a search field, the mark on a
callout, the state of a wizard step — on a 16px box at 1.5px stroke, inheriting
`currentColor`. There is no icon font and no icon library. `+`, `//`, `→`, `×`
and `✓` are set in the mono and are part of the type system rather than icons.

**There is no mark: the wordmark is the whole of the brand.** The sidebar and
the sign-in gate carry the word *Panorama* set in Questrial and nothing else.
The DNX logomark is not carried: the company's artwork is the company's, and a
mark shipped in a public repository is redistributed to everyone who clones it
and embedded in every container image `go:embed` builds, which is the same
argument that keeps Stolzl out. Nor does the console invent a mark to fill the
gap. The favicon is **PAN** in Questrial, white on graphite. Three letters survive a 16px tab strip where a diamond and three more
letters do not, and the product's own name is the more useful thing to
recognise there than the company's.

**The wordmark is 20px, not the handoff's 13.** The handoff sets it beside the
logomark, which tells the reader it is a mark; alone above a column of 11px
tracked caps in the same face, at 13px it reads as a sixth nav item that happens
to be above the group label. Twenty keeps it visibly a different thing from the
items under it. `--size-wordmark` has one consumer; the pill borrows
`--track-wordmark` and not the size.

It is **outlined rather than set**, which is the part worth knowing. An SVG
favicon renders in an isolated context with no access to the page's fonts, so a
`font-family` here falls through to whatever the machine happens to have and the
mark becomes a different typeface on every desktop. The glyph outlines come from
the same `questrial-latin-400-normal.woff2` the console ships, laid out on the
font's own advance widths with uniform tracking well under the wordmark's 0.18em
— at that size the console's own letterspacing separates three letters into
three marks. What is centred is the ink and the cap height, not the advance box
and not the em box: both carry side bearings and a descender no letter here
uses, and centring on them leaves the mark visibly low and left.

**Two themes, out of one token set.** A neutral dark grey is the default; paper
is `data-theme="light"` on the root element, which the token file listens for.
Nothing else in the stylesheet names a colour, so no rule has to know which one
is running. The choice is stored per browser rather than on the account — which
one is legible depends on the room somebody is sitting in — and it is applied at
the app root so the sign-in gate honours it too.

**The dot grid is the header's, and dark-only.** On `body` it would be the
ground under everything, including under the two surfaces this system
deliberately makes semi-transparent (the sidebar fill and the panel fill), and
would show through both, breaking the rule "one region at most, never inside a
panel". On `.topbar` it is one region, the one that opens the screen, with
nothing layered over it. Across
a full page of paper it reads as distraction, so light gets none.

**The rule between a filter row and its table is inset, and belongs to the
filter row.** Every band already ends in a full-bleed rule meaning "this section
is over", so a filter bar promoted to its own band says that about a set of
controls and the table they govern — which are one thing. Held inside the band,
the hairline says the smaller thing: the controls stop here, their table starts.
Four screens draw it and the measurements are shared: 14px under the controls,
the rule, 22px before the head.

**A screen is a stack of bands, or a column of cards.** Two shapes, and the
band is the preferred one: full-bleed sections separated by hairlines, each
answering one question and carrying its own padding out to the page edge.
Analytics and My usage are built from it; the rest are still `.page`, a padded
column of cards. A page of cards on a padded ground has two edges
everywhere — the page's and the card's — and a page of bands has one, which is
the same "structure over containment" argument one level up. It is also what
lets a band be genuinely wide: the sessions table is five columns with 410px of
fixed ones, and inside a card inside a two-column grid it would have nowhere to
put them.

**The header opens a screen rather than labelling it.** It is part of the page,
not a sticky chrome strip: the deployment is a kicker, the name is a 26px
heading in sentence case underneath it, and both align to the same 56px page
edge as everything below. A screen contributes a sub-line and controls
through the chrome context in `chrome.tsx` — which is how the range picker sits
in the header of the two screens that have one, without the others growing a
header of their own.

**The footer says the two things only a footer can.** Anything naming the
screen, the organization or the region would repeat the header at the other end
of the same page. So the footer is `Panorama·DNX·AI`, a **Source** link to the
running build's source at its commit (AGPL-3.0 section 13's offer of source to
every user, so on every screen and for every role), and the running build, on
the same hairline every band is separated by.
The version rides on `/api/v1/auth/session` rather than `/settings`, because a
developer is refused `/settings` and a footer that named a build for their
manager and nothing for them is a support conversation about who is out of
date.

**My setup separates configuration from instructions, and that is the whole
screen.** The first section of the left column is what the account is set to —
two routes, which tool, how the credential arrives. Everything under it is
derived from those three answers and is never independently editable: numbered
steps, regenerated on every change, each block already carrying the reader's
routes. The right column is the third thing and the only one the reader does
not author, which is proof the gateway received them.

It is not a set of tabs (shell, this machine, this project, sign in), because
that asks the reader to decide which of four ways of saying one thing applies to
them before they can read any of it. The answer is always the same two
questions, so the screen asks those two and prints one set of steps.
The credential mode changes the *set* of steps rather than their contents, which
is why it is a control at the top rather than a tab over the bottom.

**The split is a band whose halves are made of their own sections.** Every other
`.band.split` puts one block in each cell and lets the cell carry the padding.
Here each half is a stack of sections with hairlines between them, and a rule
drawn inside a padded cell stops short of the page edge. So the cells carry no padding at all and each
section pads itself out to the edge it owns: the page edge outside, the gutter
inside.

**The header pill and the Verify pill are one poll, and they are the same fact.**
Two subscriptions would eventually show it two ways, on the one screen whose
entire job is to report whether something arrived.

## Fonts are self-hosted

The design system imports Questrial, Lexend and Mulish from Google Fonts, and
records that as the one caveat to fix. Shipping it would make an air-gapped deployment call a
third party on every page load, which contradicts the product's central claim.
They come from `@fontsource` instead, latin subset only, hashed into the bundle
and served from the binary. The CSP has no third-party origin in it at all.

Lexend and JetBrains Mono are variable, so one file each covers every weight.
**Questrial** — `--font-mono`, and the system's own stand-in for the licensed
Stolzl Book — and **Tomorrow** — `--font-figure` — ship as statics, and the
console asks each for exactly one cut: regular. Four files, latin only. Mulish,
the system's `--font-secondary`, is not among them: nothing in the console sets
a word in it. Neither is Stolzl, and that is a licence decision rather than a
size one — see above.

## What the console does not show

**CUR reconciliation.** The mock's header chip ("CUR reconciled 99.4%") assumes
Cost and Usage Report ingestion, which is post-v1 and partly enterprise. v1
computes cost from the price table, and the console says so.

**Command-line sign-in is switched off in this release.** `panorama login`,
`panorama configure <tool> -write` and `panorama token` exist, but the
deployment refuses all of them while `cli.sign_in` is off, which it is by
default and cannot yet be turned on. So My setup offers the minted token: one
credential, one set of steps, and a sentence saying it expires in thirty days
and nothing rotates it. The switch is on Settings → Organization, disabled,
with the reason beside it: shown while it cannot be moved, because a setting
that appears on the day it starts working is one nobody was told about.

**The unsupported tools are served and not drawn.** `/api/v1/setup/tools`
returns Cline, Continue, Gemini CLI, GitHub Copilot and Cursor with
`supported: false` and the reason, and My setup does not render them: the screen
configures the tool somebody has, and five tools they are not using would be the
longest thing on it. The claim stays auditable in the payload; if it is ever
drawn, it belongs on a page about coverage, not about setup.
