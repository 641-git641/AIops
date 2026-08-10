package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// llmStatusError 先查状态码再解析错误体（错误体非 JSON 时仍返回状态码信息）。
func llmStatusError(resp *http.Response) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var errResp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != nil {
		msg = errResp.Error.Message
	}
	return fmt.Errorf("llm status %d: %s", resp.StatusCode, msg)
}
