package ext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func init() { register("wecom", "send", wecomSend) }

func wecomSend(args map[string]interface{}) (interface{}, error) {
	// 同 smsSend：凭据只从 args 传，不回落环境变量（红线：扩展原子不依赖环境参数）。
	key := strOr(args, "key", "")
	if key == "" {
		return nil, fmt.Errorf("key 必填（群机器人 key）")
	}
	content := strOr(args, "content", "")
	if content == "" {
		return nil, fmt.Errorf("content 必填")
	}
	msgtype := strOr(args, "msgtype", "text")
	if msgtype != "text" && msgtype != "markdown" {
		return nil, fmt.Errorf("msgtype 仅支持 text/markdown")
	}
	payload := map[string]interface{}{"msgtype": msgtype}
	if msgtype == "text" {
		t := map[string]interface{}{"content": content}
		if m := arrOr(args, "mentioned_list"); len(m) > 0 {
			t["mentioned_list"] = m
		}
		if m := arrOr(args, "mentioned_mobile_list"); len(m) > 0 {
			t["mentioned_mobile_list"] = m
		}
		payload["text"] = t
	} else {
		payload["markdown"] = map[string]interface{}{"content": content}
	}
	body, _ := json.Marshal(payload)
	u := "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + key
	resp, err := http.Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	json.Unmarshal(rb, &r)
	if r.ErrCode != 0 {
		return nil, fmt.Errorf("企业微信返回错误 errcode=%d errmsg=%s", r.ErrCode, r.ErrMsg)
	}
	return map[string]interface{}{"ok": true}, nil
}
