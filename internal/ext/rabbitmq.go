package ext

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

func init() {
	register("rabbitmq", "publish", rabbitmqPublish)
	register("rabbitmq", "declare", rabbitmqDeclare)
}

// amqpURL 解析连接串：优先 url 参数，否则由 host/port/user/pass 拼一个默认直连串。
func amqpURL(args map[string]interface{}) string {
	if u := strOr(args, "url", ""); u != "" {
		return u
	}
	host := strOr(args, "host", "localhost")
	port := intOr(args, "port", 5672)
	user := strOr(args, "user", "guest")
	pass := strOr(args, "pass", "guest")
	return fmt.Sprintf("amqp://%s:%s@%s:%d/", user, pass, host, port)
}

func rabbitmqPublish(args map[string]interface{}) (interface{}, error) {
	conn, err := amqp.Dial(amqpURL(args))
	if err != nil {
		return nil, fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	exchange := strOr(args, "exchange", "")
	routingKey := strOr(args, "routing_key", "")
	body := argToBytes(args["body"])
	contentType := strOr(args, "content_type", "")
	if contentType == "" {
		if isJSON(body) {
			contentType = "application/json"
		} else {
			contentType = "text/plain"
		}
	}
	pub := amqp.Publishing{
		ContentType:  contentType,
		Body:         body,
		DeliveryMode: amqp.Transient,
	}
	if boolOr(args, "persistent", true) {
		pub.DeliveryMode = amqp.Persistent
	}
	if hdr := mapOr(args, "headers"); len(hdr) > 0 {
		pub.Headers = amqp.Table(hdr)
	}
	if err := ch.PublishWithContext(context.Background(), exchange, routingKey, false, false, pub); err != nil {
		return nil, err
	}
	return map[string]interface{}{"delivered": true, "exchange": exchange, "routing_key": routingKey}, nil
}

func rabbitmqDeclare(args map[string]interface{}) (interface{}, error) {
	kind := strOr(args, "kind", "")
	if kind != "queue" && kind != "exchange" {
		return nil, fmt.Errorf("kind 必填 queue|exchange")
	}
	name := strOr(args, "name", "")
	if name == "" {
		return nil, fmt.Errorf("name 必填")
	}
	conn, err := amqp.Dial(amqpURL(args))
	if err != nil {
		return nil, fmt.Errorf("连接 RabbitMQ 失败: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	durable := boolOr(args, "durable", true)
	if kind == "queue" {
		if _, err := ch.QueueDeclare(name, durable, false, false, false, nil); err != nil {
			return nil, err
		}
	} else {
		et := strOr(args, "exchange_type", "direct")
		if err := ch.ExchangeDeclare(name, et, durable, false, false, false, nil); err != nil {
			return nil, err
		}
	}
	return map[string]interface{}{"declared": kind, "name": name}, nil
}
