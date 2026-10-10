# The console: working agreement for `ui/`

This file is read alongside the root `CLAUDE.md` when working under `ui/`, and
everything there still applies. It holds how the console looks and behaves. The
contract between the console and the server (what the API filters, renders and
refuses) is in the root file under *The console API*, because it binds a change
to `internal/controlplane` as much as a change to a screen. `ui/README.md` has
the design notes; `ui/design-tokens.css` says what the numbers are, and this
file says what they are for.

Seven screens plus a first-run wizard, built to the **Panorama design system**
(exported from Claude Design). What follows are code-review rules rather than
notes.

## Who it is for

Three audiences, one deployment, and the console shows each of them a different
subset of itself.

- **The platform or FinOps engineer** who installed it. Their context is a fresh
  container and an empty database: they are configuring SSO, mapping groups,
  minting a token and trying to get one real request to flow. The job is *make
  it work, and prove that it worked*, and most of their time is spent on screens
  with no data in them yet.
- **The engineering leader** reading spend, in a browser tab open beside a
  budget conversation. The job is *understand where the money went and how much
  of it is honestly attributed*, with enough provenance to defend the number to
  a finance team.
- **The engineer being governed**, on `/me` and `/setup`. Their tool broke, or
  they want to know what they cost. The job is *configure my client, and see my
  own numbers*, never anyone else's.

All three are technical. None of them wants to be entertained; a screen that
costs them a second is a screen that failed.

## Voice

**Dry, confident, precise.** Personality shows through craft (timing, weight,
the precision of a number, the exactness of a label), never through jokes,
mascots or winks. Copy is factual and declarative. Errors state what happened
and what to do; they do not apologise. Empty states describe the state and name
the next action.

The interface is an instrument, and its emotional goal is **earned trust**: the
numbers on screen are the real numbers, including the uncomfortable ones. The
product's central claim is that it does not store your prompts and does not rank
your people, and a console that felt loose or playful would undercut a claim
that depends entirely on being believed.

Voice test: if a line would read oddly in a runbook, it does not belong here.

## What it must never feel like

1. **A gamified dashboard.** No confetti, streaks, badges or achievement
   toasts. Celebrating usage would be celebrating spend, and it collides with
   the no-leaderboard commitment.
2. **A generic AI or devtool template.** No cyan-on-dark, purple gradients,
   glassmorphism, rounded card grids, or sparklines used as decoration.
3. **A chatty consumer app.** No emoji, no exclamation marks, no mascots, no
   "Oops!", no loading messages that try to entertain.
4. **A surveillance tool.** Nothing may make an engineer feel watched or
   measured against a peer. This governs spend, never people.

## Design principles

1. **The empty state is the first impression.** A fresh deployment is the state
   most likely to be looked at first, and half the product lives there. Every
   one explains what the screen will hold, why it is empty, and the next action,
   never "no data".
2. **Delight is craft, not commentary.** The investment goes into
   micro-interactions and how data arrives on the page, not into copy that
   performs. If the user notices the effect rather than the number, it is wrong.
3. **Honesty outranks polish.** Unattributed spend, unpriced requests and
   degraded integrations get a visual slot beside the good numbers. Never design
   a state that looks more complete than it is.
4. **The accent is spent deliberately.** It marks what is current: the active
   nav item, the figure a screen is about, selection. Anything that makes it
   ambient makes every other accent element mean less. What to press is the
   CTA's, not the accent's (below).
5. **Nothing celebrates a person.** Success moments confirm that a *system*
   works (a request flowed, a token minted, SSO resolved a group). They never
   praise, rank or score whoever is reading.

## Accessibility

**WCAG 2.2 AA, plus reduced motion.** AA contrast on text and non-text UI,
visible keyboard focus on every interactive surface, and
`prefers-reduced-motion` honoured: the token file collapses animation and
transition durations under that query, and any new motion must fall inside that
rule rather than around it.

## The design system

**The design system is the source of truth, and it is vendored rather than
referenced.** Its token set lives in `ui/design-tokens.css` — one file, a
section per file the system ships, so a re-export can be diffed against it. Six
divergences are recorded at the top of that file with the measurements behind
them: the fonts are self-hosted rather than pulled from a CDN, the ground is
neutral grey and dark is the default, each ink ramp's floor is measured
against the control fill rather than the panel, the accent and the CTA are two
tokens rather than one, two families exist beyond the system's two — Tomorrow
for figures and JetBrains Mono for code — and the size scale is the console's
rather than the system's. Nothing in `ui/app/` invents a colour, a
size or a radius.

