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

func (g *Game) Draw(screen *ebiten.Image) {
	g.initSolidColorImage()

	screen.Fill(color.RGBA{25, 25, 25, 255})

	physicalStartX := 10.0 * g.deviceScale
	physicalStartY := 10.0 * g.deviceScale
	physicalLineHeight := g.physicalLineHeight

	screenWidth, screenHeight := screen.Size()

	g.mu.Lock()
	defer g.mu.Unlock()

	listHeight := physicalLineHeight * float64(len(g.coinData))
	chartAreaStartY := listHeight + physicalStartY + (20.0 * g.deviceScale)

	for i, coin := range g.coinData {
		physicalDrawY := physicalStartY + float64(i)*physicalLineHeight
		physicalDrawX := physicalStartX

		txtColor := color.RGBA{255, 255, 255, 255}
		if coin.PreviousPrice != "" && coin.LastPrice != "" {
			prev, prevErr := strconv.ParseFloat(coin.PreviousPrice, 64)
			last, lastErr := strconv.ParseFloat(coin.LastPrice, 64)
			if prevErr == nil && lastErr == nil {
				if last > prev {
					txtColor = color.RGBA{0, 255, 0, 255}
				} else if last < prev {
					txtColor = color.RGBA{255, 0, 0, 255}
				}
			}
		}

		esset.DrawText(screen, coin.DisplayStr, 0, physicalDrawX, physicalDrawY, g.fontFace, txtColor)
	}

	if g.SelectedCoinIndex != -1 && len(g.coinData) > g.SelectedCoinIndex {
		selectedCoin := g.coinData[g.SelectedCoinIndex]
		history := selectedCoin.PriceHistory

		chartPadding := 30.0 * g.deviceScale
		chartRect := image.Rect(
			int(chartPadding),
			int(chartAreaStartY),
			screenWidth-int(chartPadding),
			screenHeight-int(chartPadding),
		)

		vector.DrawFilledRect(screen, float32(chartRect.Min.X), float32(chartRect.Min.Y), float32(chartRect.Dx()), float32(chartRect.Dy()), color.RGBA{50, 50, 50, 255}, false)

		selectedSymbolText := selectedCoin.Symbol
		symbolColor := color.RGBA{200, 200, 200, 255}
		symbolTextWidth, symbolTextHeight := text.Measure(selectedSymbolText, g.fontFace, -1)

		symbolDrawX := float64(chartRect.Min.X) + (float64(chartRect.Dx())-symbolTextWidth)/2.0
		symbolDrawY := float64(chartRect.Min.Y) - (symbolTextHeight + (5.0 * g.deviceScale))

		esset.DrawText(screen, selectedSymbolText, 0, symbolDrawX, symbolDrawY, g.fontFace, symbolColor)

		maxPointsToDisplay := chartRect.Dx()
		if maxPointsToDisplay < 100 {
			maxPointsToDisplay = 100
		}
		if maxPointsToDisplay > 1000 {
			maxPointsToDisplay = 1000
		}

		displayHistory := history
		if len(history) > maxPointsToDisplay {
			displayHistory = history[len(history)-maxPointsToDisplay:]
		}

		if len(displayHistory) > 1 {
			minPrice := displayHistory[0].Price
			maxPrice := displayHistory[0].Price
			for _, pp := range displayHistory {
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

			path := &vector.Path{}

			firstPP := displayHistory[0]
			startX := float64(chartRect.Min.X)
			startY := float64(chartRect.Max.Y) - ((firstPP.Price-minPrice)/priceRange)*float64(chartRect.Dy())
			path.MoveTo(float32(startX), float32(startY))

			for i := 1; i < len(displayHistory); i++ {
				pp1 := displayHistory[i-1]
				pp2 := displayHistory[i]

				timeDiff := pp2.Timestamp.Sub(pp1.Timestamp)

				chartX := float64(chartRect.Min.X) + (float64(i)/float64(len(displayHistory)-1))*(float64(chartRect.Dx()))
				chartY := float64(chartRect.Max.Y) - ((pp2.Price-minPrice)/priceRange)*float64(chartRect.Dy())

				if timeDiff > historyGapThreshold {
					path.MoveTo(float32(chartX), float32(chartY))
				} else {
					path.LineTo(float32(chartX), float32(chartY))
				}
			}

			vs, is := path.AppendVerticesAndIndicesForStroke(nil, nil, &vector.StrokeOptions{
				Width: 2.0 * float32(g.deviceScale),
			})

			op := &ebiten.DrawTrianglesOptions{}
			op.ColorM.Scale(0, 200.0/255.0, 255.0/255.0, 1)

			screen.DrawTriangles(vs, is, g.solidColorImage, op)

			if len(displayHistory) > 0 {
				lastPP := displayHistory[len(displayHistory)-1]
				chartX := float64(chartRect.Max.X)
				chartY := float64(chartRect.Max.Y) - ((lastPP.Price-minPrice)/priceRange)*float64(chartRect.Dy())
				vector.DrawFilledCircle(screen, float32(chartX), float32(chartY), 3.0*float32(g.deviceScale), color.RGBA{255, 255, 0, 255}, false)
			}
		} else if len(displayHistory) == 1 {
			pp := displayHistory[0]
			minPrice := pp.Price - 0.001
			priceRange := 0.002

			chartX := float64(chartRect.Min.X) + float64(chartRect.Dx())/2.0
			chartY := float64(chartRect.Max.Y) - ((pp.Price-minPrice)/priceRange)*float64(chartRect.Dy())

			vector.DrawFilledCircle(screen, float32(chartX), float32(chartY), 3.0*float32(g.deviceScale), color.RGBA{255, 255, 0, 255}, false)
		} else {
			message := "No history data yet."
			messageColor := color.RGBA{150, 150, 150, 255}
			textWidth, textHeight := text.Measure(message, g.fontFace, -1)

			msgX := float64(chartRect.Min.X) + (float64(chartRect.Dx())-textWidth)/2.0
			msgY := float64(chartRect.Min.Y) + (float64(chartRect.Dy())-textHeight)/2.0

			esset.DrawText(screen, message, 0, msgX, msgY, g.fontFace, messageColor)
		}
	} else {
		chartPadding := 30.0 * g.deviceScale
		chartRect := image.Rect(
			int(chartPadding),
			int(chartAreaStartY),
			screenWidth-int(chartPadding),
			screenHeight-int(chartPadding),
		)
		vector.DrawFilledRect(screen, float32(chartRect.Min.X), float32(chartRect.Min.Y), float32(chartRect.Dx()), float32(chartRect.Dy()), color.RGBA{50, 50, 50, 255}, false)

		message := "Click a coin to see chart."
		messageColor := color.RGBA{150, 150, 150, 255}
		textWidth, textHeight := text.Measure(message, g.fontFace, -1)
		msgX := float64(chartRect.Min.X) + (float64(chartRect.Dx())-textWidth)/2.0
		msgY := float64(chartRect.Min.Y) + (float64(chartRect.Dy())-textHeight)/2.0
		esset.DrawText(screen, message, 0, msgX, msgY, g.fontFace, messageColor)
	}
}

func (g *Game) Update() error {
	if time.Since(g.lastUpdateTime) >= updateInterval {
		g.lastUpdateTime = time.Now()
		g.updateAllPrices()
	}

	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		mx, my := ebiten.CursorPosition()

		physicalStartX := 10.0 * g.deviceScale
		physicalStartY := 10.0 * g.deviceScale
		physicalLineHeight := g.physicalLineHeight

		g.mu.Lock()
		defer g.mu.Unlock()

		for i, coin := range g.coinData {
			physicalDrawY := physicalStartY + float64(i)*physicalLineHeight
			physicalDrawX := physicalStartX

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

	return nil
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	return outsideWidth, outsideHeight
}

func main() {
	ebiten.SetWindowSize(640, 480)

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
