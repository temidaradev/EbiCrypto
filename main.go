package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text/v2"
	"github.com/hajimehoshi/ebiten/v2/vector"
	"github.com/temidaradev/esset/v2"
)

//go:embed font.ttf
var MyFont []byte

var client *http.Client

const glyphsToPreload = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789.,:/ ETHUSDTBTCBNBXP"
const baseFontSize = 12

var apiURL = "https://api.binance.com"
var updateInterval = 1 * time.Second
var pricePrecision = 3

var targetSymbols = []string{
	"ETHUSDT",
	"BTCUSDT",
	"BNBUSDT",
	"SOLUSDT",
	"XRPUSDT",
}

const stateFilename = "crypto_app_state.json"

var historyGapThreshold = updateInterval * 10

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

type AppData struct {
	CoinData []*CoinInfo `json:"coin_data"`
}

type Response struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

type Game struct {
	coinData           []*CoinInfo
	lastUpdateTime     time.Time
	mu                 sync.Mutex
	wg                 sync.WaitGroup
	fontFace           text.Face
	physicalLineHeight float64
	deviceScale        float64
	SelectedCoinIndex  int
	solidColorImage    *ebiten.Image

	// Topbar fields
	topbarHeight   float64
	dropdowns      []*Dropdown
	activeDropdown *Dropdown
	chartType      string // "line" or "candle"
	timeline       string // "1h", "4h", "1d", "1w"
}

type Dropdown struct {
	Label    string
	Options  []string
	IsOpen   bool
	Bounds   image.Rectangle
	Selected int
	OnSelect func(int)
}

func init() {
	client = &http.Client{
		Timeout: 1 * time.Second,
	}
}

func (g *Game) initSolidColorImage() {
	if g.solidColorImage == nil {
		g.solidColorImage = ebiten.NewImage(1, 1)
		g.solidColorImage.Fill(color.White)
	}
}

func getPrice(symbol string) (string, error) {
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

func (g *Game) updateSingleCoin(coin *CoinInfo) {
	defer g.wg.Done()

	newPriceStr, err := getPrice(coin.Symbol)

	g.mu.Lock()
	defer g.mu.Unlock()

	coin.IsLoading = false
	if err != nil {
		log.Printf("Could not get price [%s]: %v", coin.Symbol, err)
		coin.FetchError = err
		coin.DisplayStr = fmt.Sprintf("%s: Error", coin.Symbol)
		return
	}

	newPriceFloat, parseErr := strconv.ParseFloat(newPriceStr, 64)

	if parseErr != nil {
		log.Printf("Could not parse new price [%s]: %v, Price: %s", coin.Symbol, parseErr, newPriceStr)
		coin.FetchError = parseErr
		coin.DisplayStr = fmt.Sprintf("%s: Parse Error", coin.Symbol)
		return
	}

	coin.PreviousPrice = coin.LastPrice
	coin.LastPrice = newPriceStr
	coin.FetchError = nil

	format := fmt.Sprintf("%%s: %%.%df", pricePrecision)
	coin.DisplayStr = fmt.Sprintf(format, coin.Symbol, newPriceFloat)

	coin.PriceHistory = append(coin.PriceHistory, PricePoint{Price: newPriceFloat, Timestamp: time.Now()})
}

func (g *Game) updateAllPrices() {
	if g.coinData == nil {
		return
	}

	for _, coin := range g.coinData {
		g.wg.Add(1)
		go g.updateSingleCoin(coin)
	}
	g.wg.Wait()
}

func saveData(data AppData, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create state file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode state data: %w", err)
	}

	log.Printf("State saved to %s", filename)
	return nil
}

func loadData(filename string) (AppData, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return AppData{}, nil
		}
		return AppData{}, fmt.Errorf("failed to open state file: %w", err)
	}
	defer file.Close()

	var data AppData
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&data); err != nil {
		return AppData{}, fmt.Errorf("failed to decode state data: %w", err)
	}

	log.Printf("State loaded from %s", filename)
	return data, nil
}