**Three things about the system are load-bearing and get broken by accident.**
Structure speaks in caps and content speaks in sentence case — if a word labels,
classifies or measures, it is `--font-mono`, uppercase and tracked; if it is
read as language it is Lexend in sentence case; and if it is a numeral it is
`--font-figure`. The page title is the one exception and is argued where it is set: it is
`--font-mono` in sentence case, because the two voices are carried by case as
much as by family, and a screen announcing itself in the brand face is the
product speaking its own name rather than another line of interface copy. The shapes are the system's and
they are not square: `--radius` is 10px for a panel or a control and
`--radius-pill` is 100px for anything pressed or toggled, because the guidelines
say "buttons/chips/switches are pill. Never square buttons" — status dots and
avatars are circles, and the only sharp edge left is a full-bleed band, which
has no corner to round. And **the accent is not the CTA**: `--accent` (Future on
grey, Evolve on paper) marks what is current, `--cta` (Empower) marks what to
press, and `.btn.primary` is the only rule in the console that spends Empower.
That split is the system's own instruction — "Evolve + Future for accents and
interaction", "Empower is for CTAs only" — made mechanical, and it is not
optional: Empower is 1.7:1 on paper, so a single token doing both would have set
text in it.

**The structural face is Questrial, and Stolzl is named nowhere.** Questrial is
the system's `--font-brand` and `--font-heading`, and the system chose it itself
as the web substitute for **Stolzl Book**, the canonical DNX typeface. Stolzl is
a commercial face from The Northern Block, all rights reserved: shipping it in
an AGPL repository would redistribute it to everyone who clones, and `go:embed`
would put it in every container image too. So the licensed files are not carried
— and the name is not left in the fallback stack either, because a stack naming
it renders one console for the few people with Stolzl installed locally and
another for everybody else, which in a product UI is a bug rather than a perk.
Lexend is `--font-ui` and carries everything read as language.

`--font-mono` is not a monospace and the token keeps the name anyway, because
renaming it would touch every rule in the system to say nothing new. It **ships
one weight and the console asks for no other** — there is no heavier cut to ask
for, so a rule setting 600 on structural text gets a synthesised smear, which at
10px uppercase is a smudge rather than emphasis. Nothing in `styles.css` sets a
weight on `--font-mono` text; the size, the case and the fill do the emphasis.
That is why `.btn.primary` carries no font-weight and the binding cap in the cap
grid is marked by colour alone.

**Lexend runs at 350, not 400.** It is the only variable face in the set and so
the only one with a weight to choose; at 14px body and 13px sub-lines its tall
x-height already gives it more presence than its weight suggests.
`--weight-ui-strong` (550) is the one step above it. Figures and code do not
follow it down — they pin `--weight-figure` and 400 respectively, because a
thin monospace is where a pasted key stops being legible. Questrial needs no
pin: one cut, so a request for 350 resolves to it rather than being synthesised.

**`--font-figure` is Tomorrow, and it is the one family the design system does
not name.** A square techno grotesque whose digits read as a readout — a meter,
a scoreboard, an instrument — which is what a gateway's figures are. Questrial
sets the words around a number and Tomorrow sets the number, so a measurement is
tellable from its label before either is read.

Which text is a figure is decided by the size token, not at the call site: a
display face rationed to a few places a screen, with which figures earned it
decided case by case, is a rule nobody applies twice the same way.
`--size-mono-fig*`, `--size-figure*`, `--size-stat` and `--size-hero` are the
figure sizes and carry this face, while `--size-mono-data` is the identifier
size (an email, a slug, a group name, a work-item key) and stays `--font-mono`.

Both numeric faces have **proportional digits and neither ships `tnum`** — a
Tomorrow `1` is 424 units against an `8` at 677 — so figures do not align
digit-to-digit and a number changes width with its value; every numeric column
is right-aligned, which is what keeps a table readable, and `tabular-nums` would
be a no-op. Tomorrow is declared at weight 300 alone for its own reason: its
heavier cuts close the counters on the digits, and a 0 that reads as an 8 at
52px defeats the only thing a figure that size is for, so the choice runs one
way and light opens them further.

