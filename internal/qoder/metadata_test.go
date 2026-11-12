package qoder

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"
)

func TestNormalizeImageURLSmallAndNonPNG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	small := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	if _, state := NormalizeImageURL(small); state != "small" {
		t.Fatalf("state=%s", state)
	}
	if _, state := NormalizeImageURL("https://example.com/a.png"); state != "reference" {
		t.Fatalf("state=%s", state)
	}
	if _, state := NormalizeImageURL("data:image/jpeg;base64,AAAA"); state != "unchanged" {
		t.Fatalf("state=%s", state)
	}
	if _, state := NormalizeImageURL("data:image/png,rawbytes"); state != "unsupported" {
		t.Fatalf("state=%s", state)
	}
}

func pngDataURL(t *testing.T, level png.CompressionLevel) (string, int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), buf.Len()
}

func TestPrepareRequestImagesTranscodes(t *testing.T) {
	url, rawLen := pngDataURL(t, png.NoCompression)
	if rawLen < imgMinBytes {
		t.Fatalf("fixture too small: %d", rawLen)
	}
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
		}},
	}}
	transcoded, total := PrepareRequestImages(req)
	if total != 1 || transcoded != 1 {
		t.Fatalf("transcoded=%d total=%d", transcoded, total)
	}
	part := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	out, _ := part["image_url"].(map[string]any)["url"].(string)
	if out == url || len(out) >= len(url) {
		t.Fatalf("not replaced/reduced: %d -> %d", len(url), len(out))
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(out, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(decoded))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 400 || img.Bounds().Dy() != 400 {
		t.Fatalf("dimensions changed: %v", img.Bounds())
	}
	// 第二次命中缓存，仍计为已处理。
	if _, total := PrepareRequestImages(req); total != 1 {
		t.Fatalf("second total=%d", total)
	}
	if _, state := NormalizeImageURL(url); state != "cache_hit" {
		t.Fatalf("expected cache hit, got %s", state)
	}
}

func TestImageStatusShape(t *testing.T) {
	raw, _ := json.Marshal(ImageStatus())
	for _, want := range []string{"enabled", "codec_available", "cache_entries", "cache_bytes"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}

func TestPriceLookupAndCost(t *testing.T) {
	if p, ok := PriceFor("DeepSeek-V4-Pro", nil); !ok || p.In != 0.55 {
		t.Fatalf("alias price=%v ok=%v", p, ok)
	}
	if _, ok := PriceFor("totally-unknown", nil); ok {
		t.Fatal("unknown model got a price")
	}
	overrides := map[string]Price{"qwen3.8-max": {In: 1, Cached: 0.1, Out: 2}}
	if p, ok := PriceFor("qwen3.8-max", overrides); !ok || p.In != 1 {
		t.Fatalf("override not used: %v", p)
	}
	cost, ok := CostOf("deepseek-v4-pro", 1_000_000, 200_000, 500_000, nil)
	if !ok {
		t.Fatal("cost unavailable")
	}
	// 800k*0.55 + 200k*0.14 + 500k*2.19 = 1,565,000 /1e6
	if diff := cost - 1.563; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost=%f", cost)
	}
	if _, ok := CostOf("nope", 1, 0, 0, nil); ok {
		t.Fatal("unknown model priced")
	}
}

func TestLoadPriceOverrides(t *testing.T) {
	if got := LoadPriceOverrides(""); got != nil {
		t.Fatalf("empty path=%v", got)
	}
	if got := LoadPriceOverrides("/nonexistent/prices.json"); got != nil {
		t.Fatalf("missing file=%v", got)
	}
}

func TestModelMetadata(t *testing.T) {
	m := ModelInfo{
		Key:           "qwen3.8-max",
		MaxInputToken: 64000,
		PriceFactor:   0.2,
		IsNew:         true,
		ContextConfig: map[string]any{"64k": map[string]any{"token_count": float64(64000), "is_default": true}},
		ThinkingConfig: map[string]any{"enabled": map[string]any{"efforts": map[string]any{
			"low":  map[string]any{},
			"high": map[string]any{"is_default": true},
		}}},
		Promotion: map[string]any{
			"active":                        true,
			"window_start":                  "22:00",
			"window_end":                    "08:00",
			"timezone":                      "Asia/Shanghai",
			"discount_factor":               float64(0.25),
			"before_promotion_price_factor": float64(1),
		},
	}
	efforts, def := m.EffortListing()
	if len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "high" || def != "high" {
		t.Fatalf("efforts=%v def=%s", efforts, def)
	}
	if m.ContextWindow() != 64000 {
		t.Fatalf("window=%d", m.ContextWindow())
	}
	shanghai := time.FixedZone("CST", 8*3600)
	if !m.OffPeakActive(time.Date(2026, 11, 12, 23, 0, 0, 0, shanghai)) {
		t.Fatal("23:00 should be off-peak")
	}
	if m.OffPeakActive(time.Date(2026, 11, 12, 12, 0, 0, 0, shanghai)) {
		t.Fatal("12:00 should be peak")
	}
	view := m.MetaView(time.Date(2026, 11, 12, 23, 0, 0, 0, shanghai), nil)
	if view["context_length"] != 64000 {
		t.Fatalf("view=%v", view)
	}
	if view["is_new"] != true || view["price_factor"] != 0.2 {
		t.Fatalf("view=%v", view)
	}
	promo, _ := view["promotion"].(map[string]any)
	if promo == nil || promo["active_now"] != true || promo["valley_factor"] != 0.25 {
		t.Fatalf("promo=%v", promo)
	}
	pricing, _ := view["pricing"].(map[string]any)
	if pricing == nil || pricing["currency"] != "USD" {
		t.Fatalf("pricing=%v", pricing)
	}
}

func TestOffPeakNoPromotion(t *testing.T) {
	m := ModelInfo{}
	if _, def := m.EffortListing(); def != "" {
		t.Fatalf("def=%s", def)
	}
	if m.PromotionView(time.Now()) != nil {
		t.Fatal("promotion view without config")
	}
	if m.OffPeakActive(time.Now()) {
		t.Fatal("off-peak without config")
	}
}
