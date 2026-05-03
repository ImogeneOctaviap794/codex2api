package proxy

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestImageURLRegex_Matches(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"![image](https://cx.wyzai.top/images/abc123.png)", "https://cx.wyzai.top/images/abc123.png"},
		{"text ![](https://example.com/images/deadbeef.webp) end", "https://example.com/images/deadbeef.webp"},
		{"![](http://h.test/images/FFAA11BB.jpg)", "http://h.test/images/FFAA11BB.jpg"},
		{"![](https://h/images/abc.jpeg)", "https://h/images/abc.jpeg"},
	}
	for _, tc := range cases {
		got := imageURLRegex.FindString(tc.in)
		if got != tc.want {
			t.Errorf("match %q:\n  got  %q\n  want %q", tc.in, got, tc.want)
		}
	}
}

func TestImageURLRegex_NoMatch(t *testing.T) {
	bad := []string{
		"no image here",
		"https://example.com/foo/bar.png", // 不带 /images/ 前缀
		"https://example.com/images/NOT-HEX.png",
	}
	for _, s := range bad {
		if got := imageURLRegex.FindString(s); got != "" {
			t.Errorf("should not match %q, got %q", s, got)
		}
	}
}

func TestOfficialImageModels_Coverage(t *testing.T) {
	expected := []string{"gpt-image-2", "gpt-image-1.5", "gpt-image-1", "gpt-image-1-mini"}
	for _, m := range expected {
		if !officialImageModels[m] {
			t.Errorf("officialImageModels should contain %q", m)
		}
	}
	// 非官方值应该不在里面
	for _, m := range []string{"gpt-draw-1024x1024", "dall-e-3", "gpt-5.4", "", "gpt-image-3"} {
		if officialImageModels[m] {
			t.Errorf("officialImageModels should NOT contain %q", m)
		}
	}
}

func TestReadImageAsBase64FromURL_OK(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	// 写一个已知内容的假图到 IMAGE_STORE_DIR
	content := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG magic
	filename := "deadbeefcafebabe.png"
	if err := os.WriteFile(filepath.Join(dir, filename), content, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	url := "https://example.com/images/" + filename
	got, err := readImageAsBase64FromURL(url)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := base64.StdEncoding.EncodeToString(content)
	if got != want {
		t.Errorf("b64 mismatch:\n got %q\n want %q", got, want)
	}
}

func TestReadImageAsBase64FromURL_PathTraversalRejected(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	malicious := []string{
		"https://example.com/images/../etc/passwd",
		"https://example.com/images/..%2Fetc%2Fpasswd",
		"https://example.com/images/a/b.png", // filename 含 /
	}
	for _, u := range malicious {
		if _, err := readImageAsBase64FromURL(u); err == nil {
			t.Errorf("path traversal URL %q should fail, got nil", u)
		}
	}
}

func TestReadImageAsBase64FromURL_MissingFile(t *testing.T) {
	resetImageStorageForTest()
	dir := t.TempDir()
	t.Setenv("IMAGE_BASE_URL", "https://example.com/images")
	t.Setenv("IMAGE_STORE_DIR", dir)

	_, err := readImageAsBase64FromURL("https://example.com/images/nonexistent.png")
	if err == nil {
		t.Fatal("reading nonexistent file should fail")
	}
}
