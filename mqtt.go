package main

import (
	"fmt"
	"log"

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
	lastSeenValues       map[string]string
}

func NewMQTTReporter(client mqtt.Client, topic string) *MQTTReporter {
	reporter := &MQTTReporter{
		mqttClient:           client,
		budgetTopic:          fmt.Sprintf("stat/%s/budget", topic),
		evConnectedTopic:     fmt.Sprintf("stat/%s/ev_connected", topic),
		evSECurrentTopic:     fmt.Sprintf("stat/%s/evse_current", topic),
		evSETemperatureTopic: fmt.Sprintf("stat/%s/evse_temperature", topic),
		queue:                make(chan ToNotify, 10),
		lastSeenValues:       make(map[string]string),
	}

	go reporter.publishLoop(topic)

	return reporter
}

func (m MQTTReporter) publishSensorDiscoveryMessage(topic string, name string, unitOfMeasurement string, deviceClass string) {
	// Send a Home Assistant discovery message
	token := m.mqttClient.Publish(
		fmt.Sprintf("homeassistant/sensor/%s/%s/config", topic, name),
		0,
		true,
		fmt.Sprintf(`{"device": { "name": "Powerwall2mqtt", "identifiers": ["powerwall2mqtt"] }, "name": "%s", "state_topic": "stat/%s/%s", "unit_of_measurement": "%s", "device_class": "%s", "state_class": "measurement"}`, name, topic, name, unitOfMeasurement, deviceClass),
	)
	_ = token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("Error publishing Home Assistant discovery message: %s", err)
	}
}

func (m MQTTReporter) publishBinarySensorDiscoveryMessage(topic, name, deviceClass string) {
	// Send a Home Assistant discovery message
	token := m.mqttClient.Publish(
		fmt.Sprintf("homeassistant/binary_sensor/%s/%s/config", topic, name),
		0,
		true,
		fmt.Sprintf(`{"device": { "name": "Powerwall2mqtt", "identifiers": ["powerwall2mqtt"] }, "name": "%s", "state_topic": "stat/%s/%s", "device_class": "%s", "payload_on": "true", "payload_off": "false"}`, name, topic, name, deviceClass),
	)
	_ = token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("Error publishing Home Assistant discovery message: %s", err)
	}
}

func (m MQTTReporter) publishLoop(topic string) {
	m.publishSensorDiscoveryMessage(topic, "budget", "W", "power")
	m.publishBinarySensorDiscoveryMessage(topic, "ev_connected", "connectivity")
	m.publishSensorDiscoveryMessage(topic, "evse_current", "mA", "current")
	m.publishSensorDiscoveryMessage(topic, "evse_temperature", "°C", "temperature")

	for item := range m.queue {
		if m.lastSeenValues[item.topic] == item.payload {
			continue
		}

		token := m.mqttClient.Publish(item.topic, 0, false, item.payload)
		_ = token.Wait()
		if err := token.Error(); err != nil {
			log.Printf("Error publishing %s: %s", item.topic, err)
		} else {
			m.lastSeenValues[item.topic] = item.payload
		}
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
