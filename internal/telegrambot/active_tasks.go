package telegrambot

// activeTaskState 是单个 Telegram 用户的任务占位。Cancelling 相当于一次 CAS 标记，
// 防止重复 /cancel 为同一任务启动无界 goroutine。
type activeTaskState struct {
	TaskID     string
	Cancelling bool
}

type cancellationClaim uint8

const (
	cancellationClaimed cancellationClaim = iota
	cancellationNoTask
	cancellationCreating
	cancellationAlreadyStarted
)

// reserveActive 原子地为用户预留任务位置，保证每个 Telegram 用户最多一个活动任务。
func (b *Bot) reserveActive(userID int64) bool {
	b.activeMu.Lock()
	defer b.activeMu.Unlock()
	if _, exists := b.active[userID]; exists {
		return false
	}
	b.active[userID] = activeTaskState{}
	return true
}

// setActiveTaskID 把创建阶段的空占位更新为稳定任务 ID。
func (b *Bot) setActiveTaskID(userID int64, taskID string) {
	b.activeMu.Lock()
	if state, exists := b.active[userID]; exists {
		state.TaskID = taskID
		b.active[userID] = state
	}
	b.activeMu.Unlock()
}

// claimCancellation 原子读取活动任务并标记取消已开始。返回值区分无任务、创建中和
// 已有取消请求，调用方只有在 cancellationClaimed 时才能启动取消 goroutine。
func (b *Bot) claimCancellation(userID int64) (string, cancellationClaim) {
	b.activeMu.Lock()
	defer b.activeMu.Unlock()
	state, exists := b.active[userID]
	if !exists {
		return "", cancellationNoTask
	}
	if state.TaskID == "" {
		return "", cancellationCreating
	}
	if state.Cancelling {
		return state.TaskID, cancellationAlreadyStarted
	}
	state.Cancelling = true
	b.active[userID] = state
	return state.TaskID, cancellationClaimed
}

// clearCancellation 只在取消调用失败时释放标记，使用户可以重试。成功请求保持标记，
// 直至等待任务终态的 goroutine 通过 clearActive 删除整个记录。
func (b *Bot) clearCancellation(userID int64, expectedTaskID string) {
	b.activeMu.Lock()
	if state, exists := b.active[userID]; exists && state.TaskID == expectedTaskID {
		state.Cancelling = false
		b.active[userID] = state
	}
	b.activeMu.Unlock()
}

// clearActive 仅删除仍指向 expectedTaskID 的记录，避免迟到终态清除后续任务。
func (b *Bot) clearActive(userID int64, expectedTaskID string) {
	b.activeMu.Lock()
	if state, exists := b.active[userID]; exists && state.TaskID == expectedTaskID {
		delete(b.active, userID)
	}
	b.activeMu.Unlock()
}
