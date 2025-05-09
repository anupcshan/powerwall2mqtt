package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
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
	<script src="/assets/htmx.org@1.9.12/dist/htmx.min.js"></script>
	<script src="/assets/htmx.org@1.9.12/dist/ext/sse.js"></script>
	<style>
	table, th, td {
		border: 1px solid black;
		border-collapse: collapse;
		padding: 5px;
		text-align: center;
	}
	</style>
</head>
<body>
	<div hx-ext="sse" sse-connect="/events">
		<div>Last updated: <span sse-swap="last-updated">Never</span></div><br/>
		<table>
			<tr>
				<th>Solar</th>
				<th>Load</th>
				<th>Grid</th>
				<th>Powerwall Level</th>
				<th>Operation Mode</th>
			</tr>
			<tr>
				<td sse-swap="solar">Pending</td>
				<td sse-swap="load">Pending</td>
				<td sse-swap="site">Pending</td>
				<td sse-swap="powerwall-batt-level">Pending</td>
				<td sse-swap="powerwall-oper-mode">Pending</td>
			</tr>
		</table>

		<br/>

		<div><b>EVSE:</b></div><br/>

		<table>
			<tr>
				<th>Temp</th>
				<th>Current</th>
				<th>Power Budget</th>
				<th>Strategy</th>
				<th>EV</th>
			</tr>
			<tr>
				<td sse-swap="evse-temp">Pending</td>
				<td sse-swap="evse-current">Pending</td>
				<td sse-swap="evse-budget">Pending</td>
				<td sse-swap="evse-strategy">Pending</td>
				<td sse-swap="ev-connected">Pending</td>
			</tr>
		</table>
	</div>
</body>
</html>
`
)

var (
	labels = []string{"meter"}
	//go:embed assets
	assets embed.FS
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
	debug := flag.Bool("debug", false, "Print debug logs")
	dryRun := flag.Bool("dry-run", true, "Dry run mode (disable any writes in dry run mode)")
	haTopic := flag.String("ha-topic", "evcharger", "MQTT topic to read/write the current charge strategy")

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

	energyLevelGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "energy",
		Name:      "energy_level",
		Help:      "Energy levels for individual meters (Wh)",
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
		energyLevelGauge,
		powerGauge,
		tempGauge,
		connectedGauge,
		gridServicesEnabledGauge,
	)

	teslaClient := NewTEGClient(
		*powerwallIP,
		*password,
		*debug,
		batteryLevelGauge,
		energyExportedGauge,
		energyImportedGauge,
		energyLevelGauge,
		powerGauge,
		gridServicesEnabledGauge,
	)
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

	var reporter Reporter = NoopReporter{}

	if *haTopic != "" {
		reporter = NewMQTTReporter(mqttClient, *haTopic)
	}

	var latestEVBudget int32
	cont := NewController(
		reporter,
		func(limit int32) error {
			atomic.StoreInt32(&latestEVBudget, limit)
			if *dryRun {
				log.Printf("[DRY RUN] Setting eco power limit to %d", limit)
				return nil
			} else {
				token := mqttClient.Publish(*gridPowerInverseTopic, 0, false, fmt.Sprintf("%d", limit))
				_ = token.Wait()
				return token.Error()
			}
		},
	)

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/assets/", http.FileServer(http.FS(assets)))
	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, indexTmpl)
	}))

	var controllerStrategy atomic.Value
	controllerStrategy.Store("")

	http.Handle("/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		dataCache := make(map[string]string)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			data := map[string]string{
				"solar":                fmt.Sprintf("%.0f W", cont.GetSolarW()),
				"load":                 fmt.Sprintf("%.0f W", cont.GetLoadW()),
				"site":                 fmt.Sprintf("%.0f W", -cont.GetExportedSolarW()),
				"powerwall-batt-level": fmt.Sprintf("%.1f%%", cont.GetPowerwallBatteryLevel()),
				"powerwall-oper-mode":  cont.GetOperationMode().String(),
				"evse-temp":            cont.GetEVSETemp().String(),
				"evse-current":         fmt.Sprintf("%.1f A", float64(cont.GetEVSECurrent())/1000.0),
				"evse-budget":          fmt.Sprintf("%d W", atomic.LoadInt32(&latestEVBudget)),
				"evse-strategy":        controllerStrategy.Load().(string),
				"ev-connected":         cont.GetEVConnected().String(),
				"last-updated":         time.Now().Format(time.DateTime),
			}

			for k, v := range data {
				if dataCache[k] == v {
					continue
				}

				fmt.Fprintf(w, "event: %s\n", k)
				fmt.Fprintf(w, "data: %s\n", v)
				fmt.Fprint(w, "\n\n")
				dataCache[k] = v
			}

			w.(http.Flusher).Flush()

			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}))
	go func() {
		http.ListenAndServe(*listen, nil)
	}()

	if *haTopic != "" {
		chargeStrategyTopic := fmt.Sprintf("cmnd/%s/MODE", *haTopic)
		mqttClient.Subscribe(chargeStrategyTopic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			log.Printf("Got message: %s", msg.Payload())

			var strategy strategy
			switch string(msg.Payload()) {
			case "solar":
				strategy = strategySolar
			case "fullspeed":
				strategy = strategyFullSpeed
			case "offpeak":
				strategy = strategyOffpeak
			default:
				log.Fatalf("Charge strategy %s unknown", msg.Payload())
			}
			controllerStrategy.Store(string(msg.Payload()))
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

		_, err = teslaClient.GetSystemStatus()
		if err != nil {
			log.Fatal(err)
		}

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
			cont.SetLoadW(0)
		}

		if v, ok := metersResp["solar"]; ok {
			cont.SetSolarW(v.InstantPower)
		} else {
			cont.SetSolarW(0)
		}

		if v, ok := metersResp["battery"]; ok {
			cont.SetExportedBatteryW(v.InstantPower)
		} else {
			cont.SetExportedBatteryW(0)
		}

		soe, err := teslaClient.GetStateOfEnergy()
		if err != nil {
			log.Fatal(err)
		}
		cont.SetPowerwallBatteryLevelPercent(soe.Percentage)

		if op, err := teslaClient.GetOperation(); err != nil {
			log.Fatal(err)
		} else {
			cont.SetOperationMode(op.Mode)
		}

		if evseClient != nil {
			evseStatus, err := evseClient.GetStatus()
			if err != nil {
				// Can happen if OpenEVSE device is down for a while - log it and continue operating
				log.Printf("Error getting status from OpenEVSE: %v", err)
			} else {
				cont.SetEVSETemp(Temperature(evseStatus.Temp) * DeciCelcius)
				cont.SetEVSECurrent(evseStatus.MilliAmp)
				cont.SetEVConnected(evseStatus.Vehicle == 1)
			}
		}

		<-ticker.C
	}
}
