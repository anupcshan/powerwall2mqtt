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
	brokerUrl := flag.String("broker", "", "Broker url (e.g., tcp://127.0.0.1:1883)")
	gridPowerInverseTopic := flag.String(
		"grid-inverse-topic",
		"powerwall/excess_power",
		"Topic to log inverse/negative of grid power to",
	)
	listen := flag.String("listen", ":9900", "Listen address for Prometheus handler")

	flag.Parse()

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
			AddBroker(*brokerUrl).
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

	prometheus.MustRegister(energyExportedGuage, energyImportedGuage, powerGauge)

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

		<-ticker.C
	}
}
