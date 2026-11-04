package sandbox

import "sync"

// MaxOutput 是保留的输出字节数。失败原因通常在输出末尾，所以保留尾部。
const MaxOutput = 64 * 1024

// tailBuffer 是一个只保留最后 limit 字节的写入器，可以安全地被多个 goroutine 写入
// （exec.Cmd 的 stdout 和 stderr 指向同一个写入器时会并发写）。
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf), t.truncated
}
