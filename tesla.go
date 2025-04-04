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
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/publicsuffix"
)

type teslaClient struct {
	client                   *http.Client
	gatewayAddr              string
	password                 string
	debug                    bool
	batteryLevelGauge        *prometheus.GaugeVec
	energyExportedGauge      *prometheus.GaugeVec
	energyImportedGauge      *prometheus.GaugeVec
	energyLevelsGauge        *prometheus.GaugeVec
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
		Timeout: 15 * time.Second,
	}
}

func NewTEGClient(
	gatewayAddr string, password string, debug bool,
	batteryLevelGauge, energyExportedGauge, energyImportedGauge, energyLevelsGauge, powerGauge, gridServicesEnabledGauge *prometheus.GaugeVec,
) *teslaClient {
	return &teslaClient{
		gatewayAddr:              gatewayAddr,
		password:                 password,
		debug:                    debug,
		batteryLevelGauge:        batteryLevelGauge,
		energyExportedGauge:      energyExportedGauge,
		energyImportedGauge:      energyImportedGauge,
		energyLevelsGauge:        energyLevelsGauge,
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if c.debug {
		pretty := &bytes.Buffer{}
		if err := json.Indent(pretty, body, "", "  "); err != nil {
			return err
		}
		log.Printf("GET %s: %s", path, pretty.String())
	}

	if err := json.Unmarshal(body, result); err != nil {
		return err
	}

	if c.debug {
		log.Printf("%+v", result)
	}
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

type SystemStatus struct {
	NominalFullPackEnergyWh  float64 `json:"nominal_full_pack_energy"`
	NominalEnergyRemainingWh float64 `json:"nominal_energy_remaining"`
}

func (c *teslaClient) GetSystemStatus() (*SystemStatus, error) {
	var systemStatusResp SystemStatus
	err := getAPI(c, "/api/system_status", &systemStatusResp, func() {
		c.energyLevelsGauge.WithLabelValues("nominal-full-pack").Set(systemStatusResp.NominalFullPackEnergyWh)
		c.energyLevelsGauge.WithLabelValues("nominal-energy-remaning").Set(systemStatusResp.NominalEnergyRemainingWh)
	})

	if err != nil {
		return nil, err
	}
	return &systemStatusResp, nil
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

type OperationMode int

const (
	OperationUnknown OperationMode = iota
	OperationSelfConsumption
	OperationAutonomous
	OperationBackup
)

func (o OperationMode) String() string {
	switch o {
	case OperationSelfConsumption:
		return "self-consumption"
	case OperationBackup:
		return "backup"
	case OperationAutonomous:
		return "autonomous"
	default:
		return "unknown"
	}
}

func (o *OperationMode) UnmarshalJSON(b []byte) error {
	str := strings.Trim(string(b), `"`)

	switch str {
	case "autonomous":
		*o = OperationAutonomous
	case "self_consumption":
		*o = OperationSelfConsumption
	case "backup":
		*o = OperationBackup
	default:
		return fmt.Errorf("Unknown operation mode %s", str)
	}

	return nil
}

type Operation struct {
	BackupReservePercent float64       `json:"backup_reserve_percent"`
	Mode                 OperationMode `json:"real_mode"`
}

func (c *teslaClient) GetOperation() (*Operation, error) {
	var operation Operation
	err := getAPI(c, "/api/operation", &operation,
		func() {
			c.batteryLevelGauge.WithLabelValues("powerwall-reserve").Set(operation.BackupReservePercent)
		},
	)

	if err != nil {
		return nil, err
	}
	return &operation, nil
}