func initCoinData(loadedData AppData) []*CoinInfo {
	if len(loadedData.CoinData) > 0 {
		log.Println("Initializing coin data from loaded state.")
		for _, coin := range loadedData.CoinData {
			if coin.PriceHistory == nil {
				coin.PriceHistory = []PricePoint{}
			}
			if coin.LastPrice != "" {
				p, err := strconv.ParseFloat(coin.LastPrice, 64)
				if err == nil {
					format := fmt.Sprintf("%%s: %%.%df", pricePrecision)
					coin.DisplayStr = fmt.Sprintf(format, coin.Symbol, p)
				} else {
					coin.DisplayStr = fmt.Sprintf("%s: Parse Error", coin.Symbol)
				}
			} else {
				coin.DisplayStr = fmt.Sprintf("%s: Loading...", coin.Symbol)
			}
			coin.IsLoading = false
			coin.FetchError = nil
		}
		return loadedData.CoinData
	} else {
		log.Println("Initializing coin data from scratch.")
		coinData := make([]*CoinInfo, len(targetSymbols))
		for i, symbol := range targetSymbols {
			coinData[i] = &CoinInfo{
				Symbol:       symbol,
				DisplayStr:   fmt.Sprintf("%s: Loading...", symbol),
				IsLoading:    true,
				PriceHistory: []PricePoint{},
			}
		}
		return coinData
	}
}

func (g *Game) initTopbar() {
	topbarHeight := 38.0 * g.deviceScale
	g.topbarHeight = topbarHeight
	g.chartType = "line"
	g.timeline = "1h"

	// Compact pill-shaped dropdowns with spacing
	margin := 12
	btnW := 80
	btnH := int(topbarHeight) - 10
	g.dropdowns = []*Dropdown{
		{
			Label:   "Crypto",
			Options: targetSymbols,
			Bounds:  image.Rect(margin, 5, margin+btnW, 5+btnH),
			OnSelect: func(index int) {
				g.mu.Lock()
				g.SelectedCoinIndex = index
				g.mu.Unlock()
			},
		},
		{
			Label:   "Chart",
			Options: []string{"Line", "Candle"},
			Bounds:  image.Rect(margin+btnW+margin, 5, margin+btnW*2+margin, 5+btnH),
			OnSelect: func(index int) {
				g.mu.Lock()
				if index == 0 {
					g.chartType = "line"
				} else {
					g.chartType = "candle"
				}
				g.mu.Unlock()
			},
		},
		{
			Label:   "Time",
			Options: []string{"1h", "4h", "1d", "1w"},
			Bounds:  image.Rect(margin+btnW*2+margin*2, 5, margin+btnW*3+margin*2, 5+btnH),
			OnSelect: func(index int) {
				g.mu.Lock()
				g.timeline = g.dropdowns[2].Options[index]
				g.mu.Unlock()
			},
		},
	}
}

