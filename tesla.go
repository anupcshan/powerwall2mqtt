package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/publicsuffix"
)

type teslaClient struct {
	client                   *http.Client
	gatewayAddr              string
	password                 string
	batteryLevelGauge        *prometheus.GaugeVec
	energyExportedGauge      *prometheus.GaugeVec
	energyImportedGauge      *prometheus.GaugeVec
	gridServicesEnabledGauge *prometheus.GaugeVec
	powerGauge               *prometheus.GaugeVec
}

func newHTTPClient() *http.Client {
	// Should never fail
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})

	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func NewTEGClient(gatewayAddr string, password string,
	batteryLevelGauge, energyExportedGauge, energyImportedGauge, powerGauge, gridServicesEnabledGauge *prometheus.GaugeVec,
) *teslaClient {
	return &teslaClient{
		gatewayAddr:              gatewayAddr,
		password:                 password,
		batteryLevelGauge:        batteryLevelGauge,
		energyExportedGauge:      energyExportedGauge,
		energyImportedGauge:      energyImportedGauge,
		powerGauge:               powerGauge,
		gridServicesEnabledGauge: gridServicesEnabledGauge,
	}
}

func (c *teslaClient) Login() error {
	// Clear cookie jar and create a fresh client
	c.client = newHTTPClient()

	var buf bytes.Buffer

	loginReq := struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Email      string `json:"email"`
		ForceSmOff bool   `json:"force_sm_off"`
	}{
		Username:   "customer",
		Password:   c.password,
		Email:      "hello@example.com",
		ForceSmOff: false,
	}

	if err := json.NewEncoder(&buf).Encode(&loginReq); err != nil {
		// If this fails, there really is no point retrying
		return err
	}

	resp, err := c.client.Post(fmt.Sprintf("https://%s/api/login/Basic", c.gatewayAddr), "application/json", &buf)
	if err != nil {
		return err
	}

	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return nil
}

type Meters map[string]struct {
	InstantPower   float64 `json:"instant_power"`
	EnergyExported float64 `json:"energy_exported"`
	EnergyImported float64 `json:"energy_imported"`
}

func (c *teslaClient) GetMeterAggregates() (Meters, error) {
	resp, err := c.client.Get(fmt.Sprintf("https://%s/api/meters/aggregates", c.gatewayAddr))
	if err != nil {
		return nil, err
	}

	var metersResp Meters
	if err := json.NewDecoder(resp.Body).Decode(&metersResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	log.Printf("%+v", metersResp)

	for label, v := range metersResp {
		c.energyExportedGauge.WithLabelValues(label).Set(v.EnergyExported)
		c.energyImportedGauge.WithLabelValues(label).Set(v.EnergyImported)
		c.powerGauge.WithLabelValues(label).Set(v.InstantPower)
	}

	return metersResp, nil
}

type Soe struct {
	Percentage float64 `json:"percentage"`
}

func (c *teslaClient) GetStateOfEnergy() (*Soe, error) {
	resp, err := c.client.Get(fmt.Sprintf("https://%s/api/system_status/soe", c.gatewayAddr))
	if err != nil {
		return nil, err
	}

	var soeResp Soe
	if err := json.NewDecoder(resp.Body).Decode(&soeResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	log.Printf("%+v", soeResp)
	c.batteryLevelGauge.WithLabelValues("powerwall").Set(soeResp.Percentage)

	return &soeResp, nil
}

type GridStatus struct {
	GridServicesActive bool `json:"grid_services_active"`
}

func (c *teslaClient) GetGridStatus() (*GridStatus, error) {
	resp, err := c.client.Get(fmt.Sprintf("https://%s/api/system_status/grid_status", c.gatewayAddr))
	if err != nil {
		return nil, err
	}

	var gridStatusResp GridStatus
	if err := json.NewDecoder(resp.Body).Decode(&gridStatusResp); err != nil {
		return nil, err
	}
	_ = resp.Body.Close()

	log.Printf("%+v", gridStatusResp)

	var val float64 = 0
	if gridStatusResp.GridServicesActive {
		val = 1
	}
	c.gridServicesEnabledGauge.WithLabelValues("powerwall").Set(val)

	return &gridStatusResp, nil
}
