package ext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func init() { register("sms", "send", smsSend) }

// smsSend 经 HTTP 短信网关发送一条短信（通用原子，运营商解耦）。
//
// 凭据与网关地址**只从调用参数传**（红线：扩展原子不依赖环境参数，禁止 os.Getenv）——
// 缺失时按下面的语义处理，绝不回落到宿主进程的环境变量：环境变量是**进程级**的，
// 同一个原子被多个 Tool 调用时会隐式共享凭据，既无从追溯也会绕过密钥管理。
// 需要集中保管时用 secret 原语（`ctx.secret.get`）取值后由 Flow 注入 args。
// 参数语义与权威 schema（plugin/extensions.yaml 的 ext.sms.send）一致：
// gateway 必填，apikey / sign 选填（部分网关不需要签名）。
func smsSend(args map[string]interface{}) (interface{}, error) {
	if args["phone"] == nil {
		return nil, fmt.Errorf("phone 必填")
	}
	gateway := strOr(args, "gateway", "")
	if gateway == "" {
		return nil, fmt.Errorf("gateway 必填（短信网关 URL）")
	}
	apikey := strOr(args, "apikey", "")
	sign := strOr(args, "sign", "")
	payload := map[string]interface{}{
		"phone":       args["phone"],
		"template_id": strOr(args, "template_id", ""),
		"params":      args["params"],
		"content":     strOr(args, "content", ""),
		"sign":        sign,
		"apikey":      apikey,
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(gateway, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return map[string]interface{}{"status": resp.StatusCode, "body": string(rb)}, nil
}
