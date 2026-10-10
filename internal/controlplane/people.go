// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/store"
)

// handleListUsers serves the people this deployment governs.
//
// # What is not in this response
//
// Spend. Not per user, not for the period, not as a percentage of a quota.
//
// The screens document asks for a user list with current-period spend and quota
// usage in it, and this endpoint deliberately does not serve those. A list of
// named colleagues with a money column is a leaderboard however it is captioned,
// and it becomes a sortable one the moment a table renders it - which is the one
// thing this product has committed not to build. The commitment is in
// CLAUDE.md, in the README's principles, and in the store: no query in this
// codebase groups spend by named user except the one the policy engine uses to
// enforce a budget, and its result never leaves the process.
//
// What an administrator actually needs is here in full: who is onboarded, who
// holds which role, which tools each person has been seen using, and - through
// the policy endpoint - who is at a limit and what that limit is. Everything
// required to unblock a colleague; nothing required to rank them.
func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.db.ListUsers(r.Context())
	if err != nil {
		a.fail(w, r, "listing users", err)
		return
	}

	// Budget state, not budget spend. "Blocked" is an operational fact an
	// administrator has to act on; "$412 of $400" is a comparison.
	states := map[string]string{}
	for _, s := range a.policy.Status() {
		if s.Budget.Scope == policy.ScopeUser && s.State != policy.StateOK {
			states[s.Budget.ScopeID] = string(s.State)
		}
	}

	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		teams := make([]map[string]string, 0, len(u.Teams))
		for _, t := range u.Teams {
			teams = append(teams, map[string]string{
				"id": t.ID, "slug": t.Slug, "name": t.Name, "role": t.Role,
			})
		}

		row := map[string]any{
			"id":           u.ID,
			"email":        u.Email,
			"display_name": u.DisplayName,
			"role":         u.Role,
			// Whether the group mapping still decides this person's role, or a
			// person did. Shown because "why is this admin still an admin after
			// I took them out of the group" has no answer without it.
			"role_source": u.RoleSource,
			"status":      u.Status,
			"created_at":  u.CreatedAt.UTC().Format(time.RFC3339),
			"teams":       teams,
			"tools":       u.Tools,
			// The onboarding funnel, which is what an administrator chases:
			// an account exists, a token was issued, a first request arrived.
			"tokens":     u.Tokens,
			"onboarded":  u.Onboarded(),
			"sso_linked": u.ExternalID != "",
		}
		addTime(row, "first_request_at", u.FirstRequestAt)
		addTime(row, "last_request_at", u.LastRequestAt)
		addTime(row, "last_token_used_at", u.LastTokenUsedAt)
		if state, ok := states[u.ID]; ok {
			row["budget_state"] = state
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"users": out,
		// Said in the payload so a console cannot quietly add a column it has
		// no data for and fill it with zeroes.
		"note": "This endpoint carries no per-user spend. Panorama measures work, not workers; developers see their own usage and leaders see team aggregates.",
	})
}

type updateUserRequest struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	// RoleSource "sso" hands the account back to the group mapping: the role
	// does not change now - nothing outside a sign-in can recompute it, because
	// group membership is the provider's record and Panorama keeps no copy -
	// but the next sign-in is allowed to decide again.
	RoleSource string `json:"role_source"`
}

