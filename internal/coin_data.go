package internal

import "time"

var TargetSymbols = []string{
	"ETHUSDT",
	"BTCUSDT",
	"BNBUSDT",
	"SOLUSDT",
	"XRPUSDT",
}

type PricePoint struct {
	Price     float64   `json:"price"`
	Timestamp time.Time `json:"timestamp"`
}

type CoinInfo struct {
	Symbol        string       `json:"symbol"`
	LastPrice     string       `json:"last_price"`
	PreviousPrice string       `json:"previous_price"`
	PriceHistory  []PricePoint `json:"price_history"`
	DisplayStr    string       `json:"-"`
	FetchError    error        `json:"-"`
	IsLoading     bool         `json:"-"`
}
