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

func getAPI[T any](c *teslaClient, path string, result T, reportMetrics func()) error {
	resp, err := c.client.Get(fmt.Sprintf("https://%s%s", c.gatewayAddr, path))
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return err
	}

	log.Printf("%+v", result)
	reportMetrics()
	return nil
}

type Meters map[string]struct {
	InstantPower   float64 `json:"instant_power"`
	EnergyExported float64 `json:"energy_exported"`
	EnergyImported float64 `json:"energy_imported"`
}

func (c *teslaClient) GetMeterAggregates() (Meters, error) {
	var metersResp Meters
	err := getAPI(c, "/api/meters/aggregates", &metersResp,
		func() {
			for label, v := range metersResp {
				c.energyExportedGauge.WithLabelValues(label).Set(v.EnergyExported)
				c.energyImportedGauge.WithLabelValues(label).Set(v.EnergyImported)
				c.powerGauge.WithLabelValues(label).Set(v.InstantPower)
			}
		},
	)

	if err != nil {
		return nil, err
	}
	return metersResp, nil
}

type Soe struct {
	Percentage float64 `json:"percentage"`
}

func (c *teslaClient) GetStateOfEnergy() (*Soe, error) {
	var soeResp Soe
	err := getAPI(c, "/api/system_status/soe", &soeResp,
		func() {
			c.batteryLevelGauge.WithLabelValues("powerwall").Set(soeResp.Percentage)
		},
	)

	if err != nil {
		return nil, err
	}
	return &soeResp, nil
}

type GridStatus struct {
	GridServicesActive bool `json:"grid_services_active"`
}

func (c *teslaClient) GetGridStatus() (*GridStatus, error) {
	var gridStatusResp GridStatus
	err := getAPI(c, "/api/system_status/grid_status", &gridStatusResp,
		func() {
			var val float64 = 0
			if gridStatusResp.GridServicesActive {
				val = 1
			}
			c.gridServicesEnabledGauge.WithLabelValues("powerwall").Set(val)
		},
	)

	if err != nil {
		return nil, err
	}
	return &gridStatusResp, nil
}
