// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

func usd(t *testing.T, s string) *pricing.Money {
	t.Helper()
	m, err := pricing.ParseUSD(s)
	if err != nil {
		t.Fatalf("ParseUSD(%q): %v", s, err)
	}
	return &m
}

func at(t *testing.T, stamp string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", stamp, err)
	}
	return ts.UTC()
}

func TestPeriodWindow(t *testing.T) {
	tests := []struct {
		period      Period
		now         string
		start, end  string
		description string
	}{
		{PeriodDay, "2026-08-14T13:45:00Z", "2026-08-14T00:00:00Z", "2026-08-15T00:00:00Z", "a day"},
		// 14 August 2026 is a Friday, so the week began on the 10th.
		{PeriodWeek, "2026-08-14T13:45:00Z", "2026-08-10T00:00:00Z", "2026-08-17T00:00:00Z", "a week beginning Monday"},
		// Sunday belongs to the week that began six days earlier, not to the
		// one starting tomorrow. This is the case Go's Weekday numbering gets
		// wrong if you subtract it directly.
		{PeriodWeek, "2026-08-16T23:59:59Z", "2026-08-10T00:00:00Z", "2026-08-17T00:00:00Z", "a Sunday"},
		{PeriodWeek, "2026-08-17T00:00:00Z", "2026-08-17T00:00:00Z", "2026-08-24T00:00:00Z", "a Monday"},
		{PeriodMonth, "2026-08-14T13:45:00Z", "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z", "a month"},
		// December rolls the year, which is the arithmetic AddDate gets right
		// and hand-rolled month maths does not.
		{PeriodMonth, "2026-12-31T23:59:59Z", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z", "December"},
	}

	for _, tc := range tests {
		start, end := tc.period.Window(at(t, tc.now))
		if !start.Equal(at(t, tc.start)) {
			t.Errorf("%s: start = %s, want %s", tc.description, start.Format(time.RFC3339), tc.start)
		}
		if !end.Equal(at(t, tc.end)) {
			t.Errorf("%s: end = %s, want %s", tc.description, end.Format(time.RFC3339), tc.end)
		}
	}
}

// TestWindowIsUTCRegardlessOfInput is the guarantee a block message depends on:
// "resets at 00:00 UTC" has to be one instant for everyone the budget governs.
func TestWindowIsUTCRegardlessOfInput(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("no timezone database: %v", err)
	}

	// 14 August 2026, 09:00 in Sydney, is still 13 August in UTC.
	local := time.Date(2026, 8, 14, 9, 0, 0, 0, sydney)
	start, _ := PeriodDay.Window(local)

	if want := at(t, "2026-08-13T00:00:00Z"); !start.Equal(want) {
		t.Errorf("start = %s, want %s - the window followed the caller's clock", start.Format(time.RFC3339), want)
	}
}

func TestBudgetState(t *testing.T) {
	b := Budget{
		Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodDay, Enabled: true,
		SoftLimit: usd(t, "8.00"), HardLimit: usd(t, "10.00"),
	}

	tests := []struct {
		spent string
		want  State
	}{
		{"0.00", StateOK},
		{"7.999999999", StateOK},
		// Reaching the limit is crossing it. A budget of $8 that permits the
		// request taking it to $8.00 has a limit of "$8 and a bit".
		{"8.00", StateOverSoft},
		{"9.99", StateOverSoft},
		{"10.00", StateOverHard},
		{"1000.00", StateOverHard},
	}

	for _, tc := range tests {
		got := b.State(Spend{Cost: *usd(t, tc.spent)})
		if got != tc.want {
			t.Errorf("spent %s: state = %q, want %q", tc.spent, got, tc.want)
		}
	}
}

