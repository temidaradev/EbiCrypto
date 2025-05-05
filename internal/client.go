package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

var client *http.Client

var apiURL = "https://api.binance.com"
var UpdateInterval = 1 * time.Second
var PricePrecision = 3

type Response struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

func init() {
	client = &http.Client{
		Timeout: 1 * time.Second,
	}
}

func GetPrice(symbol string) (string, error) {
	resp, err := client.Get(fmt.Sprintf("%s/api/v3/ticker/price?symbol=%s", apiURL, symbol))
	if err != nil {
		return "", fmt.Errorf("HTTP request failed [%s]: %w", symbol, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error [%s]: %s - %s", symbol, resp.Status, string(bodyBytes))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("body read error [%s]: %w", symbol, err)
	}

	var priceResp Response
	if err := json.Unmarshal(body, &priceResp); err != nil {
		return "", fmt.Errorf("JSON parse error [%s]: %w, Received Data: %s", symbol, err, string(body))
	}

	if _, err := strconv.ParseFloat(priceResp.Price, 64); err != nil {
		return "", fmt.Errorf("invalid price format [%s]: %w, Received Price: %s", symbol, err, priceResp.Price)
	}

	return priceResp.Price, nil
}
