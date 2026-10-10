// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Command seed fills a development deployment with fake but coherent traffic,
// so the console's analytics and audit trail have something to draw.
//
// It is a development tool and lives outside the shipped binary on purpose:
// nothing here is reachable from `panorama`, and a deployment that never runs
// `go run ./scripts/seed` cannot be made to invent a request.
//
// What it writes is meant to be *reconcilable*, not merely plausible, because a
// screen tested against incoherent data teaches nothing:
//
//   - Costs are computed by the real pricing table through pricing.Compute, so
//     the figures on Analytics are the same arithmetic production does. A model
//     whose rate class is unpublished is recorded unpriced - null, not zero -
//     which is the state the product promises to show honestly and the one
//     nobody has a fixture for otherwise.
//   - Attribution is written the way the ladder writes it: a session is placed
//     once, and its whole conversation moves with it. The unattributed
//     remainder is spread deliberately across the three reasons store.Unplaced
//     partitions it into, so the coverage gap on Attribution adds up to the gap
//     on Spend.
//   - Cost centers are not assigned. Rules are written and store.Reallocate
//     derives the allocation, which is the same code path a rule edit takes.
//
// Everything it creates is recorded in a manifest under the settings key
// dev.seed.manifest, and -reset deletes exactly that and nothing else. A
// deployment's real rows - the account you sign in as, the routes you wrote -
// are never touched.
//
// Usage, against the stack `make dev` runs:
//
//	make dev-seed              # add 60 days of traffic
//	make dev-seed ARGS=-reset  # remove the last run's, then write it again
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// manifestKey is where the record of a seeding run lives.
//
// In `settings` rather than a table of its own: this is a development tool and
// must not put a migration in front of anybody, and the deployment already has
// a key-value store for exactly the things it knows about itself.
const manifestKey = "dev.seed.manifest"

// manifest is what a run created, so a later run can delete precisely that.
//
// Requests and sessions are absent by design - there are tens of thousands of
// them and they are reachable from the principals that made them, which is a
// smaller thing to keep and cannot go stale.
type manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Days      int       `json:"days"`

	Users         []string `json:"users"`
	Teams         []string `json:"teams"`
	ServiceTokens []string `json:"service_tokens"`
	WorkItems     []string `json:"work_items"`
	CostCenters   []string `json:"cost_centers"`
	CostRules     []string `json:"cost_center_rules"`
	Budgets       []string `json:"budgets"`
	Routes        []string `json:"routes"`
	AuditEvents   []string `json:"audit_events"`
}

