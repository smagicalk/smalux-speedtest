package reportpng

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
)

// fontSources 保存解析后的不可变字体表。具体 font.Face 每次 Render 单独创建，避免
// opentype.Face 内部缓存被并发报告渲染共享。
var fontSources struct {
	sync.Once
	regular   *opentype.Font
	bold      *opentype.Font
	preferred *opentype.Font
	err       error
}

// loadFontSources 解析内嵌兜底字体，并只在进程内搜索一次可选系统字体。
func loadFontSources() (regular, boldFont, preferred *opentype.Font, err error) {
	fontSources.Do(func() {
		fontSources.regular, fontSources.err = opentype.Parse(goregular.TTF)
		if fontSources.err != nil {
			return
		}
		fontSources.bold, fontSources.err = opentype.Parse(gobold.TTF)
		if fontSources.err != nil {
			return
		}
		fontSources.preferred = findPreferredFont()
		if fontSources.preferred == nil {
			fontSources.preferred = fontSources.regular
		}
	})
	return fontSources.regular, fontSources.bold, fontSources.preferred, fontSources.err
}

// findPreferredFont 按“显式配置 -> 平台常见 Unicode/CJK 字体”的顺序查找字体。
// 文件不存在或格式不受支持时继续尝试下一项，最终由内嵌字体兜底。
func findPreferredFont() *opentype.Font {
	paths := make([]string, 0, 8)
	if configured := os.Getenv("SMALUX_REPORT_FONT"); configured != "" {
		paths = append(paths, configured)
	}
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("WINDIR")
		if root == "" {
			root = `C:\Windows`
		}
		paths = append(paths,
			filepath.Join(root, "Fonts", "msyh.ttc"),
			filepath.Join(root, "Fonts", "simsun.ttc"),
			filepath.Join(root, "Fonts", "arial.ttf"),
		)
	case "darwin":
		paths = append(paths,
			"/System/Library/Fonts/PingFang.ttc",
			"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
		)
	default:
		paths = append(paths,
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed, err := parseOpenType(data)
		if err == nil {
			return parsed
		}
	}
	return nil
}

// parseOpenType 同时接受单字体 TTF/OTF 和字体集合 TTC，并从集合中选择第一个字体。
func parseOpenType(data []byte) (*opentype.Font, error) {
	if parsed, err := opentype.Parse(data); err == nil {
		return parsed, nil
	}
	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	return collection.Font(0)
}