**`--font-code` is the fourth family and is not a voice.** JetBrains Mono, for
the places that render something a person will paste into a terminal:
`.code`, `textarea.input`, the block `CopyBlock` copies. It
exists because none of the other three is a monospace, and a rendered opencode
config set in a geometric sans stops being code. It labels nothing.

**A screen is a stack of bands, or a column of cards.** The band is full-bleed:
sections down the page separated by hairlines, each answering one question and
carrying its own padding out to the 56px page edge. Analytics and My usage are
built from it; the rest are still `.page`. A page of cards on a padded ground
has two edges everywhere and a page of bands has one — the same "structure over
containment" argument one level up — and it is what lets a five-column table
with 410px of fixed columns have somewhere to sit.

**The header opens a screen rather than labelling it, and the footer says only
what a footer can.** The deployment is a kicker over a 26px heading in sentence
case, aligned to the same page edge as everything below, rather than a sticky
strip spending the top of every screen naming it. A screen contributes
a sub-line and controls through the chrome context in `chrome.tsx`, which is how
two screens have a range picker up there without the other six growing a header.
The footer repeats nothing the header says. It carries `Panorama·DNX·AI` (the
wordmark written as the address it links to), a **Source** link to the running
build's source at its commit, and the running build. Those two links are the
console's only outbound ones. Source is AGPL-3.0 section 13's offer of source to
everybody using the deployment, so it is on every screen for every role, and a
fork points it at its own repository with `version.SourceURL` at build time.
The build and the link ride on `/api/v1/auth/session` because a developer is
refused `/settings`.

**Both themes come out of one token set.** A neutral dark grey is the default;
paper is `data-theme="light"` on the root element. No rule in `styles.css` names
a colour, so no rule has to know which is running — and a rule that hardcodes
one breaks a theme nobody was looking at.

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

Dark being the unqualified `:root` inverts the system's own ordering, for the
ordinary reason: the console is an operations tool people sit in front of all
day and its screens were drawn dark, while a marketing page opens light.

**Structure over containment, close to iconless, and motion that is short.**
Panels are defined by rules and flat fills, not boxes with shadows; only a modal
carries a shadow, because only a modal floats. There is no icon font, icon
library or emoji: `+`, `//`, `→`, `×` and `✓` are set in the mono and are part
of the type system, and the brand is the word *Panorama* in the structural face
with nothing drawn beside it. Motion runs on `--ease-out`: entrances at 0.2s,
fades at 0.15s, the status dot pulsing at 2.6s as the only thing on a screen
that repeats. Nothing bounces, springs or animates on scroll.

## Screens and components

**Analytics is one screen behind tabs, and it is the start page.** "Is
everything fine", "what are we spending" and "what did it buy" are asked of the
same four endpoints, share a daily chart and attribution coverage, and would be
three date ranges to keep in step as three screens. So it is one screen at `/`
with nine tabs: Spend, Attribution, Teams, People, Work, Cost centers, Sessions,
Models & tools, Attention.

Only the active tab is mounted, and the tab and the window are both in the URL
so a view is a link. The two questions the screen has five seconds to answer sit
side by side at 52px on the first tab: **are we on budget**, and **is the spend
attached to real work**. Teams and People are offered only to a reader entitled
to them; the server refuses the data either way, so that is about what is worth
showing rather than about access.

**A budget lives with the thing it governs, and a route only reacts to it.**
Limits are set on People & teams (`/people`), on a person, a team, or the
deployment; `/routing` says what a name becomes and whether it keeps serving
once a soft limit is crossed. They are not one screen: that would put a limit on
a person under a heading about models, and leave the roster screen unable to
answer "what stops this account". Both screens say in words where the other half
is.

**"What actually stops them" is one derivation, in `people.ts`.** Personal cap,
then the tightest team hard limit, then the organization: whichever is set first
binds, and the panel names it. The roster's column, the drawers' cap grid and
the flag strip all read that one function, because two of them working it out
slightly differently would be two answers to the question the screen exists to
answer. The word "unlimited" is never used — a limit nobody set reads "not
set", and an account with nothing above it at all is amber rather than
reassured.

