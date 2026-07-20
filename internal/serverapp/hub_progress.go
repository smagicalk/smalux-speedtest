package serverapp

import "smalux-speedtest/internal/model"

const (
	progressPhaseInitialize = "初始化代理"
	progressPhaseDiscover   = "获取测速节点"
	progressPhaseLatency    = "延迟检测"
	progressPhaseDownload   = "下载测速"
	progressPhaseUpload     = "上传测速"
)

// normalizeProgress binds transient progress to the authenticated peer and its
// Assignment snapshot. Progress is not persisted, but it is forwarded to admin SSE;
// passing Client strings through unchanged would still expose credentials in a
// browser or reverse-proxy capture.
func (h *Hub) normalizeProgress(taskID string, connected *peer, input model.Progress) (model.Progress, bool) {
	if connected == nil {
		return model.Progress{}, false
	}
	h.mu.RLock()
	task := h.tasks[taskID]
	active := h.peers[connected.client.ID] == connected && task != nil && !task.terminalPending && task.targets[connected.client.ID] == "running"
	if !active {
		h.mu.RUnlock()
		return model.Progress{}, false
	}
	identity, exists := task.resultProxies[input.ProxyID]
	proxyCount := len(task.assignment.Proxies)
	candidateCount := task.assignment.CandidateCount
	topN := task.assignment.TopN
	h.mu.RUnlock()
	if !exists {
		return model.Progress{}, false
	}

	output := model.Progress{
		TaskID: taskID, ProxyID: input.ProxyID, ProxyName: identity.name, Phase: input.Phase,
	}
	limit := 0
	switch input.Phase {
	case progressPhaseInitialize:
		output.Message = identity.protocol
		limit = proxyCount
	case progressPhaseDiscover:
		output.Message = anonymousSpeedServerName
	case progressPhaseLatency:
		output.Message = anonymousSpeedServerName
		limit = candidateCount
	case progressPhaseDownload, progressPhaseUpload:
		output.Message = anonymousSpeedServerName
		limit = topN
		output.RateBPS = boundedMetric(input.RateBPS, maxResultSpeedBPS)
	default:
		return model.Progress{}, false
	}
	output.Current, output.Total = boundedProgressRange(input.Current, input.Total, limit)
	return output, true
}

func boundedProgressRange(current, total, limit int) (int, int) {
	if limit < 0 {
		limit = 0
	}
	if total < 0 {
		total = 0
	} else if total > limit {
		total = limit
	}
	if current < 0 {
		current = 0
	} else if current > total {
		current = total
	}
	return current, total
}
