package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	dryRun := flag.Bool("dry-run", true, "Dry run mode (disable any writes in dry run mode)")
	evChargeLevelTopic := flag.String("ev-charge-level-topic", "", "MQTT topic with the most recently polled EV charge level (from onstar2mqtt)")

	flag.Parse()

	if *powerwallIP == "" {
		log.Fatal("Powerwall IP not provided")
	}

	if *brokerURL == "" {
		log.Fatal("Broker URL not provided")
	}

	batteryLevelGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "battery_percentage",
		Help:      "Battery level percentage (0-100)",
	}, labels)

	energyExportedGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "energy_exported",
		Help:      "Total energy exported from individual meters (Wh)",
	}, labels)

	energyImportedGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "energy_imported",
		Help:      "Total energy imported from individual meters (Wh)",
	}, labels)

	powerGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "instantaneous_power",
		Help:      "Instantaneous power of individual CT clamps (W)",
	}, labels)

	prometheus.MustRegister(batteryLevelGauge, energyExportedGauge, energyImportedGauge, powerGauge)

	teslaClient := NewTEGClient(*powerwallIP, *password, batteryLevelGauge, energyExportedGauge, energyImportedGauge, powerGauge)
	if err := teslaClient.Login(); err != nil {
		log.Fatal(err)
	}

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

	if *evChargeLevelTopic != "" && *openEVSEAddr != "" {
		mqttClient.Subscribe(*evChargeLevelTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			log.Printf("Got message: %s", msg.Payload())
			var bLevel struct {
				EvBatteryLevel float64 `json:"ev_battery_level"`
			}

			if err := json.Unmarshal(msg.Payload(), &bLevel); err != nil {
				log.Fatal(err)
			}

			// Fetch current EVSE config
			resp, err := http.Get(fmt.Sprintf("http://%s/config", *openEVSEAddr))
			if err != nil {
				log.Fatal(err)
			}

			var evConfigResp struct {
				ChargeMode string `json:"charge_mode"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&evConfigResp); err != nil {
				log.Fatal(err)
			}
			_ = resp.Body.Close()

			log.Printf("%+v", evConfigResp)

			if *dryRun {
				return
			}

			if bLevel.EvBatteryLevel < 50 && evConfigResp.ChargeMode != "fast" {
				log.Println("Charge level too low - disabling PV divert")
				resp, err = http.Post(fmt.Sprintf("http://%s/config", *openEVSEAddr), "application/json", strings.NewReader(`{"charge_mode": "fast"}`))
				if err != nil {
					log.Fatal(err)
				}
				_ = resp.Body.Close()
			}

			if bLevel.EvBatteryLevel > 60 && evConfigResp.ChargeMode != "eco" {
				log.Println("Charge level high enough - switch to PV divert")
				resp, err = http.Post(fmt.Sprintf("http://%s/config", *openEVSEAddr), "application/json", strings.NewReader(`{"charge_mode": "eco"}`))
				if err != nil {
					log.Fatal(err)
				}
				_ = resp.Body.Close()
			}
		}).Wait()
	}

	for {
		metersResp, err := teslaClient.GetMeterAggregates()
		if err != nil {
			log.Fatal(err)
		}

		if v, ok := metersResp["site"]; ok {
			if !*dryRun {
				token := mqttClient.Publish(*gridPowerInverseTopic, 0, false, fmt.Sprintf("%f", -v.InstantPower))
				_ = token.Wait()
				if err := token.Error(); err != nil {
					log.Fatal(err)
				}
			} else {
				log.Printf("[DRY RUN] Sending %f on %s", -v.InstantPower, *gridPowerInverseTopic)
			}
		}

		_, err = teslaClient.GetStateOfEnergy()
		if err != nil {
			log.Fatal(err)
		}

		if *openEVSEAddr != "" {
			resp, err := http.Get(fmt.Sprintf("http://%s/status", *openEVSEAddr))
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
			energyImportedGauge.WithLabelValues("ev").Set(float64(evStatusResp.WattHour) + float64(evStatusResp.WattSec)/3600.0)
		}

		<-ticker.C
	}
}
