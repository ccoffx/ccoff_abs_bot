package telegram

import (
	"sync"
	"time"
)

// ==========================================
// 🛡️ 状态机底层：高并发安全封装与防泄漏
// ==========================================
type SessionState struct {
	step      string            // 私有化，强制走方法
	tempData  map[string]string // 私有化，强制走方法
	mu        sync.RWMutex      // 升级为读写锁，提升超高频读取时的并发性能
	updatedAt time.Time         // 🛡️ 新增：最后活跃时间，供清道夫协程识别僵尸会话
}

var CallbackAckStates sync.Map

// SetStep 安全写入当前步骤
func (s *SessionState) SetStep(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step = step
	s.updatedAt = time.Now()
}

// GetStep 安全读取当前步骤
func (s *SessionState) GetStep() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.step
}

// SetTemp 安全写入临时数据
func (s *SessionState) SetTemp(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tempData == nil {
		s.tempData = make(map[string]string)
	}
	s.tempData[key] = val
	s.updatedAt = time.Now()
}

// GetTemp 安全读取临时数据
func (s *SessionState) GetTemp(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tempData == nil {
		return ""
	}
	return s.tempData[key]
}

var UserSessions sync.Map

var SweeperOnce sync.Once

var groupMemberCache sync.Map

const (
	groupMemberPositiveTTL = 5 * time.Minute
	groupMemberNegativeTTL = 1 * time.Minute
	groupMemberFreshTTL    = 30 * time.Second
)

func GetSession(userID int64) *SessionState {
	val, _ := UserSessions.LoadOrStore(userID, &SessionState{
		step:      "IDLE",
		tempData:  make(map[string]string),
		updatedAt: time.Now(),
	})
	return val.(*SessionState)
}

func ClearSession(userID int64) {
	UserSessions.Delete(userID)
}
