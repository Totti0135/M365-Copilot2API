package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestWebAssetsCopiesInSync 防止两份前端副本漂移。go:embed 只打包 internal/web/web/，
// 而控制台功能开发常直接编辑仓库根 web/；两份不一致时二进制会继续提供旧界面
// （per-key usage 功能曾因此未上线）。两处必须同步修改，或改完后整体拷贝。
func TestWebAssetsCopiesInSync(t *testing.T) {
	files := []string{"index.html", "conversation.html", "login.html", "debug.html"}
	for _, name := range files {
		root, err := os.ReadFile(filepath.Join("..", "..", "web", name))
		if err != nil {
			t.Fatalf("read root web/%s: %v", name, err)
		}
		embedded, err := os.ReadFile(filepath.Join("web", name))
		if err != nil {
			t.Fatalf("read embedded internal/web/web/%s: %v", name, err)
		}
		if !bytes.Equal(root, embedded) {
			t.Errorf("web/%s 与 internal/web/web/%s 不一致 — go:embed 只 serving internal 副本，请同步两份文件", name, name)
		}
	}
}
