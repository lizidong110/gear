package ext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func init() { register("webhook", "send", webhookSend) }

func webhookSend(args map[string]interface{}) (interface{}, error) {
	url := strOr(args, "url", "")
	if url == "" {
		return nil, fmt.Errorf("url 必填")
	}
	method := strings.ToUpper(strOr(args, "method", "POST"))
	payload := args["payload"]
	if payload == nil {
		return nil, fmt.Errorf("payload 必填")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("payload 序列化失败: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range mapOr(args, "headers") {
		req.Header.Set(k, fmt.Sprint(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return map[string]interface{}{
		"status": resp.StatusCode,
		"body":   string(respBody),
	}, nil
}
