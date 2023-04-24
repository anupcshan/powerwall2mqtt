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
	observedBattery observedValues = 1 << iota
	observedSolar
	observedLR
	observedStrategy
	observedTemp
)

const (
	maxAmps       = 40
	minAmps       = 8
	volts         = 240
	maxTemp       = 500     // 50°C
	minSitePowerW = -100000 // 100kW. Don't expect single home to pull more than this from the grid.
)

type controller struct {
	lock       sync.Mutex
	cond       *sync.Cond
	seenValues observedValues

	// Sensors
	evBatteryLevelPercent float64 // 0.0 - 100.0
	exportedSolarW        float64
	tempDeciCelsius       int64
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
	updateSensor(c, &c.evBatteryLevelPercent, batt, observedBattery)
}

func (c *controller) SetExportedSolarW(solarW float64) {
	updateSensor(c, &c.exportedSolarW, solarW, observedSolar)
}

func (c *controller) SetLoadReduction(enabled bool) {
	updateSensor(c, &c.loadReductionEnabled, enabled, observedLR)
}

func (c *controller) SetControllerStrategy(strategy strategy) {
	updateSensor(c, &c.controllerStrategy, strategy, observedStrategy)
}

func (c *controller) SetEVSETempDeciCelsius(temp int64) {
	updateSensor(c, &c.tempDeciCelsius, temp, observedTemp)
}

func (c *controller) seen(checks ...observedValues) bool {
	for _, check := range checks {
		if c.seenValues&check != check {
			return false
		}
	}

	return true
}

func (c *controller) computeMaxPower() int32 {
	if !c.seen(observedStrategy) {
		// Not enough data to make informed choices - try again when we have more data.
		return math.MinInt32
	}

	var maxPower int32 = math.MaxInt32

	if c.seen(observedTemp) && c.tempDeciCelsius > maxTemp {
		maxPower = volts * minAmps
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

	if c.seen(observedBattery) {
		if c.evBatteryLevelPercent < 60 {
			return maxPower
		}
	}

	if c.seen(observedSolar) {
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
