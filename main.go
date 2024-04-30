package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	indexTmpl = `
<!DOCTYPE html>
<html>
<head>
	<meta http-equiv="refresh" content="2">
	<style>
	table, th, td {
		border: 1px solid black;
		border-collapse: collapse;
		padding: 5px;
	}
	</style>
</head>
<body>
	<table>
		<tr>
			<th>Solar</th>
			<th>Load</th>
		</tr>
		<tr>
			<td>{{printf "%.0f" .SolarW}} W</td>
			<td>{{printf "%.0f" .LoadW}} W</td>
		</tr>
	</table>
</body>
</html>
`
)

var (
	labels = []string{"meter"}
	tmpl   = template.Must(template.New("index").Parse(indexTmpl))
)

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
	evChargeStrategyTopic := flag.String("ev-charge-strategy-topic", "", "MQTT topic to read/write the current charge strategy")

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

	cont := NewController(
		func(limit float64) error {
			if *dryRun {
				log.Printf("[DRY RUN] Setting eco power limit to %f", limit)
				return nil
			} else {
				token := mqttClient.Publish(*gridPowerInverseTopic, 0, false, fmt.Sprintf("%f", limit))
				_ = token.Wait()
				return token.Error()
			}
		},
	)

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		solarW := cont.GetExportedSolarW()
		loadW := cont.GetLoadW()
		if err := tmpl.Execute(w, struct {
			SolarW float64
			LoadW  float64
		}{
			solarW,
			loadW,
		}); err != nil {
			log.Printf("Error executing template: %v", err)
		}
	}))
	go func() {
		http.ListenAndServe(*listen, nil)
	}()

	if *evChargeLevelTopic != "" {
		mqttClient.Subscribe(*evChargeLevelTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			log.Printf("Got message: %s", msg.Payload())
			var bLevel struct {
				EvBatteryLevel float64 `json:"ev_battery_level"`
			}

			if err := json.Unmarshal(msg.Payload(), &bLevel); err != nil {
				log.Fatal(err)
			}

			batteryLevelGauge.WithLabelValues("ev").Set(bLevel.EvBatteryLevel)
			cont.SetEVBatteryLevelPercent(bLevel.EvBatteryLevel)
		}).Wait()
	}

	if *evChargeStrategyTopic != "" {
		mqttClient.Subscribe(*evChargeStrategyTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			log.Printf("Got message: %s", msg.Payload())

			var strategy strategy
			switch string(msg.Payload()) {
			case "auto":
				strategy = strategyAuto
			case "fullspeed":
				strategy = strategyFullSpeed
			case "overnight":
				strategy = strategyOvernight
			default:
				log.Fatalf("Charge strategy %s unknown", msg.Payload())
			}
			cont.SetControllerStrategy(strategy)
		}).Wait()
	}

	go func() {
		if err := cont.Loop(); err != nil {
			log.Fatal(err)
		}
	}()

	for {
		gridStatus, err := teslaClient.GetGridStatus()
		if err != nil {
			log.Fatal(err)
		}

		cont.SetLoadReduction(gridStatus.GridServicesActive)

		metersResp, err := teslaClient.GetMeterAggregates()
		if err != nil {
			log.Fatal(err)
		}

		if v, ok := metersResp["site"]; ok {
			cont.SetExportedSolarW(-v.InstantPower)
		} else {
			cont.SetExportedSolarW(0)
		}

		if v, ok := metersResp["load"]; ok {
			cont.SetLoadW(v.InstantPower)
		} else {
			cont.SetExportedSolarW(0)
		}

		if v, ok := metersResp["battery"]; ok {
			cont.SetExportedBatteryW(v.InstantPower)
		} else {
			cont.SetExportedBatteryW(0)
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
			evseStatus, err := evseClient.GetStatus()
			if err != nil {
				// Can happen if OpenEVSE device is down for a while - log it and continue operating
				log.Printf("Error getting status from OpenEVSE: %v", err)
			} else {
				cont.SetEVSETemp(Temperature(evseStatus.Temp) * DeciCelcius)
			}
		}

		<-ticker.C
	}
}
