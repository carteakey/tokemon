package evolution

type Form struct {
	Stage          int    `json:"stage"`
	Slug           string `json:"form"`
	Name           string `json:"name"`
	lowerThreshold int64
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
	{Stage: 0, Slug: "egg", Name: "Egg", lowerThreshold: 0},
	{Stage: 1, Slug: "bitling", Name: "Bitling", lowerThreshold: 10},
	{Stage: 2, Slug: "bytelet", Name: "Bytelet", lowerThreshold: 100},
	{Stage: 3, Slug: "promptling", Name: "Promptling", lowerThreshold: 1_000},
	{Stage: 4, Slug: "context-cub", Name: "Context Cub", lowerThreshold: 10_000},
	{Stage: 5, Slug: "token-scout", Name: "Token Scout", lowerThreshold: 100_000},
	{Stage: 6, Slug: "code-caster", Name: "Code Caster", lowerThreshold: 1_000_000},
	{Stage: 7, Slug: "agent-beast", Name: "Agent Beast", lowerThreshold: 10_000_000},
	{Stage: 8, Slug: "context-dragon", Name: "Context Dragon", lowerThreshold: 100_000_000},
	{Stage: 9, Slug: "token-titan", Name: "Token Titan", lowerThreshold: 1_000_000_000},
	{Stage: 10, Slug: "model-eater", Name: "Model Eater", lowerThreshold: 2_000_000_000},
	{Stage: 11, Slug: "context-deity", Name: "Context Deity", lowerThreshold: 5_000_000_000},
	{Stage: 12, Slug: "reality-core", Name: "Reality Core", lowerThreshold: 10_000_000_000},
	{Stage: 13, Slug: "inference-leviathan", Name: "Inference Leviathan", lowerThreshold: 20_000_000_000},
	{Stage: 14, Slug: "parameter-colossus", Name: "Parameter Colossus", lowerThreshold: 50_000_000_000},
	{Stage: 15, Slug: "world-weaver", Name: "World Weaver", lowerThreshold: 100_000_000_000},
	{Stage: 16, Slug: "cosmic-architect", Name: "Cosmic Architect", lowerThreshold: 200_000_000_000},
	{Stage: 17, Slug: "universe-engine", Name: "Universe Engine", lowerThreshold: 500_000_000_000},
	{Stage: 18, Slug: "the-singularity", Name: "The Singularity", lowerThreshold: 1_000_000_000_000},
}

func Stage(tokens int64) int {
	if tokens <= 0 {
		return 0
	}
	for stage := len(forms) - 1; stage > 0; stage-- {
		if tokens >= forms[stage].lowerThreshold {
			return stage
		}
	}
	return 0
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
			LowerThreshold: form.lowerThreshold,
			Progress:       1,
		}
	}
	lower := form.lowerThreshold
	next := forms[stage+1].lowerThreshold
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
