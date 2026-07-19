package importer

import (
	"encoding/base64"
	"errors"
	"strings"
)

// decodeBase64 依次兼容标准/URL-safe 字母表及有填充/无填充形式。
// 分享链接生态并不统一保留末尾 '='，因此不能只使用一种 Encoding。
func decodeBase64(value string) (string, error) {
	value = strings.TrimSpace(value)
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("invalid base64")
}
