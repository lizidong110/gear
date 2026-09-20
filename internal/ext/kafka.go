package ext

import (
	"context"
	"fmt"
	"strings"

	"github.com/segmentio/kafka-go"
)

func init() { register("kafka", "produce", kafkaProduce) }

func kafkaBrokers(args map[string]interface{}) []string {
	if b, ok := args["brokers"].(string); ok && b != "" {
		return strings.Split(b, ",")
	}
	if arr, ok := args["brokers"].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

func kafkaProduce(args map[string]interface{}) (interface{}, error) {
	topic := strOr(args, "topic", "")
	if topic == "" {
		return nil, fmt.Errorf("topic 必填")
	}
	brokers := kafkaBrokers(args)
	if len(brokers) == 0 {
		return nil, fmt.Errorf("brokers 必填（如 \"127.0.0.1:9092\"）")
	}
	key := strOr(args, "key", "")
	val := argToBytes(args["value"])
	w := &kafka.Writer{
		Addr:  kafka.TCP(brokers...),
		Topic: topic,
	}
	defer w.Close()
	msg := kafka.Message{Value: val}
	if key != "" {
		msg.Key = []byte(key)
	}
	if err := w.WriteMessages(context.Background(), msg); err != nil {
		return nil, fmt.Errorf("生产 Kafka 消息失败: %w", err)
	}
	return map[string]interface{}{"produced": true, "topic": topic}, nil
}