Only a window with a **hard** limit is a candidate: a budget carrying a soft
limit alone cuts nobody off, and a person whose one cap is a weekly one is
capped, not listed under "no cap above them" because they have no monthly
budget.

**Each flag is the filter it names.** The strip above the tabs is a hairline
row, and clicking a flag narrows the roster to exactly the accounts it counted,
with the flag itself showing as pressed: a flag that only said a sentence above
a table that could already show the accounts would be a second statement of the
same fact. "No cap above them" is why it has to work that way:
it is the one condition the screen's three select boxes cannot express.

**The model catalogue is one component with two callers** (`ModelCatalogue`):
browsing it, and choosing a route's model. Browsing is reading the table;
choosing is the same rows with an action column on the end. A second table of
the same rows would drift on the thing that matters most, which is which models
are eligible at all - `routes.ts`'s `routable`: streaming, available, and
speaking an API this gateway serves. A model failing any of those is absent when
picking and present when reading, labelled with why, and one table is what keeps
that from becoming two answers. The provider filter appears only when more than
one provider has rows, and the inference-profile filter only when a row carries
a geography, because a filter every option of which matches nothing is a control
that lies.
`PeoplePicker` and `TeamPicker` in `Pickers.tsx` are the same arrangement for
people and teams, and every surface that chooses either uses them.

**A route card is two cells and the whole card is the click target.** What the
route is called and who it exists for on the left, what it becomes and what
happens to it at a soft limit on the right. There is nothing on a route that is
not editable, so an "Edit route" link would be a second thing to aim at
inside a thing already aimed at — and the one place a click could land and do
nothing. A developer, who may not edit, gets the same card without the press.

**A condition is read as the words it narrows to.** "everyone", "every team",
"every tool", or the names joined — not chips, and not an empty space. An empty
condition is not an absence to be inferred: it is everyone, and the card says
so on the line where the names would be. The route name beside it is the one
mono name on the screen and the model is not, because a route name is a literal
an engineer types into `ANTHROPIC_MODEL` and a model name is read as language.

**The route list is filtered, because it grows.** Search by name, a scope group
separating the routes open to everyone from the ones narrowed to somebody, and
a model box listing only the models these routes point at — the catalogue has
thirty and a deployment uses three. The scope group is offered only to a reader
who is served conditions: a developer is not, and a control filtering on data
the browser does not have is a control that lies.

