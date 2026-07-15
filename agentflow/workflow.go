package agentflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

const TaskBriefSchemaVersion = "0.1.0"

// TaskBrief is Golem's smallest typed projection of Agentflow's closed task
// brief schema. Pointer fields preserve whether optional signals were absent.
type TaskBrief struct {
	SchemaVersion     string    `json:"schema_version"`
	TaskType          string    `json:"task_type"`
	DeclaredRisk      string    `json:"declared_risk"`
	SecuritySensitive *bool     `json:"security_sensitive,omitempty"`
	CandidateFiles    *[]string `json:"candidate_files,omitempty"`
	BlastRadius       *string   `json:"blast_radius,omitempty"`
	ValidationNeeds   *[]string `json:"validation_needs,omitempty"`
	DeclaredSize      *string   `json:"declared_size,omitempty"`
}

// TaskBriefFromPlan projects only facts already present in a compiled plan.
// Routing remains Agentflow-owned: this function does not infer task type,
// security sensitivity, blast radius, or size.
func TaskBriefFromPlan(plan Plan, taskType string) TaskBrief {
	brief := TaskBrief{
		SchemaVersion: TaskBriefSchemaVersion,
		TaskType:      taskType,
		DeclaredRisk:  plan.RiskLevel,
	}
	seen := make(map[string]struct{})
	files := make([]string, 0)
	for _, step := range plan.Steps {
		for _, file := range step.Files {
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			files = append(files, file)
		}
	}
	if len(files) > 0 {
		brief.CandidateFiles = &files
	}
	if len(plan.ValidationGates) > 0 {
		needs := append([]string(nil), plan.ValidationGates...)
		brief.ValidationNeeds = &needs
	}
	return brief
}

type WorkflowSelection struct {
	Pack    string `json:"pack"`
	Profile string `json:"profile"`
}

type WorkflowAlternative struct {
	Profile  string `json:"profile"`
	Relation string `json:"relation"`
	Reason   string `json:"reason"`
}

type WorkflowOverride struct {
	FromProfile string `json:"from_profile"`
	ToProfile   string `json:"to_profile"`
	Reason      string `json:"reason"`
}

type WorkflowCapability struct {
	ID       string `json:"id"`
	Required bool   `json:"required"`
}

type WorkflowValidationPolicy struct {
	RequiredGates []string `json:"required_gates"`
}

type WorkflowProofPolicy struct {
	HunkAttribution  string `json:"hunk_attribution"`
	RequireReviewRun bool   `json:"require_review_run"`
}

type WorkflowContract struct {
	SchemaVersion        string                   `json:"schema_version"`
	WorkflowPack         string                   `json:"workflow_pack"`
	WorkflowProfile      string                   `json:"workflow_profile"`
	SelectedBy           string                   `json:"selected_by"`
	SelectionReason      string                   `json:"selection_reason"`
	RequiredCapabilities []WorkflowCapability     `json:"required_capabilities"`
	ReviewDepth          string                   `json:"review_depth"`
	ValidationPolicy     WorkflowValidationPolicy `json:"validation_policy"`
	ProofPolicy          WorkflowProofPolicy      `json:"proof_policy"`
}

type WorkflowRecommendation struct {
	SchemaVersion string
	Recommended   WorkflowSelection
	Selected      WorkflowSelection
	Signals       []string
	Rationale     string
	Alternatives  []WorkflowAlternative
	Override      *WorkflowOverride
	Contract      WorkflowContract
	candidateJSON []byte
}

// CandidateJSON returns a defensive copy of Agentflow's exact validated
// workflow_contract_candidate payload.
func (r WorkflowRecommendation) CandidateJSON() []byte {
	return append([]byte(nil), r.candidateJSON...)
}

// MarshalJSON persists the validated Agentflow report in its wire shape. Real
// recommendations retain Agentflow's candidate bytes; the Contract fallback
// keeps programmatically constructed test/client values serializable while the
// validation below still rejects inconsistent reports.
func (r WorkflowRecommendation) MarshalJSON() ([]byte, error) {
	candidate := r.CandidateJSON()
	if len(candidate) == 0 {
		var err error
		candidate, err = json.Marshal(r.Contract)
		if err != nil {
			return nil, fmt.Errorf("marshal workflow contract candidate: %w", err)
		}
	}
	payload := struct {
		SchemaVersion             string                `json:"schema_version"`
		Recommended               WorkflowSelection     `json:"recommended"`
		Selected                  WorkflowSelection     `json:"selected"`
		Signals                   []string              `json:"signals"`
		Rationale                 string                `json:"rationale"`
		Alternatives              []WorkflowAlternative `json:"alternatives"`
		Override                  *WorkflowOverride     `json:"override"`
		WorkflowContractCandidate json.RawMessage       `json:"workflow_contract_candidate"`
	}{
		SchemaVersion: r.SchemaVersion, Recommended: r.Recommended, Selected: r.Selected,
		Signals: append([]string(nil), r.Signals...), Rationale: r.Rationale,
		Alternatives: append([]WorkflowAlternative(nil), r.Alternatives...), Override: r.Override,
		WorkflowContractCandidate: candidate,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal Agentflow workflow recommendation: %w", err)
	}
	if _, err := parseWorkflowRecommendation(b); err != nil {
		return nil, fmt.Errorf("marshal Agentflow workflow recommendation: %w", err)
	}
	return b, nil
}

