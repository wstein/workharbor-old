package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/wstein/workharbor/internal/forge/github"
)

// AgentPushRuleSource is the read-only part of the GitHub adapter needed for
// agent-push diagnostics. It has no operation that changes forge settings.
type AgentPushRuleSource interface {
	DefaultBranchName(context.Context, string) (string, error)
	BranchRules(context.Context, string, string) ([]github.BranchRule, error)
	Ruleset(context.Context, string, int64) (github.RulesetInfo, error)
}

// InspectAgentPush inspects the default and configured integration branches for
// a repository whose agent topic push is enabled. appID is the agent's GitHub
// App identity, not the supervisor's identity unless they are the same App.
// It returns diagnostic evidence only: callers must not use it for admission.
// Rulesets cannot prove human reviewer identity or all mandatory R-30 denials;
// those gaps keep even an observed branch policy from returning OK.
func InspectAgentPush(ctx context.Context, src AgentPushRuleSource, repo, integration string, appID int64) Result {
	result := Result{Check: "agent-push-ruleset", Status: Warn}
	settings := url.URL{Scheme: "https", Host: "github.com", Path: "/" + repo + "/settings/rules"}
	result.Fix = settings.String()
	var evidence []string
	note := func(st Status, detail string) {
		if st == NotVerified {
			result.Status = NotVerified
		}
		evidence = append(evidence, detail)
	}
	if appID <= 0 {
		note(NotVerified, "agent App identity unavailable")
	}
	if src == nil {
		note(NotVerified, "forge inspector unavailable")
	} else {
		var branches []string
		def, err := src.DefaultBranchName(ctx, repo)
		if err != nil || def == "" {
			note(NotVerified, "default branch unavailable")
		} else {
			branches = append(branches, def)
		}
		if integration != "" && (len(branches) == 0 || integration != def) {
			branches = append(branches, integration)
		}
		for _, branch := range branches {
			st, detail := inspectAgentPushBranch(ctx, src, repo, branch, appID)
			note(st, oneLine(branch)+": "+detail)
		}
	}
	evidence = append(evidence,
		"human review identity not_verified",
		"outside-topic branch restrictions not_verified",
		"tag restrictions not_verified",
		"release/deployment denial not_verified",
		"diagnostic only: does not waive R-30 or authorize a run")
	result.Detail = strings.Join(evidence, "; ")
	return result
}

func inspectAgentPushBranch(ctx context.Context, src AgentPushRuleSource, repo, branch string, appID int64) (Status, string) {
	rules, err := src.BranchRules(ctx, repo, branch)
	if err != nil {
		return NotVerified, "rules unavailable"
	}
	var observed, unknown, bypass []string
	found := false
	for _, rule := range rules {
		if rule.Type != "pull_request" {
			continue
		}
		found = true
		var params struct {
			Reviews *int `json:"required_approving_review_count"`
		}
		if err := json.Unmarshal(rule.Parameters, &params); err != nil || params.Reviews == nil || *params.Reviews < 0 {
			unknown = append(unknown, "approving review parameters unavailable")
			continue
		}
		if *params.Reviews == 0 {
			continue
		}
		if rule.RulesetID <= 0 {
			unknown = append(unknown, "ruleset identity unavailable")
			continue
		}
		info, err := src.Ruleset(ctx, repo, rule.RulesetID)
		if err != nil || !info.BypassKnown {
			unknown = append(unknown, "bypass actors unavailable")
			continue
		}
		noBypass := appID > 0
		for _, actor := range info.Bypass {
			if actor.ActorType == "Integration" {
				if actor.ActorID == appID {
					bypass = append(bypass, fmt.Sprintf("agent App bypasses ruleset %d", rule.RulesetID))
					noBypass = false
				}
			} else {
				unknown = append(unknown, "agent membership in bypass actor "+oneLine(actor.ActorType)+" is unverified")
				noBypass = false
			}
		}
		if noBypass {
			observed = append(observed, "required pull request and approving review observed; agent App has no bypass in that ruleset")
		}
	}
	if len(observed) > 0 {
		// One applicable non-bypassable rule is enough to observe the branch
		// requirement, even if another applicable ruleset allows a bypass.
		return Warn, observed[0]
	}
	if len(unknown) > 0 {
		return NotVerified, strings.Join(append(bypass, unknown...), ", ")
	}
	if len(bypass) > 0 {
		return Warn, strings.Join(bypass, ", ")
	}
	if !found {
		return Warn, "missing required pull request"
	}
	if appID <= 0 {
		return NotVerified, "agent App identity unavailable"
	}
	return Warn, "missing approving review"
}
