package catalog

import (
	"errors"
	"os"

	"github.com/tokemon/tokemon/internal/usage"
	"gopkg.in/yaml.v3"
)

type Model struct {
	Provider                  string   `yaml:"provider" json:"provider"`
	DisplayName               string   `yaml:"display_name" json:"display_name"`
	Aliases                   []string `yaml:"aliases" json:"aliases"`
	Tier                      string   `yaml:"tier" json:"tier"`
	InputPricePerMillion      *float64 `yaml:"input_price_per_million" json:"input_price_per_million"`
	OutputPricePerMillion     *float64 `yaml:"output_price_per_million" json:"output_price_per_million"`
	CacheReadPricePerMillion  *float64 `yaml:"cache_read_price_per_million" json:"cache_read_price_per_million"`
	CacheWritePricePerMillion *float64 `yaml:"cache_write_price_per_million" json:"cache_write_price_per_million"`
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
	if err := yaml.Unmarshal(data, &input); err != nil {
		return nil, err
	}
	if input.SchemaVersion != "1" {
		return nil, errors.New("catalog schema_version must be \"1\"")
	}
	return New(input.Models), nil
}

func New(models map[string]Model) *Catalog {
	copyModels := make(map[string]Model, len(models))
	aliases := make(map[string]string)
	for id, model := range models {
		copyModels[id] = model
		aliases[id] = id
		for _, alias := range model.Aliases {
			aliases[alias] = id
		}
	}
	return &Catalog{Models: copyModels, Aliases: aliases}
}

func Empty() *Catalog { return New(map[string]Model{}) }

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
	amount := float64(0)
	if !addPriced(&amount, event.InputTokens, model.InputPricePerMillion) ||
		!addPriced(&amount, event.OutputTokens, model.OutputPricePerMillion) ||
		!addPriced(&amount, event.CacheReadTokens, model.CacheReadPricePerMillion) ||
		!addPriced(&amount, event.CacheWriteTokens, model.CacheWritePricePerMillion) {
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