// handleUpdateUser changes a person's role or status.
func (a *API) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	userID := r.PathValue("id")

	var req updateUserRequest
	if !decodeBody(w, r, &req) {
		return
	}

	target, err := a.db.UserByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such user.")
			return
		}
		a.fail(w, r, "reading a user", err)
		return
	}

	// Demoting or disabling yourself is how a deployment ends up with no
	// administrator and a database edit as the only way back.
	if target.ID == p.UserID {
		if req.Role != "" && req.Role != store.RoleAdmin {
			writeAPIError(w, r, http.StatusBadRequest,
				"You cannot remove your own administrator role. Ask another administrator.")
			return
		}
		if req.Status == "disabled" {
			writeAPIError(w, r, http.StatusBadRequest, "You cannot disable your own account.")
			return
		}
	}

	// ASSIGNING A ROLE IS WHAT PINS IT, so this runs on the value rather than on
	// a change to it. The common case is an account the group mapping already
	// made an administrator, which somebody wants to *keep* an administrator
	// when their groups change - and reading that as "no change" because the
	// role already matches is a pin that silently does nothing.
	if req.Role != "" {
		if !store.ValidRole(req.Role) {
			writeAPIError(w, r, http.StatusBadRequest, "Role must be admin, manager, or developer.")
			return
		}
		// Administrator is the only role assigned by hand, and this is the only
		// door it can be assigned through. Every other role is the group
		// mapping's to decide, because a role pinned here is a role the
		// provider can no longer revoke - a reasonable price for the account
		// that owns the deployment and a silent divergence for anybody else.
		// Demotion is therefore an unpin rather than a role: hand the account
		// back, and the next sign-in decides.
		if req.Role != store.RoleAdmin {
			writeAPIError(w, r, http.StatusBadRequest,
				"Only the administrator role is assigned directly. Every other role comes from the group mapping - change the mapping, or hand this account back to it.")
			return
		}
		if lerr := a.refuseLastAdminChange(r, target, req.Role, target.Status); lerr != nil {
			writeAPIError(w, r, http.StatusBadRequest, lerr.Error())
			return
		}
		if req.Role != target.Role || target.RoleSource != store.RoleSourceManual {
			if serr := a.db.SetUserRole(r.Context(), target.ID, req.Role); serr != nil {
				a.fail(w, r, "assigning a role", serr)
				return
			}
			a.log.InfoContext(r.Context(), "role assigned directly",
				"user_id", target.ID, "from", target.Role, "to", req.Role, "actor", p.UserID)
		}
	}

	// After the role, so one request may both set a role and leave the mapping
	// in charge of it from the next sign-in. Ordered the other way, setting a
	// role would immediately re-pin it.
	if req.RoleSource != "" && req.RoleSource != target.RoleSource {
		if req.RoleSource != store.RoleSourceSSO {
			writeAPIError(w, r, http.StatusBadRequest,
				`Only "sso" can be set: a role is pinned by assigning it, not by asking for it.`)
			return
		}
		if lerr := a.refuseStrandingAdmins(r, target); lerr != nil {
			writeAPIError(w, r, http.StatusBadRequest, lerr.Error())
			return
		}
		if serr := a.db.FollowGroupMapping(r.Context(), target.ID); serr != nil {
			a.fail(w, r, "handing a role back to the group mapping", serr)
			return
		}
		a.log.InfoContext(r.Context(), "role handed back to the group mapping",
			"user_id", target.ID, "role", target.Role, "actor", p.UserID)
	}

	if req.Status != "" && req.Status != target.Status {
		if req.Status != "active" && req.Status != "disabled" {
			writeAPIError(w, r, http.StatusBadRequest, `Status must be "active" or "disabled".`)
			return
		}
		if lerr := a.refuseLastAdminChange(r, target, target.Role, req.Status); lerr != nil {
			writeAPIError(w, r, http.StatusBadRequest, lerr.Error())
			return
		}
		if serr := a.db.SetUserStatus(r.Context(), target.ID, req.Status); serr != nil {
			a.fail(w, r, "changing a status", serr)
			return
		}
		a.log.InfoContext(r.Context(), "account status changed",
			"user_id", target.ID, "from", target.Status, "to", req.Status, "actor", p.UserID)
	}

	updated, err := a.db.UserByID(r.Context(), target.ID)
	if err != nil {
		a.fail(w, r, "reading a user", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": updated.ID, "email": updated.Email, "display_name": updated.DisplayName,
		"role": updated.Role, "status": updated.Status,
	})
}

// refuseLastAdminChange stops a deployment from losing its last administrator.
//
// A deployment with no administrator is recoverable - `panorama admin setup`
// opens a session that can fix the group mapping - but recovering is a trip to
// a shell, and this is one query.
func (a *API) refuseLastAdminChange(r *http.Request, target *store.User, role, status string) error {
	if target.Role != store.RoleAdmin {
		return nil
	}
	if role == store.RoleAdmin && status == "active" {
		return nil
	}

	users, err := a.db.ListUsers(r.Context())
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != target.ID && u.Role == store.RoleAdmin && u.Status == "active" {
			return nil
		}
	}
	return errors.New("this is the only active administrator; promote someone else first")
}

