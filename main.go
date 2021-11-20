package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/publicsuffix"
)

var labels = []string{"meter"}

func main() {
	log.SetFlags(log.Lshortfile | log.Ltime)

	powerwallIP := flag.String("powerwall-ip", "", "Powerwall IP")
	password := flag.String("password", "", "Powerwall password")
	pollingInterval := flag.Duration("poll-interval", 10*time.Second, "Polling interval")
	brokerURL := flag.String("broker", "", "Broker url (e.g., tcp://127.0.0.1:1883)")
	gridPowerInverseTopic := flag.String("grid-inverse-topic", "powerwall/excess_power", "Topic to log inverse/negative of grid power to")
	openEVSEAddr := flag.String("openevse", "", "OpenEVSE address (like 192.168.X.X or openevse.local)")
	listen := flag.String("listen", ":9900", "Listen address for Prometheus handler")

	flag.Parse()

	if *powerwallIP == "" {
		log.Fatal("Powerwall IP not provided")
	}

	if *brokerURL == "" {
		log.Fatal("Broker URL not provided")
	}

	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		log.Fatal(err)
	}

	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var buf bytes.Buffer

	loginReq := struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Email      string `json:"email"`
		ForceSmOff bool   `json:"force_sm_off"`
	}{
		Username:   "customer",
		Password:   *password,
		Email:      "hello@example.com",
		ForceSmOff: false,
	}

	if err := json.NewEncoder(&buf).Encode(&loginReq); err != nil {
		log.Fatal(err)
	}

	resp, err := client.Post(fmt.Sprintf("https://%s/api/login/Basic", *powerwallIP), "application/json", &buf)
	if err != nil {
		log.Fatal(err)
	}

	io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	mqttClient := mqtt.NewClient(
		mqtt.NewClientOptions().
			AddBroker(*brokerURL).
			SetAutoReconnect(true),
	)

	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Error connecting to MQTT: %s", token.Error())
	}

	ticker := time.NewTicker(*pollingInterval)

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(*listen, nil)
	}()

	batteryLevelGuage := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "battery_percentage",
		Help:      "Powerwall battery level percentage (0-100)",
	})

	energyExportedGuage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "energy_exported",
		Help:      "Total energy exported from individual meters (Wh)",
	}, labels)

	energyImportedGuage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "energy_imported",
		Help:      "Total energy imported from individual meters (Wh)",
	}, labels)

	powerGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "instantaneous_power",
		Help:      "Instantaneous power of individual CT clamps (W)",
	}, labels)

	prometheus.MustRegister(batteryLevelGuage, energyExportedGuage, energyImportedGuage, powerGauge)

	for {
		resp, err = client.Get(fmt.Sprintf("https://%s/api/meters/aggregates", *powerwallIP))
		if err != nil {
			log.Fatal(err)
		}

		var metersResp map[string]struct {
			InstantPower   float64 `json:"instant_power"`
			EnergyExported float64 `json:"energy_exported"`
			EnergyImported float64 `json:"energy_imported"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&metersResp); err != nil {
			log.Fatal(err)
		}
		_ = resp.Body.Close()

		log.Printf("%+v", metersResp)

		for label, v := range metersResp {
			energyExportedGuage.WithLabelValues(label).Set(v.EnergyExported)
			energyImportedGuage.WithLabelValues(label).Set(v.EnergyImported)
			powerGauge.WithLabelValues(label).Set(v.InstantPower)
		}

		if v, ok := metersResp["site"]; ok {
			token := mqttClient.Publish(*gridPowerInverseTopic, 0, false, fmt.Sprintf("%f", -v.InstantPower))
			_ = token.Wait()
			if err := token.Error(); err != nil {
				log.Fatal(err)
			}
		}

		resp, err = client.Get(fmt.Sprintf("https://%s/api/system_status/soe", *powerwallIP))
		if err != nil {
			log.Fatal(err)
		}

		var soeResp struct {
			Percentage float64 `json:"percentage"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&soeResp); err != nil {
			log.Fatal(err)
		}
		_ = resp.Body.Close()

		log.Printf("%+v", soeResp)

		batteryLevelGuage.Set(soeResp.Percentage)

		if *openEVSEAddr != "" {
			resp, err = http.Get(fmt.Sprintf("http://%s/status", *openEVSEAddr))
			if err != nil {
				log.Fatal(err)
			}

			var evStatusResp struct {
				MilliAmp int64   `json:"amp"`
				Pilot    int64   `json:"pilot"`
				Voltage  int64   `json:"voltage"`
				WattHour float64 `json:"watthour"`
				WattSec  float64 `json:"wattsec"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&evStatusResp); err != nil {
				log.Fatal(err)
			}
			_ = resp.Body.Close()

			log.Printf("%+v", evStatusResp)
			powerGauge.WithLabelValues("ev").Set(float64(evStatusResp.Voltage*evStatusResp.MilliAmp) / 1000)
			energyImportedGuage.WithLabelValues("ev").Set(float64(evStatusResp.WattHour) + float64(evStatusResp.WattSec)/3600.0)
		}

		<-ticker.C
	}
}
