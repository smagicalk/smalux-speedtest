package wire

import (
	"strings"
	"testing"
)

// TestEncodedSizeMeasuresFinalEnvelope 确认尺寸函数包含 Envelope 元数据而不只是 Payload，
// 使任务服务的检查值与 WebSocket Reader 实际接收的 JSON 字节一致。
func TestEncodedSizeMeasuresFinalEnvelope(t *testing.T) {
	message, err := New(TypeTaskAssign, "task", map[string]string{"value": strings.Repeat("x", 128)})
	if err != nil {
		t.Fatal(err)
	}
	size, err := EncodedSize(message)
	if err != nil {
		t.Fatal(err)
	}
	if size <= len(message.Payload) || size >= MaxServerToClientMessageBytes {
		t.Fatalf("encoded size = %d, payload = %d", size, len(message.Payload))
	}
}
