package drivers

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type IAMPolicy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}

type Statement struct {
	Effect    string                       `json:"Effect"`
	Action    StringOrSlice                `json:"Action"`
	Resource  StringOrSlice                `json:"Resource"`
	Condition map[string]map[string]string `json:"Condition,omitempty"`
}

type StringOrSlice []string

func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = []string{str}
		return nil
	}
	var slice []string
	if err := json.Unmarshal(data, &slice); err != nil {
		return err
	}
	*s = slice
	return nil
}

type STSToken struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expiration   time.Time
}

type PolicyEvaluator struct {
	policies []IAMPolicy
}

func NewPolicyEvaluator() *PolicyEvaluator {
	return &PolicyEvaluator{}
}

func (p *PolicyEvaluator) AddPolicy(policy IAMPolicy) {
	p.policies = append(p.policies, policy)
}

func (p *PolicyEvaluator) IsAllowed(action, resource string, conditions map[string]string) bool {
	for _, policy := range p.policies {
		for _, stmt := range policy.Statement {
			if p.matchesStatement(stmt, action, resource, conditions) {
				return stmt.Effect == "Allow"
			}
		}
	}
	return false
}

func (p *PolicyEvaluator) matchesStatement(stmt Statement, action, resource string, conditions map[string]string) bool {
	// Check action
	actionMatch := false
	for _, a := range stmt.Action {
		if a == "*" || a == action || matchWildcard(a, action) {
			actionMatch = true
			break
		}
	}
	if !actionMatch {
		return false
	}

	// Check resource
	resourceMatch := false
	for _, r := range stmt.Resource {
		if r == "*" || r == resource || matchWildcard(r, resource) {
			resourceMatch = true
			break
		}
	}
	if !resourceMatch {
		return false
	}

	// Check conditions
	for condType, condValues := range stmt.Condition {
		for condKey, condValue := range condValues {
			if val, ok := conditions[condKey]; ok {
				if !evaluateCondition(condType, val, condValue) {
					return false
				}
			}
		}
	}

	return true
}

func matchWildcard(pattern, str string) bool {
	return strings.HasSuffix(pattern, "*") && strings.HasPrefix(str, strings.TrimSuffix(pattern, "*"))
}

func evaluateCondition(condType, value, expected string) bool {
	switch condType {
	case "StringEquals":
		return value == expected
	case "StringLike":
		return matchWildcard(expected, value)
	default:
		return false
	}
}

// GenerateTenantPolicy creates a policy restricting access to a tenant's prefix
func GenerateTenantPolicy(bucket, tenantPrefix string) IAMPolicy {
	return IAMPolicy{
		Version: "2012-10-17",
		Statement: []Statement{
			{
				Effect:   "Allow",
				Action:   []string{"s3:PutObject", "s3:GetObject", "s3:DeleteObject"},
				Resource: []string{fmt.Sprintf("arn:aws:s3:::%s/%s/*", bucket, tenantPrefix)},
			},
			{
				Effect:   "Allow",
				Action:   []string{"s3:ListBucket"},
				Resource: []string{fmt.Sprintf("arn:aws:s3:::%s", bucket)},
				Condition: map[string]map[string]string{
					"StringLike": {"s3:prefix": fmt.Sprintf("%s/*", tenantPrefix)},
				},
			},
		},
	}
}
