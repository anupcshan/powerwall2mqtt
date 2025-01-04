package main

import (
	"fmt"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Reporter interface {
	ReportBudget(watts int32)
	ReportEVConnected(connected bool)
	ReportEVSECurrent(milliamps int64)
	ReportEVSETemperature(temp Temperature)
}

type NoopReporter struct {
}

func (NoopReporter) ReportBudget(int32)                {}
func (NoopReporter) ReportEVConnected(bool)            {}
func (NoopReporter) ReportEVSECurrent(int64)           {}
func (NoopReporter) ReportEVSETemperature(Temperature) {}

type ToNotify struct {
	topic   string
	payload string
}

type MQTTReporter struct {
	mqttClient mqtt.Client

	budgetTopic          string
	evConnectedTopic     string
	evSECurrentTopic     string
	evSETemperatureTopic string
	queue                chan ToNotify
}

func NewMQTTReporter(client mqtt.Client, topic string) *MQTTReporter {
	reporter := &MQTTReporter{
		mqttClient:           client,
		budgetTopic:          fmt.Sprintf("stat/%s/budget", topic),
		evConnectedTopic:     fmt.Sprintf("stat/%s/ev_connected", topic),
		evSECurrentTopic:     fmt.Sprintf("stat/%s/evse_current", topic),
		evSETemperatureTopic: fmt.Sprintf("stat/%s/evse_temperature", topic),
		queue:                make(chan ToNotify, 10),
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

func (m MQTTReporter) ReportEVConnected(connected bool) {
	select {
	case m.queue <- ToNotify{m.evConnectedTopic, fmt.Sprintf("%t", connected)}:
	default:
	}
}

func (m MQTTReporter) ReportEVSECurrent(milliamps int64) {
	select {
	case m.queue <- ToNotify{m.evSECurrentTopic, fmt.Sprintf("%d", milliamps)}:
	default:
	}
}

func (m MQTTReporter) ReportEVSETemperature(temp Temperature) {
	select {
	case m.queue <- ToNotify{m.evSETemperatureTopic, fmt.Sprintf("%f", temp.ToCelsius())}:
	default:
	}
}
