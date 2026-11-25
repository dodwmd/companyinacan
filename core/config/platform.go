package config

// ApprovalPolicy is the company's policy for one approval type (company.yaml `approvals:`).
// Who approves is the operator's decision; modules only declare the types they use and a default
// chain (module.v1 `approvals`).
type ApprovalPolicy struct {
	// Steps run in order; a step with Parallel set starts together with the step before it.
	Steps []ApprovalStep `yaml:"steps" json:"steps"`
	// ExpiresAfter, e.g. "72h" or "7d": a pending request expires (and is reported as such).
	ExpiresAfter string `yaml:"expires_after,omitempty" json:"expires_after,omitempty"`
	// EscalateAfter: once, the EscalateTo approvers are added to the active steps.
	EscalateAfter string     `yaml:"escalate_after,omitempty" json:"escalate_after,omitempty"`
	EscalateTo    []Approver `yaml:"escalate_to,omitempty" json:"escalate_to,omitempty"`
}

// ApprovalStep is one step of a chain.
type ApprovalStep struct {
	Name      string     `yaml:"name" json:"name"`
	Approvers []Approver `yaml:"approvers" json:"approvers"`
	// Quorum is how many distinct approvers must approve (default 1).
	Quorum   int  `yaml:"quorum,omitempty" json:"quorum,omitempty"`
	Parallel bool `yaml:"parallel,omitempty" json:"parallel,omitempty"`
	// When limits the step to requests whose amount matches; other requests skip it.
	When *StepCondition `yaml:"when,omitempty" json:"when,omitempty"`
	// Fallback is used when no approver can be resolved (e.g. the requester has no manager).
	Fallback []Approver `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// Approver names who may approve: exactly one of the fields is set.
type Approver struct {
	Role               string `yaml:"role,omitempty" json:"role,omitempty"`
	User               string `yaml:"user,omitempty" json:"user,omitempty"` // a subject, e.g. user:alice
	ManagerOfRequester bool   `yaml:"manager_of_requester,omitempty" json:"manager_of_requester,omitempty"`
}

// StepCondition is an amount range in the request's minor units (cents).
type StepCondition struct {
	AmountGTE *int64 `yaml:"amount_gte,omitempty" json:"amount_gte,omitempty"`
	AmountLT  *int64 `yaml:"amount_lt,omitempty" json:"amount_lt,omitempty"`
}

// NotificationConfig is company.yaml `notifications:`.
type NotificationConfig struct {
	// RateLimitPerHour caps external (email, webhook) messages per recipient per hour; 0 = default 30.
	RateLimitPerHour int `yaml:"rate_limit_per_hour,omitempty"`
	// Webhooks are outbound channels by name (Slack-style incoming webhooks, https only).
	Webhooks map[string]string `yaml:"webhooks,omitempty"`
}

// ManifestApproval is an approval type a module declares (module.v1 `approvals`).
type ManifestApproval struct {
	Type         string   `yaml:"type"`
	Schema       string   `yaml:"schema"`
	DefaultChain []string `yaml:"default_chain"`
}

// NotifyRule is a notification subscription rule of a module (module.v1 `notifications`): when
// the event is published, notify the recipients.
type NotifyRule struct {
	Event string `yaml:"event" json:"event"`
	// Type is the notification type (default: the event name without the version).
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Recipients: role:<name>, user:<subject>, field:<key of event data holding a subject>,
	// channel:<outbound webhook channel name>.
	Recipients []string `yaml:"recipients" json:"recipients"`
	Urgent     bool     `yaml:"urgent,omitempty" json:"urgent,omitempty"`
}
