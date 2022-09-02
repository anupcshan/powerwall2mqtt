package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
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

	currentGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "instantaneous_current",
		Help:      "Instantaneous current of individual CT clamps (A)",
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

	tempGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "temp",
		Help:      "Temperature sensor reading (°C)",
	}, labels)

	connectedGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "connected",
		Help:      "Is entity (ev, mqtt, etc) connected (1 for yes, 0 otherwise)",
	}, labels)

	gridServicesEnabledGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "grid_services_enabled",
		Help:      "Is Powerwall feeding grid in VPP event?",
	}, labels)

	prometheus.MustRegister(
		batteryLevelGauge,
		currentGauge,
		energyExportedGauge,
		energyImportedGauge,
		powerGauge,
		tempGauge,
		connectedGauge,
		gridServicesEnabledGauge,
	)

	teslaClient := NewTEGClient(*powerwallIP, *password, batteryLevelGauge, energyExportedGauge, energyImportedGauge, powerGauge, gridServicesEnabledGauge)
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

	var evseClient *openEVSEClient
	if *openEVSEAddr != "" {
		evseClient = &openEVSEClient{
			// Nearly all requests should complete in <100ms.
			client:              &http.Client{Timeout: 2 * time.Second},
			openEVSEAddr:        *openEVSEAddr,
			currentGauge:        currentGauge,
			energyImportedGauge: energyImportedGauge,
			powerGauge:          powerGauge,
			tempGauge:           tempGauge,
			connectedGauge:      connectedGauge,
		}
	}

	if *evChargeLevelTopic != "" && evseClient != nil {
		mqttClient.Subscribe(*evChargeLevelTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			log.Printf("Got message: %s", msg.Payload())
			var bLevel struct {
				EvBatteryLevel float64 `json:"ev_battery_level"`
			}

			if err := json.Unmarshal(msg.Payload(), &bLevel); err != nil {
				log.Fatal(err)
			}

			batteryLevelGauge.WithLabelValues("ev").Set(bLevel.EvBatteryLevel)

			// Fetch current EVSE config
			evConfigResp, err := evseClient.GetConfig()
			if err != nil {
				// Can happen if OpenEVSE device is down for a while - log it and continue operating
				log.Printf("Error getting config from OpenEVSE: %v", err)
				return
			}

			if *dryRun {
				return
			}

			if bLevel.EvBatteryLevel < 50 && evConfigResp.ChargeMode != "fast" {
				log.Println("Charge level too low - disabling PV divert")
				if err := evseClient.SetConfig(EVSEConfig{
					ChargeMode: "fast",
				}); err != nil {
					log.Fatal(err)
				}
			}

			if bLevel.EvBatteryLevel > 60 && evConfigResp.ChargeMode != "eco" {
				log.Println("Charge level high enough - switch to PV divert")
				if err := evseClient.SetConfig(EVSEConfig{
					ChargeMode: "eco",
				}); err != nil {
					log.Fatal(err)
				}
			}
		}).Wait()
	}

	for {
		gridStatus, err := teslaClient.GetGridStatus()
		if err != nil {
			log.Fatal(err)
		}

		metersResp, err := teslaClient.GetMeterAggregates()
		if err != nil {
			log.Fatal(err)
		}

		if v, ok := metersResp["site"]; ok {
			availablePower := -v.InstantPower
			if gridStatus.GridServicesActive {
				availablePower = 0
			}

			if !*dryRun {
				token := mqttClient.Publish(*gridPowerInverseTopic, 0, false, fmt.Sprintf("%f", availablePower))
				_ = token.Wait()
				if err := token.Error(); err != nil {
					log.Fatal(err)
				}
			} else {
				log.Printf("[DRY RUN] Sending %f on %s", availablePower, *gridPowerInverseTopic)
			}
		}

		_, err = teslaClient.GetStateOfEnergy()
		if err != nil {
			log.Fatal(err)
		}

		_, err = teslaClient.GetOperation()
		if err != nil {
			log.Fatal(err)
		}

		if evseClient != nil {
			if _, err := evseClient.GetStatus(); err != nil {
				// Can happen if OpenEVSE device is down for a while - log it and continue operating
				log.Printf("Error getting status from OpenEVSE: %v", err)
			}
		}

		<-ticker.C
	}
}