func TestBudgetValidate(t *testing.T) {
	soft, hard := usd(t, "10.00"), usd(t, "5.00")

	tests := map[string]struct {
		budget  Budget
		wantErr string
	}{
		"a budget with no limits enforces nothing": {
			budget:  Budget{Scope: ScopeOrg, Period: PeriodDay},
			wantErr: "no limit is set",
		},
		"a soft limit above the hard one can never warn": {
			budget:  Budget{Scope: ScopeOrg, Period: PeriodDay, SoftLimit: soft, HardLimit: hard},
			wantErr: "would never warn",
		},
		"a user budget needs a user": {
			budget:  Budget{Scope: ScopeUser, Period: PeriodDay, HardLimit: hard},
			wantErr: "needs the user",
		},
		"an org budget has no subject": {
			budget:  Budget{Scope: ScopeOrg, ScopeID: "usr_1", Period: PeriodDay, HardLimit: hard},
			wantErr: "has no subject id",
		},
		"an unknown period is refused": {
			budget:  Budget{Scope: ScopeOrg, Period: "fortnight", HardLimit: hard},
			wantErr: "is not one of day",
		},
		"a valid budget passes": {
			budget: Budget{Scope: ScopeOrg, Period: PeriodMonth, HardLimit: hard},
		},
	}

	for name, tc := range tests {
		err := tc.budget.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: no error, want one mentioning %q", name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not mention %q", name, err, tc.wantErr)
		}
	}
}

func TestRouteAvailability(t *testing.T) {
	in := Input{UserID: "usr_alice", Tool: "claude-code", Surface: "anthropic"}
	teams := []string{"tm_platform"}

	tests := map[string]struct {
		route Route
		want  bool
	}{
		"a route with no conditions exists for everybody": {
			route: Route{}, want: true,
		},
		"a named user matches": {
			route: Route{Users: []string{"usr_alice", "usr_bob"}}, want: true,
		},
		"a user nobody named does not": {
			route: Route{Users: []string{"usr_bob"}}, want: false,
		},
		"team membership matches": {
			route: Route{Teams: []string{"tm_platform"}}, want: true,
		},
		"a team they are not on does not": {
			route: Route{Teams: []string{"tm_data"}}, want: false,
		},
		"conditions intersect: one miss is a miss": {
			route: Route{Tools: []string{"claude-code"}, Users: []string{"usr_bob"}}, want: false,
		},
		"tool matching ignores case": {
			route: Route{Tools: []string{"Claude-Code"}}, want: true,
		},
		"a tool nobody named does not match": {
			route: Route{Tools: []string{"cline"}}, want: false,
		},
	}

	for name, tc := range tests {
		if got := tc.route.Available(in, teams); got != tc.want {
			t.Errorf("%s: Available = %v, want %v", name, got, tc.want)
		}
	}
}

// A route stops at a soft limit only when an administrator marked it so, and
// nothing worse than a soft limit reads as permitted.
//
// The severity comparison rather than an equality is what makes the second
// half true: over_hard blocks before any route is consulted, so a route that
// answered "yes, I serve at over_hard" would be describing something that
// cannot happen - and the day the order of those two checks changes, an
// equality here would quietly serve a route past a ceiling somebody set.
func TestServesAt(t *testing.T) {
	ordinary := Route{Name: "standard", Target: bedrockRef("opus"), ServesOverSoft: true}
	stops := Route{Name: "standard", Target: bedrockRef("opus"), ServesOverSoft: false}

	for _, state := range []State{StateNone, StateOK, StateOverSoft, StateOverHard} {
		if !ordinary.ServesAt(state) {
			t.Errorf("an ordinary route stopped serving at %q, which no administrator asked for", state)
		}
	}
	for _, state := range []State{StateNone, StateOK} {
		if !stops.ServesAt(state) {
			t.Errorf("a route that stops past the soft limit refused a request at %q, "+
				"which is inside budget", state)
		}
	}
	for _, state := range []State{StateOverSoft, StateOverHard} {
		if stops.ServesAt(state) {
			t.Errorf("a route marked as stopping at the soft limit served at %q", state)
		}
	}
}