func (g *Game) drawTopbar(screen *ebiten.Image) {
	screenWidth, _ := screen.Size()
	// Topbar background
	vector.DrawFilledRect(screen, 0, 0, float32(screenWidth), float32(g.topbarHeight), color.RGBA{28, 28, 28, 255}, false)
	// Draw dropdowns
	for _, dropdown := range g.dropdowns {
		// Pill background
		pillColor := color.RGBA{44, 44, 44, 255}
		if dropdown.IsOpen {
			pillColor = color.RGBA{60, 60, 60, 255}
		}
		vector.DrawFilledRect(screen, float32(dropdown.Bounds.Min.X), float32(dropdown.Bounds.Min.Y),
			float32(dropdown.Bounds.Dx()), float32(dropdown.Bounds.Dy()), pillColor, false)
		// Subtle shadow
		vector.StrokeRect(screen, float32(dropdown.Bounds.Min.X), float32(dropdown.Bounds.Min.Y),
			float32(dropdown.Bounds.Dx()), float32(dropdown.Bounds.Dy()), 1.5, color.RGBA{80, 80, 80, 80}, false)
		// Value + icon (no label prefix)
		value := dropdown.Options[dropdown.Selected]
		icon := " ▼"
		esset.DrawText(screen, value+icon, 0, float64(dropdown.Bounds.Min.X+14), float64(dropdown.Bounds.Min.Y+6), g.fontFace, color.RGBA{220, 220, 220, 255})
		// Dropdown options
		if dropdown.IsOpen {
			optionHeight := int(g.physicalLineHeight * 0.85)
			dropdownWidth := dropdown.Bounds.Dx()
			optionsHeight := optionHeight * len(dropdown.Options)
			vector.DrawFilledRect(screen, float32(dropdown.Bounds.Min.X), float32(dropdown.Bounds.Max.Y+2),
				float32(dropdownWidth), float32(optionsHeight), color.RGBA{38, 38, 38, 255}, false)
			for i, option := range dropdown.Options {
				optionY := dropdown.Bounds.Max.Y + 2 + (i * optionHeight)
				optionRect := image.Rect(dropdown.Bounds.Min.X, optionY, dropdown.Bounds.Min.X+dropdownWidth, optionY+optionHeight)
				if i == dropdown.Selected {
					vector.DrawFilledRect(screen, float32(optionRect.Min.X), float32(optionRect.Min.Y),
						float32(optionRect.Dx()), float32(optionRect.Dy()), color.RGBA{60, 60, 60, 255}, false)
				}
				esset.DrawText(screen, option, 0, float64(optionRect.Min.X+14), float64(optionRect.Min.Y+6), g.fontFace, color.White)
			}
		}
	}
	// Draw price info, small and right-aligned
	if g.SelectedCoinIndex >= 0 && g.SelectedCoinIndex < len(g.coinData) {
		selectedCoin := g.coinData[g.SelectedCoinIndex]
		priceInfo := fmt.Sprintf("%s: %s", selectedCoin.Symbol, selectedCoin.LastPrice)
		priceColor := color.RGBA{255, 255, 255, 255}
		if selectedCoin.PreviousPrice != "" && selectedCoin.LastPrice != "" {
			prev, prevErr := strconv.ParseFloat(selectedCoin.PreviousPrice, 64)
			last, lastErr := strconv.ParseFloat(selectedCoin.LastPrice, 64)
			if prevErr == nil && lastErr == nil {
				if last > prev {
					priceColor = color.RGBA{0, 255, 0, 255}
				} else if last < prev {
					priceColor = color.RGBA{255, 0, 0, 255}
				}
			}
		}
		esset.DrawText(screen, priceInfo, 0, float64(screenWidth-170), 10, g.fontFace, priceColor)
	}
}

func (g *Game) handleTopbarInput() {
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		mx, my := ebiten.CursorPosition()

		// Check if clicking on any dropdown
		for _, dropdown := range g.dropdowns {
			// Check if clicking dropdown button
			if mx >= dropdown.Bounds.Min.X && mx < dropdown.Bounds.Max.X &&
				my >= dropdown.Bounds.Min.Y && my < dropdown.Bounds.Max.Y {
				dropdown.IsOpen = !dropdown.IsOpen
				if dropdown.IsOpen {
					g.activeDropdown = dropdown
				} else if g.activeDropdown == dropdown {
					g.activeDropdown = nil
				}
				return
			}

			// Check if clicking on open dropdown options
			if dropdown.IsOpen {
				optionHeight := int(g.physicalLineHeight)
				dropdownWidth := dropdown.Bounds.Dx()
				optionsHeight := optionHeight * len(dropdown.Options)

				if mx >= dropdown.Bounds.Min.X && mx < dropdown.Bounds.Min.X+dropdownWidth &&
					my >= dropdown.Bounds.Max.Y && my < dropdown.Bounds.Max.Y+optionsHeight {
					optionIndex := (my - dropdown.Bounds.Max.Y) / optionHeight
					if optionIndex >= 0 && optionIndex < len(dropdown.Options) {
						dropdown.Selected = optionIndex
						if dropdown.OnSelect != nil {
							dropdown.OnSelect(optionIndex)
						}
						dropdown.IsOpen = false
						g.activeDropdown = nil
					}
					return
				}
			}
		}

		// Close any open dropdown if clicking elsewhere
		if g.activeDropdown != nil {
			g.activeDropdown.IsOpen = false
			g.activeDropdown = nil
		}
	}
}

