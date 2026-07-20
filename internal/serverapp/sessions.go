package serverapp

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// session 表示单个管理员登录会话。token 用于 Cookie 查找，csrf 用于状态修改请求的
// 二次校验；两者是相互独立的密码学随机值。
type session struct {
	// token 是写入 HttpOnly Cookie、用于查找会话的随机值。
	token string
	// csrf 是独立于 Cookie 的随机值，状态修改请求必须显式回传。
	csrf string
	// userID/username 将会话绑定到一个具体管理员。授权判断使用稳定 ID，
	// username 只用于页面展示。
	userID   string
	username string
	// expires 是服务端判定会话失效的绝对时间。
	expires time.Time
}

// sessionStore 是进程内管理员会话存储。
// 所有访问都由 mu 串行化，因此多个 HTTP handler 可安全并发登录、校验和退出。会话不
// 进入 SQLite，服务重启会主动让所有管理员重新认证，也避免数据库保存可复用 Session。
type sessionStore struct {
	// mu 串行化会话 map 的创建、查询、惰性过期删除和退出删除。
	mu sync.Mutex
	// sessions 以 Cookie token 为键；内容只存在于当前服务端进程内存。
	sessions map[string]*session
}

// newSessionStore 创建空会话表。
func newSessionStore() *sessionStore { return &sessionStore{sessions: make(map[string]*session)} }

// create 生成一对独立随机 token/csrf 值，并登记一个绑定账户、12 小时有效的会话。
func (s *sessionStore) create(userID, username string) *session {
	value := &session{
		token: secureToken(), csrf: secureToken(), userID: userID, username: username,
		expires: time.Now().Add(12 * time.Hour),
	}
	s.mu.Lock()
	s.sessions[value.token] = value
	s.mu.Unlock()
	return value
}

// get 查找并验证会话。过期会话在访问时惰性删除；返回的 session 创建后不再修改，
// 因而释放锁后只读其字段是安全的。
func (s *sessionStore) get(token string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[token]
	if !ok || time.Now().After(value.expires) {
		delete(s.sessions, token)
		return nil, false
	}
	return value, true
}

// delete 使给定会话立即失效；删除不存在的 token 是幂等操作。
func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// deleteUser 主动撤销指定账户的全部会话。即使遗漏调用，session 的数据库
// 启用状态复验仍会在下一次请求时拒绝该账户。
func (s *sessionStore) deleteUser(userID string) {
	s.mu.Lock()
	for token, value := range s.sessions {
		if value.userID == userID {
			delete(s.sessions, token)
		}
	}
	s.mu.Unlock()
}

// secureToken 返回 32 字节密码学随机数的无填充 URL-safe Base64 表示，适合 Cookie 和
// HTTP Header。rand.Read 在受支持系统上正常不会失败；此处沿用当前接口的无错误返回约定。
func secureToken() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}
