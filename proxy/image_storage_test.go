package proxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 一张最小合法 PNG（1x1 透明像素）的 base64
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// 仅含 WebP magic 的最小字节（"RIFF" + 4B size + "WEBP" + VP8 头）
// 落盘只看 magic 决定扩展名，不做有效性解码。
const tinyWebPMagicBase64 = "UklGRgAAAABXRUJQVlA4IA=="

// 仅含 JPEG magic 的最小字节（FFD8 起头 + JFIF 段）
const tinyJPEGMagicBase64 = "/9j/4AAQSkZJRgAA"

func TestSaveImageFromBase64_DisabledWhenBaseURLEmpty(t *testing.T) {
	resetImageStorageForTest()
	t.Setenv("IMAGE_BASE_URL", "")
	t.Setenv("IMAGE_STORE_DIR", t.TempDir())

	url, err := SaveImageFromBase64(tinyPNGBase64)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if url != "" {
		t.Fatalf("expected empty url (disabled), got %q", url)
	}
}

func TestSaveImageFromBase64_EnabledWritesFileAndReturnsURL(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	url, err := SaveImageFromBase64(tinyPNGBase64)
	if err != nil {
		t.Fatalf("save err: %v", err)
	}
	if !strings.HasPrefix(url, "https://example.com/images/") {
		t.Fatalf("url prefix mismatch: %s", url)
	}
	if !strings.HasSuffix(url, ".png") {
		t.Fatalf("url ext should be .png: %s", url)
	}

	// 文件应被实际写入
	name := strings.TrimPrefix(url, "https://example.com/images/")
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	raw, _ := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if info.Size() != int64(len(raw)) {
		t.Fatalf("file size = %d, want %d", info.Size(), len(raw))
	}
}

func TestSaveImageFromBase64_Idempotent(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	u1, err := SaveImageFromBase64(tinyPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := SaveImageFromBase64(tinyPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 {
		t.Fatalf("same input should yield same URL: %s vs %s", u1, u2)
	}
	// 文件应只存在一个
	entries, _ := os.ReadDir(dir)
	pngs := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			pngs++
		}
	}
	if pngs != 1 {
		t.Fatalf("expected 1 png file, got %d", pngs)
	}
}

func TestSaveImageFromBase64_WebPExtension(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	url, err := SaveImageFromBase64(tinyWebPMagicBase64)
	if err != nil {
		t.Fatalf("save err: %v", err)
	}
	if !strings.HasSuffix(url, ".webp") {
		t.Errorf("URL should end with .webp, got %q", url)
	}
	// 文件应真实存在且扩展名正确
	matches, _ := filepath.Glob(filepath.Join(dir, "*.webp"))
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 .webp file, got %d: %v", len(matches), matches)
	}
}

func TestSaveImageFromBase64_JPEGExtension(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	url, err := SaveImageFromBase64(tinyJPEGMagicBase64)
	if err != nil {
		t.Fatalf("save err: %v", err)
	}
	if !strings.HasSuffix(url, ".jpg") {
		t.Errorf("URL should end with .jpg, got %q", url)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 .jpg file, got %d: %v", len(matches), matches)
	}
}