// refuseStrandingAdmins refuses handing back the last administrator the
// mapping cannot re-grant.
//
// Unpinning is not a role change - the account is still an administrator when
// the request returns - so refuseLastAdminChange never sees it. What it is is a
// *delayed* demotion: the group mapping decides at the next sign-in, and if
// nothing in that mapping can grant admin, the deployment discovers it has no
// administrator at whatever hour that person happens to sign in next, with a
// database edit as the only way back. The failure and the action that caused it
// are days apart, which is exactly the kind nobody connects.
//
// It asks whether the mapping *could* grant admin, not whether it will: group
// membership is the provider's record and Panorama keeps no copy, so "will this
// person still be an admin tomorrow" is not a question this side of the wire
// can answer. A group mapped to admin, or a default role of admin, is enough to
// make the hand-back recoverable.
func (a *API) refuseStrandingAdmins(r *http.Request, target *store.User) error {
	if target.Role != store.RoleAdmin || target.RoleSource != store.RoleSourceManual {
		return nil
	}

	mapping, err := a.db.GroupRoles(r.Context())
	if err != nil {
		return err
	}
	for _, m := range mapping {
		if m.Role == store.RoleAdmin {
			return nil
		}
	}
	switch v, serr := a.db.Setting(r.Context(), settingDefaultRole); {
	case serr == nil && v == store.RoleAdmin:
		return nil
	case serr != nil && !errors.Is(serr, store.ErrNotFound):
		return serr
	}

	users, err := a.db.ListUsers(r.Context())
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != target.ID && u.Role == store.RoleAdmin && u.Status == "active" &&
			u.RoleSource == store.RoleSourceManual {
			return nil
		}
	}
	return errors.New(
		"no group is mapped to admin, so this is the only administrator this deployment can have - map a group to admin first, or make another account an administrator here")
}

// handleListTeams serves the teams. Everyone signed in may see them: a team is
// an aggregate, and knowing which teams exist is how a developer understands
// which budget governs them.
func (a *API) handleListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := a.db.ListTeams(r.Context())
	if err != nil {
		a.fail(w, r, "listing teams", err)
		return
	}

	out := make([]map[string]any, 0, len(teams))
	for _, t := range teams {
		members, cerr := a.db.TeamMemberCount(r.Context(), t.ID)
		if cerr != nil {
			a.fail(w, r, "counting team members", cerr)
			return
		}
		out = append(out, map[string]any{
			"id": t.ID, "slug": t.Slug, "name": t.Name,
			"description": t.Description, "members": members,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": out})
}

// handleTeam serves one team and who is on it.
func (a *API) handleTeam(w http.ResponseWriter, r *http.Request) {
	detail, err := a.db.TeamWithMembers(r.Context(), r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such team.")
			return
		}
		a.fail(w, r, "reading a team", err)
		return
	}

	members := make([]map[string]any, 0, len(detail.Members))
	for _, m := range detail.Members {
		members = append(members, map[string]any{
			"user_id": m.UserID, "email": m.Email, "display_name": m.DisplayName,
			"role": m.Role, "status": m.Status,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": detail.ID, "slug": detail.Slug, "name": detail.Name,
		"description": detail.Description, "members": members,
	})
}

type createTeamRequest struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// handleCreateTeam makes a team.
func (a *API) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	var req createTeamRequest
	if !decodeBody(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPIError(w, r, http.StatusBadRequest, "A team needs a name.")
		return
	}

	team, err := a.db.CreateTeam(r.Context(), name, strings.TrimSpace(req.Slug), req.Description)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		a.fail(w, r, "creating a team", err)
		return
	}

	a.log.InfoContext(r.Context(), "team created",
		"team_id", team.ID, "slug", team.Slug, "actor", PrincipalFrom(r.Context()).UserID)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id": team.ID, "slug": team.Slug, "name": team.Name, "description": team.Description,
	})
}

type teamMemberRequest struct {
	// Email rather than a user id, because it is what an administrator has in
	// front of them and what the policy documents already use.
	Email string `json:"email"`
	Role  string `json:"role"`
	// Remove turns this into a removal, so membership is one endpoint rather
	// than a POST and a DELETE with a user id in the path that a browser form
	// cannot easily produce.
	Remove bool `json:"remove"`

	// Members, when present, is the whole roster: the team ends up with exactly
	// these people and nobody else. A pointer rather than a slice because an
	// empty list is a real instruction - "this team has nobody on it" - and has
	// to be tellable apart from a request that is using the single-person form
	// above.
	//
	// The console sends this one. It edits a roster as a list of checkboxes,
	// and translating that into a stream of add and remove calls would leave a
	// half-applied roster on the first failure, with nothing on the screen
	// saying which half landed.
	Members *[]rosterMember `json:"members"`
}

