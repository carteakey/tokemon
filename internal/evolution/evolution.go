package evolution

import "math"

type Form struct {
	Stage int    `json:"stage"`
	Slug  string `json:"form"`
	Name  string `json:"name"`
}

type Snapshot struct {
	LifetimeTokens  int64   `json:"lifetime_tokens"`
	Stage           int     `json:"stage"`
	Form            string  `json:"form"`
	FormName        string  `json:"form_name"`
	LowerThreshold  int64   `json:"lower_threshold"`
	NextThreshold   *int64  `json:"next_threshold,omitempty"`
	TokensRemaining *int64  `json:"tokens_remaining,omitempty"`
	Progress        float64 `json:"progress"`
}

var forms = []Form{
	{Stage: 0, Slug: "egg", Name: "Egg"},
	{Stage: 1, Slug: "bitling", Name: "Bitling"},
	{Stage: 2, Slug: "bytelet", Name: "Bytelet"},
	{Stage: 3, Slug: "promptling", Name: "Promptling"},
	{Stage: 4, Slug: "context-cub", Name: "Context Cub"},
	{Stage: 5, Slug: "token-scout", Name: "Token Scout"},
	{Stage: 6, Slug: "code-caster", Name: "Code Caster"},
	{Stage: 7, Slug: "agent-beast", Name: "Agent Beast"},
	{Stage: 8, Slug: "context-dragon", Name: "Context Dragon"},
	{Stage: 9, Slug: "token-titan", Name: "Token Titan"},
	{Stage: 10, Slug: "model-eater", Name: "Model Eater"},
	{Stage: 11, Slug: "context-deity", Name: "Context Deity"},
	{Stage: 12, Slug: "the-singularity", Name: "The Singularity"},
}

func Stage(tokens int64) int {
	if tokens <= 0 {
		return 0
	}
	stage := 0
	threshold := int64(10)
	for stage < len(forms)-1 && tokens >= threshold {
		stage++
		if threshold <= math.MaxInt64/10 {
			threshold *= 10
		}
	}
	return stage
}

func FormForStage(stage int) Form {
	if stage < 0 {
		stage = 0
	}
	if stage >= len(forms) {
		stage = len(forms) - 1
	}
	return forms[stage]
}

func Forms() []Form {
	result := make([]Form, len(forms))
	copy(result, forms)
	return result
}

func SnapshotFor(tokens int64) Snapshot {
	if tokens < 0 {
		tokens = 0
	}
	stage := Stage(tokens)
	form := FormForStage(stage)
	if stage == len(forms)-1 {
		return Snapshot{
			LifetimeTokens: tokens,
			Stage:          stage,
			Form:           form.Slug,
			FormName:       form.Name,
			LowerThreshold: pow10(stage),
			Progress:       1,
		}
	}
	lower := int64(0)
	if stage > 0 {
		lower = pow10(stage)
	}
	next := pow10(stage + 1)
	remaining := next - tokens
	progress := float64(tokens-lower) / float64(next-lower)
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	return Snapshot{
		LifetimeTokens:  tokens,
		Stage:           stage,
		Form:            form.Slug,
		FormName:        form.Name,
		LowerThreshold:  lower,
		NextThreshold:   &next,
		TokensRemaining: &remaining,
		Progress:        progress,
	}
}

func pow10(exponent int) int64 {
	value := int64(1)
	for i := 0; i < exponent; i++ {
		if value > math.MaxInt64/10 {
			return math.MaxInt64
		}
		value *= 10
	}
	return value
}