**A People & teams drawer commits once, through its own Save changes.** The
role, three windows of limits and the roster are held until that button is
pressed, and Cancel discards them. A limit does not save itself: four controls
writing at four different moments would leave the drawer with no state an
administrator could reason about and no way to back out of a half-made
decision, and a person's role, their teams and their cap are one decision made
at one sitting. `on` is stored separately from `value` (a box somebody cleared
in order to retype is still a limit that is switched on, and reading it as "no
limit" would delete their budget between two keystrokes), and every save is
acknowledged in a toast.

The two destructive actions are outside the form and act immediately:
deactivating an account and deleting a team are not edits to be staged behind
the button that saves a budget, because that is how somebody deletes a team
while raising its limit. Everything else on the screen — a picker's Apply
included — stages into the drawer that opened it.

**A limit's window is a tab, and the panel is that tab's content.** Daily,
weekly and monthly are three budgets, so a switch and a figure that silently
mean one and then silently mean another are how a team gets capped at $600 a
month. The tabs are attached to the panel for that reason, each tab whose
window carries a limit shows an accent square, and the save writes every window
that changed rather than only the one on screen.

**Beside every limit panel is every cap that reaches the subject**, scope down
and window across: personal, then each team, then the organization. The
smallest hard limit set in a window is the **binding** one, and it is the only
accent on the grid — its row's label goes with it, so the answer reads off one
line rather than being worked out from twelve figures, and a sentence beneath
says it in words. The grid reflects the edit in progress, because the question
it answers is "what will stop them if I save this". Every window is on it
because an administrator raising a monthly cap needs to see the daily circuit
breaker that will now bind first.

**Lists that can hold an organisation are virtualised.** An identity provider
with three thousand accounts is a normal deployment, and three thousand rows in
the document take a second to appear and stutter on every keystroke of the
search meant to rescue them. `VirtualList` keeps the DOM at roughly a
screenful; the rows it is given are one line and a sub-line so the fixed row
height is true, which is why the roster's team cell shows two chips and a count
rather than wrapping.

**The Outcome column states the word that was recorded, under a filter that
narrows to three.** "Soft limit" and "refused" both belong to "Blocked &
refused" and mean opposite things to whoever is reading — past a hard limit
nothing runs until somebody raises it, past a soft one the engineer names
another route and carries on. And a cost of `—` is a request that never reached
a provider, which is not the same absence as "unpriced": one has no cost, the
other has one nobody can compute, and `$0.00` would be false about both.

**Integrations is a band per source, and the rail is the narrow half.** A
source gets 260px: its name, the word for its state, what connecting it buys,
and — only while it is true — what is lost without it. The rest of the width is
the one thing there is to do about it. What an administrator came here to do is
paste a credential, so the form gets the room and the argument for it sits
beside the work rather than above it; the rail is sticky for the same reason,
so "Panorama never enumerates a repository" is still on screen at the moment
somebody is deciding what scope to grant an App.

**Jira is one line tall, and that is the design rather than a placeholder.** It
is out of v1 and naming it as a provider is refused at startup, so there is
nothing to configure and no form to draw — but what it would buy is still said,
in the same words the GitHub rail uses, because a reader comparing the two is
comparing exactly that. Its name is set at the muted step: the word is still
structure, and it is not a heading over anything.

**A connected source reads as the facts the form left behind** — same grid, the
same register labels, values where the boxes were. The credential is not among
them, because no endpoint returns one and a masked value here would imply it
could be revealed. Four words rather than two: `not connected`, `never run`,
`failing`, `connected`. A connection that has never run and one that is failing
are both "not working", and an operator reaching for the logs needs to know
which.

**Rates are not on this screen.** A rate belongs beside the model it is a rate
for, which is the catalogue on `/routing`. Two figures are shown nowhere yet: the
cache-write rate, and which source a rate came from.

**Settings is six documents behind tabs**: Organization, Single sign-on, Model
providers, Web search, Cost centers, Integrations. All six are configuration an
administrator sets once and then lives with, which is one shape. Web search is
built in the Model providers shape, because it is the same kind of thing: a
backend reached with the deployment's own role, a switch, and a check that
proves it (here a tool listing, which costs nothing). Integrations is
not buried there: a failing source raises an item on Analytics' Attention tab,
which is where a reader is when they would want to know.

**Model providers is a band per provider in the Integrations shape**, and the
same blocks render inside the setup step, framed as a card's contents there.
Choosing a region asks it what it offers before anything is saved (a preflight
that writes nothing) and names the routes the change would strand; saving
rebuilds the provider, reads its catalogue on the spot and refreshes its prices
in the background. Turning off a provider a route uses is refused, naming the
routes. The IAM policy is served by the endpoint rather than written into the
console's source, because a copy in the console would be a third place the
policy lived and the first to go stale. Five words for its state
rather than two: `not configured`, `disabled`, `failing`, `never checked`,
`connected`.

**The Organization tab is a document, and its band is split because the two
halves are the same question.** What this deployment calls itself and what it keeps is
on the left; what it is configured to do is on the right, and the rule between
them is what says which half an administrator owns and which half the manifest
does. No card appears on the screen at all — everything here is read rather than
operated, and a container around a list of four facts would be a box around a
sentence.

**A fact is a row here and a stacked pair on Integrations, and that is not an
inconsistency.** There the label names a field somebody filled in and the two
are read together; here the labels are questions whose shape everybody already
knows - endpoint, model providers, issuer, groups claim - and only the right-hand column
is scanned down.

**The group mapping is a table that is stated, and decided in a panel.** It is
not a text box and a select on every row, which would make the list a form and
put a free-text field in front of somebody whose provider's group names they
have no way to spell. It is built from the same head-and-rows vocabulary as every
other table in the console: the row says what is mapped and the whole row opens
the panel, and the `×` sits outside that target, because "change this" and
"delete this" are two different aims and a button inside a button is not markup.
The head stays up when there is nothing mapped — what the columns are is part of
the answer to "what would a mapping even be", which is the question somebody
looking at an empty one has — and *Add a mapping* sits on the table's own label
line rather than under the rows, so it belongs to the table in both states. The
chips above say what the provider has actually sent; click one and it is what
the panel opens with, and typing a group is still allowed, since a group nobody
has signed in from yet is a legitimate thing to map ahead of time.

**The theme switch stays in the sidebar.** The Settings handoff puts it in that
screen's header, which would make one screen's header the only place in the
console to change a preference that governs every screen.

**An opened table row shows a contained panel, not loose text.** Every
expandable table opens the same thing: a block indented under the row, an accent
hairline along its top saying which row opened it, an inset fill separating it
from the rows either side, and a 1px grid gap over that fill drawing the rules
between its cells. `.detail` owns all of it, so a table cannot open a different
shape by accident — and the gap being the rule is what makes a four-cell panel
and a two-cell one ruled identically without either knowing how many cells it
has. The indent is the caller's (`--detail-indent`), because it is that table's
first track plus its gap plus the row padding.

**A panel is where something is edited; a modal is where something is picked
for it.** Both live in `ui.tsx` and there is one of each.

`Drawer` holds the edit, and every setting is an edit. A person, a team, a
route, a limit, an integration, the deployment itself — all of them are
configured in the side panel. The decision is comparative — "is this the
expensive one", "does the platform team already have one", "is this cap tighter
than the team's" — and the panel leaves the list that answers that visible
behind the scrim, where a centred dialog would cover the thing being configured
with the form for configuring it.

`Modal` is the transient interaction over a surface nobody is editing. Mostly
that is choosing items out of a set, on behalf of a panel that
stays open behind it. Which people are on this team, which teams this person is
on, which model this route points at — and that is the whole list. There are
three call sites, two in `Pickers.tsx` and one in `ModelCatalogue.tsx`. It is
centred over the page and the panel stays open underneath, because the panel is
the thing being filled in: replacing it to choose what goes in it is how a
half-made edit gets lost. A picker writes on Apply rather than leaving a second button to
press.

The fourth call site is *Check a rule* on Settings, and it shows that the rule
is about staging rather than picking. It picks nothing: it asks the allocation list which rule claims a
repository and reports the answer. What made a modal right for it is that
**nothing is staged behind it** — the hazard the picker rule exists for is an
overlay replacing a half-made edit and losing it, and a read-only question has
no edit to lose. It is a modal *rather than* a field and a button under the
table because that would hold a band permanently to answer a question somebody
asks twice a quarter.

The half that gets forgotten is still the same half: a modal is never where
configuration is authored. A new screen's worth of settings is a panel however
dialog-shaped it feels, and a `Modal` that stages an edit should not get through
review.

Both take Escape, move focus in on open and return it on close, and take a
click on the scrim. They share one stack and only the one on top answers
Escape: two window listeners both fire for one keypress and `stopPropagation`
does not stop a sibling, so without the stack the panel underneath would close
first and take the edit with it.

**Configuration and instructions are separate things, and My setup is built on
that.** The left column's first section is what the account is set to — two
routes, which tool, how the credential arrives — and it is the only part
anybody changes. Everything under it is derived: numbered steps regenerated
from those three answers, never independently editable. The right column is the
third thing and the only one the reader does not author, which is proof the
gateway received them. The screen asks which tool and how the credential gets
there, and prints one set of steps, rather than offering several ways to say one
thing and making the reader decide which applies before reading any of them.

**The credential mode decides which steps exist, not just what is in them.**
Sign in is *sign this machine in* then *write the config*; a minted token is
*mint one* then *point the tool at the gateway*. Stamping is a third step where
the deployment offers one, which is Claude Code alone — opencode's headers are
static strings in a file, so a repository captured when the file was written is
wrong at the next checkout.

**The header pill and the Verify pill are one poll.** They say the same fact and
a screen where they disagreed would be a screen nobody believes about the fact
it exists to report.

**Money is rendered from integer nanodollars.** `format.ts` is the only place
rounding happens. A decimal string from the API is for display of a published
rate, never for arithmetic.

Nothing in the console loads from a third-party origin: fonts are bundled and
the CSP names no external host. An air-gapped deployment is a supported one.
