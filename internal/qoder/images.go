package qoder

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"image/png"
	"strings"
	"sync"
)

// 内联图片归一化：只处理 data:image/png 的"大图"，目标是在不改变像素、不丢
// alpha 的前提下减小请求体；解不开、不划算、带非 RGB 形态的一律原样透传。
// 不联网、不落盘，缓存按 sha256 定量淘汰。
const (
	imgMinBytes   = 256 << 10 // 小于 256KiB 不处理
	imgMaxDecode  = 16 << 20  // 原始字节上限
	imgMaxCache   = 32 << 20
	imgCacheItems = 32
)

type imageCacheEntry struct {
	key string
	url string
}

var imageCache struct {
	sync.Mutex
	entries map[[32]byte]string
	order   []imageCacheEntry
	bytes   int
}

// ImageTranscodeEnabled 由配置决定；默认开启。
var ImageTranscodeEnabled = true

// NormalizeImageURL 返回 (归一化后的 URL, 状态)。状态用于日志/面板观测。
func NormalizeImageURL(url string) (string, string) {
	if !ImageTranscodeEnabled {
		return url, "disabled"
	}
	if !strings.HasPrefix(url, "data:image/") {
		return url, "reference"
	}
	header, encoded, ok := strings.Cut(url, ",")
	if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return url, "unsupported"
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(header), "data:"), ";base64")
	if mime != "image/png" {
		return url, "unchanged"
	}
	if len(encoded) < imgMinBytes*4/3 {
		return url, "small"
	}
	if len(encoded) > imgMaxDecode*4/3 {
		return url, "too_large"
	}
	digest := sha256.Sum256([]byte(url))
	if hit, ok := imageCacheGet(digest); ok {
		return hit, "cache_hit"
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return url, "decode_error"
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return url, "decode_error"
	}
	if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
		return url, "unsupported"
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return url, "encode_error"
	}
	// 只有省下至少 20% 才替换，避免"越压越大"。
	if buf.Len() >= len(raw)*80/100 {
		return url, "not_worth"
	}
	out := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	imageCachePut(digest, out, buf.Len())
	return out, "transcoded"
}

func imageCacheGet(key [32]byte) (string, bool) {
	imageCache.Lock()
	defer imageCache.Unlock()
	v, ok := imageCache.entries[key]
	return v, ok
}

func imageCachePut(key [32]byte, url string, size int) {
	imageCache.Lock()
	defer imageCache.Unlock()
	if imageCache.entries == nil {
		imageCache.entries = map[[32]byte]string{}
	}
	if _, exists := imageCache.entries[key]; exists {
		return
	}
	if size > imgMaxCache {
		return
	}
	imageCache.entries[key] = url
	imageCache.order = append(imageCache.order, imageCacheEntry{key: string(key[:]), url: url})
	imageCache.bytes += size
	for len(imageCache.order) > imgCacheItems || imageCache.bytes > imgMaxCache {
		old := imageCache.order[0]
		imageCache.order = imageCache.order[1:]
		var k [32]byte
		copy(k[:], old.key)
		if c, ok := imageCache.entries[k]; ok {
			imageCache.bytes -= len(c)
			delete(imageCache.entries, k)
		}
	}
}

// PrepareRequestImages 就地归一化 chat 请求中所有 image_url 内联图片。
// 返回处理计数（transcoded, total），便于日志观测。
func PrepareRequestImages(req map[string]any) (int, int) {
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return 0, 0
	}
	transcoded, total := 0, 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		parts, _ := mm["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if strField(pm, "type") != "image_url" {
				continue
			}
			box, _ := pm["image_url"].(map[string]any)
			if box == nil {
				continue
			}
			u, _ := box["url"].(string)
			if u == "" {
				continue
			}
			total++
			out, state := NormalizeImageURL(u)
			if state == "transcoded" || state == "cache_hit" {
				box["url"] = out
				transcoded++
			}
		}
	}
	return transcoded, total
}

// ImageStatus 供 /healthz 与面板透出的无内容快照。
func ImageStatus() map[string]any {
	imageCache.Lock()
	n, b := len(imageCache.entries), imageCache.bytes
	imageCache.Unlock()
	return map[string]any{
		"enabled":         ImageTranscodeEnabled,
		"codec_available": true,
		"cache_entries":   n,
		"cache_bytes":     b,
	}
}
