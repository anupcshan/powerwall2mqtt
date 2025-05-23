package main

import (
	"fmt"
	"log"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Reporter interface {
	ReportBudget(watts int32)
	ReportLoad(watts int32)
	ReportEVConnected(connected bool)
	ReportEVSECurrent(milliamps int64)
	ReportEVSETemperature(temp Temperature)
	ReportSolar(watts int32)
	ReportBatteryExport(watts int32)
	ReportBatteryLevel(percent float64)
}

type NoopReporter struct {
}

func (NoopReporter) ReportBudget(int32)                {}
func (NoopReporter) ReportLoad(int32)                  {}
func (NoopReporter) ReportEVConnected(bool)            {}
func (NoopReporter) ReportEVSECurrent(int64)           {}
func (NoopReporter) ReportEVSETemperature(Temperature) {}
func (NoopReporter) ReportSolar(int32)                 {}
func (NoopReporter) ReportBatteryExport(int32)         {}
func (NoopReporter) ReportBatteryLevel(float64)        {}

type ToNotify struct {
	topic   string
	payload string
}

type MQTTReporter struct {
	mqttClient mqtt.Client

	budgetTopic          string
	loadTopic            string
	evConnectedTopic     string
	evSECurrentTopic     string
	evSETemperatureTopic string
	solarTopic           string
	batteryExportTopic   string
	batteryLevelTopic    string
	queue                chan ToNotify
	lastSeenValues       map[string]string
}

func NewMQTTReporter(client mqtt.Client, topic string) *MQTTReporter {
	reporter := &MQTTReporter{
		mqttClient:           client,
		budgetTopic:          fmt.Sprintf("stat/%s/budget", topic),
		loadTopic:            fmt.Sprintf("stat/%s/load", topic),
		evConnectedTopic:     fmt.Sprintf("stat/%s/ev_connected", topic),
		evSECurrentTopic:     fmt.Sprintf("stat/%s/evse_current", topic),
		evSETemperatureTopic: fmt.Sprintf("stat/%s/evse_temperature", topic),
		solarTopic:           fmt.Sprintf("stat/%s/solar", topic),
		batteryExportTopic:   fmt.Sprintf("stat/%s/battery_export", topic),
		batteryLevelTopic:    fmt.Sprintf("stat/%s/battery_level", topic),
		queue:                make(chan ToNotify, 10),
		lastSeenValues:       make(map[string]string),
	}

	go reporter.publishLoop(topic)

	return reporter
}

func (m MQTTReporter) publishSensorDiscoveryMessage(topic, name, friendlyName, unitOfMeasurement, deviceClass string) {
	// Send a Home Assistant discovery message
	token := m.mqttClient.Publish(
		fmt.Sprintf("homeassistant/sensor/%s/%s/config", topic, name),
		0,
		true,
		fmt.Sprintf(`{"device": { "name": "Powerwall2mqtt", "identifiers": ["powerwall2mqtt"] }, "name": "%s", "state_topic": "stat/%s/%s", "unit_of_measurement": "%s", "device_class": "%s", "state_class": "measurement"}`, friendlyName, topic, name, unitOfMeasurement, deviceClass),
	)
	_ = token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("Error publishing Home Assistant discovery message: %s", err)
	}
}

func (m MQTTReporter) publishBinarySensorDiscoveryMessage(topic, name, friendlyName, deviceClass string) {
	// Send a Home Assistant discovery message
	token := m.mqttClient.Publish(
		fmt.Sprintf("homeassistant/binary_sensor/%s/%s/config", topic, name),
		0,
		true,
		fmt.Sprintf(`{"device": { "name": "Powerwall2mqtt", "identifiers": ["powerwall2mqtt"] }, "name": "%s", "state_topic": "stat/%s/%s", "device_class": "%s", "payload_on": "true", "payload_off": "false"}`, friendlyName, topic, name, deviceClass),
	)
	_ = token.Wait()
	if err := token.Error(); err != nil {
		log.Printf("Error publishing Home Assistant discovery message: %s", err)
	}
}

func (m MQTTReporter) publishLoop(topic string) {
	m.publishSensorDiscoveryMessage(topic, "budget", "Power Budget", "W", "power")
	m.publishSensorDiscoveryMessage(topic, "load", "Load Power", "W", "power")
	m.publishBinarySensorDiscoveryMessage(topic, "ev_connected", "EV Connected", "connectivity")
	m.publishSensorDiscoveryMessage(topic, "evse_current", "EVSE Current", "mA", "current")
	m.publishSensorDiscoveryMessage(topic, "evse_temperature", "EVSE Temperature", "°C", "temperature")
	m.publishSensorDiscoveryMessage(topic, "solar", "Solar Power", "W", "power")
	m.publishSensorDiscoveryMessage(topic, "battery_export", "Battery Export", "W", "power")
	m.publishSensorDiscoveryMessage(topic, "battery_level", "Battery Level", "%", "battery")

	for item := range m.queue {
		if m.lastSeenValues[item.topic] == item.payload {
			continue
		}

		token := m.mqttClient.Publish(item.topic, 0, true, item.payload)
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

func (m MQTTReporter) ReportLoad(watts int32) {
	select {
	case m.queue <- ToNotify{m.loadTopic, fmt.Sprintf("%d", watts)}:
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

func (m MQTTReporter) ReportSolar(watts int32) {
	select {
	case m.queue <- ToNotify{m.solarTopic, fmt.Sprintf("%d", watts)}:
	default:
	}
}

func (m MQTTReporter) ReportBatteryExport(watts int32) {
	select {
	case m.queue <- ToNotify{m.batteryExportTopic, fmt.Sprintf("%d", watts)}:
	default:
	}
}

func (m MQTTReporter) ReportBatteryLevel(percent float64) {
	select {
	case m.queue <- ToNotify{m.batteryLevelTopic, fmt.Sprintf("%.1f", percent)}:
	default:
	}
}