type rosterMember struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// handleTeamMembers adds or removes one person, or replaces the whole roster.
func (a *API) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	var req teamMemberRequest
	if !decodeBody(w, r, &req) {
		return
	}

	teamID, err := a.db.TeamIDBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such team.")
			return
		}
		a.fail(w, r, "reading a team", err)
		return
	}

	if req.Members != nil {
		a.setTeamRoster(w, r, teamID, *req.Members)
		return
	}

	userID, err := a.db.UserIDByEmail(r.Context(), store.NormalizeEmail(req.Email))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not created here on purpose. Accounts arrive by signing in, or by
			// an administrator minting a token; conjuring one from a team form
			// would create a person nobody can authenticate as.
			writeAPIError(w, r, http.StatusNotFound,
				"Nobody with that email address has a Panorama account yet. They appear once they sign in.")
			return
		}
		a.fail(w, r, "reading a user", err)
		return
	}

	if req.Remove {
		if rerr := a.db.RemoveTeamMember(r.Context(), teamID, userID); rerr != nil {
			a.fail(w, r, "removing a team member", rerr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": true})
		return
	}

	role := req.Role
	if role == "" {
		role = "member"
	}
	if role != "member" && role != "lead" {
		writeAPIError(w, r, http.StatusBadRequest, `Team role must be "member" or "lead".`)
		return
	}

	if aerr := a.db.AddTeamMember(r.Context(), teamID, userID, role); aerr != nil {
		a.fail(w, r, "adding a team member", aerr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": true, "role": role})
}

// setTeamRoster makes a team's membership exactly the list it was given.
//
// Every id is checked against the accounts that exist before anything is
// written. A membership row referencing nobody would fail on the foreign key
// and arrive as a 500, which reads as "Panorama is broken" rather than "that
// person has left".
func (a *API) setTeamRoster(w http.ResponseWriter, r *http.Request, teamID string, roster []rosterMember) {
	users, err := a.db.ListUsers(r.Context())
	if err != nil {
		a.fail(w, r, "reading people", err)
		return
	}
	known := make(map[string]bool, len(users))
	for _, u := range users {
		known[u.ID] = true
	}

	members := make([]store.TeamMemberInput, 0, len(roster))
	for _, m := range roster {
		if !known[m.UserID] {
			writeAPIError(w, r, http.StatusBadRequest,
				"One of those people is no longer in this deployment. Reload the screen and try again.")
			return
		}
		members = append(members, store.TeamMemberInput{UserID: m.UserID, Role: m.Role})
	}

	if err := a.db.SetTeamMembers(r.Context(), teamID, members); err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		a.fail(w, r, "saving a team roster", err)
		return
	}

	a.log.InfoContext(r.Context(), "team roster set",
		"team_id", teamID, "members", len(members), "actor", PrincipalFrom(r.Context()).UserID)

	writeJSON(w, http.StatusOK, map[string]any{"members": len(members), "saved": true})
}

// handleDeleteTeam removes a team.
//
// Refused while a pipeline still bills it. `service_tokens.team_id` cascades,
// so deleting the team would revoke those credentials - and a build that starts
// failing at 3am because somebody tidied up a team list at 5pm is the kind of
// outage nobody traces back. Revoking the token is one click on this same
// screen, and doing it deliberately is the point.
func (a *API) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	team, err := a.db.TeamBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such team.")
			return
		}
		a.fail(w, r, "reading a team", err)
		return
	}

	tokens, err := a.db.TeamServiceTokens(r.Context(), team.ID)
	if err != nil {
		a.fail(w, r, "counting service tokens", err)
		return
	}
	if tokens > 0 {
		writeAPIError(w, r, http.StatusConflict, plural(tokens,
			"A service token still bills this team. Revoke it first - deleting the team would revoke it silently, and the pipeline holding it would fail at whatever hour it next runs.",
			"%d service tokens still bill this team. Revoke them first - deleting the team would revoke them silently, and the pipelines holding them would fail at whatever hour they next run."))
		return
	}

	removed, err := a.db.DeleteTeam(r.Context(), team.ID)
	if err != nil {
		a.fail(w, r, "deleting a team", err)
		return
	}
	if !removed {
		writeAPIError(w, r, http.StatusNotFound, "No such team.")
		return
	}

	a.log.InfoContext(r.Context(), "team deleted",
		"team_id", team.ID, "slug", team.Slug, "actor", PrincipalFrom(r.Context()).UserID)

	writeJSON(w, http.StatusOK, map[string]any{"slug": team.Slug, "removed": true})
}

// plural picks the singular or the "%d" form. One sentence per case rather than
// a count spliced into a sentence written for the other one.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}
