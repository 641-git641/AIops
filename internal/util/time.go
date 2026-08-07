package util

import "time"

// 统一时钟入口，方便测试时注入。
var timeNow = time.Now

// SetClock 覆盖时钟（测试用）。
func SetClock(fn func() time.Time) {
	if fn != nil {
		timeNow = fn
	}
}

func now() time.Time { return timeNow() }