func (g *Game) Draw(screen *ebiten.Image) {
	g.initSolidColorImage()

	screen.Fill(color.RGBA{22, 22, 22, 255})
	g.drawTopbar(screen)

	chartPadding := 32.0 * g.deviceScale
	chartTop := g.topbarHeight + chartPadding
	chartLeft := chartPadding
	screenWidth, screenHeight := screen.Size()
	chartWidth := float64(screenWidth) - chartPadding*2
	chartHeight := float64(screenHeight) - chartTop - chartPadding

	// Card-like chart area
	vector.DrawFilledRect(screen, float32(chartLeft), float32(chartTop), float32(chartWidth), float32(chartHeight), color.RGBA{38, 38, 38, 255}, false)
	vector.StrokeRect(screen, float32(chartLeft), float32(chartTop), float32(chartWidth), float32(chartHeight), 2, color.RGBA{60, 60, 60, 255}, false)

	g.mu.Lock()
	defer g.mu.Unlock()

	// Chart title
	if g.SelectedCoinIndex >= 0 && g.SelectedCoinIndex < len(g.coinData) {
		selectedCoin := g.coinData[g.SelectedCoinIndex]
		chartTitle := fmt.Sprintf("%s %s Chart (%s)", selectedCoin.Symbol, strings.Title(g.chartType), g.timeline)
		esset.DrawText(screen, chartTitle, 0, chartLeft+12, chartTop-28, g.fontFace, color.RGBA{180, 180, 180, 255})
	}

	// Draw grid lines and axis labels
	gridLines := 6
	for i := 0; i <= gridLines; i++ {
		// Horizontal grid
		gy := chartTop + (chartHeight*float64(i))/float64(gridLines)
		vector.StrokeLine(screen, float32(chartLeft), float32(gy), float32(chartLeft+chartWidth), float32(gy), 1, color.RGBA{60, 60, 60, 128}, false)
	}
	for i := 0; i <= gridLines; i++ {
		// Vertical grid
		gx := chartLeft + (chartWidth*float64(i))/float64(gridLines)
		vector.StrokeLine(screen, float32(gx), float32(chartTop), float32(gx), float32(chartTop+chartHeight), 1, color.RGBA{60, 60, 60, 128}, false)
	}

	// Draw chart data
	if g.SelectedCoinIndex >= 0 && g.SelectedCoinIndex < len(g.coinData) {
		selectedCoin := g.coinData[g.SelectedCoinIndex]
		history := selectedCoin.PriceHistory
		if len(history) > 0 {
			minPrice := history[0].Price
			maxPrice := history[0].Price
			for _, pp := range history {
				if pp.Price < minPrice {
					minPrice = pp.Price
				}
				if pp.Price > maxPrice {
					maxPrice = pp.Price
				}
			}
			priceRange := maxPrice - minPrice
			if priceRange == 0 {
				priceRange = 1.0
				minPrice -= 0.001
				maxPrice += 0.001
			}
			// Draw price axis labels
			for i := 0; i <= gridLines; i++ {
				price := minPrice + (priceRange*float64(gridLines-i))/float64(gridLines)
				gy := chartTop + (chartHeight*float64(i))/float64(gridLines)
				label := fmt.Sprintf("%.2f", price)
				esset.DrawText(screen, label, 0, chartLeft-60, gy-8, g.fontFace, color.RGBA{180, 180, 180, 255})
			}
			// Draw time axis labels (start/end)
			if len(history) > 1 {
				startTime := history[0].Timestamp.Format("15:04")
				endTime := history[len(history)-1].Timestamp.Format("15:04")
				esset.DrawText(screen, startTime, 0, chartLeft, chartTop+chartHeight+8, g.fontFace, color.RGBA{180, 180, 180, 255})
				esset.DrawText(screen, endTime, 0, chartLeft+chartWidth-40, chartTop+chartHeight+8, g.fontFace, color.RGBA{180, 180, 180, 255})
			}
			// Draw chart line or candles
			if g.chartType == "line" {
				path := &vector.Path{}
				for i, pp := range history {
					x := chartLeft + (float64(i)/float64(len(history)-1))*chartWidth
					y := chartTop + chartHeight - ((pp.Price-minPrice)/priceRange)*chartHeight
					if i == 0 {
						path.MoveTo(float32(x), float32(y))
					} else {
						path.LineTo(float32(x), float32(y))
					}
				}
				vs, is := path.AppendVerticesAndIndicesForStroke(nil, nil, &vector.StrokeOptions{
					Width: 2.5 * float32(g.deviceScale),
				})
				op := &ebiten.DrawTrianglesOptions{}
				op.ColorM.Scale(0, 200.0/255.0, 255.0/255.0, 1)
				screen.DrawTriangles(vs, is, g.solidColorImage, op)
			} else {
				// Candlestick: draw as vertical bars for now
				candleW := chartWidth / float64(len(history))
				for i, pp := range history {
					x := chartLeft + float64(i)*candleW
					y := chartTop + chartHeight - ((pp.Price-minPrice)/priceRange)*chartHeight
					vector.DrawFilledRect(screen, float32(x), float32(y-8), float32(candleW*0.7), 16, color.RGBA{0, 200, 255, 255}, false)
				}
			}
		}
	}
}

