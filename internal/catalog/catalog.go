package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/usage"
	"gopkg.in/yaml.v3"
)

const SchemaVersion = "2"

type Pricing struct {
	Currency                  string   `yaml:"currency" json:"currency"`
	Mode                      string   `yaml:"mode" json:"mode"`
	EffectiveFrom             string   `yaml:"effective_from,omitempty" json:"effective_from,omitempty"`
	VerifiedAt                string   `yaml:"verified_at" json:"verified_at"`
	Source                    string   `yaml:"source" json:"source"`
	InputPricePerMillion      *float64 `yaml:"input_per_million" json:"input_per_million"`
	OutputPricePerMillion     *float64 `yaml:"output_per_million" json:"output_per_million"`
	CacheReadPricePerMillion  *float64 `yaml:"cached_input_per_million,omitempty" json:"cached_input_per_million,omitempty"`
	CacheWritePricePerMillion *float64 `yaml:"cache_write_per_million,omitempty" json:"cache_write_per_million,omitempty"`
}

type Model struct {
	Provider    string   `yaml:"provider" json:"provider"`
	DisplayName string   `yaml:"display_name" json:"display_name"`
	Aliases     []string `yaml:"aliases" json:"aliases"`
	Tier        string   `yaml:"tier" json:"tier"`
	Pricing     Pricing  `yaml:"pricing" json:"pricing"`
}

type file struct {
	SchemaVersion string           `yaml:"schema_version"`
	Models        map[string]Model `yaml:"models"`
}

type Catalog struct {
	Models  map[string]Model
	Aliases map[string]string
}

func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var input file
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if input.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("catalog schema_version must be %q", SchemaVersion)
	}
	return New(input.Models)
}

func New(models map[string]Model) (*Catalog, error) {
	copyModels := make(map[string]Model, len(models))
	aliases := make(map[string]string)
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if err := validateModel(id, models[id]); err != nil {
			return nil, err
		}
		aliases[id] = id
	}
	for _, id := range ids {
		model := models[id]
		copyModels[id] = model
		seen := make(map[string]struct{}, len(model.Aliases))
		for _, rawAlias := range model.Aliases {
			alias := strings.TrimSpace(rawAlias)
			if _, exists := seen[alias]; exists {
				return nil, fmt.Errorf("model %q declares alias %q more than once", id, alias)
			}
			seen[alias] = struct{}{}
			if alias == id {
				continue
			}
			if owner, exists := aliases[alias]; exists {
				return nil, fmt.Errorf("model %q alias %q conflicts with model %q", id, alias, owner)
			}
			aliases[alias] = id
		}
	}
	return &Catalog{Models: copyModels, Aliases: aliases}, nil
}

func MustNew(models map[string]Model) *Catalog {
	catalog, err := New(models)
	if err != nil {
		panic(err)
	}
	return catalog
}

func Empty() *Catalog { return MustNew(map[string]Model{}) }

func validateModel(id string, model Model) error {
	if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
		return errors.New("catalog model ID must not be empty or contain surrounding whitespace")
	}
	if strings.TrimSpace(model.Provider) == "" {
		return fmt.Errorf("model %q provider is required", id)
	}
	if strings.TrimSpace(model.DisplayName) == "" {
		return fmt.Errorf("model %q display_name is required", id)
	}
	if len(model.Aliases) == 0 {
		return fmt.Errorf("model %q must declare at least one alias", id)
	}
	for _, alias := range model.Aliases {
		if strings.TrimSpace(alias) == "" || alias != strings.TrimSpace(alias) {
			return fmt.Errorf("model %q has an empty alias or one with surrounding whitespace", id)
		}
	}
	pricing := model.Pricing
	if pricing.Currency != "USD" {
		return fmt.Errorf("model %q pricing currency must be USD", id)
	}
	if pricing.Mode != "standard" {
		return fmt.Errorf("model %q pricing mode must be standard", id)
	}
	if pricing.InputPricePerMillion == nil || pricing.OutputPricePerMillion == nil {
		return fmt.Errorf("model %q input and output prices are required", id)
	}
	for name, price := range map[string]*float64{
		"input": pricing.InputPricePerMillion, "output": pricing.OutputPricePerMillion,
		"cached input": pricing.CacheReadPricePerMillion, "cache write": pricing.CacheWritePricePerMillion,
	} {
		if price != nil && *price < 0 {
			return fmt.Errorf("model %q %s price cannot be negative", id, name)
		}
	}
	if err := validDate(pricing.VerifiedAt); err != nil {
		return fmt.Errorf("model %q verified_at: %w", id, err)
	}
	if pricing.EffectiveFrom != "" {
		if err := validDate(pricing.EffectiveFrom); err != nil {
			return fmt.Errorf("model %q effective_from: %w", id, err)
		}
	}
	source, err := url.ParseRequestURI(pricing.Source)
	if err != nil || source.Scheme != "https" || source.Host == "" {
		return fmt.Errorf("model %q pricing source must be an absolute HTTPS URL", id)
	}
	return nil
}

func validDate(value string) error {
	if value == "" {
		return errors.New("date is required")
	}
	if _, err := time.Parse(time.DateOnly, value); err != nil {
		return errors.New("must use YYYY-MM-DD")
	}
	return nil
}

func (c *Catalog) Resolve(raw string) (string, Model, bool) {
	if c == nil {
		return "", Model{}, false
	}
	id, ok := c.Aliases[raw]
	if !ok {
		return "", Model{}, false
	}
	model, ok := c.Models[id]
	return id, model, ok
}

func Estimate(event usage.Event, model Model) (*float64, bool) {
	// A provider-reported aggregate total cannot be split reliably across input,
	// output, and cache rates. Keep it unpriced instead of presenting $0.00.
	if event.InputTokens == nil && event.OutputTokens == nil && event.CacheReadTokens == nil && event.CacheWriteTokens == nil {
		return nil, false
	}
	pricing := model.Pricing
	amount := float64(0)
	if !addPriced(&amount, event.InputTokens, pricing.InputPricePerMillion) ||
		!addPriced(&amount, event.OutputTokens, pricing.OutputPricePerMillion) ||
		!addPriced(&amount, event.CacheReadTokens, pricing.CacheReadPricePerMillion) ||
		!addPriced(&amount, event.CacheWriteTokens, pricing.CacheWritePricePerMillion) {
		return nil, false
	}
	return usage.Float64(amount), true
}

func addPriced(total *float64, tokens *int64, price *float64) bool {
	if tokens == nil || *tokens == 0 {
		return true
	}
	if price == nil {
		return false
	}
	*total += float64(*tokens) * *price / 1_000_000
	return true
}
