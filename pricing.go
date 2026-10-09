package claudebot

import (
	"regexp"
	"sort"
)

// Price is USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	CacheWrite float64 `json:"cacheWrite"`
	CacheRead  float64 `json:"cacheRead"`
	Output     float64 `json:"output"`
}

// priceEntry is one row of the built-in table.
type priceEntry struct {
	Price
	// Large is the price for a request whose input exceeds LargeThreshold
	// tokens. nil means one price regardless of size.
	Large *Price
	// LargeThreshold is the input size (input + cache write + cache read)
	// above which Large applies.
	LargeThreshold int64
	// Dated means the model can also be referred to with a date suffix
	// (claude-sonnet-4-5-20250929)。4.6 より前のモデルは別名と日付付きの ID
	// の両方で呼べるため。
	Dated bool
}

// priceTable is the built-in price table (2026-10-09 時点の公式の価格。
// https://platform.claude.com/docs/en/about-claude/pricing)。
//
// Models API (/v1/models) は単価を返さないので、表をプラグインに持つ。
// 値下げはあっても値上げは無い前提で、変わったらプラグインの更新で直す。
// 退役したモデル (Opus 4.1 / Opus 4 / Sonnet 4 / Haiku 3.5) は呼べないので
// 入れない。Batch API・fast mode・inference_geo は使わないので含めない。
var priceTable = map[string]priceEntry{
	"claude-fable-5-1":  {Price: Price{10, 12.50, 0.25, 50}},
	"claude-fable-5":    {Price: Price{10, 12.50, 1, 50}},
	"claude-mythos-5-1": {Price: Price{10, 12.50, 0.25, 50}},
	"claude-mythos-5":   {Price: Price{10, 12.50, 1, 50}},
	"claude-opus-5-5":   {Price: Price{4, 5, 0.20, 20}},
	"claude-opus-5":     {Price: Price{5, 6.25, 0.50, 25}},
	"claude-opus-4-8":   {Price: Price{5, 6.25, 0.50, 25}},
	"claude-opus-4-7":   {Price: Price{5, 6.25, 0.50, 25}},
	"claude-opus-4-6":   {Price: Price{5, 6.25, 0.50, 25}},
	"claude-opus-4-5":   {Price: Price{5, 6.25, 0.50, 25}, Dated: true},
	"claude-sonnet-5-5": {Price: Price{2, 2.50, 0.10, 10}},
	"claude-sonnet-5":   {Price: Price{2, 2.50, 0.20, 10}},
	"claude-sonnet-4-6": {Price: Price{3, 3.75, 0.30, 15}},
	"claude-sonnet-4-5": {Price: Price{3, 3.75, 0.30, 15}, Dated: true},
	"claude-haiku-5-5": {
		Price:          Price{0.10, 0.125, 0.01, 0.50},
		Large:          &Price{0.50, 0.625, 0.05, 2.50},
		LargeThreshold: 100_000,
	},
	"claude-haiku-4-5": {Price: Price{1, 1.25, 0.10, 5}, Dated: true},
}

var datedSuffix = regexp.MustCompile(`^(.+)-[0-9]{8}$`)

// tokens is the usage of one or more API calls.
type tokens struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	CacheRead     int64 `json:"cacheReadTokens"`
}

func (t tokens) total() int64 { return t.Input + t.Output + t.CacheCreation + t.CacheRead }

func (t *tokens) add(o tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheCreation += o.CacheCreation
	t.CacheRead += o.CacheRead
}

// pricer resolves prices from the operator's overrides and the table.
type pricer struct {
	overrides map[string]Price
}

func newPricer(overrides []PriceOverride) pricer {
	p := pricer{overrides: map[string]Price{}}
	for _, o := range overrides {
		p.overrides[canonicalModel(o.Model)] = o.Price
	}
	return p
}

// canonicalModel maps a dated ID of a pre-4.6 model to its alias
// (claude-sonnet-4-5-20250929 → claude-sonnet-4-5)。それ以外はそのまま返す。
//
// 別名と日付付きの ID は同じモデルなので、上書きもどちらで書いても両方に効く
// ようにする。
func canonicalModel(model string) string {
	m := datedSuffix.FindStringSubmatch(model)
	if m == nil {
		return model
	}
	if e, ok := priceTable[m[1]]; ok && e.Dated {
		return m[1]
	}
	return model
}

// lookup returns the price for model and a request of the given input size.
// 上書きは表より優先し、段階は持たない (運営者が入れた 1 つの単価で数える)。
func (p pricer) lookup(model string, promptSize int64) (Price, bool) {
	key := canonicalModel(model)
	if o, ok := p.overrides[key]; ok {
		return o, true
	}
	e, ok := priceTable[key]
	if !ok {
		return Price{}, false
	}
	if e.Large != nil && promptSize > e.LargeThreshold {
		return *e.Large, true
	}
	return e.Price, true
}

// cost returns the estimated USD for t at price pr.
func cost(t tokens, pr Price) float64 {
	return (float64(t.Input)*pr.Input +
		float64(t.CacheCreation)*pr.CacheWrite +
		float64(t.CacheRead)*pr.CacheRead +
		float64(t.Output)*pr.Output) / 1_000_000
}

// priceRow is one row of the table shown on the admin page.
type priceRow struct {
	Model string `json:"model"`
	Price
	Large          *Price `json:"large,omitempty"`
	LargeThreshold int64  `json:"largeThreshold,omitempty"`
	Dated          bool   `json:"dated"`
}

func priceRows() []priceRow {
	rows := make([]priceRow, 0, len(priceTable))
	for m, e := range priceTable {
		rows = append(rows, priceRow{Model: m, Price: e.Price, Large: e.Large, LargeThreshold: e.LargeThreshold, Dated: e.Dated})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	return rows
}