// UnmarshalJSON re-applies the same fail-closed validation used for live
// recommend-workflow output, so a durable handoff cannot bypass contract checks.
func (r *WorkflowRecommendation) UnmarshalJSON(data []byte) error {
	parsed, err := parseWorkflowRecommendation(data)
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// VerifyMaterializedWorkflowContract binds a persisted recommendation to the
// Agentflow-owned contract already approved and written during planning.
func (r WorkflowRecommendation) VerifyMaterializedWorkflowContract(data []byte) error {
	materialized, err := parseWorkflowContract(data)
	if err != nil {
		return fmt.Errorf("parse materialized Agentflow workflow contract: %w", err)
	}
	if !reflect.DeepEqual(materialized, r.Contract) {
		return fmt.Errorf("materialized Agentflow workflow contract does not match the approved workflow handoff")
	}
	return nil
}

type recommendationProjection struct {
	SchemaVersion             *string                `json:"schema_version"`
	Recommended               *WorkflowSelection     `json:"recommended"`
	Selected                  *WorkflowSelection     `json:"selected"`
	Signals                   *[]string              `json:"signals"`
	Rationale                 *string                `json:"rationale"`
	Alternatives              *[]WorkflowAlternative `json:"alternatives"`
	Override                  json.RawMessage        `json:"override"`
	WorkflowContractCandidate json.RawMessage        `json:"workflow_contract_candidate"`
}

type workflowContractProjection struct {
	SchemaVersion        *string                             `json:"schema_version"`
	WorkflowPack         *string                             `json:"workflow_pack"`
	WorkflowProfile      *string                             `json:"workflow_profile"`
	SelectedBy           *string                             `json:"selected_by"`
	SelectionReason      *string                             `json:"selection_reason"`
	RequiredCapabilities *[]workflowCapabilityProjection     `json:"required_capabilities"`
	ReviewDepth          *string                             `json:"review_depth"`
	ValidationPolicy     *workflowValidationPolicyProjection `json:"validation_policy"`
	ProofPolicy          *workflowProofPolicyProjection      `json:"proof_policy"`
}

type workflowCapabilityProjection struct {
	ID       *string `json:"id"`
	Required *bool   `json:"required"`
}

type workflowValidationPolicyProjection struct {
	RequiredGates *[]string `json:"required_gates"`
}

type workflowProofPolicyProjection struct {
	HunkAttribution  *string `json:"hunk_attribution"`
	RequireReviewRun *bool   `json:"require_review_run"`
}

type workflowOverrideProjection struct {
	FromProfile *string `json:"from_profile"`
	ToProfile   *string `json:"to_profile"`
	Reason      *string `json:"reason"`
}

// RecommendWorkflow asks Agentflow to classify explicit signals. Golem only
// validates and forwards the machine contract; it contains no routing policy.
func (c *Client) RecommendWorkflow(ctx context.Context, brief TaskBrief, selectedProfile, reason string) (WorkflowRecommendation, error) {
	stdin, err := json.Marshal(brief)
	if err != nil {
		return WorkflowRecommendation{}, fmt.Errorf("marshal Agentflow task brief: %w", err)
	}
	args := []string{"recommend-workflow", "--stdin", "--json"}
	if selectedProfile != "" {
		args = append(args, "--selected-profile", selectedProfile)
		if reason != "" {
			args = append(args, "--reason", reason)
		}
	}
	out, err := c.callInput(ctx, "recommend-workflow", args, true, stdin)
	if err != nil {
		return WorkflowRecommendation{}, err
	}
	return parseWorkflowRecommendation(out)
}

func parseWorkflowRecommendation(out []byte) (WorkflowRecommendation, error) {
	var p recommendationProjection
	if err := json.Unmarshal(out, &p); err != nil {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: parse %q: %w", out, err)
	}
	missing := ""
	switch {
	case p.SchemaVersion == nil:
		missing = "schema_version"
	case p.Recommended == nil:
		missing = "recommended"
	case p.Selected == nil:
		missing = "selected"
	case p.Signals == nil:
		missing = "signals"
	case p.Rationale == nil:
		missing = "rationale"
	case p.Alternatives == nil:
		missing = "alternatives"
	case p.Override == nil:
		missing = "override"
	case p.WorkflowContractCandidate == nil:
		missing = "workflow_contract_candidate"
	}
	if missing != "" {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: missing required field %s", missing)
	}
	if *p.SchemaVersion != "0.1.0" {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: incompatible schema_version %q (upgrade Golem/Agentflow together)", *p.SchemaVersion)
	}
	if err := validateSelection("recommended", *p.Recommended); err != nil {
		return WorkflowRecommendation{}, err
	}
	if err := validateSelection("selected", *p.Selected); err != nil {
		return WorkflowRecommendation{}, err
	}
	if len(*p.Signals) == 0 || !allNonBlank(*p.Signals) {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: signals must contain non-empty strings")
	}
	if strings.TrimSpace(*p.Rationale) == "" {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: rationale must be non-empty")
	}
	for i, alternative := range *p.Alternatives {
		if !knownWorkflowProfile(alternative.Profile) ||
			(alternative.Relation != "cheaper" && alternative.Relation != "safer") ||
			strings.TrimSpace(alternative.Reason) == "" {
			return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: invalid alternatives[%d]", i)
		}
	}
	override, err := parseWorkflowOverride(p.Override)
	if err != nil {
		return WorkflowRecommendation{}, err
	}
	changed := *p.Recommended != *p.Selected
	if changed && override == nil {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: selected profile changed without override")
	}
	if !changed && override != nil {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: override present without a selection change")
	}
	if override != nil && (override.FromProfile != p.Recommended.Profile || override.ToProfile != p.Selected.Profile) {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: override does not match recommended/selected profiles")
	}
	contract, err := parseWorkflowContract(p.WorkflowContractCandidate)
	if err != nil {
		return WorkflowRecommendation{}, err
	}
	if contract.WorkflowPack != p.Selected.Pack || contract.WorkflowProfile != p.Selected.Profile {
		return WorkflowRecommendation{}, fmt.Errorf("agentflow recommend-workflow: workflow_contract_candidate workflow_profile does not match selected profile")
	}
	return WorkflowRecommendation{
		SchemaVersion: *p.SchemaVersion, Recommended: *p.Recommended, Selected: *p.Selected,
		Signals: append([]string(nil), (*p.Signals)...), Rationale: *p.Rationale,
		Alternatives: append([]WorkflowAlternative(nil), (*p.Alternatives)...), Override: override,
		Contract: contract, candidateJSON: append([]byte(nil), p.WorkflowContractCandidate...),
	}, nil
}

func validateSelection(field string, selection WorkflowSelection) error {
	if selection.Pack != "agentflow-default" {
		return fmt.Errorf("agentflow recommend-workflow: %s.pack %q is unknown", field, selection.Pack)
	}
	if !knownWorkflowProfile(selection.Profile) {
		return fmt.Errorf("agentflow recommend-workflow: %s.profile %q is unknown", field, selection.Profile)
	}
	return nil
}

func knownWorkflowProfile(profile string) bool {
	switch profile {
	case "docs-only", "small-bugfix", "medium-feature", "large-feature", "high-risk":
		return true
	default:
		return false
	}
}

func parseWorkflowOverride(raw json.RawMessage) (*WorkflowOverride, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var p workflowOverrideProjection
	if err := decodeStrict(raw, &p); err != nil {
		return nil, fmt.Errorf("agentflow recommend-workflow: invalid override: %w", err)
	}
	if p.FromProfile == nil || p.ToProfile == nil || p.Reason == nil ||
		!knownWorkflowProfile(value(p.FromProfile)) || !knownWorkflowProfile(value(p.ToProfile)) ||
		strings.TrimSpace(value(p.Reason)) == "" {
		return nil, fmt.Errorf("agentflow recommend-workflow: override requires known from_profile/to_profile and non-empty reason")
	}
	return &WorkflowOverride{FromProfile: *p.FromProfile, ToProfile: *p.ToProfile, Reason: *p.Reason}, nil
}

func parseWorkflowContract(raw json.RawMessage) (WorkflowContract, error) {
	var p workflowContractProjection
	if err := decodeStrict(raw, &p); err != nil {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: invalid workflow_contract_candidate: %w", err)
	}
	missing := ""
	switch {
	case p.SchemaVersion == nil:
		missing = "schema_version"
	case p.WorkflowPack == nil:
		missing = "workflow_pack"
	case p.WorkflowProfile == nil:
		missing = "workflow_profile"
	case p.SelectedBy == nil:
		missing = "selected_by"
	case p.SelectionReason == nil:
		missing = "selection_reason"
	case p.RequiredCapabilities == nil:
		missing = "required_capabilities"
	case p.ReviewDepth == nil:
		missing = "review_depth"
	case p.ValidationPolicy == nil:
		missing = "validation_policy"
	case p.ProofPolicy == nil:
		missing = "proof_policy"
	case p.ValidationPolicy.RequiredGates == nil:
		missing = "validation_policy.required_gates"
	case p.ProofPolicy.HunkAttribution == nil:
		missing = "proof_policy.hunk_attribution"
	case p.ProofPolicy.RequireReviewRun == nil:
		missing = "proof_policy.require_review_run"
	}
	if missing != "" {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: workflow_contract_candidate missing required field %s", missing)
	}
	if *p.SchemaVersion != "0.1.0" {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: workflow contract schema_version %q is incompatible", *p.SchemaVersion)
	}
	if strings.TrimSpace(*p.WorkflowPack) == "" || !knownWorkflowProfile(*p.WorkflowProfile) ||
		strings.TrimSpace(*p.SelectedBy) == "" || strings.TrimSpace(*p.SelectionReason) == "" {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: invalid workflow contract identity")
	}
	if !knownReviewDepth(*p.ReviewDepth) {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: unknown review_depth %q", *p.ReviewDepth)
	}
	if !knownHunkPolicy(*p.ProofPolicy.HunkAttribution) {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: unknown proof_policy.hunk_attribution %q", *p.ProofPolicy.HunkAttribution)
	}
	if !allNonBlank(*p.ValidationPolicy.RequiredGates) {
		return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: validation_policy.required_gates contains a blank value")
	}
	capabilities := make([]WorkflowCapability, len(*p.RequiredCapabilities))
	for i, capability := range *p.RequiredCapabilities {
		if capability.ID == nil || strings.TrimSpace(*capability.ID) == "" || capability.Required == nil {
			return WorkflowContract{}, fmt.Errorf("agentflow recommend-workflow: invalid required_capabilities[%d]", i)
		}
		capabilities[i] = WorkflowCapability{ID: *capability.ID, Required: *capability.Required}
	}
	return WorkflowContract{
		SchemaVersion: *p.SchemaVersion, WorkflowPack: *p.WorkflowPack,
		WorkflowProfile: *p.WorkflowProfile, SelectedBy: *p.SelectedBy,
		SelectionReason: *p.SelectionReason, RequiredCapabilities: capabilities,
		ReviewDepth:      *p.ReviewDepth,
		ValidationPolicy: WorkflowValidationPolicy{RequiredGates: append([]string(nil), (*p.ValidationPolicy.RequiredGates)...)},
		ProofPolicy:      WorkflowProofPolicy{HunkAttribution: *p.ProofPolicy.HunkAttribution, RequireReviewRun: *p.ProofPolicy.RequireReviewRun},
	}, nil
}

func knownReviewDepth(depth string) bool {
	switch depth {
	case "none", "light", "standard", "spec_quality", "deep":
		return true
	default:
		return false
	}
}

func knownHunkPolicy(policy string) bool {
	return policy == "off" || policy == "observe" || policy == "enforce"
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// MaterializeWorkflowContract gives Agentflow the exact candidate it returned.
// The only durable write is performed by workflow-contract --from-json.
func (c *Client) MaterializeWorkflowContract(ctx context.Context, recommendation WorkflowRecommendation) error {
	candidate := recommendation.CandidateJSON()
	if len(candidate) == 0 {
		return fmt.Errorf("agentflow workflow-contract: recommendation has no candidate")
	}
	tmp, err := os.CreateTemp("", "golem-workflow-contract-*.json")
	if err != nil {
		return fmt.Errorf("create staged workflow contract: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(candidate); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write staged workflow contract: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged workflow contract: %w", err)
	}
	args := append([]string{"workflow-contract"}, c.rootArgs("--from-json", tmp.Name())...)
	out, errb, exit, err := c.r.Run(ctx, args, nil)
	if err != nil {
		return err
	}
	if exit != 0 {
		detail := strings.TrimSpace(string(errb))
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		return &CommandError{Cmd: "workflow-contract", Exit: exit, Stderr: detail}
	}
	return nil
}