func TestRouteValidate(t *testing.T) {
	tests := map[string]struct {
		route   Route
		wantErr string
	}{
		"a route needs a name": {
			route:   Route{Target: bedrockRef("haiku")},
			wantErr: "needs a name",
		},
		"a wildcard would make the allowlist not one": {
			route:   Route{Name: "*", Target: bedrockRef("haiku")},
			wantErr: "wildcard",
		},
		// The name everybody picks first, and the one Claude Code never sends:
		// it resolves "default" to the account's runtime default before the
		// request is made, so the route sits there being refused by a name
		// nobody typed.
		"a route named default is never asked for": {
			route:   Route{Name: "default", Target: bedrockRef("haiku")},
			wantErr: "resolves before it sends",
		},
		"the alias names go the same way": {
			route:   Route{Name: "opus", Target: bedrockRef("haiku")},
			wantErr: "resolves before it sends",
		},
		"and case is not a way around it": {
			route:   Route{Name: "Sonnet", Target: bedrockRef("haiku")},
			wantErr: "resolves before it sends",
		},
		"a name that merely contains a reserved word is fine": {
			route: Route{Name: "opus-for-platform", Target: bedrockRef("haiku")},
		},
		"a route needs somewhere to send a request": {
			route:   Route{Name: "standard"},
			wantErr: "needs a model",
		},
		"a model on a provider this build does not serve is refused": {
			route:   Route{Name: "standard", Target: providers.ModelRef{Provider: "openrouter", Model: "x"}},
			wantErr: "does not serve",
		},
		"an ordinary route passes": {
			route: Route{Name: "standard", Target: bedrockRef("haiku")},
		},
		"a route that stops at the soft limit passes": {
			route: Route{Name: "standard", Teams: []string{"tm_1"}, Target: bedrockRef("opus"),
				ServesOverSoft: false},
		},
	}

	for name, tc := range tests {
		err := tc.route.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v does not mention %q", name, err, tc.wantErr)
		}
	}
}

func TestBlockMessageAnswersTheFourQuestions(t *testing.T) {
	b := Budget{
		Scope: ScopeUser, ScopeID: "usr_1", Period: PeriodMonth, Name: "personal",
		Enabled: true, HardLimit: usd(t, "20.00"),
	}
	spend := Spend{Cost: *usd(t, "20.35"), Requests: 412, Unpriced: 3}

	msg := blockMessage(b, spend, at(t, "2026-08-14T13:45:00Z"))

	for _, want := range []string{
		"personal",             // which limit
		"You have used",        // whose
		"$20.35",               // how much
		"$20.00",               // of what
		"2026-09-01 00:00 UTC", // when it comes back, absolutely
		"in 17 days",           // and relatively
		"could not be priced",  // the gap in the figure
		"administrator",        // who can change it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the block message does not contain %q:\n%s", want, msg)
		}
	}
}

func TestWarnMessageIsOneLine(t *testing.T) {
	b := Budget{
		Scope: ScopeTeam, ScopeID: "tm_1", Period: PeriodWeek, Name: "platform",
		Enabled: true, SoftLimit: usd(t, "50.00"), HardLimit: usd(t, "100.00"),
	}

	msg := warnMessage(b, Spend{Cost: *usd(t, "51.00"), Requests: 20}, at(t, "2026-08-14T13:45:00Z"))

	// It travels in an HTTP header, where a newline is not a formatting problem
	// but a header injection.
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("the warning is not header-safe: %q", msg)
	}
	if !strings.Contains(msg, "Your team has used") {
		t.Errorf("the warning does not say whose budget it is: %q", msg)
	}
	if !strings.Contains(msg, "still being served") {
		t.Errorf("the warning does not say the request went through: %q", msg)
	}
}

func TestHumanUntil(t *testing.T) {
	tests := map[time.Duration]string{
		30 * time.Second:    "in under a minute",
		5 * time.Minute:     "in 5 minutes",
		time.Hour:           "in 1 hour",
		25 * time.Hour:      "in 1 day",
		17 * 24 * time.Hour: "in 17 days",
		-1 * time.Second:    "any moment now",
	}
	for d, want := range tests {
		if got := humanUntil(d); got != want {
			t.Errorf("humanUntil(%s) = %q, want %q", d, got, want)
		}
	}
}