func main() {
	log.SetFlags(0)

	var (
		driver = flag.String("driver", envOr("PANORAMA_DATABASE_DRIVER", config.DriverPostgres),
			"database driver: postgres or sqlite")
		dsn = flag.String("dsn", envOr("PANORAMA_DATABASE_DSN",
			"postgres://panorama:panorama-dev-password@postgres:5432/panorama?sslmode=disable"),
			"database DSN")
		days  = flag.Int("days", 60, "how many days of history to write")
		reset = flag.Bool("reset", false, "delete the previous run's data first")
		only  = flag.Bool("reset-only", false, "delete the previous run's data and stop")
		seed  = flag.Uint64("seed", 20260905, "random seed; the same seed writes the same traffic")
	)
	flag.Parse()

	if err := run(context.Background(), *driver, *dsn, *days, *reset || *only, *only, *seed); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run(ctx context.Context, driver, dsn string, days int, reset, resetOnly bool, seed uint64) error {
	db, err := store.Open(ctx, config.Database{Driver: driver, DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	s := &seeder{
		db:  db,
		ctx: ctx,
		// Deterministic on purpose: the same seed has to produce the same
		// deployment twice, which is the whole value of a seeder. Nothing it
		// generates is a secret.
		rng: mrand.New(mrand.NewPCG(seed, seed^0x9e3779b9)), //nolint:gosec // fake data, not credentials
		// Anchored to a fixed hour so a run is a whole number of days and two
		// runs on the same day cover the same window.
		now:         time.Now().UTC().Truncate(24 * time.Hour).Add(23 * time.Hour),
		days:        days,
		costCenters: map[string]costCenterRow{},
		tokens:      map[string]string{},
	}

	if reset {
		removed, err := s.reset()
		if err != nil {
			return err
		}
		log.Printf("reset: removed %d rows written by the previous run", removed)
		if resetOnly {
			return nil
		}
	}

	return s.seed()
}

type seeder struct {
	db  *store.DB
	ctx context.Context
	rng *mrand.Rand
	now time.Time

	days   int
	prices *pricing.Table
	man    manifest

	// costCenters is the resolved chart of accounts, keyed by lower-cased code,
	// so a stamp can be resolved the way the data plane resolves one.
	costCenters map[string]costCenterRow
	// tokens maps a person to the credential their requests were made with.
	tokens map[string]string
}

// costCenterRow is what the seeder needs to know about a line to stamp it.
type costCenterRow struct {
	id           string
	acceptsStamp bool
}

// ---------------------------------------------------------------------------
// The cast
// ---------------------------------------------------------------------------

type teamSpec struct {
	slug, name, description string
	// soft and hard are this team's monthly limits, as multiples of what it
	// actually spent this month.
	//
	// Multiples rather than figures, and set after the traffic is written,
	// because a limit is only interesting relative to the spend it governs: a
	// round $9,000 against $60 of seeded traffic leaves every budget comfortable
	// and every screen that reacts to one blank. Below 1.0 is a team past that
	// line, which is a state somebody has to be able to look at.
	soft, hard float64
	// traffic scales how much this team spends. A team at zero is provisioned
	// and idle, which is one of the things Analytics' Attention tab exists to
	// name.
	traffic float64
}

var teamSpecs = []teamSpec{
	// Past its soft limit: the routes marked as not serving past one are
	// refused, and everything else carries on.
	{"payments", "Payments", "Payments platform and the ledger", 0.8, 2.0, 1.0},
	{"platform", "Platform", "Infrastructure, CI, and the gateway itself", 2.0, 3.0, 1.15},
	// Past its hard limit, which is what a stopped team looks like.
	{"mobile", "Mobile", "iOS and Android clients", 0.5, 0.9, 0.55},
	{"data", "Data & ML", "Pipelines, warehouse, and model evaluation", 1.6, 2.5, 0.7},
	{"qa", "Quality engineering", "Test strategy and release verification", 0, 0, 0},
}

type personSpec struct {
	email, name, role string
	teams             []string
	leads             string
	// tool is what this person's editor sends. One person is on Cline, which
	// here is configured without cache_control and so reads nothing back from
	// the prompt cache - the finding Analytics' Attention tab is for.
	tool string
	// traffic scales this person's volume relative to their colleagues.
	traffic float64
	// disabled marks an account that has been deactivated but still owns
	// history, which is the state the roster's flags are about.
	disabled bool
	// soft and hard are a personal monthly cap, as multiples of what this
	// person actually spent. Zero means nobody set them one, which is the
	// common case and the one the roster reports as "not set".
	soft, hard float64
}

var personSpecs = []personSpec{
	{"mia.okafor@example.com", "Mia Okafor", store.RoleManager, []string{"payments"}, "payments", "claude-code", 1.2, false, 0, 0},
	{"tomas.lindqvist@example.com", "Tomas Lindqvist", store.RoleDeveloper, []string{"payments"}, "", "claude-code", 1.5, false, 0.75, 1.6},
	{"priya.raman@example.com", "Priya Raman", store.RoleDeveloper, []string{"payments", "platform"}, "", "opencode", 1.1, false, 0, 0},
	{"declan.moore@example.com", "Declan Moore", store.RoleDeveloper, []string{"payments"}, "", "claude-code", 0.8, false, 0, 0},

	{"harriet.vance@example.com", "Harriet Vance", store.RoleManager, []string{"platform"}, "platform", "claude-code", 0.9, false, 0, 0},
	{"yusuf.demir@example.com", "Yusuf Demir", store.RoleDeveloper, []string{"platform"}, "", "claude-code", 1.6, false, 0, 0},
	{"noa.ben-ari@example.com", "Noa Ben-Ari", store.RoleDeveloper, []string{"platform"}, "", "opencode", 1.2, false, 2.0, 3.0},
	{"kwame.asante@example.com", "Kwame Asante", store.RoleDeveloper, []string{"platform", "data"}, "", "claude-code", 1.0, false, 0, 0},

	{"sofia.marchetti@example.com", "Sofia Marchetti", store.RoleManager, []string{"mobile"}, "mobile", "claude-code", 0.7, false, 0, 0},
	{"jae-won.park@example.com", "Jae-won Park", store.RoleDeveloper, []string{"mobile"}, "", "claude-code", 0.9, false, 0, 0},
	{"ines.duarte@example.com", "Inês Duarte", store.RoleDeveloper, []string{"mobile"}, "", "cline", 0.6, false, 0, 0},

	{"amara.nwosu@example.com", "Amara Nwosu", store.RoleDeveloper, []string{"data"}, "", "claude-code", 1.1, false, 0, 0},
	{"leo.castellanos@example.com", "Leo Castellanos", store.RoleDeveloper, []string{"data"}, "", "opencode", 0.8, false, 0, 0},

	{"rowan.pike@example.com", "Rowan Pike", store.RoleDeveloper, []string{"qa"}, "", "claude-code", 0, false, 0, 0},
	{"marek.novak@example.com", "Marek Novák", store.RoleDeveloper, []string{"platform"}, "", "claude-code", 0.4, true, 0, 0},
}

// ---------------------------------------------------------------------------
// The work
// ---------------------------------------------------------------------------

type repoSpec struct {
	name string
	// ticketPrefix is the tracker project branches in this repository name.
	// Empty means work here is branch-shaped and never carries a ticket, which
	// is what leaves a repository in the unattributed remainder for a reason a
	// naming convention would fix.
	ticketPrefix string
	team         string
	// subjects seed branch names and titles.
	subjects []string
}

var repoSpecs = []repoSpec{
	{"acme/payments-api", "PAY", "payments", []string{
		"idempotent-refunds", "settlement-retries", "webhook-replay", "3ds-challenge-flow",
		"ledger-reconciliation", "capture-timeout", "dispute-evidence-upload",
	}},
	{"acme/payments-web", "PAY", "payments", []string{
		"checkout-a11y", "saved-cards", "receipt-download", "apple-pay-sheet",
	}},
	{"acme/platform-infra", "PLAT", "platform", []string{
		"karpenter-rollout", "vpc-endpoint-policy", "otel-collector", "ecr-lifecycle-rules",
		"blue-green-cutover", "sso-session-length",
	}},
	{"acme/panorama", "PLAT", "platform", []string{
		"cost-center-rules", "mantle-provider", "attribution-ladder", "audit-trail-filters",
		"virtualised-roster", "opencode-provider-block",
	}},
	{"acme/billing-infra", "PLAT", "platform", []string{
		"cur-export", "savings-plan-alarms", "budget-anomaly-rule",
	}},
	{"acme/data-pipeline", "DATA", "data", []string{
		"dbt-incremental-models", "warehouse-partitioning", "feature-store-backfill",
		"schema-registry-bump",
	}},
	// No ticket prefix and no cost center rule: this is the repository the
	// Attention tab is meant to raise, and the one whose sessions land in
	// "a branch nobody could place".
	{"acme/mobile-app", "", "mobile", []string{
		"offline-cart", "push-token-refresh", "biometric-unlock", "crash-free-sessions",
		"deeplink-router",
	}},
}

// ---------------------------------------------------------------------------
// The catalogue
// ---------------------------------------------------------------------------

type routeSpec struct {
	// model is the reference a route stores: provider/model.
	name, model, description string
	servesOverSoft           bool
	teams                    []string
	// share is how much of the deployment's traffic goes through this route.
	share float64
}

// fastRoute is the route Claude Code's background calls go to.
var fastRoute = routeSpecs[1]

var routeSpecs = []routeSpec{
	{"standard", "bedrock/global.anthropic.claude-sonnet-5", "", true, nil, 0.58},
	{"fast", "bedrock/global.anthropic.claude-haiku-4-5-20251001-v1:0", "", true, nil, 0.26},
	{
		name: "open-weight", model: "bedrock-mantle/openai.gpt-oss-120b",
		description: "An open-weight model on bedrock-mantle, for opencode and Codex. " +
			"No seeded rate, so its requests record unpriced until prices refresh.",
		servesOverSoft: true, share: 0.06,
	},
	{
		name: "deep", model: "bedrock/global.anthropic.claude-opus-4-5-20251101-v1:0",
		description: "Opus for the hard ones. Stops at a soft limit rather than " +
			"quietly spending the rest of the month at five times the rate.",
		servesOverSoft: false, teams: []string{"platform", "payments"}, share: 0.07,
	},
	{
		name: "archive", model: "bedrock/apac.anthropic.claude-3-5-sonnet-20241022-v2:0",
		description: "Older Sonnet in ap-southeast, kept for one batch job. Its " +
			"cache-read rate is unpublished, so requests through it record unpriced.",
		servesOverSoft: true, teams: []string{"data"}, share: 0.03,
	},
}

// ---------------------------------------------------------------------------
// The accounting lines
// ---------------------------------------------------------------------------

type costCenterSpec struct {
	code, name, owner, description string
	planNanos                      int64
	acceptsStamp                   bool
	rules                          []ruleSpec
}

type ruleSpec struct {
	matchOn, pattern string
}

var costCenterSpecs = []costCenterSpec{
	{
		"CC-100", "Payments platform", "finance@example.com",
		"Everything billed to the payments product line.",
		4_000_000_000_000, true,
		[]ruleSpec{{"ticket", "PAY-*"}, {"repo", "acme/payments-*"}},
	},
	{
		"CC-200", "Core infrastructure", "platform@example.com",
		"Shared platform, CI, and the gateway.",
		12_000_000_000_000, true,
		[]ruleSpec{{"ticket", "PLAT-*"}, {"repo", "acme/*-infra"}, {"repo", "acme/panorama"}},
	},
	{
		"CC-400", "Data platform", "data@example.com",
		"Warehouse, pipelines, and model evaluation.",
		3_000_000_000_000, true,
		[]ruleSpec{{"ticket", "DATA-*"}, {"repo", "acme/data-pipeline"}},
	},
	{
		"CC-900", "Capitalised R&D", "cfo@example.com",
		"Capitalised development. Refuses stamps: this line is reached by evidence or not at all.",
		8_000_000_000_000, false,
		[]ruleSpec{{"ticket", "RND-*"}},
	},
}

// catchAllRule is the rule the seeder removes if it finds one.
//
// A repository pattern of `*` claims everything, which leaves the unallocated
// worklist empty and the Cost centers tab a single line. Both are the opposite
// of what this data is for, so it goes - and the run says so, because deleting
// somebody's rule silently would be worse than the boring report.
var catchAllRule = ruleSpec{"repo", "*"}

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

func (s *seeder) seed() error {
	if err := s.loadPrices(); err != nil {
		return err
	}

	teams, err := s.seedTeams()
	if err != nil {
		return err
	}
	people, err := s.seedPeople(teams)
	if err != nil {
		return err
	}
	if rerr := s.seedRoutes(teams); rerr != nil {
		return rerr
	}
	if cerr := s.seedCostCenters(); cerr != nil {
		return cerr
	}
	work, err := s.seedWork()
	if err != nil {
		return err
	}
	ci, err := s.seedServiceTokens(teams, people)
	if err != nil {
		return err
	}

	requests, sessions, err := s.seedTraffic(people, work, ci)
	if err != nil {
		return err
	}

	// Budgets last, because a limit is written against the spend it governs and
	// that spend does not exist until the traffic above does.
	if berr := s.seedBudgets(teams, people); berr != nil {
		return berr
	}

	if aerr := s.seedAudit(people, teams); aerr != nil {
		return aerr
	}

	// The allocation is derived, never authored - the same call a rule edit
	// makes, so what the console shows is what the rules actually say.
	allocated, err := s.db.Reallocate(s.ctx)
	if err != nil {
		return fmt.Errorf("reallocate: %w", err)
	}

	if err := s.saveManifest(); err != nil {
		return err
	}

	log.Printf("seeded %d days: %d people, %d teams, %d work items, %d sessions, %d requests",
		s.days, len(people), len(teams), len(work), sessions, requests)
	log.Printf("allocated %d work items across %d cost centers", allocated, len(costCenterSpecs))
	log.Printf("manifest written to settings[%s]; `-reset` removes exactly this", manifestKey)
	return nil
}

func (s *seeder) loadPrices() error {
	entries, err := s.db.LoadPrices(s.ctx)
	if err != nil {
		return fmt.Errorf("load prices: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("the price table is empty: start the gateway once so the price " +
			"refresher runs, or every cost here would be null")
	}
	s.prices = pricing.NewTable(entries)
	log.Printf("priced against %d rate rows already in this deployment", s.prices.Len())
	return nil
}

func (s *seeder) seedTeams() (map[string]*store.Team, error) {
	out := make(map[string]*store.Team, len(teamSpecs))
	for _, spec := range teamSpecs {
		team, err := s.db.TeamBySlug(s.ctx, spec.slug)
		switch {
		case err == nil:
			out[spec.slug] = team
			continue
		case !errors.Is(err, store.ErrNotFound):
			return nil, fmt.Errorf("team %s: %w", spec.slug, err)
		}

		team, err = s.db.CreateTeam(s.ctx, spec.name, spec.slug, spec.description)
		if err != nil {
			return nil, fmt.Errorf("create team %s: %w", spec.slug, err)
		}
		out[spec.slug] = team
		s.man.Teams = append(s.man.Teams, team.ID)
	}
	return out, nil
}

// person is a seeded account with everything the traffic generator needs.
type person struct {
	spec    personSpec
	user    *store.User
	tokenID string
	teamIDs []string
}

func (s *seeder) seedPeople(teams map[string]*store.Team) ([]*person, error) {
	out := make([]*person, 0, len(personSpecs))
	for _, spec := range personSpecs {
		user, err := s.db.EnsureUser(s.ctx, spec.email, spec.name, spec.role)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", spec.email, err)
		}
		s.man.Users = append(s.man.Users, user.ID)

		// A manager holds their role because somebody granted it, which is what
		// role_source records and what stops the next sign-in taking it away.
		source := store.RoleSourceSSO
		if spec.role != store.RoleDeveloper {
			source = store.RoleSourceManual
		}
		status := "active"
		if spec.disabled {
			status = "disabled"
		}
		if uerr := s.exec(`UPDATE users SET role_source = ?, status = ?, created_at = ? WHERE id = ?`,
			source, status, s.now.AddDate(0, 0, -s.days-s.rng.IntN(120)), user.ID); uerr != nil {
			return nil, uerr
		}

		p := &person{spec: spec, user: user}
		for _, slug := range spec.teams {
			team, ok := teams[slug]
			if !ok {
				return nil, fmt.Errorf("person %s names unknown team %q", spec.email, slug)
			}
			role := store.TeamRoleMember
			if spec.leads == slug {
				role = store.TeamRoleLead
			}
			if merr := s.db.AddTeamMember(s.ctx, team.ID, user.ID, role); merr != nil {
				return nil, fmt.Errorf("team member %s: %w", spec.email, merr)
			}
			p.teamIDs = append(p.teamIDs, team.ID)
		}

		// One gateway token each, so a row of the audit trail points at the
		// credential that made it rather than at nothing.
		hash, prefix := s.credential("apt")
		expires := s.now.AddDate(0, 0, 30)
		token, err := s.db.CreateGatewayToken(s.ctx, user.ID, "laptop", hash, prefix, &expires)
		if err != nil {
			return nil, fmt.Errorf("token for %s: %w", spec.email, err)
		}
		p.tokenID = token.ID
		s.tokens[user.ID] = token.ID

		out = append(out, p)
	}
	return out, nil
}

func (s *seeder) seedRoutes(teams map[string]*store.Team) error {
	// Both providers, configured the way an administrator would have: runtime in
	// the deployment's home region, Mantle in the US region that gets models
	// first. Left alone when a row exists, like every other seeded thing.
	for _, p := range []providers.Settings{
		{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1", ConfiguredBy: "seed"},
		{Kind: providers.KindBedrockMantle, Enabled: true, Region: "us-west-2", ConfiguredBy: "seed"},
	} {
		if _, _, err := s.db.SeedProvider(s.ctx, p); err != nil {
			return fmt.Errorf("provider %s: %w", p.Kind, err)
		}
	}

	for _, spec := range routeSpecs {
		var exists int
		if err := s.queryRow(`SELECT count(*) FROM model_routes WHERE name = ?`, spec.name).Scan(&exists); err != nil {
			return fmt.Errorf("route %s: %w", spec.name, err)
		}
		if exists > 0 {
			continue
		}

		ids := make([]string, 0, len(spec.teams))
		for _, slug := range spec.teams {
			ids = append(ids, teams[slug].ID)
		}
		matchTeams, err := json.Marshal(ids)
		if err != nil {
			return err
		}

		created := s.now.AddDate(0, 0, -s.days)
		if err := s.exec(`
			INSERT INTO model_routes (
				name, description, enabled_by, created_at, updated_at,
				match_users, match_teams, match_tools, target_model, serves_over_soft
			) VALUES (?, ?, '', ?, ?, '[]', ?, '[]', ?, ?)`,
			spec.name, spec.description, created, created,
			string(matchTeams), spec.model, spec.servesOverSoft); err != nil {
			return err
		}
		s.man.Routes = append(s.man.Routes, spec.name)
	}
	return nil
}

// seedBudgets writes limits derived from what was actually spent.
//
// Not round figures chosen up front, which produce a deployment where every
// budget is comfortable: no
// team past a line, no account flagged, and the whole half of the console that
// reacts to a limit rendering its empty state. A limit only means anything
// relative to the spend beneath it.
//
// Spend is measured the way the ledger measures it: `attempts > 0`, so a
// request a budget blocked is never charged to the budget that blocked it.
func (s *seeder) seedBudgets(teams map[string]*store.Team, people []*person) error {
	month := time.Date(s.now.Year(), s.now.Month(), 1, 0, 0, 0, 0, time.UTC)

	org, err := s.spendSince(`SELECT '', coalesce(sum(cost_nanos), 0) FROM requests
	                          WHERE attempts > 0 AND started_at >= ?`, month)
	if err != nil {
		return err
	}
	// The organization's three windows. Generous on purpose, and generously
	// apart: a deployment-wide limit is a backstop, one that binds before any
	// team's does would make every other budget on the screen unreadable, and a
	// soft limit that rounds onto its own hard limit is a warning nobody ever
	// sees.
	for period, start := range map[string]time.Time{
		"day":   s.now.Truncate(24 * time.Hour),
		"week":  s.now.Truncate(24*time.Hour).AddDate(0, 0, -int(s.now.Weekday())),
		"month": month,
	} {
		spent, serr := s.spendSince(`SELECT '', coalesce(sum(cost_nanos), 0) FROM requests
		                            WHERE attempts > 0 AND started_at >= ?`, start)
		if serr != nil {
			return serr
		}
		if berr := s.upsertBudget("org", "", period, "the organization",
			limitOf(spent[""], 4), limitOf(spent[""], 8)); berr != nil {
			return berr
		}
	}

	// A team's spend reaches it two ways and they are made disjoint in the
	// query rather than assumed so: through membership for a person, and off
	// the row for a pipeline, which has no membership to reach through.
	byTeam, err := s.spendSince(`
		SELECT tm.team_id, coalesce(sum(r.cost_nanos), 0)
		FROM requests r JOIN team_members tm ON tm.user_id = r.user_id
		WHERE r.attempts > 0 AND r.started_at >= ? AND r.user_id IS NOT NULL
		GROUP BY tm.team_id`, month)
	if err != nil {
		return err
	}
	byService, err := s.spendSince(`
		SELECT r.team_id, coalesce(sum(r.cost_nanos), 0)
		FROM requests r
		WHERE r.attempts > 0 AND r.started_at >= ? AND r.user_id IS NULL AND r.team_id IS NOT NULL
		GROUP BY r.team_id`, month)
	if err != nil {
		return err
	}
	for id, n := range byService {
		byTeam[id] += n
	}

	for _, spec := range teamSpecs {
		if spec.hard == 0 {
			continue
		}
		spent := byTeam[teams[spec.slug].ID]
		if berr := s.upsertBudget("team", teams[spec.slug].ID, "month", spec.name,
			limitOf(spent, spec.soft), limitOf(spent, spec.hard)); berr != nil {
			return berr
		}
	}

	byUser, err := s.spendSince(`
		SELECT user_id, coalesce(sum(cost_nanos), 0) FROM requests
		WHERE attempts > 0 AND started_at >= ? AND user_id IS NOT NULL
		GROUP BY user_id`, month)
	if err != nil {
		return err
	}
	for _, p := range people {
		if p.spec.hard == 0 {
			continue
		}
		spent := byUser[p.user.ID]
		if err := s.upsertBudget("user", p.user.ID, "month", p.spec.name,
			limitOf(spent, p.spec.soft), limitOf(spent, p.spec.hard)); err != nil {
			return err
		}
	}

	log.Printf("budgets written against %s of month-to-date spend", pricing.Money(org[""]).Display())
	return nil
}

// spendSince runs a two-column query - key, nanodollars - with one timestamp
// argument.
func (s *seeder) spendSince(query string, since time.Time) (map[string]int64, error) {
	rows, err := s.db.QueryContext(s.ctx, s.rebind(query), since)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstLine(query), err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var (
			key   string
			spent int64
		)
		if err := rows.Scan(&key, &spent); err != nil {
			return nil, err
		}
		out[key] = spent
	}
	return out, rows.Err()
}

// limitOf turns a multiple of observed spend into the round figure somebody
// would have typed, never below ten dollars - a limit of $0 blocks a team that
// has simply not started yet.
func limitOf(spent int64, multiple float64) int64 {
	const ten = 10_000_000_000
	n := int64(float64(spent) * multiple)
	n = (n + ten/2) / ten * ten
	if n < ten {
		n = ten
	}
	return n
}

func (s *seeder) upsertBudget(scope, scopeID, period, name string, soft, hard int64) error {
	var id string
	err := s.queryRow(`SELECT id FROM budgets WHERE scope = ? AND scope_id = ? AND period = ?`,
		scope, scopeID, period).Scan(&id)
	if err == nil {
		return s.exec(`UPDATE budgets SET soft_limit_nanos = ?, hard_limit_nanos = ?, name = ?, updated_at = ? WHERE id = ?`,
			soft, hard, name, s.now, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("budget %s/%s: %w", scope, period, err)
	}

	id = store.NewID("bdg")
	if err := s.exec(`
		INSERT INTO budgets (id, scope, scope_id, period, soft_limit_nanos, hard_limit_nanos, name, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, scope, scopeID, period, soft, hard, name, true, s.now, s.now); err != nil {
		return err
	}
	s.man.Budgets = append(s.man.Budgets, id)
	return nil
}

func (s *seeder) seedCostCenters() error {
	// The catch-all first, because everything below it is pointless while it
	// stands.
	var catchAll string
	err := s.queryRow(`SELECT id FROM cost_center_rules WHERE match_on = ? AND lower(pattern) = ?`,
		catchAllRule.matchOn, catchAllRule.pattern).Scan(&catchAll)
	switch {
	case err == nil:
		if derr := s.exec(`DELETE FROM cost_center_rules WHERE id = ?`, catchAll); derr != nil {
			return derr
		}
		log.Printf("removed the `repo *` catch-all rule: it claimed every repository, "+
			"which leaves the unallocated worklist empty (rule %s)", catchAll)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("catch-all rule: %w", err)
	}

	if err := s.upsertCostCenters(); err != nil {
		return err
	}

	// Positions are assigned in two passes, tickets before repositories.
	//
	// First match wins, so an ordered list is where "ticket prefix, then
	// repository" is actually written down: an RND ticket in a repository some
	// other line claims has to reach RND, and it only does if its rule sits
	// above the repository's.
	position := 0
	for _, pass := range []string{"ticket", "repo"} {
		if err := s.writeCostCenterRules(pass, &position); err != nil {
			return err
		}
	}
	return nil
}

// upsertCostCenters writes the chart of accounts, adopting any line whose code
// this deployment already holds.
func (s *seeder) upsertCostCenters() error {
	for _, spec := range costCenterSpecs {
		var id string
		err := s.queryRow(`SELECT id FROM cost_centers WHERE lower(code) = ?`, strings.ToLower(spec.code)).Scan(&id)
		switch {
		case err == nil:
			if uerr := s.exec(`
				UPDATE cost_centers SET name = ?, owner = ?, description = ?, plan_nanos = ?,
				       accepts_stamp = ?, archived = false, updated_at = ? WHERE id = ?`,
				spec.name, spec.owner, spec.description, spec.planNanos, spec.acceptsStamp, s.now, id); uerr != nil {
				return uerr
			}
		case errors.Is(err, sql.ErrNoRows):
			id = store.NewID("cct")
			if ierr := s.exec(`
				INSERT INTO cost_centers (id, code, name, owner, description, plan_nanos,
				        warn_at_percent, accepts_stamp, archived, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, 80, ?, false, ?, ?)`,
				id, spec.code, spec.name, spec.owner, spec.description, spec.planNanos,
				spec.acceptsStamp, s.now, s.now); ierr != nil {
				return ierr
			}
			s.man.CostCenters = append(s.man.CostCenters, id)
		default:
			return fmt.Errorf("cost center %s: %w", spec.code, err)
		}

		s.costCenters[strings.ToLower(spec.code)] = costCenterRow{id: id, acceptsStamp: spec.acceptsStamp}
	}
	return nil
}

// writeCostCenterRules writes every rule of one match kind, in spec order.
func (s *seeder) writeCostCenterRules(match string, position *int) error {
	for _, spec := range costCenterSpecs {
		id := s.costCenters[strings.ToLower(spec.code)].id
		for _, rule := range spec.rules {
			if rule.matchOn != match {
				continue
			}
			*position++
			// One rule per pattern across every line, which the schema
			// enforces: rewrite whatever holds this pattern rather than
			// colliding with it.
			if err := s.exec(`DELETE FROM cost_center_rules WHERE match_on = ? AND lower(pattern) = ?`,
				rule.matchOn, strings.ToLower(rule.pattern)); err != nil {
				return err
			}
			ruleID := store.NewID("ccr")
			if err := s.exec(`
				INSERT INTO cost_center_rules (id, cost_center_id, position, match_on, pattern, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				ruleID, id, *position, rule.matchOn, rule.pattern, s.now, s.now); err != nil {
				return err
			}
			s.man.CostRules = append(s.man.CostRules, ruleID)
		}
	}
	return nil
}

// workItem is one seeded unit of work, with the session-facing facts a request
// would have revealed about it.
type workItem struct {
	id     string
	kind   string
	key    string
	repo   string
	branch string
	ticket string
	team   string
}

func (s *seeder) seedWork() ([]*workItem, error) {
	var out []*workItem
	ticketNo := 1000

	for _, repo := range repoSpecs {
		for _, subject := range repo.subjects {
			opened := s.now.AddDate(0, 0, -s.rng.IntN(s.days))
			branchName := "feat/" + subject
			ticketKey := ""
			if repo.ticketPrefix != "" {
				ticketNo += 7 + s.rng.IntN(23)
				ticketKey = fmt.Sprintf("%s-%d", repo.ticketPrefix, ticketNo)
				branchName = fmt.Sprintf("feat/%s-%s", ticketKey, subject)
			}

			// The branch is what a request body reveals, so it is always a work
			// item. The ticket and the pull request are upgrades on top of it.
			branch, err := s.upsertWorkItem(workRow{
				kind: "branch", key: repo.name + "@" + branchName,
				repo: repo.name, branch: branchName,
				title: titleFor(subject), source: "inferred",
				firstSeen: opened, lastSeen: opened.AddDate(0, 0, 1+s.rng.IntN(6)),
			})
			if err != nil {
				return nil, err
			}
			out = append(out, &workItem{
				id: branch, kind: "branch", key: repo.name + "@" + branchName,
				repo: repo.name, branch: branchName, ticket: ticketKey, team: repo.team,
			})

			if ticketKey == "" {
				continue
			}

			// Two thirds of branches reach a pull request, and most of those
			// land. `merged_at` decides the outcome, never the status word.
			if s.rng.Float64() > 0.35 {
				number := 300 + s.rng.IntN(400)
				outcome, status, merged, closed := s.resolution(opened)
				pr, perr := s.upsertWorkItem(workRow{
					kind: "pull_request", key: fmt.Sprintf("%s#%d", repo.name, number),
					repo: repo.name, branch: branchName,
					title:  titleFor(subject),
					url:    fmt.Sprintf("https://github.com/%s/pull/%d", repo.name, number),
					status: status, outcome: outcome, source: "github",
					externalID: "PR_" + s.token(8),
					firstSeen:  opened, lastSeen: opened.AddDate(0, 0, 2+s.rng.IntN(9)),
					opened: &opened, merged: merged, closed: closed,
					checked: &s.now, confirmed: &opened,
				})
				if perr != nil {
					return nil, perr
				}
				out = append(out, &workItem{
					id: pr, kind: "pull_request", key: fmt.Sprintf("%s#%d", repo.name, number),
					repo: repo.name, branch: branchName, ticket: ticketKey, team: repo.team,
				})
				if lerr := s.link(branch, pr, "opened_as", opened); lerr != nil {
					return nil, lerr
				}
			}

			outcome, status, _, closed := s.resolution(opened)
			ticket, err := s.upsertWorkItem(workRow{
				kind: "ticket", key: ticketKey,
				repo: repo.name, title: titleFor(subject),
				url:     "https://example.atlassian.net/browse/" + ticketKey,
				status:  status,
				outcome: outcome, source: "inferred", project: repo.ticketPrefix,
				firstSeen: opened, lastSeen: opened.AddDate(0, 0, 3+s.rng.IntN(14)),
				closed: closed,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, &workItem{
				id: ticket, kind: "ticket", key: ticketKey,
				repo: repo.name, branch: branchName, ticket: ticketKey, team: repo.team,
			})
			if err := s.link(branch, ticket, "implements", opened); err != nil {
				return nil, err
			}
		}
	}

	// A handful of capitalised-R&D tickets, so CC-900 has something on it and
	// the stamp it refuses has somewhere it was aiming.
	for i := range 4 {
		key := fmt.Sprintf("RND-%d", 40+i*3)
		opened := s.now.AddDate(0, 0, -s.rng.IntN(s.days))
		id, err := s.upsertWorkItem(workRow{
			kind: "ticket", key: key, repo: "acme/panorama",
			title: "Evaluate a second provider surface", project: "RND",
			url:     "https://example.atlassian.net/browse/" + key,
			status:  "In progress",
			outcome: "open", source: "inferred",
			firstSeen: opened, lastSeen: s.now,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, &workItem{
			id: id, kind: "ticket", key: key, repo: "acme/panorama",
			branch: "spike/" + key, ticket: key, team: "platform",
		})
	}

	return out, nil
}

// resolution picks an outcome and the timestamps that justify it.
//
// The outcome is decided by whether there is a merge, never by the status word:
// a merged pull request reports state "closed" on GitHub, so grouping on the
// word files every merge as abandoned.
func (s *seeder) resolution(opened time.Time) (outcome, status string, merged, closed *time.Time) {
	end := opened.AddDate(0, 0, 1+s.rng.IntN(11))
	if end.After(s.now) {
		end = s.now
	}
	switch r := s.rng.Float64(); {
	case r < 0.62:
		return "landed", "Merged", &end, &end
	case r < 0.80:
		return "open", "In progress", nil, nil
	default:
		return "abandoned", "Closed", nil, &end
	}
}

// workRow is the shape of one work_items insert.
type workRow struct {
	kind, key, repo, branch     string
	title, url, status, outcome string
	source, project, externalID string
	firstSeen, lastSeen         time.Time
	opened, merged, closed      *time.Time
	checked, confirmed          *time.Time
}

func (s *seeder) upsertWorkItem(w workRow) (string, error) {
	var id string
	err := s.queryRow(`SELECT id FROM work_items WHERE kind = ? AND key = ?`, w.kind, w.key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("work item %s: %w", w.key, err)
	}

	id = store.NewID("wi")
	if err := s.exec(`
		INSERT INTO work_items (
			id, kind, key, repo, branch, title, url, status, source,
			first_seen_at, last_seen_at, author, project, outcome, external_id,
			checked_at, confirmed_at, opened_at, merged_at, closed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, w.kind, w.key, w.repo, w.branch, w.title, w.url, w.status, w.source,
		w.firstSeen, w.lastSeen, w.project, w.outcome, w.externalID,
		w.checked, w.confirmed, w.opened, w.merged, w.closed); err != nil {
		return "", err
	}
	s.man.WorkItems = append(s.man.WorkItems, id)
	return id, nil
}

func (s *seeder) link(from, to, relation string, at time.Time) error {
	return s.exec(`
		INSERT INTO work_item_links (from_id, to_id, relation, created_at)
		VALUES (?, ?, ?, ?)`, from, to, relation, at)
}

// serviceToken is a pipeline credential, which bills a team and has no person
// behind it.
type serviceToken struct {
	id, teamID, teamSlug string
	// stamp is the cost center code this pipeline asserts on every request.
	stamp string
}

func (s *seeder) seedServiceTokens(teams map[string]*store.Team, people []*person) ([]*serviceToken, error) {
	specs := []struct{ team, name, stamp string }{
		{"platform", "github-actions", ""},
		// A pipeline that builds one product and says so. This is the case no
		// repository pattern expresses, which is the whole argument for a stamp.
		{"payments", "release-pipeline", "CC-100"},
		// A stamp at a line that refuses them. The request is served and the
		// rules allocate it; the rejected code is kept so it surfaces.
		{"data", "nightly-eval", "CC-900"},
	}

	var out []*serviceToken
	for _, spec := range specs {
		team := teams[spec.team]
		hash, prefix := s.credential("apk")
		id := store.NewID("svc")
		created := s.now.AddDate(0, 0, -s.days)
		expires := s.now.AddDate(0, 0, 90)
		if err := s.exec(`
			INSERT INTO service_tokens (id, team_id, created_by, name, prefix, token_hash, created_at, last_used_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, team.ID, people[0].user.ID, spec.name, prefix, hash, created, s.now, expires); err != nil {
			return nil, err
		}
		s.man.ServiceTokens = append(s.man.ServiceTokens, id)
		out = append(out, &serviceToken{id: id, teamID: team.ID, teamSlug: spec.team, stamp: spec.stamp})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Traffic
// ---------------------------------------------------------------------------

// The ladder read from both ends: the four ways a conversation gets placed, and
// the three ways it does not.
//
// The unplaced three are not noise. store.Unplaced partitions the unattributed
// remainder into exactly these, so writing them deliberately is what makes the
// Attribution tab's breakdown add up to the gap on Spend.
const (
	placeTicket  = "ticket"
	placePR      = "pull_request"
	placeBranch  = "branch"
	placeMention = "mention"

	unplacedNoRepo   = "no_repository"
	unplacedNoBranch = "no_branch"
	unplacedNoItem   = "no_work_item"
)

// session is one conversation about to be written.
type session struct {
	id          string
	userID      string
	serviceID   string
	teamID      string
	tool        string
	startedAt   time.Time
	repo        string
	branch      string
	workItemID  string
	rung        string
	confidence  string
	evidence    string
	route       routeSpec
	turns       int
	stampCode   string
	stampCenter string
}

func (s *seeder) seedTraffic(people []*person, work []*workItem, ci []*serviceToken) (int, int, error) {
	byRepo := map[string][]*workItem{}
	for _, w := range work {
		byRepo[w.repo] = append(byRepo[w.repo], w)
	}
	teamTraffic := map[string]float64{}
	for _, t := range teamSpecs {
		teamTraffic[t.slug] = t.traffic
	}

	var requests, sessions int

	for day := s.days - 1; day >= 0; day-- {
		date := s.now.AddDate(0, 0, -day)

		// Weekends are quiet but not empty, and volume grows toward today, so
		// the daily chart has a shape rather than a band of noise.
		factor := 0.62 + 0.55*float64(s.days-day)/float64(s.days)
		switch date.Weekday() {
		case time.Saturday, time.Sunday:
			factor *= 0.18
		default:
		}
		// One bad week: an agent left running against a large repository. It is
		// what an anomaly looks like on the chart, and what a soft limit is for.
		if day >= 11 && day <= 13 {
			factor *= 2.4
		}

		for _, p := range people {
			if p.spec.traffic == 0 {
				continue
			}
			// A deactivated account stops spending on the day it is
			// deactivated, and keeps every request it made before then.
			if p.spec.disabled && day < 18 {
				continue
			}

			repos := s.reposFor(p.spec.teams)
			if len(repos) == 0 {
				continue
			}

			expected := p.spec.traffic * teamTraffic[p.spec.teams[0]] * factor * 2.1
			for range s.count(expected) {
				repo := repos[s.rng.IntN(len(repos))]
				n, err := s.writeSession(s.buildSession(p, repo, byRepo[repo.name], date))
				if err != nil {
					return 0, 0, err
				}
				requests += n
				sessions++
			}
		}

		// The pipelines. Fewer conversations, and the only traffic that bills a
		// team with no person behind it.
		for _, token := range ci {
			for range s.count(1.4 * factor) {
				n, err := s.writeSession(s.buildCISession(token, byRepo, date))
				if err != nil {
					return 0, 0, err
				}
				requests += n
				sessions++
			}
		}
	}

	return requests, sessions, nil
}

func (s *seeder) reposFor(teams []string) []repoSpec {
	var out []repoSpec
	for _, r := range repoSpecs {
		for _, t := range teams {
			if r.team == t {
				out = append(out, r)
			}
		}
	}
	return out
}

// count turns an expected number of conversations into a whole one, keeping the
// fractional part as a probability so somebody at 0.4 sessions a day is idle
// most days rather than never.
func (s *seeder) count(expected float64) int {
	n := int(expected)
	if s.rng.Float64() < expected-float64(n) {
		n++
	}
	return n
}

func (s *seeder) buildSession(p *person, repo repoSpec, items []*workItem, date time.Time) *session {
	sess := &session{
		id:        store.NewID("ses"),
		userID:    p.user.ID,
		tool:      p.spec.tool,
		startedAt: s.workingHour(date),
		route:     s.routeFor(p.spec.teams),
		turns:     3 + s.rng.IntN(34),
	}

	place := s.place(repo)
	switch place {
	case unplacedNoRepo:
		// Nothing in the conversation revealed a checkout. Real, common, and
		// the reason the console calls this "exploratory" rather than the store
		// doing so.
		sess.turns = 2 + s.rng.IntN(9)
	case unplacedNoBranch:
		sess.repo = repo.name
	case unplacedNoItem:
		sess.repo = repo.name
		sess.branch = []string{"main", "wip/local-debug", "scratch/" + s.token(3)}[s.rng.IntN(3)]
	default:
		item := s.pick(items, place)
		if item == nil {
			sess.repo = repo.name
			sess.branch = "main"
			break
		}
		sess.repo = item.repo
		sess.branch = item.branch
		sess.workItemID = item.id
		switch place {
		case placeTicket:
			sess.rung, sess.confidence, sess.evidence = "structural", "high", "branch name"
		case placePR:
			sess.rung, sess.confidence, sess.evidence = "structural", "high", "gh pr view output"
		case placeBranch:
			sess.rung, sess.confidence, sess.evidence = "structural", "medium", "git remote and branch"
		case placeMention:
			sess.rung, sess.confidence, sess.evidence = "content", "low", "ticket key in conversation"
		}
	}

	// A small share of people stamp their own work. The valid one lands on
	// CC-100; the typo lands nowhere, which is the point of keeping it.
	switch r := s.rng.Float64(); {
	case r < 0.02:
		sess.stampCode, sess.stampCenter = s.stamp("CC-100")
	case r < 0.026:
		sess.stampCode, sess.stampCenter = s.stamp("CC-1O0")
	}

	return sess
}

// place chooses how this conversation ends up on the ladder.
//
// A repository with no tracker convention cannot produce a ticket, so its
// branches are placed as branches or not at all - which is exactly why
// acme/mobile-app carries the unallocated worklist item.
func (s *seeder) place(repo repoSpec) string {
	r := s.rng.Float64()
	if repo.ticketPrefix == "" {
		switch {
		case r < 0.40:
			return placeBranch
		case r < 0.75:
			return unplacedNoItem
		case r < 0.90:
			return unplacedNoBranch
		default:
			return unplacedNoRepo
		}
	}
	switch {
	case r < 0.36:
		return placeTicket
	case r < 0.50:
		return placePR
	case r < 0.59:
		return placeBranch
	case r < 0.62:
		return placeMention
	case r < 0.76:
		return unplacedNoRepo
	case r < 0.88:
		return unplacedNoBranch
	default:
		return unplacedNoItem
	}
}

func (s *seeder) pick(items []*workItem, place string) *workItem {
	want := map[string]string{
		placeTicket: "ticket", placeMention: "ticket",
		placePR: "pull_request", placeBranch: "branch",
	}[place]

	var candidates []*workItem
	for _, w := range items {
		if w.kind == want {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[s.rng.IntN(len(candidates))]
}

func (s *seeder) buildCISession(token *serviceToken, byRepo map[string][]*workItem, date time.Time) *session {
	var repo repoSpec
	for _, r := range repoSpecs {
		if r.team == token.teamSlug {
			repo = r
			break
		}
	}

	sess := &session{
		id:        store.NewID("ses"),
		serviceID: token.id,
		teamID:    token.teamID,
		tool:      "anthropic-sdk",
		startedAt: date.Truncate(24 * time.Hour).Add(time.Duration(s.rng.IntN(24)) * time.Hour),
		route:     routeSpecs[1], // the pipelines run on the cheap route
		turns:     4 + s.rng.IntN(12),
		repo:      repo.name,
	}
	if items := byRepo[repo.name]; len(items) > 0 {
		item := items[s.rng.IntN(len(items))]
		sess.branch = item.branch
		sess.workItemID = item.id
		sess.rung, sess.confidence, sess.evidence = "structural", "high", "branch name"
	}
	if token.stamp != "" {
		sess.stampCode, sess.stampCenter = s.stamp(token.stamp)
	}
	return sess
}

// stamp resolves a code the way the data plane does: a line that exists and
// accepts stamps becomes the allocation, and anything else is kept verbatim so
// it can be counted rather than lost.
func (s *seeder) stamp(code string) (recorded, centerID string) {
	cc, ok := s.costCenters[strings.ToLower(code)]
	if ok && cc.acceptsStamp {
		return "", cc.id
	}
	return code, ""
}

// workingHour puts a conversation somewhere in a working day, with a tail into
// the evening.
func (s *seeder) workingHour(date time.Time) time.Time {
	hour := 8 + s.rng.IntN(11)
	if s.rng.Float64() < 0.08 {
		hour = 19 + s.rng.IntN(4)
	}
	return date.Truncate(24 * time.Hour).
		Add(time.Duration(hour)*time.Hour +
			time.Duration(s.rng.IntN(60))*time.Minute +
			time.Duration(s.rng.IntN(60))*time.Second)
}

// writeSession writes the conversation and every turn of it.
func (s *seeder) writeSession(sess *session) (int, error) {
	at := sess.startedAt
	written := 0
	last := at

	for turn := range sess.turns {
		rec := s.turn(sess, turn, at)
		if err := s.db.RecordRequest(s.ctx, rec); err != nil {
			return 0, fmt.Errorf("record request: %w", err)
		}
		written++
		last = at

		// What else a served Claude Code turn makes: background calls to its
		// fast model (titles, summaries, command checks) on most turns, and
		// now and then a web search the model asked for. Both belong to the
		// sitting, and the trail is meant to show them in it.
		if sess.tool == "claude-code" && rec.Decision == "allowed" && rec.Surface == "anthropic" {
			if s.rng.Float64() < 0.6 {
				if err := s.db.RecordRequest(s.ctx, s.fastCall(sess, at.Add(2*time.Second))); err != nil {
					return 0, fmt.Errorf("record request: %w", err)
				}
				written++
			}
			if s.rng.Float64() < 0.07 {
				if err := s.db.RecordRequest(s.ctx, s.search(sess, at.Add(6*time.Second))); err != nil {
					return 0, fmt.Errorf("record request: %w", err)
				}
			}
		}
		at = at.Add(time.Duration(15+s.rng.IntN(240)) * time.Second)
	}

	if err := s.exec(`
		INSERT INTO sessions (
			id, fingerprint, method, user_id, tool, started_at, last_seen_at, requests,
			work_item_id, rung, confidence, repo, branch, service_token_id
		) VALUES (?, ?, 'conversation-prefix', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.id, s.token(32), nullIfEmpty(sess.userID), sess.tool,
		sess.startedAt, last, written,
		nullIfEmpty(sess.workItemID), sess.rung, sess.confidence, sess.repo, sess.branch,
		sess.serviceID); err != nil {
		return 0, err
	}

	if sess.workItemID != "" {
		if err := s.exec(`
			INSERT INTO attributions (id, session_id, work_item_id, rung, confidence, evidence, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			store.NewID("att"), sess.id, sess.workItemID, sess.rung, sess.confidence, sess.evidence,
			sess.startedAt, last); err != nil {
			return 0, err
		}
	}

	return written, nil
}

// turn builds one request.
func (s *seeder) turn(sess *session, index int, at time.Time) store.RequestRecord {
	provider, model, _ := strings.Cut(sess.route.model, "/")
	surface := "anthropic"
	if provider == string(providers.KindBedrockMantle) {
		// The open-weight route speaks the OpenAI APIs only.
		surface = "openai-chat"
		if sess.tool == "codex-cli" {
			surface = "openai-responses"
		}
	}
	rec := store.RequestRecord{
		RequestID:       s.token(16),
		UserID:          sess.userID,
		TeamID:          sess.teamID,
		ServiceTokenID:  sess.serviceID,
		SessionID:       sess.id,
		StartedAt:       at,
		Surface:         surface,
		Provider:        provider,
		RequestedModel:  sess.route.name,
		ResolvedModel:   model,
		Streaming:       true,
		StatusCode:      200,
		Decision:        "allowed",
		BudgetState:     "ok",
		Attempts:        1,
		Tool:            sess.tool,
		UserAgent:       userAgent(sess.tool),
		ClientIP:        fmt.Sprintf("10.%d.%d.%d", 20+s.rng.IntN(4), s.rng.IntN(256), 1+s.rng.IntN(254)),
		CostCenterID:    sess.stampCenter,
		CostCenterStamp: sess.stampCode,
	}
	if sess.userID != "" {
		rec.TokenID = s.tokens[sess.userID]
	}

	// What the gateway did. The refusals come first because they are the ones
	// with no provider behind them: attempts stays zero, nothing is billed, and
	// the budget that blocked the request must not be charged for it.
	switch r := s.rng.Float64(); {
	case r < 0.012:
		rec.Decision, rec.StatusCode, rec.Attempts = "blocked", 429, 0
		rec.BudgetState, rec.ErrorKind = "over_hard", "budget_exceeded"
		rec.Streaming = false
		return rec
	case r < 0.022:
		// Past a soft limit a route stops; it does not change model. The
		// engineer is told which routes are still served and switches.
		rec.Decision, rec.StatusCode, rec.Attempts = "blocked", 429, 0
		rec.BudgetState, rec.ErrorKind = "over_soft", "route_over_soft"
		rec.RequestedModel, rec.ResolvedModel = "deep", ""
		rec.Streaming = false
		return rec
	case r < 0.030:
		// A model id sent by a client is a name nobody routed.
		rec.Decision, rec.StatusCode, rec.Attempts = "refused", 404, 0
		rec.ErrorKind = "unknown_route"
		rec.RequestedModel, rec.ResolvedModel = "anthropic.claude-sonnet-4-20250514-v1:0", ""
		rec.Streaming = false
		return rec
	case r < 0.040:
		rec.Decision, rec.StatusCode = "error", 503
		rec.ErrorKind = "unavailable"
		rec.Attempts = 1
		rec.Duration = time.Duration(2+s.rng.IntN(9)) * time.Second
		return rec
	case r < 0.055 && sess.route.name == "standard":
		// The provider was throttling the model. A route is one model, so the
		// engineer's client was sent a 429 and retried; nothing ran and nothing
		// was billed, and the attempt still counts as one.
		rec.Decision, rec.StatusCode = "error", 429
		rec.ErrorKind = "throttled"
		rec.Attempts = 1
		rec.Duration = time.Duration(300+s.rng.IntN(900)) * time.Millisecond
		return rec
	}

	// Coding-agent economics live in the cache, so the token mix is the shape
	// that matters: the opening turn writes the context, and every turn after
	// it reads that context back at a tenth of the input rate.
	switch {
	case sess.tool == "cline":
		// A harness with no cache_control resends the whole conversation at the
		// full input rate every turn. This is the finding, written as the
		// numbers that produce it rather than as a flag.
		rec.InputTokens = int64(18_000 + s.rng.IntN(90_000) + index*2_400)
		rec.OutputTokens = int64(180 + s.rng.IntN(1_600))
	case index == 0:
		rec.InputTokens = int64(900 + s.rng.IntN(3_200))
		rec.OutputTokens = int64(250 + s.rng.IntN(1_100))
		rec.CacheWriteTokens = int64(11_000 + s.rng.IntN(52_000))
	default:
		rec.InputTokens = int64(40 + s.rng.IntN(900))
		rec.OutputTokens = int64(200 + s.rng.IntN(2_400))
		rec.CacheReadTokens = int64(21_000 + s.rng.IntN(170_000) + index*1_500)
		if s.rng.Float64() < 0.42 {
			rec.CacheWriteTokens = int64(1_800 + s.rng.IntN(19_000))
		}
	}
	// The one-hour cache is a separately published rate, so the split has to be
	// recorded at request time - it cannot be recovered afterwards.
	if rec.CacheWriteTokens > 0 && s.rng.Float64() < 0.18 {
		rec.CacheWrite1hTokens = rec.CacheWriteTokens / 3
	}

	rec.Duration = time.Duration(3_000+s.rng.IntN(60_000)) * time.Millisecond
	rec.TimeToFirstToken = time.Duration(380_000+s.rng.IntN(2_100_000)) * time.Microsecond

	// Priced through the real table, so a model whose rate class is unpublished
	// is recorded unpriced rather than cheap. `archive` is exactly that model,
	// which is the reason it is in the catalogue at all.
	cost, entry, err := s.prices.Price(rec.Provider, rec.ResolvedModel, rec.Usage())
	if err == nil {
		rec.Cost = &cost
		rec.CostSource = entry.Source
	}
	return rec
}

// fastCall is one of Claude Code's background calls to its fast model: a short
// prompt of its own, a few hundred tokens, nothing cached.
func (s *seeder) fastCall(sess *session, at time.Time) store.RequestRecord {
	provider, model, _ := strings.Cut(fastRoute.model, "/")
	rec := store.RequestRecord{
		RequestID: s.token(16), UserID: sess.userID, TeamID: sess.teamID,
		ServiceTokenID: sess.serviceID, SessionID: sess.id, StartedAt: at,
		Surface: "anthropic", Provider: provider,
		RequestedModel: fastRoute.name, ResolvedModel: model,
		StatusCode: 200, Decision: "allowed", BudgetState: "ok", Attempts: 1,
		Tool: sess.tool, UserAgent: userAgent(sess.tool),
		InputTokens:  int64(300 + s.rng.IntN(2_200)),
		OutputTokens: int64(8 + s.rng.IntN(60)),
		Duration:     time.Duration(400+s.rng.IntN(1_400)) * time.Millisecond,
	}
	if sess.userID != "" {
		rec.TokenID = s.tokens[sess.userID]
	}
	if cost, entry, err := s.prices.Price(rec.Provider, rec.ResolvedModel, rec.Usage()); err == nil {
		rec.Cost, rec.CostSource = &cost, entry.Source
	}
	return rec
}

// search is a web search over the gateway's MCP tool, priced at the published
// rate and placed in its conversation the way the endpoint places one.
func (s *seeder) search(sess *session, at time.Time) store.RequestRecord {
	price := websearch.PublishedPrice
	rec := store.RequestRecord{
		RequestID: s.token(16), UserID: sess.userID, TeamID: sess.teamID,
		ServiceTokenID: sess.serviceID, StartedAt: at, SessionFromSitting: true,
		Surface: "mcp", Provider: string(websearch.KindAgentCore),
		RequestedModel: "web_search", ResolvedModel: "web-search",
		StatusCode: 200, Decision: "allowed", BudgetState: "ok", Attempts: 1,
		Tool: sess.tool, UserAgent: "claude-code/2.1.4 (sdk-cli)",
		WebSearches: 1, CostSource: "aws-published",
		Cost:     &pricing.Cost{WebSearch: price, Total: price},
		Duration: time.Duration(900+s.rng.IntN(2_500)) * time.Millisecond,
	}
	if sess.userID != "" {
		rec.TokenID = s.tokens[sess.userID]
	}
	return rec
}

func userAgent(tool string) string {
	switch tool {
	case "claude-code":
		return "claude-cli/2.1.4 (external, cli)"
	case "opencode":
		return "opencode/0.6.11 (panorama)"
	case "cline":
		return "Cline/3.18.0 vscode/1.99.2"
	default:
		return "anthropic-sdk-python/0.69.0"
	}
}

// routeFor picks the route a person's request goes through, honouring the
// conditions on the routes that have them: a route somebody may not use is not
// one they can be seen using.
func (s *seeder) routeFor(teams []string) routeSpec {
	var eligible []routeSpec
	total := 0.0
	for _, r := range routeSpecs {
		if len(r.teams) > 0 && !anyOf(r.teams, teams) {
			continue
		}
		eligible = append(eligible, r)
		total += r.share
	}

	pick := s.rng.Float64() * total
	for _, r := range eligible {
		pick -= r.share
		if pick <= 0 {
			return r
		}
	}
	return eligible[0]
}

func anyOf(want, have []string) bool {
	for _, w := range want {
		for _, h := range have {
			if w == h {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The administrative trail
// ---------------------------------------------------------------------------

func (s *seeder) seedAudit(people []*person, teams map[string]*store.Team) error {
	type event struct {
		action, targetType, targetID string
		metadata                     map[string]any
	}

	admin := people[0]
	events := []event{
		{"team.create", "team", teams["payments"].ID, map[string]any{"slug": "payments"}},
		{"team.create", "team", teams["platform"].ID, map[string]any{"slug": "platform"}},
		{"budget.save", "budget", teams["platform"].ID, map[string]any{"period": "month", "hard_limit_nanos": 9_000_000_000_000}},
		{"route.save", "route", "deep", map[string]any{"target": "bedrock/global.anthropic.claude-opus-4-5-20251101-v1:0", "serves_over_soft": false}},
		{"route.save", "route", "fast", map[string]any{"target": "bedrock/global.anthropic.claude-haiku-4-5-20251001-v1:0"}},
		{"provider.save", "provider", "bedrock-mantle", map[string]any{"region": "us-west-2", "enabled": true}},
		{"cost_center.create", "cost_center", "CC-400", map[string]any{"plan_nanos": 3_000_000_000_000}},
		{"cost_center.rule.create", "cost_center", "CC-100", map[string]any{"match_on": "repo", "pattern": "acme/payments-*"}},
		{"cost_center.reallocate", "cost_center", "", map[string]any{"reason": "rule added"}},
		{"integration.connect", "integration", "github", map[string]any{"mode": "app"}},
		{"user.role", "user", people[4].user.ID, map[string]any{"from": "developer", "to": "manager", "source": "manual"}},
		{"user.deactivate", "user", people[len(people)-1].user.ID, map[string]any{"reason": "left the company"}},
		{"token.create", "gateway_token", people[1].tokenID, map[string]any{"name": "laptop"}},
		{"token.revoke", "gateway_token", people[3].tokenID, map[string]any{"reason": "rotated"}},
		{"sso.mapping.save", "oidc_group_role", "platform-engineering", map[string]any{"role": "manager"}},
		{"team.members.replace", "team", teams["data"].ID, map[string]any{"members": 2}},
	}

	for i, e := range events {
		id := store.NewID("aud")
		at := s.now.AddDate(0, 0, -(s.days - 2 - i*3))
		if at.After(s.now) {
			at = s.now
		}
		blob, err := json.Marshal(e.metadata)
		if err != nil {
			return err
		}
		if err := s.exec(`
			INSERT INTO audit_events (
				id, occurred_at, actor_type, actor_user_id, action,
				target_type, target_id, source_ip, user_agent, metadata
			) VALUES (?, ?, 'user', ?, ?, ?, ?, ?, ?, `+s.jsonPlaceholder()+`)`,
			id, at, admin.user.ID, e.action,
			e.targetType, e.targetID, "203.0.113.14", "Mozilla/5.0 (Macintosh)", string(blob)); err != nil {
			return err
		}
		s.man.AuditEvents = append(s.man.AuditEvents, id)
	}

	// A system actor, because the confirmation job is not a person and the
	// trail has to be able to say so.
	id := store.NewID("aud")
	blob, err := json.Marshal(map[string]any{"source": "github", "confirmed": 41, "absent": 6})
	if err != nil {
		return err
	}
	if err := s.exec(`
		INSERT INTO audit_events (
			id, occurred_at, actor_type, actor_user_id, action,
			target_type, target_id, source_ip, user_agent, metadata
		) VALUES (?, ?, 'system', NULL, 'work_item.confirm', 'integration', 'github', '', '', `+s.jsonPlaceholder()+`)`,
		id, s.now.Add(-2*time.Hour), string(blob)); err != nil {
		return err
	}
	s.man.AuditEvents = append(s.man.AuditEvents, id)
	return nil
}

// ---------------------------------------------------------------------------
// The manifest, and undoing a run
// ---------------------------------------------------------------------------

func (s *seeder) saveManifest() error {
	s.man.Version = 1
	s.man.CreatedAt = time.Now().UTC()
	s.man.Days = s.days

	blob, err := json.Marshal(s.man)
	if err != nil {
		return err
	}
	if err := s.db.SetSetting(s.ctx, manifestKey, string(blob)); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// reset removes exactly what a previous run wrote.
//
// The order is the schema's rather than a preference: requests reference
// sessions and principals, and requests.user_id is ON DELETE SET NULL - so
// deleting the people first would leave their traffic behind with nobody
// attached to it, which is worse than either extreme.
func (s *seeder) reset() (int64, error) {
	raw, err := s.db.Setting(s.ctx, manifestKey)
	if errors.Is(err, store.ErrNotFound) {
		log.Printf("no previous run recorded under settings[%s]; nothing to reset", manifestKey)
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read manifest: %w", err)
	}

	var man manifest
	if err := json.Unmarshal([]byte(raw), &man); err != nil {
		return 0, fmt.Errorf("parse manifest: %w", err)
	}

	var total int64
	steps := []struct {
		query string
		ids   []string
	}{
		// Traffic, reached through the principals that made it.
		{`DELETE FROM requests WHERE session_id IN (SELECT id FROM sessions WHERE user_id IN (%s))`, man.Users},
		{`DELETE FROM requests WHERE session_id IN (SELECT id FROM sessions WHERE service_token_id IN (%s))`, man.ServiceTokens},
		{`DELETE FROM requests WHERE user_id IN (%s)`, man.Users},
		{`DELETE FROM requests WHERE service_token_id IN (%s)`, man.ServiceTokens},
		// Attributions cascade from sessions.
		{`DELETE FROM sessions WHERE user_id IN (%s)`, man.Users},
		{`DELETE FROM sessions WHERE service_token_id IN (%s)`, man.ServiceTokens},
		// Work items cascade their links.
		{`DELETE FROM work_items WHERE id IN (%s)`, man.WorkItems},
		{`DELETE FROM audit_events WHERE id IN (%s)`, man.AuditEvents},
		{`DELETE FROM budgets WHERE id IN (%s)`, man.Budgets},
		{`DELETE FROM service_tokens WHERE id IN (%s)`, man.ServiceTokens},
		{`DELETE FROM cost_center_rules WHERE id IN (%s)`, man.CostRules},
		{`DELETE FROM cost_centers WHERE id IN (%s)`, man.CostCenters},
		{`DELETE FROM model_routes WHERE name IN (%s)`, man.Routes},
		// Team membership and gateway tokens cascade from users and teams.
		{`DELETE FROM users WHERE id IN (%s)`, man.Users},
		{`DELETE FROM teams WHERE id IN (%s)`, man.Teams},
	}

	for _, step := range steps {
		if len(step.ids) == 0 {
			continue
		}
		n, err := s.execN(fmt.Sprintf(step.query, placeholders(len(step.ids))), toArgs(step.ids)...)
		if err != nil {
			return total, err
		}
		total += n
	}

	if _, err := s.db.DeleteSetting(s.ctx, manifestKey); err != nil {
		return total, fmt.Errorf("clear manifest: %w", err)
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// rebind turns `?` placeholders into whatever the driver wants, the same way
// the store does. Written here rather than reached for because the store's is
// unexported, and a development seeder is not a reason to widen a package's
// surface.
func (s *seeder) rebind(query string) string {
	if s.db.Driver() != config.DriverPostgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsonPlaceholder is `?` with the cast Postgres wants for a jsonb column, which
// SQLite neither wants nor understands.
func (s *seeder) jsonPlaceholder() string {
	if s.db.Driver() == config.DriverPostgres {
		return "?::jsonb"
	}
	return "?"
}

func (s *seeder) exec(query string, args ...any) error {
	_, err := s.execN(query, args...)
	return err
}

func (s *seeder) execN(query string, args ...any) (int64, error) {
	res, err := s.db.ExecContext(s.ctx, s.rebind(query), args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", firstLine(query), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (s *seeder) queryRow(query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(s.ctx, s.rebind(query), args...)
}

// credential mints a token the way the product does: a hash to store and a
// prefix to show. The token itself is discarded, because nothing seeded here
// ever needs to authenticate.
func (s *seeder) credential(kind string) (hash, prefix string) {
	return s.token(32), kind + "_" + s.token(4)
}

func (s *seeder) token(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic("seed: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstLine(query string) string {
	trimmed := strings.TrimSpace(query)
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		return strings.TrimSpace(trimmed[:i])
	}
	return trimmed
}

// titleFor turns a branch subject into the sentence a tracker would hold.
func titleFor(subject string) string {
	words := strings.Split(subject, "-")
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
