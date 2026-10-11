package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/forge/github"
)

func TestAgentPushRulesetInspection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rules)
		app    int64
		want   Status
		detail string
	}{
		{"required rules present", nil, appID, Warn, "main: required pull request and approving review observed"},
		{"missing policy", func(r *rules) { r.byBranch["main"] = nil }, appID, Warn, "main: missing required pull request"},
		{"integration bypass", func(r *rules) {
			r.byBranch["develop"] = []github.BranchRule{rule("pull_request", 5, `{"required_approving_review_count":1}`)}
			r.info[5] = github.RulesetInfo{BypassKnown: true, Bypass: []github.BypassActor{{ActorID: appID, ActorType: "Integration"}}}
		}, appID, Warn, "develop: agent App bypasses ruleset 5"},
		{"other App bypass does not bypass agent policy", func(r *rules) {
			r.info[4] = github.RulesetInfo{BypassKnown: true, Bypass: []github.BypassActor{{ActorID: appID + 1, ActorType: "Integration"}}}
		}, appID, Warn, "main: required pull request and approving review observed"},
		{"ruleset read fails", func(r *rules) { r.infoErr = errors.New("offline") }, appID, NotVerified, "main: bypass actors unavailable"},
		{"review field absent", func(r *rules) { r.byBranch["main"] = []github.BranchRule{rule("pull_request", 4, `{}`)} }, appID, NotVerified, "main: approving review parameters unavailable"},
		{"review missing", func(r *rules) {
			r.byBranch["main"] = []github.BranchRule{rule("pull_request", 4, `{"required_approving_review_count":0}`)}
		}, appID, Warn, "main: missing approving review"},
		{"malformed review", func(r *rules) {
			r.byBranch["main"] = []github.BranchRule{rule("pull_request", 4, `{"required_approving_review_count":"one"}`)}
		}, appID, NotVerified, "main: approving review parameters unavailable"},
		{"agent bypass", func(r *rules) {
			r.info[4] = github.RulesetInfo{BypassKnown: true, Bypass: []github.BypassActor{{ActorID: appID, ActorType: "Integration"}}}
		}, appID, Warn, "main: agent App bypasses ruleset 4"},
		{"bypass visibility incomplete", func(r *rules) { r.info[4] = github.RulesetInfo{} }, appID, NotVerified, "main: bypass actors unavailable"},
		{"role membership unresolved", func(r *rules) {
			r.info[4] = github.RulesetInfo{BypassKnown: true, Bypass: []github.BypassActor{{ActorID: 5, ActorType: "RepositoryRole"}}}
		}, appID, NotVerified, "main: agent membership in bypass actor RepositoryRole is unverified"},
		{"agent identity unavailable", nil, 0, NotVerified, "agent App identity unavailable"},
		{"forge unreachable", func(r *rules) { r.branchErr = map[string]error{"main": errors.New("offline")} }, appID, NotVerified, "main: rules unavailable"},
		{"rules visibility incomplete", func(r *rules) { r.unreadable = true }, appID, NotVerified, "main: rules unavailable"},
		{"integration missing", func(r *rules) { r.byBranch["develop"] = nil }, appID, Warn, "develop: missing required pull request"},
		{"default unavailable", func(r *rules) { r.def = "" }, appID, NotVerified, "default branch unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rules{def: "main", byBranch: map[string][]github.BranchRule{"main": {rule("pull_request", 4, `{"required_approving_review_count":1}`)}, "develop": {rule("pull_request", 4, `{"required_approving_review_count":1}`)}}, info: map[int64]github.RulesetInfo{4: {BypassKnown: true}}}
			if tc.change != nil {
				tc.change(r)
			}
			got := InspectAgentPush(context.Background(), r, "owner/repo", "develop", tc.app)
			if got.Status != tc.want || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("got %+v, want %s containing %q", got, tc.want, tc.detail)
			}
			for _, boundary := range []string{"human review identity not_verified", "outside-topic branch restrictions not_verified", "tag restrictions not_verified", "release/deployment denial not_verified", "does not waive R-30 or authorize a run"} {
				if !strings.Contains(got.Detail, boundary) {
					t.Errorf("missing boundary %q: %s", boundary, got.Detail)
				}
			}
			if got.Fix != "https://github.com/owner/repo/settings/rules" {
				t.Errorf("fix = %q", got.Fix)
			}
			if Failed([]Result{got}) {
				t.Fatal("diagnostic refused")
			}
			if tc.name == "required rules present" && strings.Join(r.askedFor, ",") != "owner/repo:main,owner/repo:develop" {
				t.Fatalf("branches inspected: %v", r.askedFor)
			}
		})
	}
}

func TestAgentPushInspectionWithoutSourceIsNotVerified(t *testing.T) {
	got := InspectAgentPush(context.Background(), nil, "owner/repo", "", appID)
	if got.Status != NotVerified || !strings.Contains(got.Detail, "forge inspector unavailable") {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentPushInspectsDefaultOnceWhenIntegrationMatches(t *testing.T) {
	r := &rules{def: "main"}
	got := InspectAgentPush(context.Background(), r, "owner/repo", "main", appID)
	if got.Status != Warn || len(r.askedFor) != 1 {
		t.Fatalf("got %+v, read %v", got, r.askedFor)
	}
}

type unavailableDefaultRules struct {
	*rules
	name string
	err  error
}

func (r unavailableDefaultRules) DefaultBranchName(context.Context, string) (string, error) {
	return r.name, r.err
}

func TestAgentPushInspectsIntegrationWhenDefaultUnavailable(t *testing.T) {
	for _, lookup := range []struct {
		name string
		def  string
		err  error
	}{
		{"empty default", "", nil},
		{"lookup error", "", errors.New("offline")},
		{"name with lookup error", "stale-main", errors.New("offline")},
	} {
		for _, policy := range []struct {
			name   string
			review int
			bypass []github.BypassActor
			detail string
		}{
			{"missing review", 0, nil, "develop: missing approving review"},
			{"agent bypass", 1, []github.BypassActor{{ActorID: appID, ActorType: "Integration"}}, "develop: agent App bypasses ruleset 4"},
		} {
			t.Run(lookup.name+"/"+policy.name, func(t *testing.T) {
				r := &rules{byBranch: map[string][]github.BranchRule{"develop": {rule("pull_request", 4, fmt.Sprintf(`{"required_approving_review_count":%d}`, policy.review))}}, info: map[int64]github.RulesetInfo{4: {BypassKnown: true, Bypass: policy.bypass}}}
				src := unavailableDefaultRules{rules: r, name: lookup.def, err: lookup.err}
				got := InspectAgentPush(context.Background(), src, "owner/repo", "develop", appID)
				if strings.Join(r.askedFor, ",") != "owner/repo:develop" {
					t.Errorf("integration was not independently inspected: %v", r.askedFor)
				}
				if got.Status != NotVerified || !strings.Contains(got.Detail, "default branch unavailable") || !strings.Contains(got.Detail, policy.detail) {
					t.Fatalf("got %+v; want unavailable default and %q", got, policy.detail)
				}
			})
		}
	}
}
