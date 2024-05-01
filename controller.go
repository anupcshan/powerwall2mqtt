package main

import (
	"math"
	"sync"
	"time"
)

type strategy int

const (
	strategyUnknown strategy = iota
	strategyAuto
	strategyFullSpeed
	strategyOvernight
)

type observedValues int

const (
	observedEVBattery observedValues = 1 << iota
	observedExportedSolar
	observedBattery
	observedLR
	observedStrategy
	observedTemp
	observedLoad
	observedSolar
)

type Temperature int64

const (
	DeciCelcius Temperature = 1
	Celsius                 = 10 * DeciCelcius
)

const (
	maxAmps       = 40
	minAmps       = 8
	volts         = 240
	minSitePowerW = -100000 // 100kW. Don't expect single home to pull more than this from the grid.
)

var tempClamps = []struct {
	temp    Temperature
	maxAmps int32
}{
	{50 * Celsius, 8},
	{49 * Celsius, 12},
	{48 * Celsius, 16},
	{47 * Celsius, 24},
	{46 * Celsius, 32},
}

type controller struct {
	lock       sync.Mutex
	cond       *sync.Cond
	seenValues observedValues

	// Sensors
	evBatteryLevelPercent float64 // 0.0 - 100.0
	exportedBatteryW      float64
	exportedSolarW        float64
	solarW                float64
	loadW                 float64
	temp                  Temperature
	loadReductionEnabled  bool
	controllerStrategy    strategy
	setEcoPowerLimit      func(float64) error
}

func NewController(
	setEcoPowerLimit func(float64) error,
) *controller {
	cont := &controller{
		setEcoPowerLimit: setEcoPowerLimit,
	}

	cont.cond = sync.NewCond(&cont.lock)
	return cont
}

func updateSensor[T comparable](c *controller, oldValue *T, newValue T, obs observedValues) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var shouldNotify bool
	if *oldValue != newValue {
		*oldValue = newValue
		shouldNotify = true
	}
	c.seenValues |= obs
	if shouldNotify {
		c.cond.Signal()
	}
}

func (c *controller) SetEVBatteryLevelPercent(batt float64) {
	updateSensor(c, &c.evBatteryLevelPercent, batt, observedEVBattery)
}

func (c *controller) SetExportedBatteryW(batteryW float64) {
	updateSensor(c, &c.exportedBatteryW, batteryW, observedBattery)
}

func (c *controller) SetExportedSolarW(solarW float64) {
	updateSensor(c, &c.exportedSolarW, solarW, observedExportedSolar)
}

func (c *controller) SetSolarW(solarW float64) {
	updateSensor(c, &c.solarW, solarW, observedSolar)
}

func (c *controller) SetLoadW(loadW float64) {
	updateSensor(c, &c.loadW, loadW, observedLoad)
}

func (c *controller) SetLoadReduction(enabled bool) {
	updateSensor(c, &c.loadReductionEnabled, enabled, observedLR)
}

func (c *controller) SetControllerStrategy(strategy strategy) {
	updateSensor(c, &c.controllerStrategy, strategy, observedStrategy)
}

func (c *controller) SetEVSETemp(temp Temperature) {
	updateSensor(c, &c.temp, temp, observedTemp)
}

func (c *controller) GetSolarW() float64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.solarW
}

func (c *controller) GetLoadW() float64 {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.loadW
}

func (c *controller) seen(checks ...observedValues) bool {
	for _, check := range checks {
		if c.seenValues&check != check {
			return false
		}
	}

	return true
}

func maxPowerForTemp(temp Temperature) int32 {
	var maxPower int32 = math.MaxInt32

	for _, tempClamp := range tempClamps {
		if temp > tempClamp.temp {
			maxPower = volts * tempClamp.maxAmps
			return maxPower
		}
	}

	return maxPower
}

func (c *controller) computeMaxPower() int32 {
	if !c.seen(observedStrategy) {
		// Not enough data to make informed choices - try again when we have more data.
		return math.MinInt32
	}

	var maxPower int32 = math.MaxInt32

	if c.seen(observedTemp) {
		maxPower = maxPowerForTemp(c.temp)
	}

	if c.controllerStrategy == strategyFullSpeed {
		return maxPower
	}

	if c.controllerStrategy == strategyOvernight && time.Now().Hour() < 6 /* 12AM to 6AM */ {
		return maxPower
	}

	// Load reduction is fairly high priority - it usually means bad weather (heatwave or storm).
	// Don't try to charge during this time.
	// It is up to the operator to manually charge at full speed before we're in bad weather.
	if c.seen(observedLR) && c.loadReductionEnabled {
		return 0
	}

	if c.seen(observedBattery) && c.exportedBatteryW > 200 {
		// If battery is exporting non-trivial power, shut off EV charging.
		// This can happen if Tesla gateway is set to "timed based control".
		// During peak period, solar gets exported to grid and battery exports
		// to load. No point charging the EV during this time.
		return 0
	}

	if c.seen(observedEVBattery) {
		if c.evBatteryLevelPercent < 60 {
			return maxPower
		}
	}

	if c.seen(observedExportedSolar) {
		if maxPower < int32(c.exportedSolarW) {
			return maxPower
		}

		return int32(c.exportedSolarW)
	}

	return maxPower
}

func (c *controller) singleLoop() error {
	c.lock.Lock()
	c.cond.Wait()

	defer c.lock.Unlock()

	maxPower := c.computeMaxPower()

	if maxPower < minSitePowerW {
		// Not enough data. Don't take action
		return nil
	}

	if maxPower > volts*maxAmps {
		maxPower = volts * maxAmps
	}

	return c.setEcoPowerLimit(float64(maxPower))
}

func (c *controller) Loop() error {
	for {
		if err := c.singleLoop(); err != nil {
			return err
		}
	}
}
