package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

type openEVSEClient struct {
	client              *http.Client
	openEVSEAddr        string
	currentGauge        *prometheus.GaugeVec
	energyImportedGauge *prometheus.GaugeVec
	powerGauge          *prometheus.GaugeVec
	tempGauge           *prometheus.GaugeVec
	connectedGauge      *prometheus.GaugeVec
}

type EVSEStatus struct {
	MilliAmp      int64   `json:"amp"`
	Temp          int64   `json:"temp"`
	Pilot         int64   `json:"pilot"`
	Voltage       int64   `json:"voltage"`
	TotalEnergy   float64 `json:"total_energy"`
	Vehicle       int64   `json:"vehicle"`
	MQTTConnected int64   `json:"mqtt_connected"`
}

func (c *openEVSEClient) GetStatus() (*EVSEStatus, error) {
	resp, err := c.client.Get(fmt.Sprintf("http://%s/status", c.openEVSEAddr))
	if err != nil {
		return nil, err
	}

	var evStatusResp EVSEStatus

	if err := json.NewDecoder(resp.Body).Decode(&evStatusResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	c.currentGauge.WithLabelValues("ev").Set(float64(evStatusResp.MilliAmp) / 1000)
	c.energyImportedGauge.WithLabelValues("ev").Set(evStatusResp.TotalEnergy * 1000)
	c.powerGauge.WithLabelValues("ev").Set(float64(evStatusResp.Voltage*evStatusResp.MilliAmp) / 1000)
	c.tempGauge.WithLabelValues("ev").Set(float64(evStatusResp.Temp) / 10)
	c.connectedGauge.WithLabelValues("ev").Set(float64(evStatusResp.Vehicle))
	c.connectedGauge.WithLabelValues("mqtt").Set(float64(evStatusResp.MQTTConnected))

	return &evStatusResp, nil
}