func (g *Game) Update() error {
	if time.Since(g.lastUpdateTime) >= updateInterval {
		g.lastUpdateTime = time.Now()
		g.updateAllPrices()
	}

	g.handleTopbarInput()

	// Only handle coin selection if no dropdown is active
	if g.activeDropdown == nil {
		if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
			mx, my := ebiten.CursorPosition()

			physicalStartY := g.topbarHeight + (10.0 * g.deviceScale) // Adjust start Y to account for topbar

			g.mu.Lock()
			defer g.mu.Unlock()

			for i, coin := range g.coinData {
				physicalDrawY := physicalStartY + float64(i)*g.physicalLineHeight
				physicalDrawX := 10.0 * g.deviceScale

				textWidth, textHeight := text.Measure(coin.DisplayStr, g.fontFace, -1)

				physicalBounds := image.Rect(
					int(physicalDrawX),
					int(physicalDrawY),
					int(physicalDrawX+textWidth),
					int(physicalDrawY+textHeight),
				)

				if mx >= physicalBounds.Min.X && mx < physicalBounds.Max.X &&
					my >= physicalBounds.Min.Y && my < physicalBounds.Max.Y {
					g.SelectedCoinIndex = i
					log.Printf("Clicked on %s (Index %d)", coin.Symbol, i)
					break
				}
			}
		}
	}

	return nil
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	return outsideWidth, outsideHeight
}

func main() {
	ebiten.SetWindowSize(800, 600) // Increased window size to accommodate topbar

	deviceScale := ebiten.Monitor().DeviceScaleFactor()

	scaledFontSize := baseFontSize * deviceScale
	fontFace, err := esset.GetFont(MyFont, int(scaledFontSize))
	if err != nil {
		log.Fatalf("Font could not be loaded with scaled size %f: %v", scaledFontSize, err)
	}

	fmt.Println("Glyph caching...")
	tempImage := ebiten.NewImage(1, 1)
	opts := &text.DrawOptions{}
	text.Draw(tempImage, glyphsToPreload, fontFace, opts)
	fmt.Println("Glyph caching done.")

	physicalLineHeight := scaledFontSize * 1.5
	physicalLineHeight += 5.0 * deviceScale

	loadedData, err := loadData(stateFilename)
	if err != nil {
		log.Printf("Error loading state: %v. Starting with empty state.", err)
	}

	g := &Game{
		coinData:           initCoinData(loadedData),
		lastUpdateTime:     time.Now().Add(-updateInterval),
		fontFace:           fontFace,
		physicalLineHeight: physicalLineHeight,
		deviceScale:        deviceScale,
		SelectedCoinIndex:  0,
	}

	g.initTopbar() // Initialize topbar

	if len(g.coinData) > 0 && g.SelectedCoinIndex == -1 {
		g.SelectedCoinIndex = 0
	} else if len(g.coinData) == 0 {
		g.SelectedCoinIndex = -1
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan

		g.mu.Lock()
		dataToSave := AppData{CoinData: g.coinData}
		g.mu.Unlock()

		if err := saveData(dataToSave, stateFilename); err != nil {
			log.Printf("Error saving state on exit: %v", err)
		}

		os.Exit(0)
	}()

	ebiten.SetWindowTitle("Multi CryptoView")
	if err := ebiten.RunGame(g); err != nil {
		log.Fatal(err)
	}
}