func TestSaveImageFromBase64_InvalidBase64(t *testing.T) {
	resetImageStorageForTest()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", t.TempDir())

	_, err := SaveImageFromBase64("!!not-base64!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestImageMarkdownFromBase64_FallsBackWhenDisabled(t *testing.T) {
	resetImageStorageForTest()
	t.Setenv("IMAGE_BASE_URL", "") // disabled
	t.Setenv("IMAGE_STORE_DIR", t.TempDir())

	md := imageMarkdownFromBase64(tinyPNGBase64)
	if !strings.Contains(md, "data:image/png;base64,"+tinyPNGBase64) {
		t.Fatalf("should fallback to data URL, got: %s", md)
	}
}

func TestImageMarkdownFromBase64_UsesURLWhenEnabled(t *testing.T) {
	resetImageStorageForTest()
	t.Setenv("IMAGE_BASE_URL", "https://cx.wyzai.top/images")
	t.Setenv("IMAGE_STORE_DIR", t.TempDir())

	md := imageMarkdownFromBase64(tinyPNGBase64)
	if strings.Contains(md, "data:image/png;base64") {
		t.Fatalf("should not contain data URL when enabled, got: %s", md)
	}
	if !strings.Contains(md, "https://cx.wyzai.top/images/") {
		t.Fatalf("should contain static URL, got: %s", md)
	}
	if !strings.Contains(md, "![image](") {
		t.Fatal("should be markdown image syntax")
	}
}

// 非流式 /v1/responses 场景：把 output_item.done 采集到的 image_generation_call item
// 合并回 response.completed 的 response.output[] 里，替换原先缺失 result 字段的占位。
func TestMergeImageItemsIntoResponse(t *testing.T) {
	// 模拟 response.completed 里的 response 对象：output[0] 是 image_gen_call 但 result 缺失
	response := []byte(`{
		"id":"resp_abc",
		"status":"completed",
		"output":[
			{"id":"ig_1","type":"image_generation_call","status":"completed"},
			{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"done"}]}
		]
	}`)
	// 流式 output_item.done 里采到的完整 item（带 result）
	items := map[string]json.RawMessage{
		"ig_1": []byte(`{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo..."}`),
	}
	got := MergeImageItemsIntoResponse(response, items)

	if r := gjson.GetBytes(got, "output.0.result").String(); r != "iVBORw0KGgo..." {
		t.Fatalf("output[0].result = %q, want merged", r)
	}
	// 其他 item 不受影响
	if r := gjson.GetBytes(got, "output.1.content.0.text").String(); r != "done" {
		t.Fatalf("msg item broken: %q", r)
	}
}

func TestMergeImageItemsIntoResponse_EmptyItemsReturnsUnchanged(t *testing.T) {
	body := []byte(`{"output":[{"id":"x","type":"message"}]}`)
	got := MergeImageItemsIntoResponse(body, nil)
	if string(got) != string(body) {
		t.Fatal("empty items should return unchanged")
	}
	got = MergeImageItemsIntoResponse(body, map[string]json.RawMessage{})
	if string(got) != string(body) {
		t.Fatal("empty map should return unchanged")
	}
}

func TestMergeImageItemsIntoResponse_UnknownIDIgnored(t *testing.T) {
	body := []byte(`{"output":[{"id":"ig_keep","type":"image_generation_call"}]}`)
	items := map[string]json.RawMessage{
		"ig_other": []byte(`{"id":"ig_other","type":"image_generation_call","result":"xxx"}`),
	}
	got := MergeImageItemsIntoResponse(body, items)
	// ig_other 不在 existingIdx 里 → append 到末尾，不动 output[0]
	if gjson.GetBytes(got, "output.0.result").Exists() {
		t.Fatal("output.0 should stay untouched")
	}
	if got1 := gjson.GetBytes(got, "output.1.result").String(); got1 != "xxx" {
		t.Fatalf("output.1.result = %q, want xxx (appended)", got1)
	}
}

// 上游 response.output = [] 时（实测 image_generation tool 启用后会这样），
// 合并应该把 item append 到空数组里。
func TestMergeImageItemsIntoResponse_AppendsToEmptyOutput(t *testing.T) {
	body := []byte(`{"id":"resp_x","status":"completed","output":[]}`)
	items := map[string]json.RawMessage{
		"ig_1": []byte(`{"id":"ig_1","type":"image_generation_call","result":"AAAA"}`),
	}
	got := MergeImageItemsIntoResponse(body, items)

	if n := gjson.GetBytes(got, "output.#").Int(); n != 1 {
		t.Fatalf("output len = %d, want 1 (appended)", n)
	}
	if r := gjson.GetBytes(got, "output.0.result").String(); r != "AAAA" {
		t.Fatalf("output.0.result = %q, want AAAA", r)
	}
	if r := gjson.GetBytes(got, "output.0.id").String(); r != "ig_1" {
		t.Fatalf("output.0.id = %q", r)
	}
}

// 完全没有 output 字段时也要能工作（创建空数组后 append）。
func TestMergeImageItemsIntoResponse_CreatesOutputFieldWhenMissing(t *testing.T) {
	body := []byte(`{"id":"resp_x","status":"completed"}`)
	items := map[string]json.RawMessage{
		"ig_1": []byte(`{"id":"ig_1","type":"image_generation_call","result":"BB"}`),
	}
	got := MergeImageItemsIntoResponse(body, items)
	if r := gjson.GetBytes(got, "output.0.result").String(); r != "BB" {
		t.Fatalf("output.0.result = %q, want BB", r)
	}
}
