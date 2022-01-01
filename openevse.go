package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	vehicleGauge        *prometheus.GaugeVec
}

type EVSEConfig struct {
	ChargeMode string `json:"charge_mode"`
}

type EVSEStatus struct {
	MilliAmp int64   `json:"amp"`
	Temp     int64   `json:"temp"`
	Pilot    int64   `json:"pilot"`
	Voltage  int64   `json:"voltage"`
	WattHour float64 `json:"watthour"`
	WattSec  float64 `json:"wattsec"`
	Vehicle  int64   `json:"vehicle"`
}

func (c *openEVSEClient) GetConfig() (*EVSEConfig, error) {
	// Fetch current EVSE config
	resp, err := c.client.Get(fmt.Sprintf("http://%s/config", c.openEVSEAddr))
	if err != nil {
		return nil, err
	}

	var evConfigResp EVSEConfig

	if err := json.NewDecoder(resp.Body).Decode(&evConfigResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	log.Printf("%+v", evConfigResp)

	return &evConfigResp, nil
}

func (c *openEVSEClient) SetConfig(cfg EVSEConfig) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	resp, err := c.client.Post(fmt.Sprintf("http://%s/config", c.openEVSEAddr), "application/json", &buf)
	if err != nil {
		return err
	}

	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
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

	log.Printf("%+v", evStatusResp)
	c.currentGauge.WithLabelValues("ev").Set(float64(evStatusResp.MilliAmp) / 1000)
	c.energyImportedGauge.WithLabelValues("ev").Set(float64(evStatusResp.WattHour) + float64(evStatusResp.WattSec)/3600.0)
	c.powerGauge.WithLabelValues("ev").Set(float64(evStatusResp.Voltage*evStatusResp.MilliAmp) / 1000)
	c.tempGauge.WithLabelValues("ev").Set(float64(evStatusResp.Temp) / 10)
	c.vehicleGauge.WithLabelValues("ev").Set(float64(evStatusResp.Vehicle))

	return &evStatusResp, nil
}
