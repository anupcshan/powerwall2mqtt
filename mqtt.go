package main

import (
	"fmt"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Reporter interface {
	ReportBudget(watts int32)
}

type NoopReporter struct {
}

func (NoopReporter) ReportBudget(int32) {}

type ToNotify struct {
	topic   string
	payload string
}

type MQTTReporter struct {
	mqttClient mqtt.Client

	budgetTopic string
	queue       chan ToNotify
}

func NewMQTTReporter(client mqtt.Client, topic string) *MQTTReporter {
	reporter := &MQTTReporter{
		mqttClient:  client,
		budgetTopic: fmt.Sprintf("stat/%s/budget", topic),
		queue:       make(chan ToNotify, 10),
	}

	go reporter.publishLoop()

	return reporter
}

func (m MQTTReporter) publishLoop() {
	for item := range m.queue {
		token := m.mqttClient.Publish(item.topic, 0, false, item.payload)
		_ = token.Wait()
		_ = token.Error()
	}
}

func (m MQTTReporter) ReportBudget(watts int32) {
	select {
	case m.queue <- ToNotify{m.budgetTopic, fmt.Sprintf("%d", watts)}:
	default:
	}
}
