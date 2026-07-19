package wire

import (
	"encoding/json"

	"smalux-speedtest/internal/model"
)

const (
	TypeHello        = "client.hello"
	TypeWelcome      = "server.welcome"
	TypePing         = "ping"
	TypePong         = "pong"
	TypeTaskAssign   = "task.assign"
	TypeTaskAck      = "task.ack"
	TypeTaskProgress = "task.progress"
	TypeTaskResult   = "task.result"
	TypeTaskComplete = "task.complete"
	TypeTaskFailed   = "task.failed"
	TypeTaskCancel   = "task.cancel"
)

type Envelope struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	MessageID string          `json:"message_id"`
	TaskID    string          `json:"task_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func New(messageType, taskID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Version:   model.ProtocolVersion,
		Type:      messageType,
		MessageID: model.NewID(),
		TaskID:    taskID,
		Payload:   raw,
	}, nil
}

func Decode[T any](message Envelope) (T, error) {
	var value T
	err := json.Unmarshal(message.Payload, &value)
	return value, err
}
