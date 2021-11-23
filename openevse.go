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
	openEVSEAddr        string
	energyImportedGauge *prometheus.GaugeVec
	powerGauge          *prometheus.GaugeVec
}

type EVSEConfig struct {
	ChargeMode string `json:"charge_mode"`
}

type EVSEStatus struct {
	MilliAmp int64   `json:"amp"`
	Pilot    int64   `json:"pilot"`
	Voltage  int64   `json:"voltage"`
	WattHour float64 `json:"watthour"`
	WattSec  float64 `json:"wattsec"`
}

func (c *openEVSEClient) GetConfig() (*EVSEConfig, error) {
	// Fetch current EVSE config
	resp, err := http.Get(fmt.Sprintf("http://%s/config", c.openEVSEAddr))
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
	resp, err := http.Post(fmt.Sprintf("http://%s/config", c.openEVSEAddr), "application/json", &buf)
	if err != nil {
		return err
	}

	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

func (c *openEVSEClient) GetStatus() (*EVSEStatus, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s/status", c.openEVSEAddr))
	if err != nil {
		return nil, err
	}

	var evStatusResp EVSEStatus

	if err := json.NewDecoder(resp.Body).Decode(&evStatusResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	log.Printf("%+v", evStatusResp)
	c.powerGauge.WithLabelValues("ev").Set(float64(evStatusResp.Voltage*evStatusResp.MilliAmp) / 1000)
	c.energyImportedGauge.WithLabelValues("ev").Set(float64(evStatusResp.WattHour) + float64(evStatusResp.WattSec)/3600.0)

	return &evStatusResp, nil
}
