package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ImageStorage 负责将 image_generation_call 返回的 base64 PNG 写入本地磁盘，
// 并返回可被 nginx 静态目录直出的 URL。
//
// 两个环境变量：
//   - IMAGE_BASE_URL  公网可访问的图像 URL 前缀（不带尾部斜杠），
//     例如 "https://cx.wyzai.top/images"。留空则禁用本地存储，
//     所有调用返回空字符串，由调用方退回到 data URL 模式。
//   - IMAGE_STORE_DIR 容器内磁盘存储目录，默认 "/app/images"。
//     建议 docker volume 挂载到宿主机的持久化目录。
//
// 文件名：<sha256(base64)[:16]>.png，天然去重 + 短路径。
// 存在即复用，不重复写盘。

var (
	imageStorageInitOnce   atomic.Bool
	imageStorageConfigured atomic.Bool
	imageStorageBaseURL    atomic.Value // string
	imageStorageDir        atomic.Value // string
)

const defaultImageStoreDir = "/app/images"

// initImageStorage 懒加载读取环境变量（只读一次，后续 atomic 读取）。
// 返回 true 表示已启用，false 表示留空（禁用，退回 data URL）。
func initImageStorage() bool {
	if imageStorageInitOnce.Load() {
		return imageStorageConfigured.Load()
	}
	if !imageStorageInitOnce.CompareAndSwap(false, true) {
		// 另一个 goroutine 已经初始化
		return imageStorageConfigured.Load()
	}

	base := strings.TrimRight(strings.TrimSpace(os.Getenv("IMAGE_BASE_URL")), "/")
	dir := strings.TrimSpace(os.Getenv("IMAGE_STORE_DIR"))
	if dir == "" {
		dir = defaultImageStoreDir
	}

	if base == "" {
		imageStorageConfigured.Store(false)
		log.Printf("[image-storage] disabled (IMAGE_BASE_URL 未配置，返回 data URL)")
		return false
	}

	// 尝试创建目录；失败则禁用（回退 data URL）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		imageStorageConfigured.Store(false)
		log.Printf("[image-storage] 创建目录 %s 失败，禁用本地存储：%v", dir, err)
		return false
	}

	imageStorageBaseURL.Store(base)
	imageStorageDir.Store(dir)
	imageStorageConfigured.Store(true)
	log.Printf("[image-storage] enabled: dir=%s base_url=%s", dir, base)
	return true
}

// SaveImageFromBase64 解码 base64 PNG 并写入本地磁盘，返回公网 URL。
//
// 返回值语义：
//   - (url, nil)  : 写盘成功（或已存在），返回可直出的 URL
//   - ("", nil)   : 本地存储未启用（调用方应退回到 data URL）
//   - ("", err)   : 明确错误（非法 base64 等）
func SaveImageFromBase64(b64 string) (string, error) {
	if !initImageStorage() {
		return "", nil
	}
	if len(b64) == 0 {
		return "", errors.New("empty base64")
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(raw) < 12 {
		return "", errors.New("decoded bytes too short")
	}
	// 合理性校验：识别 PNG / JPEG / WebP magic（方便上游切换 output_format）
	isPNG := raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4E && raw[3] == 0x47
	isJPEG := raw[0] == 0xFF && raw[1] == 0xD8
	// WebP: "RIFF" (4B) + size (4B) + "WEBP" (4B)
	isWebP := raw[0] == 0x52 && raw[1] == 0x49 && raw[2] == 0x46 && raw[3] == 0x46 &&
		raw[8] == 0x57 && raw[9] == 0x45 && raw[10] == 0x42 && raw[11] == 0x50
	if !isPNG && !isJPEG && !isWebP {
		// 仍按 png 处理但留个日志
		log.Printf("[image-storage] 未识别的图像 magic: % x (continuing as png)", raw[:12])
	}
	ext := ".png"
	switch {
	case isJPEG:
		ext = ".jpg"
	case isWebP:
		ext = ".webp"
	}

	sum := sha256.Sum256(raw)
	name := hex.EncodeToString(sum[:8]) + ext // 16 hex chars

	dir, _ := imageStorageDir.Load().(string)
	base, _ := imageStorageBaseURL.Load().(string)

	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		// 已存在：直接返回 URL
		return base + "/" + name, nil
	}

	// 原子写：先写临时文件，再 rename（避免并发下读到半成品）
	tmp, err := os.CreateTemp(dir, ".tmp-*"+ext)
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		// 如果目标已存在（并发），视为成功
		if _, statErr := os.Stat(path); statErr == nil {
			return base + "/" + name, nil
		}
		return "", err
	}
	// 让容器外的 nginx 进程能读取（容器默认 umask 会给 0600）
	if err := os.Chmod(path, 0o644); err != nil {
		log.Printf("[image-storage][v2] chmod 0644 failed: %v", err)
	}
	return base + "/" + name, nil
}

// imageMarkdownFromBase64 尝试把 base64 落盘为静态图片 URL；失败或未启用时
// 回退为 data URL。返回可直接作为 Chat Completions content delta 的 markdown。
//
// 前后各加两个换行是为了和上下文文本分隔，避免客户端把图像粘连在最后一段文字里。
func imageMarkdownFromBase64(b64 string) string {
	if url, err := SaveImageFromBase64(b64); err == nil && url != "" {
		return "\n\n![image](" + url + ")\n"
	} else if err != nil {
		log.Printf("[image-storage] fallback to data URL, save failed: %v", err)
	}
	return "\n\n![image](data:image/png;base64," + b64 + ")\n"
}

// MergeImageItemsIntoResponse 将流式 `response.output_item.done` 事件中采集到的
// image_generation_call 完整 item（含 result 字段）合并回 `response.completed`
// 的 response 对象里。Codex 在流式场景下 response.completed.output[] 中的
// image_generation_call item 往往 **缺失 result 字段** 甚至整个 item 都不在
// output 里（实测上游在开启 image_generation tool 时 output=[]），图像数据
// 仅通过 SSE 事件下发。此函数：
//  1. 如果 output[] 已存在相同 id 的 image_generation_call item → 原位替换
//  2. 否则 → append 到 output 数组末尾
//
// 让非流式 /v1/responses 客户端也能拿到 `item.result` 里的 base64 图像。
func MergeImageItemsIntoResponse(response []byte, items map[string]json.RawMessage) []byte {
	if len(items) == 0 {
		return response
	}

	// 先把 output[] 中已存在的 image_generation_call item 的 id → index 建立索引
	existingIdx := make(map[string]int)
	if output := gjson.GetBytes(response, "output"); output.IsArray() {
		for i, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			if id := item.Get("id").String(); id != "" {
				existingIdx[id] = i
			}
		}
	} else if !output.Exists() {
		// 响应里完全没有 output 字段时，先建立一个空数组
		if b, err := sjson.SetRawBytes(response, "output", []byte("[]")); err == nil {
			response = b
		}
	}

	for id, raw := range items {
		if len(raw) == 0 {
			continue
		}
		var (
			path string
		)
		if idx, ok := existingIdx[id]; ok {
			path = fmt.Sprintf("output.%d", idx)
		} else {
			// sjson 用 -1 索引表示 append
			path = "output.-1"
		}
		if newBody, err := sjson.SetRawBytes(response, path, raw); err == nil {
			response = newBody
		}
	}
	return response
}

// resetImageStorageForTest 重置内部状态，仅供单测使用。
func resetImageStorageForTest() {
	imageStorageInitOnce.Store(false)
	imageStorageConfigured.Store(false)
	imageStorageBaseURL.Store("")
	imageStorageDir.Store("")
}
